package pricing

import (
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestStoredCachePricingRequiresUnambiguousTTL(t *testing.T) {
	ev := model.UsageEvent{Model: "test-cache-only", CacheCreationTokens: 1_000_000}
	for _, tc := range []struct {
		name  string
		rates Rates
		cost  int64
		known bool
	}{
		{"different rates", Rates{Input: 1e-6, CacheWrite5m: 1.25e-6, CacheWrite1h: 2e-6}, 0, false},
		{"same rates", Rates{Input: 1e-6, CacheWrite5m: 2e-6, CacheWrite1h: 2e-6}, 2_000_000, true},
		{"free", Rates{Free: true}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := New(Options{Overrides: map[string]Rates{ev.Model: tc.rates}})
			cost, _, known := engine.PriceStoredEvent(ev)
			if known != tc.known || cost != tc.cost {
				t.Fatalf("stored cost=%d known=%v", cost, known)
			}
		})
	}
}
