package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const modelsDevTestJSON = `{"test-provider":{"models":{"test-model":{"cost":{"input":2,"output":4,"cache_read":0.5}}}}}`

func TestModelsDevRefreshCacheAndOfflineFallback(t *testing.T) {
	oldNow := nowFn
	now := time.Now().UTC()
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = oldNow })
	fail, calls := false, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/litellm" {
			_, _ = w.Write([]byte(upstreamJSON))
			return
		}
		calls++
		if fail {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(modelsDevTestJSON))
	}))
	defer srv.Close()
	dir := t.TempDir()
	e := New(Options{DataDir: dir, Refresh: true})
	e.url, e.modelsDevURL = srv.URL+"/litellm", srv.URL+"/modelsdev"
	e.modelsDevEmbedded = &Table{Source: "modelsdev-embedded-test", ProviderScoped: true, Models: map[string]Rates{"test-provider/test-model": {Input: 1e-6, Output: 2e-6}}}
	charge := Charge{Provider: "test-provider", Model: "test-model", Input: 100}
	if cost, _, ok := e.Price(charge); !ok || cost != 100 {
		t.Fatalf("offline embedded price = %d, %v", cost, ok)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cost, source, ok := e.Price(charge); !ok || cost != 200 || !strings.HasPrefix(source, "modelsdev-") {
		t.Fatalf("refreshed price = %d, %q, %v", cost, source, ok)
	}
	path := filepath.Join(dir, modelsDevCacheFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded := New(Options{DataDir: dir})
	if cost, _, ok := loaded.Price(charge); !ok || cost != 200 {
		t.Fatalf("fresh cache reload = %d, %v", cost, ok)
	}
	if err := e.Refresh(context.Background()); err != nil || calls != 1 {
		t.Fatalf("fresh cache fetched again: calls=%d, err=%v", calls, err)
	}
	now = now.Add(25 * time.Hour)
	fail = true
	if err := e.Refresh(context.Background()); err == nil || calls != 2 {
		t.Fatalf("expired refresh = calls %d, %v", calls, err)
	}
	if cost, source, ok := e.Price(charge); !ok || cost != 100 || source != "modelsdev-embedded-test" {
		t.Fatalf("failed refresh did not fall back: %d, %q, %v", cost, source, ok)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed refresh damaged last good cache")
	}
	if err := e.Refresh(context.Background()); err != nil || calls != 2 {
		t.Fatalf("failed feed retried before 24h: calls=%d, err=%v", calls, err)
	}
	if New(Options{DataDir: dir}).modelsDevRefreshed != nil {
		t.Fatal("stale cache promoted on offline startup")
	}
	now = now.Add(25 * time.Hour)
	fail = false
	if err := e.Refresh(context.Background()); err != nil || calls != 3 {
		t.Fatalf("next-day recovery = calls %d, %v", calls, err)
	}
}

func TestModelsDevRefreshIndependentOfLiteLLM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/litellm" {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(modelsDevTestJSON))
	}))
	defer srv.Close()
	e := New(Options{DataDir: t.TempDir(), Refresh: true})
	e.url, e.modelsDevURL = srv.URL+"/litellm", srv.URL+"/modelsdev"
	before := e.Revision()
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("missing LiteLLM failure")
	}
	if cost, _, ok := e.Price(Charge{Provider: "test-provider", Model: "test-model", Input: 100}); !ok || cost != 200 || e.Revision() == before {
		t.Fatalf("LiteLLM failure prevented Models.dev update: %d, %v", cost, ok)
	}
}

func TestModelsDevProviderIsolationAndFreePrice(t *testing.T) {
	e := New(Options{})
	e.modelsDevEmbedded = &Table{Source: "modelsdev-embedded-test", ProviderScoped: true, Models: map[string]Rates{
		"github-copilot/private-haiku": {Input: 1e-6},
		"opencode/free-model":          {Free: true},
	}}
	for _, provider := range []string{"", "anthropic", "openai"} {
		if _, _, ok := e.Price(Charge{Provider: provider, Model: "private-haiku", Input: 100}); ok {
			t.Fatalf("wrong provider %q matched", provider)
		}
	}
	if cost, _, ok := e.Price(Charge{Provider: "github", Model: "private-haiku", Input: 100}); !ok || cost != 100 {
		t.Fatalf("Copilot provider mapping = %d, %v", cost, ok)
	}
	if cost, source, ok := e.Price(Charge{Provider: "opencode", Model: "free-model", Input: 100}); !ok || cost != 0 || source != "modelsdev-embedded-test+free" {
		t.Fatalf("confirmed free = %d, %q, %v", cost, source, ok)
	}
}
