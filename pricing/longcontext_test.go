package pricing

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// solRates are gpt-5.6-sol's published rates, base and long-context card, copied
// from LiteLLM (and matching OpenAI's own two-card pricing table:
// $5.00 / $0.50 / $6.25 / $30.00 short, $10.00 / $1.00 / $12.50 / $45.00 long).
// A real card is used because the whole point of the tier is the size of the
// swing, and invented rates would hide it.
var solRates = Rates{
	Input:        5e-06,
	Output:       3e-05,
	CacheRead:    5e-07,
	CacheWrite5m: 6.25e-06,
	Long: LongContext{
		Threshold:    272_000,
		Input:        1e-05,
		Output:       4.5e-05,
		CacheRead:    1e-06,
		CacheWrite5m: 1.25e-05,
	},
}

// marginalCost is the OTHER reading of LiteLLM's "_above_<N>k_tokens" naming —
// each bucket split at the threshold, the excess billed at the long rate — which
// is what the field name literally says and what no provider documents. It
// exists only so the tests can assert aiusage does NOT do this.
func marginalCost(r Rates, c Charge) int64 {
	split := func(tokens int64, base, above float64) float64 {
		if above > 0 && tokens > r.Long.Threshold {
			return float64(r.Long.Threshold)*base + float64(tokens-r.Long.Threshold)*above
		}
		return float64(tokens) * base
	}
	usd := split(c.Input, r.Input, r.Long.Input) +
		split(c.Output, r.Output, r.Long.Output) +
		split(c.CacheRead, r.CacheRead, r.Long.CacheRead) +
		split(c.CacheWrite5m, r.CacheWrite5m, r.Long.CacheWrite5m)
	return microUSD(usd)
}

// TestThresholdComesFromTheFieldName pins the parse that recovers a per-model
// boundary from LiteLLM's key names. All five boundaries the live table publishes
// are covered, because a hardcoded 200K would be wrong for four of them.
func TestThresholdComesFromTheFieldName(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want int64
	}{
		{"input_cost_per_token_above_128k_tokens", 128_000},
		{"input_cost_per_token_above_200k_tokens", 200_000},
		{"input_cost_per_token_above_256k_tokens", 256_000},
		{"input_cost_per_token_above_272k_tokens", 272_000},
		{"input_cost_per_token_above_512k_tokens", 512_000},
	} {
		t.Run(tc.key, func(t *testing.T) {
			raw := map[string]json.RawMessage{
				"model": json.RawMessage(`{"input_cost_per_token": 1e-06, "output_cost_per_token": 1e-05,
					"` + tc.key + `": 2e-06}`),
			}
			r, ok := decodeModels(raw)["model"]
			if !ok {
				t.Fatalf("entry did not decode")
			}
			if r.Long.Threshold != tc.want {
				t.Errorf("threshold = %d, want %d", r.Long.Threshold, tc.want)
			}
			if r.Long.Input != 2e-06 {
				t.Errorf("long input = %v, want 2e-06", r.Long.Input)
			}
		})
	}
}

// TestLongContextCardReadsEveryBucket checks each above-threshold key lands in
// its own field, including the 1h cache write CROSSED with long context, which
// is the one composite in LiteLLM's grammar aiusage models both halves of.
func TestLongContextCardReadsEveryBucket(t *testing.T) {
	raw := map[string]json.RawMessage{"m": json.RawMessage(`{
		"input_cost_per_token": 3e-06,
		"output_cost_per_token": 1.5e-05,
		"cache_read_input_token_cost": 3e-07,
		"cache_creation_input_token_cost": 3.75e-06,
		"cache_creation_input_token_cost_above_1hr": 6e-06,
		"input_cost_per_token_above_200k_tokens": 6e-06,
		"output_cost_per_token_above_200k_tokens": 2.25e-05,
		"cache_read_input_token_cost_above_200k_tokens": 6e-07,
		"cache_creation_input_token_cost_above_200k_tokens": 7.5e-06,
		"cache_creation_input_token_cost_above_1hr_above_200k_tokens": 1.2e-05
	}`)}
	got := decodeModels(raw)["m"].Long
	want := LongContext{
		Threshold:    200_000,
		Input:        6e-06,
		Output:       2.25e-05,
		CacheRead:    6e-07,
		CacheWrite5m: 7.5e-06,
		CacheWrite1h: 1.2e-05,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("long card = %+v, want %+v", got, want)
	}
}

// TestLongContextCardIgnoresOtherTiersAndUnits is the allow-list half of the
// parse. "_priority" and "_flex" are service tiers this package does not model,
// so reading one as THE long-context rate would bill a standard request off a
// priority card; the per-character/image/video keys are not token rates at all.
// None of them may create a threshold either — a model whose only above-fields
// are priority rates has no tier aiusage can apply.
func TestLongContextCardIgnoresOtherTiersAndUnits(t *testing.T) {
	raw := map[string]json.RawMessage{"m": json.RawMessage(`{
		"input_cost_per_token": 5e-06,
		"output_cost_per_token": 3e-05,
		"input_cost_per_token_above_272k_tokens_priority": 1e-05,
		"input_cost_per_token_above_272k_tokens_flex": 5e-06,
		"output_cost_per_token_above_272k_tokens_flex": 2.25e-05,
		"cache_read_input_token_cost_above_272k_tokens_priority": 1e-06,
		"input_cost_per_character_above_128k_tokens": 1e-06,
		"input_cost_per_image_above_128k_tokens": 2e-06,
		"input_cost_per_video_per_second_above_128k_tokens": 3e-06,
		"output_cost_per_character_above_128k_tokens": 4e-06
	}`)}
	if got := decodeModels(raw)["m"].Long; got != (LongContext{}) {
		t.Errorf("long card = %+v, want none: no field here is a long-context token rate", got)
	}
}

// TestLongContextTakesTheLowestThresholdDeterministically covers a shape upstream
// does not currently publish (verified: no model in the 3,020-entry table carries
// two boundaries). If it ever does, the card must not be blended across
// boundaries and must not depend on map iteration order.
func TestLongContextTakesTheLowestThresholdDeterministically(t *testing.T) {
	raw := map[string]json.RawMessage{"m": json.RawMessage(`{
		"input_cost_per_token": 1e-06,
		"output_cost_per_token": 1e-05,
		"input_cost_per_token_above_200k_tokens": 2e-06,
		"input_cost_per_token_above_512k_tokens": 4e-06,
		"output_cost_per_token_above_512k_tokens": 4e-05
	}`)}
	for i := 0; i < 20; i++ {
		got := decodeModels(raw)["m"].Long
		want := LongContext{Threshold: 200_000, Input: 2e-06}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: long card = %+v, want %+v (lowest boundary, its rates only)", i, got, want)
		}
	}
}

// TestWholeRequestNotMarginal is the semantic fixture. A crossing request bills
// EVERY bucket off the long card ("for the full request", per OpenAI; "all
// tokens (input and output)", per Google), not just the excess above the line.
//
// The two readings differ by 33x on this one charge, and by a factor of ten
// across the 551 crossing rows measured on this machine's ledger (+$74.53
// whole-request against +$7.09 marginal), because a crossing turn is broad
// rather than deep: a 356K context is a 300K cache read plus a 56K input, and
// neither bucket alone clears 272K by much.
func TestWholeRequestNotMarginal(t *testing.T) {
	c := Charge{Input: 56_000, CacheRead: 300_000, Output: 2_000}
	if c.ContextTokens() != 356_000 {
		t.Fatalf("context = %d, want 356000", c.ContextTokens())
	}

	// base:     56000*5e-6 + 300000*5e-7 + 2000*3e-5   = 0.280 + 0.150 + 0.060
	// long:     56000*1e-5 + 300000*1e-6 + 2000*4.5e-5 = 0.560 + 0.300 + 0.090
	// marginal: only the cache read clears the line, by 28000 tokens.
	const (
		base     = 490_000
		whole    = 950_000
		marginal = 504_000
	)
	if got := (Rates{Input: solRates.Input, Output: solRates.Output, CacheRead: solRates.CacheRead}).Cost(c); got != base {
		t.Fatalf("base-rates cost = %d, want %d", got, base)
	}
	if got := marginalCost(solRates, c); got != marginal {
		t.Fatalf("marginal reading = %d, want %d", got, marginal)
	}
	got := solRates.Cost(c)
	if got == marginal {
		t.Fatalf("cost = %d: the whole request must switch cards, not just the excess above the line", got)
	}
	if got != whole {
		t.Fatalf("cost = %d, want %d (every bucket at the long rate)", got, whole)
	}
	if wholeDelta, marginalDelta := got-base, int64(marginal-base); wholeDelta < 10*marginalDelta {
		t.Errorf("whole-request delta %d is not an order of magnitude over the marginal delta %d; "+
			"the fixture no longer distinguishes the two readings", wholeDelta, marginalDelta)
	}
}

// TestCachedContextSelectsTheLongCard is the trap this change exists to avoid.
// The tier is chosen by the request's WHOLE prompt, cached or not: an
// implementation that thresholds on uncached Input alone looks correct, passes
// every other test here, and fixes 3% of the exposure — 17 of the 551 crossing
// rows on this machine's ledger cross on Input alone, all 551 cross on
// Input + CacheRead.
func TestCachedContextSelectsTheLongCard(t *testing.T) {
	c := Charge{Input: 50_000, CacheRead: 250_000, Output: 1_000}
	if c.Input > solRates.Long.Threshold {
		t.Fatal("fixture is useless: uncached input alone already crosses")
	}
	if c.ContextTokens() <= solRates.Long.Threshold {
		t.Fatal("fixture is useless: the whole prompt does not cross")
	}

	// long: 50000*1e-5 + 250000*1e-6 + 1000*4.5e-5 = 0.500 + 0.250 + 0.045
	// base: 50000*5e-6 + 250000*5e-7 + 1000*3e-5   = 0.250 + 0.125 + 0.030
	const (
		whole      = 795_000
		inputOnly  = 405_000
		difference = whole - inputOnly
	)
	got := solRates.Cost(c)
	if got == inputOnly {
		t.Fatalf("cost = %d: the threshold was measured on uncached input, ignoring %d cached tokens",
			got, c.CacheRead)
	}
	if got != whole {
		t.Fatalf("cost = %d, want %d (cache reads count toward the threshold)", got, whole)
	}
	if difference <= 0 {
		t.Fatal("fixture cannot tell the two thresholds apart")
	}
}

// TestBelowThresholdIsUnchanged is the regression: everything that does not cross
// must price exactly as it did before this package learned about tiers. The
// expected numbers are the pre-change output for the same rates and charges.
func TestBelowThresholdIsUnchanged(t *testing.T) {
	flat := Rates{
		Input: solRates.Input, Output: solRates.Output,
		CacheRead: solRates.CacheRead, CacheWrite5m: solRates.CacheWrite5m,
	}
	cases := []struct {
		name   string
		charge Charge
		want   int64
	}{
		// 100000*5e-6 + 5000*3e-5 = 0.500 + 0.150
		{"well below", Charge{Input: 100_000, Output: 5_000}, 650_000},
		// 200000*5e-6 + 71999*5e-7 + 1000*3e-5 = 1.000 + 0.0359995 + 0.030
		{"one token below", Charge{Input: 200_000, CacheRead: 71_999, Output: 1_000}, 1_066_000},
		// Exactly at the boundary: the providers say "> 272K", not ">=".
		{"exactly at the boundary", Charge{Input: 272_000, Output: 1_000}, 1_390_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.charge.ContextTokens() > solRates.Long.Threshold {
				t.Fatalf("case crosses the threshold (%d)", tc.charge.ContextTokens())
			}
			flatCost := flat.Cost(tc.charge)
			if flatCost != tc.want {
				t.Fatalf("pre-tier rates = %d, want %d", flatCost, tc.want)
			}
			if got := solRates.Cost(tc.charge); got != flatCost {
				t.Errorf("tiered rates = %d, want %d: a request under the threshold changed price", got, flatCost)
			}
		})
	}
}

// TestEmbeddedSnapshotBelowThresholdIsUnchanged widens the regression to the
// whole shipped table: for every model, a modest charge must cost exactly what
// the same rates cost with the long-context card removed. If tiering ever leaks
// into ordinary requests, this fails on every affected model at once.
func TestEmbeddedSnapshotBelowThresholdIsUnchanged(t *testing.T) {
	tbl := embeddedTable()
	if tbl == nil {
		t.Fatal("embedded snapshot did not load")
	}
	c := Charge{Input: 20_000, Output: 2_000, CacheRead: 40_000, CacheWrite5m: 5_000}
	var tiered int
	for name, r := range tbl.Models {
		if r.Long.Threshold > 0 {
			tiered++
		}
		flat := r
		flat.Long = LongContext{}
		if got, want := r.Cost(c), flat.Cost(c); got != want {
			t.Fatalf("%s: sub-threshold charge = %d, want %d", name, got, want)
		}
	}
	if tiered == 0 {
		t.Error("no model in the embedded snapshot carries a long-context card: the generator stripped the fields again")
	}
}

// TestEmbeddedSnapshotCarriesPublishedTiers is the air-gap guard. The snapshot's
// filter used to keep only seven fixed field names, so the tier rates were
// physically absent from the binary and a firewalled install flat-priced every
// long-context turn with no way to notice. gpt-5.6-sol is the model this
// machine's ledger actually crossed on.
func TestEmbeddedSnapshotCarriesPublishedTiers(t *testing.T) {
	tbl := embeddedTable()
	if tbl == nil {
		t.Fatal("embedded snapshot did not load")
	}
	r, ok := tbl.Lookup(model.ProviderOpenAI, "gpt-5.6-sol")
	if !ok {
		t.Fatal("embedded snapshot cannot price gpt-5.6-sol")
	}
	want := LongContext{
		Threshold:    272_000,
		Input:        1e-05,
		Output:       4.5e-05,
		CacheRead:    1e-06,
		CacheWrite5m: 1.25e-05,
	}
	if !reflect.DeepEqual(r.Long, want) {
		t.Errorf("gpt-5.6-sol long card = %+v, want %+v", r.Long, want)
	}
}

// TestTierOnlyEntryStaysUnpriced keeps "unpriced is not $0.00" true for the new
// fields: an entry publishing only above-threshold rates prices nothing, must not
// count as a table hit, and must let the next rung try.
func TestTierOnlyEntryStaysUnpriced(t *testing.T) {
	raw := map[string]json.RawMessage{"tier-only": json.RawMessage(`{
		"input_cost_per_token_above_200k_tokens": 6e-06,
		"output_cost_per_token_above_200k_tokens": 2.25e-05
	}`)}
	if models := decodeModels(raw); len(models) != 0 {
		t.Fatalf("decoded %v, want the entry skipped as unpriceable", models)
	}

	e := New(Options{})
	e.refreshed = &Table{Source: "litellm-test", Models: map[string]Rates{
		"tier-only": {Long: LongContext{Threshold: 200_000, Input: 6e-06}},
	}}
	micro, source, ok := e.Price(Charge{Model: "tier-only", Input: 300_000})
	if ok || micro != 0 || source != "" {
		t.Errorf("tier-only model: %d,%q,%v want unpriced", micro, source, ok)
	}
}

// TestAggregateChargeNeverTiers protects the display-time valuation path, which
// prices one Charge per (model, tool, tier) GROUP rather than per request. Its
// token counts are a sum over many requests, so they are not a prompt size: a
// thousand short turns must not add up to one long-context request.
func TestAggregateChargeNeverTiers(t *testing.T) {
	c := Charge{Input: 1_000_000, Output: 100_000, Aggregate: true}
	flat := solRates
	flat.Long = LongContext{}
	if got, want := solRates.Cost(c), flat.Cost(c); got != want {
		t.Errorf("aggregate charge = %d, want %d (base rates)", got, want)
	}

	single := c
	single.Aggregate = false
	if solRates.Cost(single) == flat.Cost(single) {
		t.Error("the fixture crosses no threshold, so it proves nothing about aggregates")
	}
}

// TestLongContextStampsThePriceSource checks a row billed off the second card
// says so. price_source is an open label nothing parses, so the suffix rides on
// the existing vocabulary rather than a new column, and a sub-threshold charge
// against the same model keeps the plain stamp.
func TestLongContextStampsThePriceSource(t *testing.T) {
	e := New(Options{})
	e.refreshed = &Table{Source: "litellm-2026-08-16", Models: map[string]Rates{"m": solRates}}

	_, source, ok := e.Price(Charge{Model: "m", Input: 300_000})
	if !ok || source != "litellm-2026-08-16+long-context" {
		t.Errorf("crossing charge stamped %q,%v want litellm-2026-08-16+long-context", source, ok)
	}
	_, source, ok = e.Price(Charge{Model: "m", Input: 1_000})
	if !ok || source != "litellm-2026-08-16" {
		t.Errorf("short charge stamped %q,%v want the plain table stamp", source, ok)
	}
}

// TestOverrideKeepsThePublishedTier pins how a config override composes with a
// tier it cannot express. The card comes from the rung the override displaced,
// because "override this rate" is not "delete the long-context tier" — without
// that, setting one rate would silently restore flat pricing for the model. The
// rate the user DID name still wins above the threshold: the top rung is the top
// rung on both cards, and 4.5e-05 here would be the table quietly overruling it.
func TestOverrideKeepsThePublishedTier(t *testing.T) {
	e := New(Options{Overrides: map[string]Rates{"m": {Output: 6e-05}}})
	e.refreshed = &Table{Source: "litellm-2026-08-16", Models: map[string]Rates{"m": solRates}}

	c := Charge{Model: "m", Input: 300_000, Output: 1_000}
	micro, source, ok := e.Price(c)
	// 300000*1e-5 (the table's long input) + 1000*6e-5 (the user's output rate)
	if !ok || micro != 3_060_000 {
		t.Fatalf("override + tier = %d,%v want 3060000", micro, ok)
	}
	if !strings.HasSuffix(source, "+long-context") {
		t.Errorf("source = %q, want the long-context suffix", source)
	}
}

// TestSnapshotFieldCoversEveryDecodedTag stops the snapshot generator and the
// decoder from drifting apart: every fixed field name the decoder reads by struct
// tag must be one the generator is told to keep. A field dropped here is a rate
// that silently vanishes from every air-gapped install.
func TestSnapshotFieldCoversEveryDecodedTag(t *testing.T) {
	typ := reflect.TypeOf(litellmEntry{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if !litellmFixedFields[name] {
			t.Errorf("litellmFixedFields is missing %q, which the decoder reads", name)
		}
		if !SnapshotField(name) {
			t.Errorf("SnapshotField(%q) = false: the generator would strip a field the decoder reads", name)
		}
	}
	if got, want := len(litellmFixedFields), typ.NumField()-1; got != want {
		t.Errorf("litellmFixedFields has %d entries for %d tagged fields", got, want)
	}
	for _, key := range []string{
		"input_cost_per_token_above_272k_tokens",
		"cache_creation_input_token_cost_above_1hr_above_200k_tokens",
	} {
		if !SnapshotField(key) {
			t.Errorf("SnapshotField(%q) = false, want the tier fields kept", key)
		}
	}
	for _, key := range []string{
		"input_cost_per_token_above_272k_tokens_priority",
		"input_cost_per_token_priority",
		"litellm_provider",
		"max_input_tokens",
	} {
		if SnapshotField(key) {
			t.Errorf("SnapshotField(%q) = true, want it filtered out", key)
		}
	}
}
