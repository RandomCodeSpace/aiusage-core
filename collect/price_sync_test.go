package collect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/pricing"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

type latePricer struct {
	available bool
	refresh   bool
}

func (p *latePricer) Refresh(context.Context) error {
	if p.refresh {
		p.available = true
	}
	return nil
}

func (p *latePricer) Revision() string {
	if p.available {
		return "rates-available"
	}
	return "rates-missing"
}

func (p *latePricer) PriceEvent(e model.UsageEvent) (int64, string, bool) {
	return e.TotalTokens * 2, "litellm-test", p.available
}

func TestCycleSyncsHistoricalPricesAfterRefresh(t *testing.T) {
	ctx := context.Background()
	ledger := realStore(t)
	event := cycleEvent("historical", model.ToolCodex, refDay, 100)
	if _, err := ledger.InsertEvents(ctx, []model.UsageEvent{event}); err != nil {
		t.Fatal(err)
	}
	p := &latePricer{}
	reg := adapter.NewRegistry() // no source replay is needed to price history
	run := func(want int) {
		t.Helper()
		stats, err := RunOnce(ctx, reg, ledger, adapter.DiscoverConfig{}, WithPricer(p))
		if err != nil || len(stats.Errors) != 0 || stats.PricesSynced != want || stats.EventsInserted != 0 {
			t.Fatalf("cycle = %+v, %v; want %d prices and no inserted events", stats, err, want)
		}
	}
	run(0)
	p.refresh = true
	run(1)
	run(0)
	sum, err := ledger.Summarize(ctx, store.Filter{})
	if err != nil || sum.Totals.Events != 1 || sum.Totals.Total != 100 || sum.Totals.CostMicroUSD != 200 || sum.Totals.UnpricedEvents != 0 || sum.Totals.ComputedCostEvents != 1 {
		t.Fatalf("synced totals = %+v, %v", sum, err)
	}
	assertCycleRollupMatchesLedger(t, ledger, "tool")
}

type failingPriceSyncStore struct{ *fakeStore }

func (s *failingPriceSyncStore) SyncUnpriced(context.Context, string, func(model.UsageEvent) (int64, string, bool)) (int, error) {
	return 0, errors.New("injected price sync failure")
}

func TestCycleReportsPriceSyncFailureAndContinues(t *testing.T) {
	st := &failingPriceSyncStore{newFakeStore()}
	ad := &fakeAdapter{id: model.ToolCodex, class: model.EventLevel, emit: func(int) adapter.Observation {
		return adapter.Observation{Events: []model.UsageEvent{cycleEvent("new", model.ToolCodex, refDay, 100)}}
	}}
	stats, err := RunOnce(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}, WithPricer(&latePricer{available: true}))
	if err != nil || stats.PricesSynced != 0 || stats.EventsInserted != 1 || len(stats.Errors) != 1 || !strings.Contains(stats.Errors[0], "price sync: injected price sync failure") {
		t.Fatalf("sync failure must remain visible without stopping collection: %+v, %v", stats, err)
	}
}

func TestCycleSyncsEmbeddedConfirmedFreePrice(t *testing.T) {
	ctx := context.Background()
	ledger := realStore(t)
	event := cycleEvent("free-historical", model.ToolOpenCode, refDay, 100)
	event.Provider, event.Model = "opencode", "big-pickle"
	if _, err := ledger.InsertEvents(ctx, []model.UsageEvent{event}); err != nil {
		t.Fatal(err)
	}
	stats, err := RunOnce(ctx, adapter.NewRegistry(), ledger, adapter.DiscoverConfig{}, WithPricer(pricing.New(pricing.Options{})))
	if err != nil || len(stats.Errors) != 0 || stats.PricesSynced != 1 {
		t.Fatalf("embedded free sync = %+v, %v", stats, err)
	}
	events, err := ledger.ListEvents(ctx, store.Filter{})
	if err != nil || len(events) != 1 || events[0].CostMicroUSD == nil || *events[0].CostMicroUSD != 0 || !strings.HasSuffix(events[0].PriceSource, "+free") {
		t.Fatalf("embedded free result = %+v, %v", events, err)
	}
}
