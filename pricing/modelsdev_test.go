package pricing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

func modelsDevTestFeed(provider, name, cost string) []byte {
	return []byte(fmt.Sprintf(`{%q:{"models":{%q:{"cost":%s}}}}`, provider, name, cost))
}

func TestModelsDevSnapshotUnitsAndProviderIdentity(t *testing.T) {
	pinNow(t, time.Date(2026, 9, 12, 23, 30, 0, 0, time.FixedZone("test", -3600)))
	raw := []byte(`{
  "first":{"id":"first","env":["SECRET_NAME"],"models":{
   "same":{"id":"same","name":"display name","cost":{"input":1,"output":5,"cache_read":0.1,"cache_write":1.25}}
  }},
  "second":{"models":{"same":{"cost":{"input":2,"output":10}}}},
  "github-copilot":{"models":{"claude-haiku-4.5":{"cost":{"input":1,"output":5}}}}
 }`)
	encoded, err := modelsDevSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("SECRET_NAME")) || bytes.Contains(encoded, []byte("display name")) {
		t.Fatal("non-pricing metadata survived filtering")
	}
	var doc modelsDevDoc
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Meta.Source != ModelsDevURL || doc.Meta.Fetched != "2026-09-13" {
		t.Fatalf("metadata = %+v", doc.Meta)
	}
	table, err := decodeModelsDevSnapshot(encoded, "modelsdev")
	if err != nil {
		t.Fatal(err)
	}
	if !table.ProviderScoped || table.Source != "modelsdev-2026-09-13" || len(table.Models) != 3 {
		t.Fatalf("table = %+v", table)
	}
	want := Rates{Input: 1e-6, Output: 5e-6, CacheRead: 1e-7, CacheWrite5m: 1.25e-6}
	if got := table.Models["first/same"]; !sameModelsDevRates(got, want) {
		t.Fatalf("USD/million conversion = %+v, want %+v", got, want)
	}
	if got := table.Models["second/same"].Input; got != 2e-6 {
		t.Fatalf("provider rates collided: %v", got)
	}
	if _, ok := table.Models["same"]; ok {
		t.Fatal("created a bare model alias")
	}
	if _, ok := table.Models["github/claude-haiku-4.5"]; ok {
		t.Fatal("rewrote provider identity")
	}
	if _, ok := table.Models["github-copilot/claude-haiku-4.5"]; !ok {
		t.Fatal("lost actual provider identity")
	}
	// Both paths use the same filter: re-normalizing the envelope's providers
	// changes neither source-unit prices nor resolved rates.
	bare, err := json.Marshal(doc.Providers)
	if err != nil {
		t.Fatal(err)
	}
	again, err := BuildModelsDevSnapshot(bare)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte("Copyright (c) 2025 models.dev")) {
		t.Fatal("snapshot lost the binary-retained MIT notice")
	}
	if !bytes.Equal(encoded, again) {
		t.Fatal("snapshot normalization is not stable")
	}
}

func TestModelsDevRequiredPricesRejectInvalidInput(t *testing.T) {
	costs := map[string]string{
		"missing":                          `{}`,
		"null object":                      `null`,
		"wrong object":                     `[]`,
		"missing output":                   `{"input":1}`,
		"missing input":                    `{"output":1}`,
		"null input":                       `{"input":null,"output":1}`,
		"null output":                      `{"input":1,"output":null}`,
		"null cache":                       `{"input":1,"output":2,"cache_read":null}`,
		"free cache read with paid input":  `{"input":1,"output":2,"cache_read":0}`,
		"free cache write with paid input": `{"input":1,"output":2,"cache_write":0}`,
		"wrong numeric type":               `{"input":"1","output":2}`,
		"negative input":                   `{"input":-1,"output":2}`,
		"negative cache":                   `{"input":1,"output":2,"cache_write":-1}`,
		"negative ignored audio":           `{"input":1,"output":2,"input_audio":-1}`,
		"overflow":                         `{"input":1e999,"output":2}`,
		"per-token units":                  `{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}`,
		"declared different units":         `{"input":1,"output":2,"unit":"cents_per_token"}`,
		"unknown price dimension":          `{"input":1,"output":2,"cache_write_1h":3}`,
		"unrepresentable reasoning":        `{"input":1,"output":2,"reasoning":3}`,
		"unknown all-zero placeholder":     `{"input":0,"output":0}`,
	}
	for name, cost := range costs {
		t.Run(name, func(t *testing.T) {
			raw := modelsDevTestFeed("test", "model", cost)
			if encoded, err := modelsDevSnapshot(raw); err == nil || encoded != nil {
				t.Fatalf("bad feed produced snapshot: %s, %v", encoded, err)
			}
			snapshot := append([]byte(`{"providers":`), raw...)
			snapshot = append(snapshot, '}')
			if table, err := decodeModelsDevSnapshot(snapshot, "modelsdev"); err == nil || table != nil {
				t.Fatalf("bad cache produced table: %+v, %v", table, err)
			}
		})
	}
	for _, raw := range []string{``, `{`, `null`, `[]`, `{}`, `{"error":"unavailable"}`, `{"test":{"models":null}}`} {
		if encoded, err := modelsDevSnapshot([]byte(raw)); err == nil || encoded != nil {
			t.Fatalf("invalid document produced snapshot: %q, %v", raw, err)
		}
	}
}

func TestModelsDevFreeRequiresConfirmedProviderAndExplicitZeros(t *testing.T) {
	for _, tc := range []struct {
		provider, name, cost string
		wantFree, wantKept   bool
	}{
		{"opencode", "hy3-free", `{"input":0,"output":0,"cache_read":0}`, true, true},
		{"opencode", "big-pickle", `{"input":0,"output":0,"cache_read":0,"cache_write":0}`, true, true},
		{"other", "big-pickle", `{"input":0,"output":0}`, false, false},
		{"opencode", "other-free", `{"input":0,"output":0}`, false, false},
		{"opencode", "big-pickle", `{"input":null,"output":0}`, false, false},
		{"opencode", "big-pickle", `{"input":0}`, false, false},
		{"opencode", "big-pickle", `{"input":0,"output":0,"cache_write":1}`, false, false},
		{"opencode", "big-pickle", `{"input":0,"output":0,"input_audio":1}`, false, false},
		{"opencode", "hy3-free", `{"input":0,"output":0,"output_audio":1}`, false, false},
		{"opencode", "big-pickle", `{"input":1,"output":0}`, false, true},
		{"opencode", "big-pickle", `{"input":0,"output":0,"tiers":[{"input":1,"output":2,"tier":{"type":"context","size":200000}}]}`, false, false},
	} {
		t.Run(tc.provider+"/"+tc.name+"/"+tc.cost, func(t *testing.T) {
			encoded, err := modelsDevSnapshot(modelsDevTestFeed(tc.provider, tc.name, tc.cost))
			if !tc.wantKept {
				if err == nil {
					t.Fatal("unknown or incomplete zero rate was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			table, err := decodeModelsDevSnapshot(encoded, "modelsdev")
			if err != nil {
				t.Fatal(err)
			}
			if got := table.Models[tc.provider+"/"+tc.name].Free; got != tc.wantFree {
				t.Fatalf("Free=%v want %v", got, tc.wantFree)
			}
		})
	}
}

func TestModelsDevContextTiersPreserveThreshold(t *testing.T) {
	for _, tc := range []struct {
		name, cost string
		threshold  int64
	}{
		{"legacy", `{"input":1,"output":5,"context_over_200k":{"input":2,"output":10,"cache_read":0.2,"cache_write":2.5}}`, 200000},
		{"explicit beats legacy name", `{"input":1,"output":5,"tiers":[{"input":2,"output":10,"cache_read":0.2,"cache_write":2.5,"tier":{"type":"context","size":272000}}],"context_over_200k":{"input":2,"output":10}}`, 272000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := modelsDevSnapshot(modelsDevTestFeed("test", "tiered", tc.cost))
			if err != nil {
				t.Fatal(err)
			}
			table, err := decodeModelsDevSnapshot(encoded, "modelsdev")
			if err != nil {
				t.Fatal(err)
			}
			rates := table.Models["test/tiered"]
			want := LongContext{Threshold: tc.threshold, Input: 2e-6, Output: 10e-6, CacheRead: 0.2 / 1e6, CacheWrite5m: 2.5 / 1e6}
			if !sameModelsDevRates(Rates{Long: rates.Long}, Rates{Long: want}) {
				t.Fatalf("long rates=%+v want %+v", rates.Long, want)
			}
			if rates.longApplies(Charge{Input: tc.threshold}) || !rates.longApplies(Charge{Input: tc.threshold, CacheRead: 1}) {
				t.Fatal("whole-prompt boundary changed")
			}
			if rates.longApplies(Charge{Input: tc.threshold + 1, Aggregate: true}) {
				t.Fatal("aggregate selected a request tier")
			}
		})
	}
	for _, suffix := range []string{
		`"tiers":null`,
		`"tiers":[{"input":2,"output":10,"tier":{"type":"context","size":200000}},{"input":3,"output":15,"tier":{"type":"context","size":400000}}]`,
		`"tiers":[{"input":2,"output":10,"tier":{"type":"output","size":200000}}]`,
		`"tiers":[{"input":2,"output":10,"tier":{"type":"context","size":0}}]`,
		`"tiers":[{"input":2,"output":10,"tier":{"type":"context","size":200000.5}}]`,
		`"tiers":[{"input":2,"tier":{"type":"context","size":200000}}]`,
		`"tiers":[{"input":0,"output":10,"tier":{"type":"context","size":200000}}]`,
		`"tiers":[{"input":2,"output":0,"tier":{"type":"context","size":200000}}]`,
		`"context_over_200k":{"input":2}`,
		`"context_over_200k":null`,
	} {
		cost := `{"input":1,"output":5,` + suffix + `}`
		if _, err := modelsDevSnapshot(modelsDevTestFeed("test", "tiered", cost)); err == nil {
			t.Fatalf("unsupported tier was flat-priced: %s", cost)
		}
	}
}

func TestModelsDevLongCacheZeroDiffersFromMissing(t *testing.T) {
	for _, key := range []string{"cache_read", "cache_write"} {
		for _, published := range []bool{false, true} {
			field := ""
			if published {
				field = fmt.Sprintf(",%q:0", key)
			}
			cost := fmt.Sprintf(`{"input":0,"output":1,%q:1,"tiers":[{"input":0,"output":2%s,"tier":{"type":"context","size":200000}}]}`, key, field)
			_, err := modelsDevSnapshot(modelsDevTestFeed("test", "model", cost))
			if (err != nil) != published {
				t.Fatalf("%s published zero=%v: %v", key, published, err)
			}
		}
	}
}

func TestModelsDevSkipsBadNeighborsWithoutProviderCollisions(t *testing.T) {
	raw := []byte(`{
  "a/b":{"models":{"c":{"cost":{"input":99,"output":99}}}},
  "a":{"models":{
   "b/c":{"cost":{"input":1,"output":2}},
   "bad":"not an object",
   "mismatch":{"id":"other","cost":{"input":3,"output":4}},
   "negative":{"cost":{"input":-1,"output":2}},
   "metadata":{"cost":{"input":1,"output":2,"reasoning":2,"input_audio":4}}
  }},
  "wrong-id":{"id":"other","models":{"x":{"cost":{"input":99,"output":99}}}}
 }`)
	encoded, err := modelsDevSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	table, err := decodeModelsDevSnapshot(encoded, "modelsdev")
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Models) != 2 || table.Models["a/b/c"].Input != 1e-6 {
		t.Fatalf("model/provider qualification failed: %+v", table.Models)
	}
	if bytes.Contains(encoded, []byte("reasoning")) || bytes.Contains(encoded, []byte("input_audio")) {
		t.Fatal("unsupported usage dimensions persisted")
	}
}

func TestEmbeddedModelsDevSnapshot(t *testing.T) {
	table := embeddedModelsDevTable()
	if table == nil || !table.ProviderScoped || len(table.Models) == 0 {
		t.Fatal("embedded provider prices unavailable")
	}
	if embeddedModelsDevTable() != table {
		t.Fatal("embedded table was rebuilt")
	}
	want := map[string]Rates{
		"openai/gpt-5.3-codex-spark":      {Input: 1.75e-6, Output: 14e-6, CacheRead: 0.175 / 1e6},
		"github-copilot/claude-haiku-4.5": {Input: 1e-6, Output: 5e-6, CacheRead: 0.1 / 1e6, CacheWrite5m: 1.25e-6},
		"opencode/hy3-free":               {Free: true},
		"opencode/big-pickle":             {Free: true},
	}
	for key, rates := range want {
		if got := table.Models[key]; !sameModelsDevRates(got, rates) {
			t.Fatalf("%s=%+v want %+v", key, got, rates)
		}
	}
	for _, key := range []string{"gpt-5.3-codex-spark", "openai/gpt-5.6-luna-fast", "openai/gpt-5.4-mini-fast", "ollama/gemma4:31b-cloud"} {
		if _, ok := table.Models[key]; ok {
			t.Fatalf("invented exact model price %q", key)
		}
	}
}

// Decimal source values can differ by one float ULP after /1e6 conversion.
// The tolerance here is less than one micro-dollar for a trillion tokens.
func sameModelsDevRates(a, b Rates) bool {
	if a.Free != b.Free || a.Long.Threshold != b.Long.Threshold {
		return false
	}
	left := [...]float64{a.Input, a.Output, a.CacheRead, a.CacheWrite5m, a.CacheWrite1h, a.InputBatch, a.OutputBatch, a.Long.Input, a.Long.Output, a.Long.CacheRead, a.Long.CacheWrite5m, a.Long.CacheWrite1h}
	right := [...]float64{b.Input, b.Output, b.CacheRead, b.CacheWrite5m, b.CacheWrite1h, b.InputBatch, b.OutputBatch, b.Long.Input, b.Long.Output, b.Long.CacheRead, b.Long.CacheWrite5m, b.Long.CacheWrite1h}
	for i, v := range left {
		if math.Abs(v-right[i]) > 1e-19 {
			return false
		}
	}
	return true
}
