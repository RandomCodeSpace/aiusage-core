package collect

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// tokenPricer values a charge at one micro-USD per non-cache token, refusing
// the models it does not know. It exercises the collector's stamping path
// without pulling a real price table into this package.
type tokenPricer struct {
	known    map[string]bool
	refreshN int
}

func (p *tokenPricer) PriceEvent(e model.UsageEvent) (int64, string, bool) {
	if !p.known[e.Model] {
		return 0, "", false
	}
	micro := e.InputTokens + e.OutputTokens
	if model.ReasoningModeFor(e.Tool) == model.ReasoningAdditive {
		micro += e.ReasoningTokens
	}
	return micro, "embedded-test", true
}

func (p *tokenPricer) Refresh(context.Context) error {
	p.refreshN++
	return nil
}

// TestRunCycleStampsCost proves the collector stamps a cost on every new
// event-level record it can price, records the price source, and leaves the
// rest unpriced rather than stamping a zero.
func TestRunCycleStampsCost(t *testing.T) {
	priced := model.UsageEvent{
		Tool: model.ToolCodex, Model: "gpt-5", EventTime: refDay.Add(time.Hour),
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		DedupKey: "codex|priced", Kind: model.KindUsage,
	}
	unknown := model.UsageEvent{
		Tool: model.ToolCodex, Model: "mystery", EventTime: refDay.Add(2 * time.Hour),
		InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		DedupKey: "codex|unknown", Kind: model.KindUsage,
	}
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Events: []model.UsageEvent{priced, unknown}}
		},
	}
	st := newFakeStore()
	p := &tokenPricer{known: map[string]bool{"gpt-5": true}}

	if _, err := RunOnce(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}, WithPricer(p)); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	evs, _ := st.ListEvents(context.Background(), store.Filter{})
	byKey := map[string]model.UsageEvent{}
	for _, e := range evs {
		byKey[e.DedupKey] = e
	}
	got := byKey["codex|priced"]
	if c, ok := got.Cost(); !ok || c != 150 {
		t.Errorf("priced event cost = %d,%v want 150,true", c, ok)
	}
	if got.PriceSource != "embedded-test" {
		t.Errorf("price source = %q, want embedded-test", got.PriceSource)
	}
	got = byKey["codex|unknown"]
	if c, ok := got.Cost(); ok || c != 0 {
		t.Errorf("unpriceable event cost = %d,%v want 0,false (NULL, not free)", c, ok)
	}
	if got.PriceSource != "" {
		t.Errorf("unpriceable event price source = %q, want empty", got.PriceSource)
	}
	if p.refreshN != 1 {
		t.Errorf("refresh attempts = %d, want exactly 1 per cycle", p.refreshN)
	}
}

// TestRunCycleWithoutPricerLeavesEventsUnpriced pins the default: no pricer
// means every row is stored unpriced, which display surfaces value later.
func TestRunCycleWithoutPricerLeavesEventsUnpriced(t *testing.T) {
	ev := model.UsageEvent{
		Tool: model.ToolCodex, Model: "gpt-5", EventTime: refDay,
		InputTokens: 100, TotalTokens: 100, DedupKey: "codex|nopricer", Kind: model.KindUsage,
	}
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{Events: []model.UsageEvent{ev}} },
	}
	st := newFakeStore()
	if _, err := RunOnce(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	evs, _ := st.ListEvents(context.Background(), store.Filter{})
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if _, ok := evs[0].Cost(); ok {
		t.Errorf("event was priced without a pricer: %+v", evs[0])
	}
}

// TestSyntheticEventCarriesProviderAndCost covers the aggregate path: the
// snapshot's billing provider must reach the synthetic delta event (without it,
// hermes/gemini/agy would store a blank provider and never price), and the
// delta must be stamped like any other event.
func TestSyntheticEventCarriesProviderAndCost(t *testing.T) {
	snap := model.AggregateSnapshot{
		Tool: model.ToolGemini, Key: "turn-1", Model: "gemini-3-flash-preview",
		Provider: model.ProviderGoogle, SessionID: "s1",
		InputTokens: 300, OutputTokens: 200, ReasoningTokens: 100, TotalTokens: 600,
		ObservedTime: refDay.Add(time.Hour),
	}
	ad := &fakeAdapter{
		id: model.ToolGemini, class: model.Aggregate,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Snapshots: []model.AggregateSnapshot{snap}}
		},
	}
	st := newFakeStore()
	p := &tokenPricer{known: map[string]bool{"gemini-3-flash-preview": true}}

	if _, err := RunOnce(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}, WithPricer(p)); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	evs, _ := st.ListEvents(context.Background(), store.Filter{})
	if len(evs) != 1 {
		t.Fatalf("synthetic events = %d, want 1", len(evs))
	}
	if evs[0].Provider != model.ProviderGoogle {
		t.Fatalf("synthetic provider = %q, want %q", evs[0].Provider, model.ProviderGoogle)
	}
	// gemini bills reasoning on top of output, so the delta costs in+out+reasoning.
	if c, ok := evs[0].Cost(); !ok || c != 600 {
		t.Errorf("synthetic cost = %d,%v want 600,true", c, ok)
	}
}

// TestAdapterSuppliedCostSurvivesTheLadder. A cost the adapter already stamped
// came from the HARNESS's own accounting — copilot's vendor-priced nano-AIU
// figure, crush's session cost, goose's provider-reported number — and the
// ladder is a public-rate-card estimate of the same charge. The estimate must
// not overwrite the vendor's own value, which is exactly what happened whenever
// the table happened to know the model id.
func TestAdapterSuppliedCostSurvivesTheLadder(t *testing.T) {
	vendor := model.UsageEvent{
		Tool: model.ToolCopilot, Model: "gpt-5", EventTime: refDay.Add(time.Hour),
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		DedupKey: "copilot|vendor", Kind: model.KindUsage,
	}
	vendor.SetCost(7, "copilot-nano-aiu")

	ad := &fakeAdapter{
		id: model.ToolCopilot, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Events: []model.UsageEvent{vendor}}
		},
	}
	st := newFakeStore()
	// The pricer knows this model and would value the same charge at 150.
	p := &tokenPricer{known: map[string]bool{"gpt-5": true}}

	if _, err := RunOnce(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}, WithPricer(p)); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	evs, _ := st.ListEvents(context.Background(), store.Filter{})
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if c, ok := evs[0].Cost(); !ok || c != 7 {
		t.Errorf("cost = %d,%v want the vendor's 7,true", c, ok)
	}
	if evs[0].PriceSource != "copilot-nano-aiu" {
		t.Errorf("price source = %q, want the adapter's own stamp", evs[0].PriceSource)
	}
}
