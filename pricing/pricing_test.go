package pricing

import (
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// TestCostRoundsHalfUp pins the micro-USD rounding rule. Sub-micro fractions
// must round half up so a long run of tiny charges neither vanishes nor
// inflates.
func TestCostRoundsHalfUp(t *testing.T) {
	cases := []struct {
		name string
		rate float64
		toks int64
		want int64
	}{
		{"below half rounds down", 4e-07, 1, 0},
		{"half rounds up", 5e-07, 1, 1},
		{"above half rounds up", 9e-07, 1, 1},
		{"three dollars per million", 3e-06, 1_000_000, 3_000_000},
		{"exact micro", 1e-06, 7, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Rates{Input: tc.rate}.Cost(Charge{Input: tc.toks})
			if got != tc.want {
				t.Errorf("cost = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCostComponentsAndFallbacks checks every token bucket contributes at its
// own rate, and that an unpublished cache rate falls back to the input rate
// rather than to zero.
func TestCostComponentsAndFallbacks(t *testing.T) {
	full := Rates{
		Input:        1e-06,
		Output:       1e-05,
		CacheRead:    1e-07,
		CacheWrite5m: 1.25e-06,
		CacheWrite1h: 2e-06,
	}
	got := full.Cost(Charge{Input: 1000, Output: 100, CacheRead: 10_000, CacheWrite5m: 2000, CacheWrite1h: 1000})
	// 1000*1 + 100*10 + 10000*0.1 + 2000*1.25 + 1000*2 = 1000+1000+1000+2500+2000
	if want := int64(7500); got != want {
		t.Errorf("full-rate cost = %d, want %d", got, want)
	}

	bare := Rates{Input: 1e-06, Output: 1e-05}
	got = bare.Cost(Charge{CacheRead: 1000, CacheWrite5m: 1000, CacheWrite1h: 1000})
	// Every cache bucket falls back to the input rate: 3000 tokens * 1 micro.
	if want := int64(3000); got != want {
		t.Errorf("fallback cost = %d, want %d", got, want)
	}
}

// TestCostBatchTier verifies a batch service tier uses the batch rates when the
// table publishes them, and the standard rates when it does not.
func TestCostBatchTier(t *testing.T) {
	r := Rates{Input: 1e-06, Output: 1e-05, InputBatch: 5e-07, OutputBatch: 5e-06}
	c := Charge{Input: 1000, Output: 1000}

	if got, want := r.Cost(c), int64(11_000); got != want {
		t.Errorf("standard tier = %d, want %d", got, want)
	}
	c.ServiceTier = "batch"
	if got, want := r.Cost(c), int64(5500); got != want {
		t.Errorf("batch tier = %d, want %d", got, want)
	}

	noBatch := Rates{Input: 1e-06, Output: 1e-05}
	if got, want := noBatch.Cost(c), int64(11_000); got != want {
		t.Errorf("batch tier without batch rates = %d, want %d (standard)", got, want)
	}
}

// TestReasoningModePerAdapter is the per-adapter pricing fixture: a tool whose
// reasoning tokens sit INSIDE output must not pay for them twice, and a tool
// that reports them alongside output must pay for them at the output rate.
// The expected side of each case comes from model.ReasoningModeFor, so adding a
// tool there without deciding its mode shows up here as a wrong bill.
func TestReasoningModePerAdapter(t *testing.T) {
	rates := Rates{Input: 1e-06, Output: 1e-05}
	const (
		in        = 1000
		out       = 500
		reasoning = 200
	)
	subsetCost := int64(in*1 + out*10)               // 6000 micro
	additiveCost := int64(in*1 + (out+reasoning)*10) // 8000 micro

	for _, tool := range []string{
		model.ToolClaudeCode, model.ToolCodex, model.ToolHermes,
		model.ToolOpenCode, model.ToolGemini, model.ToolAgy, model.ToolCopilot,
	} {
		t.Run(tool, func(t *testing.T) {
			ev := model.UsageEvent{
				Tool:            tool,
				Model:           "m",
				InputTokens:     in,
				OutputTokens:    out,
				ReasoningTokens: reasoning,
			}
			c := ChargeFor(ev)
			additive := model.ReasoningModeFor(tool) == model.ReasoningAdditive
			if c.AdditiveReasoning != additive {
				t.Fatalf("AdditiveReasoning = %v, want %v", c.AdditiveReasoning, additive)
			}
			want := subsetCost
			if additive {
				want = additiveCost
			}
			if got := rates.Cost(c); got != want {
				t.Errorf("cost = %d, want %d", got, want)
			}
		})
	}
}

// TestChargeForCacheTTLSplit checks the transient cache-write split reaches the
// charge intact, and that a split which does not account for the whole
// cache-creation count is discarded in favour of the cheaper all-5m reading
// rather than silently dropping the unexplained tokens.
func TestChargeForCacheTTLSplit(t *testing.T) {
	consistent := model.UsageEvent{
		CacheCreationTokens: 300,
		CacheTTL:            model.CacheWriteTTL{Ephemeral5m: 200, Ephemeral1h: 100},
	}
	c := ChargeFor(consistent)
	if c.CacheWrite5m != 200 || c.CacheWrite1h != 100 {
		t.Errorf("split = %d/%d, want 200/100", c.CacheWrite5m, c.CacheWrite1h)
	}

	partial := model.UsageEvent{
		CacheCreationTokens: 300,
		CacheTTL:            model.CacheWriteTTL{Ephemeral5m: 50},
	}
	c = ChargeFor(partial)
	if c.CacheWrite5m != 300 || c.CacheWrite1h != 0 {
		t.Errorf("inconsistent split = %d/%d, want 300/0", c.CacheWrite5m, c.CacheWrite1h)
	}

	none := model.UsageEvent{CacheCreationTokens: 300}
	c = ChargeFor(none)
	if c.CacheWrite5m != 300 || c.CacheWrite1h != 0 {
		t.Errorf("absent split = %d/%d, want 300/0", c.CacheWrite5m, c.CacheWrite1h)
	}
}

// TestLookupPrefersProviderNamespace verifies a provider-namespaced key wins
// over the bare model id, so a proxied model is priced at the proxy's published
// rates when the table has them.
func TestLookupPrefersProviderNamespace(t *testing.T) {
	tbl := &Table{Source: "test", Models: map[string]Rates{
		"gemini-2.5-pro":        {Input: 9e-06, Output: 9e-06},
		"gemini/gemini-2.5-pro": {Input: 1e-06, Output: 1e-06},
	}}
	r, ok := tbl.Lookup(model.ProviderGoogle, "gemini-2.5-pro")
	if !ok || r.Input != 1e-06 {
		t.Errorf("google lookup = %+v,%v want the gemini/ namespaced rates", r, ok)
	}
	r, ok = tbl.Lookup("", "gemini-2.5-pro")
	if !ok || r.Input != 9e-06 {
		t.Errorf("bare lookup = %+v,%v want the bare rates", r, ok)
	}
}

// TestLookupStripsVendorPrefix covers ids that arrive already namespaced by the
// source (opencode/openrouter style): the bare tail is the last candidate tried.
func TestLookupStripsVendorPrefix(t *testing.T) {
	tbl := &Table{Source: "test", Models: map[string]Rates{"gpt-5": {Input: 1e-06}}}
	if _, ok := tbl.Lookup("", "openrouter/gpt-5"); !ok {
		t.Errorf("namespaced id did not fall back to the bare model key")
	}
}

// TestUnpriceableTableRowMisses proves an all-zero table row is a MISS, not a
// free price: the next rung of the ladder must get its chance.
func TestUnpriceableTableRowMisses(t *testing.T) {
	tbl := &Table{Source: "test", Models: map[string]Rates{"free": {}}}
	if _, ok := tbl.Lookup("", "free"); ok {
		t.Errorf("all-zero rates reported as a hit")
	}
}

// TestEmbeddedSnapshotPricesKnownModels guards the go:embed floor: a corrupt or
// mis-filtered snapshot must fail here rather than silently leave every install
// unpriced.
func TestEmbeddedSnapshotPricesKnownModels(t *testing.T) {
	tbl := embeddedTable()
	if tbl == nil {
		t.Fatal("embedded snapshot did not load")
	}
	if len(tbl.Models) < 100 {
		t.Errorf("embedded snapshot has %d models, want a filtered table of at least 100", len(tbl.Models))
	}
	if tbl.Source == "embedded-unknown" || tbl.Source == "" {
		t.Errorf("embedded source = %q, want embedded-<snapshot date>", tbl.Source)
	}
	for _, tc := range []struct{ provider, name string }{
		{model.ProviderAnthropic, "claude-sonnet-4-6"},
		{model.ProviderOpenAI, "gpt-5"},
		{model.ProviderOpenAI, "gpt-5-codex"},
		{model.ProviderGoogle, "gemini-3-flash-preview"},
	} {
		r, ok := tbl.Lookup(tc.provider, tc.name)
		if !ok {
			t.Errorf("embedded snapshot cannot price %s/%s", tc.provider, tc.name)
			continue
		}
		if r.Input <= 0 || r.Output <= 0 {
			t.Errorf("%s rates = %+v, want nonzero input and output", tc.name, r)
		}
	}
}
