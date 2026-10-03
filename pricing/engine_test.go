package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// upstreamJSON is a minimal stand-in for LiteLLM's bare model map.
const upstreamJSON = `{
  "claude-sonnet-4-6": {"input_cost_per_token": 2e-06, "output_cost_per_token": 2e-05,
                        "litellm_provider": "anthropic", "mode": "chat"},
  "not-a-model": "sample_spec"
}`

// TestLadderPrecedence walks every rung: an override beats the refreshed table,
// which beats the embedded snapshot, and a model none of them knows is unpriced
// rather than free.
func TestLadderPrecedence(t *testing.T) {
	const m = "claude-sonnet-4-6"
	charge := Charge{Model: m, Provider: model.ProviderAnthropic, Input: 1_000_000}

	// Rung 3 only: the embedded snapshot.
	e := New(Options{})
	micro, source, ok := e.Price(charge)
	if !ok || micro <= 0 {
		t.Fatalf("embedded rung: %d,%q,%v want a price", micro, source, ok)
	}
	embeddedMicro, embeddedSource := micro, source
	if got := embeddedSource[:len("embedded-")]; got != "embedded-" {
		t.Errorf("source = %q, want an embedded-* stamp", embeddedSource)
	}

	// Rung 2 wins over rung 3.
	e.refreshed = &Table{Source: "litellm-2026-08-09", Models: map[string]Rates{m: {Input: 7e-06}}}
	micro, source, ok = e.Price(charge)
	if !ok || micro != 7_000_000 || source != "litellm-2026-08-09" {
		t.Fatalf("refreshed rung: %d,%q,%v want 7000000,litellm-2026-08-09,true", micro, source, ok)
	}

	// Rung 1 wins over both.
	e = New(Options{Overrides: map[string]Rates{m: {Input: 1e-05}}})
	e.refreshed = &Table{Source: "litellm-2026-08-09", Models: map[string]Rates{m: {Input: 7e-06}}}
	micro, source, ok = e.Price(charge)
	if !ok || micro != 10_000_000 || source != SourceOverride {
		t.Fatalf("override rung: %d,%q,%v want 10000000,override,true", micro, source, ok)
	}

	// Nothing knows this model: unpriced, and emphatically not zero.
	micro, source, ok = e.Price(Charge{Model: "no-such-model-anywhere", Input: 1_000_000})
	if ok || micro != 0 || source != "" {
		t.Fatalf("unknown model: %d,%q,%v want 0,\"\",false", micro, source, ok)
	}

	if embeddedMicro == 7_000_000 {
		t.Errorf("embedded rate coincidentally equals the test's refreshed rate; the test proves nothing")
	}
}

// TestPriceNeverReportsSilentZero is the "$0.00 is a lie" guard: real usage no
// rung on the ladder can value must report unpriced, so the display layer shows
// a dash instead of a free lunch. ("weird" is in no other table, so the
// fall-through to the lower rungs finds nothing either.)
func TestPriceNeverReportsSilentZero(t *testing.T) {
	e := New(Options{})
	// Output-only rates against an input-only charge: a real 0 for real tokens.
	e.refreshed = &Table{Source: "litellm-test", Models: map[string]Rates{
		"weird": {Output: 1e-05},
	}}
	micro, source, ok := e.Price(Charge{Model: "weird", Input: 1000})
	if ok || micro != 0 || source != "" {
		t.Fatalf("zero-valued charge: %d,%q,%v want unpriced", micro, source, ok)
	}

	// A charge with no tokens at all is genuinely free and may be stamped.
	micro, _, ok = e.Price(Charge{Model: "weird"})
	if !ok || micro != 0 {
		t.Fatalf("empty charge: %d,%v want 0,true", micro, ok)
	}
}

// TestPartialOverrideKeepsLowerRungRates is the partial-override contract: an
// override that names only the output rate must not disable pricing (or, worse,
// value tokens at zero) for the charge shapes it says nothing about. The rates
// it left out come from the rung it displaced, and the stamp names both.
func TestPartialOverrideKeepsLowerRungRates(t *testing.T) {
	const m = "claude-sonnet-4-6"
	e := New(Options{Overrides: map[string]Rates{m: {Output: 4e-05}}})
	e.refreshed = &Table{Source: "litellm-2026-08-09", Models: map[string]Rates{
		m: {Input: 3e-06, Output: 1.5e-05, CacheRead: 3e-07},
	}}

	// Input-only charge: the override says nothing about input, so the
	// refreshed rate prices it instead of the whole model going unpriced.
	micro, source, ok := e.Price(Charge{Model: m, Input: 1_000_000})
	if !ok || micro != 3_000_000 {
		t.Fatalf("input-only charge: %d,%q,%v want 3000000 from the lower rung", micro, source, ok)
	}
	if source != SourceOverride+"+litellm-2026-08-09" {
		t.Errorf("source = %q, want the composite stamp", source)
	}

	// Mixed charge: the user's output rate applies, input still comes from the
	// table. Valuing input at zero here would have stamped a WRONG number.
	micro, source, ok = e.Price(Charge{Model: m, Input: 1_000_000, Output: 1_000_000})
	if !ok || micro != 43_000_000 {
		t.Fatalf("mixed charge: %d,%q,%v want 43000000 (3 input + 40 output)", micro, source, ok)
	}

	// A charge the override prices unaided keeps the plain stamp.
	micro, source, ok = e.Price(Charge{Model: m, Output: 1_000_000})
	if !ok || micro != 40_000_000 || source != SourceOverride {
		t.Fatalf("output-only charge: %d,%q,%v want 40000000,override,true", micro, source, ok)
	}
}

// TestPartialOverrideFillsCacheAndBatchRates pins the rest of the merge. The
// override names input and output, so those two are settled by the user; every
// rate it left unset - cache read, both cache writes, and the two batch rates -
// must still come from the rung the override displaced.
//
// Without that, Cost's documented fallbacks take over and bill cache and batch
// tokens at the OVERRIDE's input/output rate. For the common override (a user
// correcting the standard rates upward) that silently prices a cache read, the
// cheapest token there is, at the dearest rate in the table. Each case carries
// the number the unfilled merge would have produced, so a regression cannot
// pass by coincidence.
func TestPartialOverrideFillsCacheAndBatchRates(t *testing.T) {
	const m = "claude-sonnet-4-6"
	e := New(Options{Overrides: map[string]Rates{m: {Input: 1e-05, Output: 4e-05}}})
	e.refreshed = &Table{Source: "litellm-2026-08-09", Models: map[string]Rates{
		m: {
			Input: 3e-06, Output: 1.5e-05,
			CacheRead: 3e-07, CacheWrite5m: 3.75e-06, CacheWrite1h: 6e-06,
			InputBatch: 1.5e-06, OutputBatch: 7.5e-06,
		},
	}}

	tests := []struct {
		name   string
		charge Charge
		want   int64
		// unfilled is the same charge priced by a merge that stopped at
		// input/output: the number this test exists to reject.
		unfilled int64
	}{
		{"cache read", Charge{CacheRead: 1_000_000}, 300_000, 10_000_000},
		{"cache write 5m", Charge{CacheWrite5m: 1_000_000}, 3_750_000, 10_000_000},
		{"cache write 1h", Charge{CacheWrite1h: 1_000_000}, 6_000_000, 10_000_000},
		{"batch input", Charge{ServiceTier: "batch", Input: 1_000_000}, 1_500_000, 10_000_000},
		{"batch output", Charge{ServiceTier: "batch", Output: 1_000_000}, 7_500_000, 40_000_000},
		{
			name:     "override input with table cache read",
			charge:   Charge{Input: 1_000_000, CacheRead: 1_000_000},
			want:     10_300_000,
			unfilled: 20_000_000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == tc.unfilled {
				t.Fatalf("case cannot tell a filled merge from an unfilled one")
			}
			c := tc.charge
			c.Model, c.Provider = m, model.ProviderAnthropic

			micro, source, ok := e.Price(c)
			if !ok {
				t.Fatalf("charge %+v went unpriced", c)
			}
			if micro == tc.unfilled {
				t.Fatalf("cost = %d, the unfilled merge's number: the lower rung's rate was dropped", micro)
			}
			if micro != tc.want {
				t.Fatalf("cost = %d, want %d", micro, tc.want)
			}
			// The lower rung moved the price, so it must be named in the stamp.
			if source != SourceOverride+"+litellm-2026-08-09" {
				t.Errorf("source = %q, want the composite stamp", source)
			}
		})
	}
}

// TestPartialOverrideWithoutLowerRungStillPrices keeps the merge honest when
// there is nothing to merge with: an override for a model no table knows still
// prices what it can, and still refuses to invent a zero for what it cannot.
func TestPartialOverrideWithoutLowerRungStillPrices(t *testing.T) {
	e := New(Options{Overrides: map[string]Rates{"private-model": {Output: 4e-05}}})

	micro, source, ok := e.Price(Charge{Model: "private-model", Output: 1_000_000})
	if !ok || micro != 40_000_000 || source != SourceOverride {
		t.Fatalf("output charge: %d,%q,%v want 40000000,override,true", micro, source, ok)
	}
	if micro, source, ok := e.Price(Charge{Model: "private-model", Input: 1_000_000}); ok || micro != 0 || source != "" {
		t.Fatalf("input charge with no input rate anywhere: %d,%q,%v want unpriced", micro, source, ok)
	}
}

// TestZeroValuationFallsThroughToNextRung applies the unpriceable-row recovery
// to the other miss shape: a table that knows the model but publishes no rate
// for the tokens being charged must hand the charge down the ladder instead of
// declaring it unpriced while a lower rung can still value it.
func TestZeroValuationFallsThroughToNextRung(t *testing.T) {
	e := New(Options{})
	e.refreshed = &Table{Source: "litellm-test", Models: map[string]Rates{
		"claude-sonnet-4-6": {Output: 1e-05},
	}}

	micro, source, ok := e.Price(Charge{
		Model: "claude-sonnet-4-6", Provider: model.ProviderAnthropic, Input: 1_000_000,
	})
	if !ok || micro <= 0 {
		t.Fatalf("input-only charge: %d,%q,%v want the embedded rung's price", micro, source, ok)
	}
	if len(source) < len("embedded-") || source[:len("embedded-")] != "embedded-" {
		t.Errorf("source = %q, want the embedded rung that actually priced it", source)
	}
}

// TestPriceEventUsesToolReasoningRule checks the collector-facing entry point
// threads the per-tool reasoning rule through to the bill.
func TestPriceEventUsesToolReasoningRule(t *testing.T) {
	e := New(Options{Overrides: map[string]Rates{"m": {Input: 1e-06, Output: 1e-05}}})

	subset, _, ok := e.PriceEvent(model.UsageEvent{
		Tool: model.ToolClaudeCode, Model: "m", OutputTokens: 100, ReasoningTokens: 50,
	})
	if !ok || subset != 1000 {
		t.Fatalf("subset tool = %d,%v want 1000,true", subset, ok)
	}
	additive, _, ok := e.PriceEvent(model.UsageEvent{
		Tool: model.ToolOpenCode, Model: "m", OutputTokens: 100, ReasoningTokens: 50,
	})
	if !ok || additive != 1500 {
		t.Fatalf("additive tool = %d,%v want 1500,true", additive, ok)
	}
}

// TestRefreshFetchesCachesAndSwaps covers the happy path: the upstream table is
// fetched, becomes the middle rung, and lands in the data dir so the next
// process starts with it.
func TestRefreshFetchesCachesAndSwaps(t *testing.T) {
	pinNow(t, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(upstreamJSON))
	}))
	defer srv.Close()

	dir := t.TempDir()
	e := New(Options{DataDir: dir, Refresh: true})
	e.url = srv.URL
	e.modelsDevURL = "" // this test isolates the LiteLLM feed

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	micro, source, ok := e.Price(Charge{
		Model: "claude-sonnet-4-6", Provider: model.ProviderAnthropic, Input: 1_000_000,
	})
	if !ok || micro != 2_000_000 || source != "litellm-2026-08-09" {
		t.Fatalf("after refresh: %d,%q,%v want 2000000,litellm-2026-08-09,true", micro, source, ok)
	}

	// A fresh process must pick the cache up without touching the network.
	reloaded := New(Options{DataDir: dir, Refresh: false})
	micro, source, ok = reloaded.Price(Charge{
		Model: "claude-sonnet-4-6", Provider: model.ProviderAnthropic, Input: 1_000_000,
	})
	if !ok || micro != 2_000_000 || source != "litellm-2026-08-09" {
		t.Fatalf("cache reload: %d,%q,%v want the refreshed table", micro, source, ok)
	}
}

// TestRefreshDisabledNeverFetches proves pricing.refresh=false is a real air
// gap: no request leaves the process.
func TestRefreshDisabledNeverFetches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Write([]byte(upstreamJSON))
	}))
	defer srv.Close()

	e := New(Options{DataDir: t.TempDir(), Refresh: false})
	e.url = srv.URL
	e.modelsDevURL = "" // this test isolates the LiteLLM feed
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if hits != 0 {
		t.Errorf("refresh disabled but the URL was fetched %d time(s)", hits)
	}
}

// TestRefreshThrottlesOnCacheAge lets the caller invoke Refresh every cycle: a
// cache younger than the interval skips the network entirely.
func TestRefreshThrottlesOnCacheAge(t *testing.T) {
	pinNow(t, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Write([]byte(upstreamJSON))
	}))
	defer srv.Close()

	dir := t.TempDir()
	e := New(Options{DataDir: dir, Refresh: true})
	e.url = srv.URL
	e.modelsDevURL = "" // this test isolates the LiteLLM feed

	for i := 0; i < 3; i++ {
		if err := e.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("fetches = %d, want 1 (throttled by the cache age)", hits)
	}

	// Age the cache past the interval and the next call fetches again.
	old := nowFn().Add(-2 * refreshInterval)
	if err := os.Chtimes(filepath.Join(dir, cacheFile), old, old); err != nil {
		t.Fatalf("age cache: %v", err)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh after ageing: %v", err)
	}
	if hits != 2 {
		t.Errorf("fetches = %d, want 2 after the cache expired", hits)
	}
}

// TestRefreshFailureKeepsPreviousTable is the silence contract: a dead upstream
// must leave the loaded table exactly as it was, and must not poison the cache.
func TestRefreshFailureKeepsPreviousTable(t *testing.T) {
	pinNow(t, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	e := New(Options{DataDir: dir, Refresh: true})
	e.url = srv.URL
	e.modelsDevURL = "" // this test isolates the LiteLLM feed
	e.refreshed = &Table{Source: "litellm-old", Models: map[string]Rates{"m": {Input: 1e-06}}}

	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error from a failing upstream")
	}
	micro, source, ok := e.Price(Charge{Model: "m", Input: 1_000_000})
	if !ok || micro != 1_000_000 || source != "litellm-old" {
		t.Errorf("after failed refresh: %d,%q,%v want the previous table intact", micro, source, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, cacheFile)); err == nil {
		t.Errorf("a failed refresh wrote a cache file")
	}
}

// TestRefreshRejectsGarbageUpstream stops a mangled or truncated feed from
// replacing a working table with nothing.
func TestRefreshRejectsGarbageUpstream(t *testing.T) {
	pinNow(t, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"broken":`))
	}))
	defer srv.Close()

	e := New(Options{DataDir: t.TempDir(), Refresh: true})
	e.url = srv.URL
	e.modelsDevURL = "" // this test isolates the LiteLLM feed
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("expected a parse error for a truncated upstream table")
	}
	if e.refreshed != nil {
		t.Errorf("garbage upstream installed a table: %+v", e.refreshed)
	}
}

// TestDecodeSkipsUnpriceableEntries checks the tolerant decoder: upstream ships
// non-object records and rows with no rates, and neither may cost the user the
// rest of the table.
func TestDecodeSkipsUnpriceableEntries(t *testing.T) {
	raw, err := decodeUpstream([]byte(upstreamJSON))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	models := decodeModels(raw)
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1 (the string record must be skipped)", len(models))
	}
	if _, ok := models["claude-sonnet-4-6"]; !ok {
		t.Errorf("priceable model missing from %v", models)
	}
}

// pinNow freezes the package clock for the duration of a test.
func pinNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := nowFn
	nowFn = func() time.Time { return at }
	t.Cleanup(func() { nowFn = prev })
}
