// Package hermes implements an AGGREGATE adapter for the Hermes CLI.
//
// Hermes records per-session token counters in a SQLite database at
// <home>/state.db. A single session's columns GROW as the session runs across
// many polls, so this adapter is aggregate: it emits one AggregateSnapshot per
// session row (the current cumulative totals) keyed by the session id. The
// collector compares each snapshot against the last stored state and appends a
// positive delta as an immutable event, so totals never undercount and survive
// a later deletion of the source row.
//
// CRITICAL: strictly read-only. The SQLite database is opened with mode=ro so a
// poll can never create, lock for writing, or modify the agent's state.
package hermes

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the pure-Go "sqlite" driver

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

const (
	// HomeEnv may hold a comma-separated list of Hermes home directories.
	HomeEnv = "HERMES_HOME"
	// dbName is the SQLite state database within a Hermes home.
	dbName = "state.db"
	// driverName is the modernc.org/sqlite database/sql driver name.
	driverName = "sqlite"
	// metaProject preserves the adapter's established session project label.
	metaProject = "hermes"
)

// sessionsQuery selects every session that has a model attributed. The token
// columns are cumulative running totals that grow as the session continues.
const sessionsQuery = `SELECT id, model, billing_provider, started_at, ended_at,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens
	FROM sessions
	WHERE model IS NOT NULL AND TRIM(model) != ''`

// Adapter reads the Hermes state database. Read-only.
type Adapter struct{}

// New returns a Hermes adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolHermes }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Hermes" }

// Capabilities declares what this project can say about Hermes.
//
// Cost is COMPUTED and there is NO activity at all: this adapter references
// model.ActivityEvent nowhere, so its surface exposes usage and nothing else.
//
// The v2026.9.11 live session capture and exact writer accounting evidence
// are recorded in testdata/live-2026.9.11.provenance.md.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolHermes,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(model.ToolHermes),
		Tier:      model.TierLive,
	}
}

// homes returns the configured Hermes home directories. HERMES_HOME may be a
// comma-separated list; otherwise the discovery root (override or ~/.hermes).
func (a Adapter) homes(cfg adapter.DiscoverConfig) []string {
	if env := strings.TrimSpace(os.Getenv(HomeEnv)); env != "" {
		var out []string
		for _, p := range strings.Split(env, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	def := ""
	if cfg.Home != "" {
		def = filepath.Join(cfg.Home, ".hermes")
	}
	return []string{cfg.Root(model.ToolHermes, def)}
}

// Discover locates each <home>/state.db that exists as a regular file.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source
	for _, home := range a.homes(cfg) {
		if home == "" {
			continue
		}
		db := filepath.Join(home, dbName)
		if !isFile(db) {
			continue
		}
		if _, dup := seen[db]; dup {
			continue
		}
		seen[db] = struct{}{}
		srcs = append(srcs, adapter.Source{
			Tool:  model.ToolHermes,
			Class: model.Aggregate,
			Path:  db,
			Label: "Hermes sessions: " + db,
			Meta:  map[string]string{"home": home},
		})
	}
	return srcs, nil
}

// ckptState is the incremental gate persisted in the checkpoint. Level 1: the
// db + WAL file stamps — every Hermes commit touches one of them, so equal
// stamps mean the database cannot hold new data and is not even opened.
// Level 2: a content hash per session row — session rows GROW IN PLACE across
// polls (a rowid watermark is inapplicable), so an unchanged hash skips the
// row's snapshot and with it the collector's per-cell state read/write.
type ckptState struct {
	DBSize   int64             `json:"dbSize"`
	DBMTime  int64             `json:"dbMtime"`
	WALSize  int64             `json:"walSize"`
	WALMTime int64             `json:"walMtime"`
	Sessions map[string]string `json:"sessions,omitempty"`
}

// fileStamp returns (size, mtimeNS) for path, or (-1, 0) when absent — a WAL
// appearing or vanishing must break gate equality.
func fileStamp(path string) (int64, int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return -1, 0
	}
	return fi.Size(), fi.ModTime().UnixNano()
}

// Collect opens the state database read-only and emits one AggregateSnapshot
// per session row. A malformed/unreadable row is skipped rather than failing
// the whole cycle; a non-fatal error is returned describing skipped rows.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental applies the two-level gate described on ckptState. A nil
// cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	gate := ckptState{}
	gate.DBSize, gate.DBMTime = fileStamp(src.Path)
	gate.WALSize, gate.WALMTime = fileStamp(src.Path + "-wal")

	var prev ckptState
	if cp != nil && cp.State != "" {
		if err := json.Unmarshal([]byte(cp.State), &prev); err == nil {
			if prev.DBSize == gate.DBSize && prev.DBMTime == gate.DBMTime &&
				prev.WALSize == gate.WALSize && prev.WALMTime == gate.WALMTime {
				return adapter.Observation{}, nil // untouched db: skip, keep stored checkpoint
			}
		}
	}

	db, err := openReadOnly(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("hermes: open %s: %w", src.Path, err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, sessionsQuery)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: sessions") || strings.Contains(err.Error(), "no such column:") {
			return adapter.Observation{}, fmt.Errorf("hermes: query %s: %w: %w", src.Path, adapter.ErrSourceFormat, err)
		}
		return adapter.Observation{}, fmt.Errorf("hermes: query %s: %w", src.Path, err)
	}
	defer rows.Close()

	now := time.Now().UTC()
	gate.Sessions = make(map[string]string)
	var snaps []model.AggregateSnapshot
	var skipped int
	for rows.Next() {
		var (
			id, mdl                       string
			provider, startedAt, endedAt  sql.NullString
			input, output                 sql.NullInt64
			cacheRead, cacheWrite, reason sql.NullInt64
		)
		if err := rows.Scan(&id, &mdl, &provider, &startedAt, &endedAt,
			&input, &output, &cacheRead, &cacheWrite, &reason); err != nil {
			skipped++
			continue
		}
		id = strings.TrimSpace(id)
		mdl = strings.TrimSpace(mdl)
		if id == "" || mdl == "" {
			skipped++
			continue
		}

		hash := rowHash(id, mdl, provider.String, startedAt.String, endedAt.String,
			input.Int64, output.Int64, cacheRead.Int64, cacheWrite.Int64, reason.Int64)
		gate.Sessions[id] = hash
		if prev.Sessions[id] == hash {
			continue // row content unchanged since the last applied cycle
		}

		in := adapter.NonNeg(input.Int64)
		out := adapter.NonNeg(output.Int64)
		cCreate := adapter.NonNeg(cacheWrite.Int64) // cache_write_tokens -> CacheCreation
		cRead := adapter.NonNeg(cacheRead.Int64)
		reasoning := adapter.NonNeg(reason.Int64)
		// Hermes v2026.9.11 CanonicalUsage separates the cache input buckets
		// and reads reasoning from output-token details. Its total_tokens
		// includes output once; see the versioned capture provenance.
		total := in + out + cCreate + cRead

		// Prefer the row's real timestamps so a delta accrued while the daemon
		// was down lands inside the session's window, not as a spike at the
		// restart second: ended_at first, started_at next, poll time last.
		obs := now
		if ts := parseTime(endedAt.String); !ts.IsZero() {
			obs = ts
		} else if ts := parseTime(startedAt.String); !ts.IsZero() {
			obs = ts
		}

		snaps = append(snaps, model.AggregateSnapshot{
			Tool:                model.ToolHermes,
			Key:                 id,
			Model:               mdl,
			Provider:            strings.TrimSpace(provider.String),
			SessionID:           id,
			Project:             metaProject,
			ObservedTime:        obs,
			InputTokens:         in,
			OutputTokens:        out,
			CacheCreationTokens: cCreate,
			CacheReadTokens:     cRead,
			ReasoningTokens:     reasoning,
			TotalTokens:         total,
			SourcePath:          src.Path,
			Raw:                 rawJSON(provider.String, startedAt.String),
		})
	}
	if err := rows.Err(); err != nil {
		// Incomplete scan: no checkpoint, so the next cycle re-reads in full.
		return adapter.Observation{Snapshots: snaps}, fmt.Errorf("hermes: iterate %s: %w", src.Path, err)
	}

	obs := adapter.Observation{Snapshots: snaps}
	if skipped > 0 {
		return obs, fmt.Errorf("hermes: %w: skipped %d malformed session row(s) in %s", adapter.ErrSourceFormat, skipped, src.Path)
	}
	if stateJSON, err := json.Marshal(gate); err == nil {
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: model.ToolHermes, SourcePath: src.Path, State: string(stateJSON),
		}
	}
	return obs, nil
}

// rowHash fingerprints one session row's read columns. FNV-1a is sufficient:
// a collision only skips one poll of one row until the row next changes.
func rowHash(id, mdl, provider, startedAt, endedAt string, nums ...int64) string {
	h := fnv.New64a()
	for _, s := range []string{id, mdl, provider, startedAt, endedAt} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	for _, n := range nums {
		h.Write([]byte(strconv.FormatInt(n, 10)))
		h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// openReadOnly opens a SQLite database strictly read-only. mode=ro prevents
// create/write/lock; query_only additionally refuses any write statement on
// the connection; busy_timeout keeps a transient lock from failing the poll.
// immutable=1 is deliberately NOT used: state.db is written concurrently by
// Hermes, and SQLite documents wrong results when an immutable-flagged file
// changes — a stale read below the stored baseline trips the collector's
// reset branch and re-adds the full current value as new usage, forever.
func openReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	dsn := u.String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// parseTime accepts Hermes's Unix seconds and legacy textual timestamps.
// A naked SQLite datetime carries no zone and is taken as UTC.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.DateTime} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	if seconds, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= -62135596800 && seconds < 253402300800 {
		whole, fraction := math.Modf(seconds)
		return time.Unix(int64(whole), int64(fraction*1e9)).UTC()
	}
	return time.Time{}
}

// rawJSON builds the audit blob carrying provider + start time. Built by hand
// (no encoding/json) since both values are short, controlled strings.
func rawJSON(provider, startedAt string) string {
	var b strings.Builder
	b.WriteString(`{"billing_provider":`)
	b.WriteString(quote(provider))
	b.WriteString(`,"started_at":`)
	b.WriteString(quote(startedAt))
	b.WriteByte('}')
	return b.String()
}

// quote returns a minimally-escaped JSON string literal.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// isFile reports whether path exists and is a regular file.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
