package pricing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RefreshURL is the upstream LiteLLM price table. Hardcoded on purpose: the
// config exposes only an off switch and per-model overrides, which already
// cover the air-gapped and the disagrees-with-the-table cases without turning
// the price feed into an arbitrary user-controlled fetch target.
const RefreshURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

const ModelsDevURL = "https://models.dev/api.json"

const (
	// cacheFile is the refreshed table's name inside the data dir.
	cacheFile          = "prices-litellm.json"
	modelsDevCacheFile = "prices-modelsdev.json"
	// refreshInterval throttles the network fetch: a cache younger than this
	// is reused untouched, so a caller may invoke Refresh every cycle.
	refreshInterval = 24 * time.Hour
	// refreshTimeout bounds the whole fetch. Pricing is best-effort; it must
	// never hold a collection cycle open.
	refreshTimeout = 10 * time.Second
	// maxTableBytes caps the download. Upstream is ~1.7MB; this is headroom,
	// not a target.
	maxTableBytes = 32 << 20
)

// nowFn is the clock seam used to date a refreshed table (tests pin it).
var nowFn = func() time.Time { return time.Now().UTC() }

// Options configures the pricing engine.
type Options struct {
	// DataDir holds the refreshed table's cache. Empty disables both reading
	// the cache and refreshing it, leaving overrides plus the embedded floor.
	DataDir string
	// Refresh enables the runtime refresh (config pricing.refresh, default
	// true). It gates the network only: an existing cache is still read.
	Refresh bool
	// Overrides are per-model rates that beat every table.
	Overrides map[string]Rates
}

// Engine resolves charges through the ladder. It is safe for concurrent use:
// Refresh swaps the middle rung under a write lock while Price reads it.
type Engine struct {
	overrides         *Table
	embedded          *Table
	modelsDevEmbedded *Table

	mu                   sync.RWMutex
	refreshed            *Table
	revision             string
	modelsDevRefreshed   *Table
	modelsDevLastAttempt time.Time

	dataDir string
	refresh bool
	client  *http.Client
	// url is RefreshURL in production. It is a field, not a constant read, only
	// so tests can point the fetch at a local server; nothing outside this
	// package can change where prices come from.
	url          string
	modelsDevURL string
}

// New loads embedded tables and local caches without touching the network.
// Models.dev uses only a fresh valid cache; otherwise its embedded table serves.
func New(opt Options) *Engine {
	e := &Engine{
		embedded:          embeddedTable(),
		modelsDevEmbedded: embeddedModelsDevTable(),
		dataDir:           opt.DataDir,
		refresh:           opt.Refresh,
		client:            &http.Client{Timeout: refreshTimeout},
		url:               RefreshURL,
		modelsDevURL:      ModelsDevURL,
	}
	if len(opt.Overrides) > 0 {
		models := make(map[string]Rates, len(opt.Overrides))
		for k, v := range opt.Overrides {
			if v.Priceable() {
				models[k] = v
			}
		}
		if len(models) > 0 {
			e.overrides = &Table{Source: SourceOverride, Models: models}
		}
	}
	if t, err := loadCache(opt.DataDir); err == nil {
		e.refreshed = t
	}
	if opt.DataDir != "" && fresh(filepath.Join(opt.DataDir, modelsDevCacheFile)) {
		if data, err := os.ReadFile(filepath.Join(opt.DataDir, modelsDevCacheFile)); err == nil {
			e.modelsDevRefreshed, _ = decodeModelsDevSnapshot(data, "modelsdev")
		}
	}
	e.revision = priceRevision(e.overrides, e.refreshed, e.modelsDevRefreshed, e.embedded, e.modelsDevEmbedded)
	return e
}

// Revision identifies the loaded rates. Collectors use it
// to retry older unpriced usage only when the pricing information changes.
func (e *Engine) Revision() string {
	if e == nil {
		return ""
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.revision
}

func priceRevision(tables ...*Table) string {
	// A fetch date changes provenance, not rates. Including it would restart
	// an unfinished historical pass every day even for identical prices.
	rates := make([]map[string]Rates, len(tables))
	for i, table := range tables {
		if table != nil {
			rates[i] = table.Models
		}
	}
	data, err := json.Marshal(rates)
	if err != nil {
		return "" // invalid programmatic rates cannot identify a sync pass
	}
	return fmt.Sprintf("prices-v1-%x", sha256.Sum256(data))
}

// tables returns the ladder in priority order, skipping absent rungs.
func (e *Engine) tables() []*Table {
	e.mu.RLock()
	refreshed := e.refreshed
	modelsDev := e.modelsDevRefreshed
	e.mu.RUnlock()

	out := make([]*Table, 0, 5)
	for _, t := range []*Table{e.overrides, refreshed, modelsDev, e.embedded, e.modelsDevEmbedded} {
		if t != nil && len(t.Models) > 0 {
			out = append(out, t)
		}
	}
	return out
}

// Refresh independently updates the LiteLLM and Models.dev feeds when due.
// Disabled refresh or an empty data dir prevents all downloads. Models.dev
// attempts at most once per 24 hours per engine, reuses a fresh cache across
// restarts, and falls back to its embedded snapshot on download/parse failure.
// LiteLLM retains its last loaded table on failure. Errors do not stop pricing.
func (e *Engine) Refresh(ctx context.Context) error {
	if e == nil || !e.refresh || e.dataDir == "" {
		return nil
	}
	// Each source gets its own attempt; a failed feed must not block the other.
	return errors.Join(e.refreshLiteLLM(ctx), e.refreshModelsDev(ctx))
}

func (e *Engine) refreshLiteLLM(ctx context.Context) error {
	if fresh(filepath.Join(e.dataDir, cacheFile)) {
		return nil
	}

	data, err := e.fetch(ctx)
	if err != nil {
		return err
	}
	raw, err := decodeUpstream(data)
	if err != nil {
		return err
	}
	doc := snapshotDoc{Models: raw}
	doc.Meta.Source = e.url
	doc.Meta.Fetched = nowFn().Format("2006-01-02")

	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("pricing: encode refreshed table: %w", err)
	}
	table, err := decodeSnapshot(encoded, "litellm")
	if err != nil {
		return err
	}

	e.mu.Lock()
	e.refreshed = table
	e.revision = priceRevision(e.overrides, table, e.modelsDevRefreshed, e.embedded, e.modelsDevEmbedded)
	e.mu.Unlock()

	return writeCache(e.dataDir, encoded)
}

func (e *Engine) refreshModelsDev(ctx context.Context) error {
	if e.modelsDevURL == "" {
		return nil
	}
	e.mu.Lock()
	now := nowFn()
	if (!e.modelsDevLastAttempt.IsZero() && now.Sub(e.modelsDevLastAttempt) < refreshInterval) ||
		(e.modelsDevRefreshed != nil && fresh(filepath.Join(e.dataDir, modelsDevCacheFile))) {
		e.mu.Unlock()
		return nil
	}
	e.modelsDevLastAttempt = now
	e.mu.Unlock()

	data, err := e.fetchURL(ctx, e.modelsDevURL)
	if err == nil {
		data, err = modelsDevSnapshot(data)
	}
	var table *Table
	if err == nil {
		table, err = decodeModelsDevSnapshot(data, "modelsdev")
	}
	e.mu.Lock()
	// A failed or invalid refresh uses the embedded Models.dev snapshot.
	// The last good file stays intact, but stale cache data is not promoted.
	e.modelsDevRefreshed = table
	e.revision = priceRevision(e.overrides, e.refreshed, table, e.embedded, e.modelsDevEmbedded)
	e.mu.Unlock()
	if err != nil {
		return fmt.Errorf("pricing: models.dev refresh: %w", err)
	}
	return writePriceCache(e.dataDir, modelsDevCacheFile, data)
}

// fetch downloads the upstream table with a bounded body and timeout.
func (e *Engine) fetch(ctx context.Context) ([]byte, error) {
	return e.fetchURL(ctx, e.url)
}

func (e *Engine) fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("pricing: build refresh request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch price table: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing: fetch price table: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTableBytes+1))
	if err != nil {
		return nil, fmt.Errorf("pricing: read price table: %w", err)
	}
	if len(data) > maxTableBytes {
		return nil, fmt.Errorf("pricing: price table exceeds size limit")
	}
	return data, nil
}

// fresh reports whether the cache file exists and is younger than
// refreshInterval, i.e. whether the network fetch can be skipped.
func fresh(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return nowFn().Sub(fi.ModTime()) < refreshInterval
}

// loadCache reads the previously refreshed table from the data dir.
func loadCache(dataDir string) (*Table, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("pricing: no data dir")
	}
	data, err := os.ReadFile(filepath.Join(dataDir, cacheFile))
	if err != nil {
		return nil, err
	}
	return decodeSnapshot(data, "litellm")
}

// writeCache stores the refreshed table atomically: a torn write would poison
// every later startup, and the ladder has no way to tell a truncated cache from
// a genuinely small one.
func writeCache(dataDir string, data []byte) error {
	return writePriceCache(dataDir, cacheFile, data)
}

func writePriceCache(dataDir, filename string, data []byte) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("pricing: create data dir %s: %w", dataDir, err)
	}
	tmp, err := os.CreateTemp(dataDir, filename+".tmp*")
	if err != nil {
		return fmt.Errorf("pricing: create price cache: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("pricing: write price cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pricing: close price cache: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("pricing: chmod price cache: %w", err)
	}
	if err := os.Rename(name, filepath.Join(dataDir, filename)); err != nil {
		return fmt.Errorf("pricing: install price cache: %w", err)
	}
	return nil
}
