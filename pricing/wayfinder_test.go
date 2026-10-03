package pricing

import (
	"math"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestComputedCostSaturates(t *testing.T) {
	bound := float64(math.MaxInt64) / 1e6
	below := math.Nextafter(bound, 0)
	for _, tc := range []struct {
		name  string
		rate  float64
		input int64
		want  int64
	}{
		{"below", below, 1, int64(math.Floor(below*1e6 + .5))},
		{"at", bound, 1, math.MaxInt64},
		{"above", math.Nextafter(bound, math.Inf(1)), 1, math.MaxInt64},
		{"product infinity", 1e300, math.MaxInt64, math.MaxInt64},
		{"zero", 1, 0, 0},
		{"half up", .0000005, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Rates{Input: tc.rate}).Cost(Charge{Input: tc.input}); got != tc.want {
				t.Fatalf("cost = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLongContextOneHourFallbackUsesSelectedCard(t *testing.T) {
	r := Rates{Input: 1e-6, CacheWrite5m: 3e-6, CacheWrite1h: 6e-6,
		Long: LongContext{Threshold: 100, Input: 2e-6, CacheWrite5m: 7.5e-6}}
	for _, tc := range []struct {
		name   string
		charge Charge
		want   int64
	}{
		{"threshold", Charge{Input: 90, CacheWrite1h: 10}, 150},
		{"long 1h", Charge{Input: 100, CacheWrite1h: 10}, 275},
		{"long 5m", Charge{Input: 100, CacheWrite5m: 10}, 275},
		{"long unsplit", ChargeFor(model.UsageEvent{InputTokens: 100, CacheCreationTokens: 10}), 275},
		{"aggregate", Charge{Input: 100, CacheWrite1h: 10, Aggregate: true}, 160},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Cost(tc.charge); got != tc.want {
				t.Fatalf("cost = %d, want %d", got, tc.want)
			}
		})
	}
	r.Long.CacheWrite1h = 12e-6
	if got := r.Cost(Charge{Input: 100, CacheWrite1h: 10}); got != 320 {
		t.Fatalf("explicit crossed rate = %d, want 320", got)
	}
	engine := New(Options{Overrides: map[string]Rates{"crossed": r}})
	micro, source, ok := engine.Price(Charge{Model: "crossed", Input: 100, CacheWrite1h: 10})
	if !ok || micro != 320 || source != "override+long-context" {
		t.Fatalf("override = %d %q %v", micro, source, ok)
	}
}

func TestEmbeddedLongContextOneHourFallback(t *testing.T) {
	table := embeddedTable()
	if table == nil {
		t.Fatal("embedded table missing")
	}
	for _, id := range []string{"claude-sonnet-4-20250514", "vertex_ai/claude-sonnet-4", "vertex_ai/claude-sonnet-4-5", "vertex_ai/claude-sonnet-4-5@20250929", "vertex_ai/claude-sonnet-4@20250514"} {
		r := table.Models[id]
		if r.CacheWrite1h != 6e-6 || r.Long.CacheWrite5m != 7.5e-6 || r.Long.CacheWrite1h != 0 {
			t.Fatalf("%s no longer exercises the missing crossed rate: %+v", id, r)
		}
		c := Charge{Input: r.Long.Threshold, CacheWrite1h: 10}
		want := microUSD(float64(c.Input)*r.Long.Input + 10*r.Long.CacheWrite5m)
		if got := r.Cost(c); got != want {
			t.Errorf("%s cost = %d, want %d", id, got, want)
		}
	}
}
