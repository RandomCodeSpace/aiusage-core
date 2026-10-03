package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPricingRevisionTracksAvailableRates(t *testing.T) {
	oldNow := nowFn
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = oldNow })
	dir := t.TempDir()
	body, fail := upstreamJSON, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	e := New(Options{DataDir: dir, Refresh: true})
	e.url = srv.URL
	e.modelsDevURL = "" // this test isolates the LiteLLM feed
	initial := e.Revision()
	if initial == "" || initial != New(Options{}).Revision() {
		t.Fatal("same embedded rates need a stable nonempty revision")
	}
	refresh := func() error {
		// Aged against the pinned clock, not the wall clock: freshness is
		// judged by nowFn, and a wall-clock age crosses it once the real date
		// passes the pinned one.
		old := now.Add(-48 * time.Hour)
		path := filepath.Join(dir, cacheFile)
		if _, err := os.Stat(path); err == nil {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return e.Refresh(context.Background())
	}
	if err := refresh(); err != nil {
		t.Fatal(err)
	}
	first := e.Revision()
	if first == initial || first != New(Options{DataDir: dir}).Revision() {
		t.Fatal("new rates must change the revision and survive cache reload")
	}
	now = now.Add(24 * time.Hour)
	if err := refresh(); err != nil || e.Revision() != first || New(Options{DataDir: dir}).Revision() != first {
		t.Fatalf("next-day identical refresh changed revision: %v", err)
	}
	body = `{"new-private-model":{"input_cost_per_token":0.000002,"output_cost_per_token":0.000004,"litellm_provider":"openai","mode":"chat"}}`
	if err := refresh(); err != nil || e.Revision() == first {
		t.Fatalf("changed rates with the same fetch date kept revision: %v", err)
	}
	latest := e.Revision()
	fail = true
	if err := refresh(); err == nil || e.Revision() != latest {
		t.Fatalf("failed refresh replaced working revision: %v", err)
	}
}

func TestPricingRevisionIncludesOverrides(t *testing.T) {
	a := New(Options{Overrides: map[string]Rates{"a": {Input: 1e-6}, "b": {Output: 2e-6}}})
	b := New(Options{Overrides: map[string]Rates{"b": {Output: 2e-6}, "a": {Input: 1e-6}}})
	c := New(Options{Overrides: map[string]Rates{"a": {Input: 3e-6}, "b": {Output: 2e-6}}})
	if a.Revision() != b.Revision() || a.Revision() == c.Revision() {
		t.Fatal("revision must ignore map order and detect changed overrides")
	}
}
