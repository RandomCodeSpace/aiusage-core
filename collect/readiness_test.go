package collect

import (
	"context"
	"errors"
	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/pricing"
	"github.com/RandomCodeSpace/aiusage-core/store"
	"testing"
)

func TestRunOnceCancellationDuringFinalSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ad := &fakeAdapter{id: model.ToolCodex, class: model.EventLevel, emit: func(int) adapter.Observation { cancel(); return adapter.Observation{} }}
	stats, err := RunOnce(ctx, adapter.NewRegistry(ad), newFakeStore(), adapter.DiscoverConfig{})
	if !errors.Is(err, context.Canceled) || !stats.Canceled {
		t.Fatalf("last source cancellation: err=%v canceled=%v", err, stats.Canceled)
	}
}

type cancelDiscoveryAdapter struct {
	*fakeAdapter
	cancel context.CancelFunc
}

func (a cancelDiscoveryAdapter) Discover(context.Context, adapter.DiscoverConfig) ([]adapter.Source, error) {
	a.cancel()
	return nil, context.Canceled
}

func TestRunOnceCancellationAtEmptyBoundaries(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		st := newFakeStore()
		stats, err := RunOnce(ctx, adapter.NewRegistry(), st, adapter.DiscoverConfig{})
		if !errors.Is(err, context.Canceled) || !stats.Canceled || st.ensureCalls != 0 {
			t.Fatalf("canceled cycle did work or reported completion: %+v %v", stats, err)
		}
	})
	t.Run("last discovery", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ad := cancelDiscoveryAdapter{fakeAdapter: &fakeAdapter{id: model.ToolCodex}, cancel: cancel}
		stats, err := RunOnce(ctx, adapter.NewRegistry(ad), newFakeStore(), adapter.DiscoverConfig{})
		if !errors.Is(err, context.Canceled) || !stats.Canceled {
			t.Fatalf("canceled discovery reported completion: %+v %v", stats, err)
		}
	})
}

func TestHistoricalCacheWriteRemainsUnpricedWithoutTTL(t *testing.T) {
	ctx := context.Background()
	ledger := realStore(t)
	event := cycleEvent("historical-1h", model.ToolClaudeCode, refDay, 1_000_000)
	event.Model = "review-private-model"
	event.InputTokens = 0
	event.OutputTokens = 0
	event.CacheReadTokens = 0
	event.ReasoningTokens = 0
	event.CacheCreationTokens = 1_000_000
	event.CacheTTL = model.CacheWriteTTL{Ephemeral1h: 1_000_000}
	ad := &fakeAdapter{id: model.ToolClaudeCode, class: model.EventLevel, emit: func(int) adapter.Observation { return adapter.Observation{Events: []model.UsageEvent{event}} }}
	first, err := RunOnce(ctx, adapter.NewRegistry(ad), ledger, adapter.DiscoverConfig{}, WithoutRaw())
	if err != nil || len(first.Errors) > 0 {
		t.Fatalf("ingest: %+v %v", first, err)
	}
	p := pricing.New(pricing.Options{Overrides: map[string]pricing.Rates{event.Model: {Input: 1e-6, Output: 1e-6, CacheWrite5m: 1.25e-6, CacheWrite1h: 2e-6}}})
	expected, _, ok := p.PriceEvent(event)
	if !ok || expected != 2_000_000 {
		t.Fatalf("source pricing: %d %v", expected, ok)
	}
	second, err := RunOnce(ctx, adapter.NewRegistry(), ledger, adapter.DiscoverConfig{}, WithPricer(p))
	if err != nil || len(second.Errors) > 0 || second.PricesSynced != 0 {
		t.Fatalf("sync: %+v %v", second, err)
	}
	events, err := ledger.ListEvents(ctx, store.Filter{})
	if err != nil || len(events) != 1 {
		t.Fatalf("events: %+v %v", events, err)
	}
	if events[0].CostMicroUSD != nil {
		t.Fatalf("deferred pricing guessed %d without stored TTL", *events[0].CostMicroUSD)
	}
	immediate := realStore(t)
	stats, err := RunOnce(ctx, adapter.NewRegistry(ad), immediate, adapter.DiscoverConfig{}, WithPricer(p), WithoutRaw())
	if err != nil || len(stats.Errors) != 0 {
		t.Fatalf("immediate pricing: %+v %v", stats, err)
	}
	priced, err := immediate.ListEvents(ctx, store.Filter{})
	if err != nil || len(priced) != 1 || priced[0].CostMicroUSD == nil || *priced[0].CostMicroUSD != expected {
		t.Fatalf("immediate cache pricing: %+v %v", priced, err)
	}
}

func TestAggregatePricingDoesNotUseRequestThreshold(t *testing.T) {
	ctx := context.Background()
	ledger := realStore(t)
	ad := &fakeAdapter{id: model.ToolHermes, class: model.Aggregate, emit: func(int) adapter.Observation {
		return adapter.Observation{Snapshots: []model.AggregateSnapshot{{Tool: model.ToolHermes, Key: "session", Model: "review-private-model", InputTokens: 300_000, TotalTokens: 300_000}}}
	}}
	p := pricing.New(pricing.Options{Overrides: map[string]pricing.Rates{"review-private-model": {Input: 1e-6, Output: 1e-6, Long: pricing.LongContext{Threshold: 200_000, Input: 2e-6, Output: 2e-6}}}})
	stats, err := RunOnce(ctx, adapter.NewRegistry(ad), ledger, adapter.DiscoverConfig{}, WithPricer(p))
	if err != nil || len(stats.Errors) > 0 {
		t.Fatalf("ingest: %+v %v", stats, err)
	}
	events, err := ledger.ListEvents(ctx, store.Filter{})
	if err != nil || len(events) != 1 || events[0].CostMicroUSD == nil {
		t.Fatalf("events: %+v %v", events, err)
	}
	if got := *events[0].CostMicroUSD; got != 300_000 {
		t.Fatalf("300k aggregate tokens from several short requests cost %d, expected base-rate 300000; source=%s", got, events[0].PriceSource)
	}
	if cost, _, ok := p.PriceEvent(events[0]); !ok || cost != 300_000 {
		t.Fatalf("stored aggregate loses pricing meaning: %d %v", cost, ok)
	}
	ordinary := events[0]
	ordinary.DedupKey = "hermes|ordinary-request"
	if cost, _, ok := p.PriceEvent(ordinary); !ok || cost != 600_000 {
		t.Fatalf("ordinary request lost threshold pricing: %d %v", cost, ok)
	}
}
