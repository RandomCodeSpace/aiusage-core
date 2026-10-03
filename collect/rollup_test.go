package collect

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// realStore opens a SQLite store in a temp dir, so these tests drive the
// production write path rather than the in-memory fake. The rollup only exists
// in the real store: the fake has no derived table to fall out of step.
func realStore(t *testing.T) *store.Ledger {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// assertCycleRollupMatchesLedger compares the two tables through store queries
// on both sides, so SQLite does the local bucketing for the ledger and the
// rollup alike. Events are placed on exact UTC hour starts, which makes the
// comparison independent of the system zone (see store/rollup_test.go).
func assertCycleRollupMatchesLedger(t *testing.T, st *store.Ledger, dims ...string) {
	t.Helper()
	ctx := context.Background()
	f := store.Filter{GroupBy: dims}

	want, err := st.Summarize(ctx, f)
	if err != nil {
		t.Fatalf("summarize ledger: %v", err)
	}
	got, err := st.SummarizeRollup(ctx, f)
	if err != nil {
		t.Fatalf("summarize rollup: %v", err)
	}
	if len(got.Buckets) != len(want.Buckets) {
		t.Fatalf("group=%v: rollup has %d buckets, ledger %d", dims, len(got.Buckets), len(want.Buckets))
	}
	for i := range want.Buckets {
		w, g := want.Buckets[i], got.Buckets[i]
		for _, dim := range dims {
			if w.Keys[dim] != g.Keys[dim] {
				t.Fatalf("group=%v bucket %d: key %q = %q (rollup) vs %q (ledger)",
					dims, i, dim, g.Keys[dim], w.Keys[dim])
			}
		}
		if w.Events != g.Events || w.Total != g.Total || w.Input != g.Input ||
			w.CostMicroUSD != g.CostMicroUSD || w.UnpricedEvents != g.UnpricedEvents {
			t.Errorf("group=%v bucket %d %v: rollup %+v != ledger %+v", dims, i, w.Keys, g, w)
		}
	}
	if want.Totals.Total != got.Totals.Total || want.Totals.Events != got.Totals.Events {
		t.Errorf("group=%v totals: rollup events=%d total=%d, ledger events=%d total=%d",
			dims, got.Totals.Events, got.Totals.Total, want.Totals.Events, want.Totals.Total)
	}
}

// cycleEvent builds an event-level observation record on an exact UTC hour.
func cycleEvent(key, tool string, at time.Time, total int64) model.UsageEvent {
	return model.UsageEvent{
		Tool:                tool,
		Model:               "m-" + tool,
		Project:             "/w/" + tool,
		SessionID:           "s-" + tool,
		EventTime:           at,
		InputTokens:         total / 2,
		OutputTokens:        total / 4,
		CacheCreationTokens: total / 8,
		CacheReadTokens:     total / 8,
		TotalTokens:         total,
		DedupKey:            key,
		Kind:                model.KindUsage,
	}
}

// TestCycleMaintainsRollupThroughTheStore drives whole collection cycles
// against a real store and proves the derived rollup agrees with the ledger
// after each one, without a rebuild: the deltas ride the collector's own
// transactions. Both an event-level adapter and an aggregate one participate,
// since they commit through different store methods.
func TestCycleMaintainsRollupThroughTheStore(t *testing.T) {
	st := realStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	events := &fakeAdapter{
		id:    "evt-tool",
		class: model.EventLevel,
		emit: func(call int) adapter.Observation {
			at := base.Add(time.Duration(call*3) * time.Hour)
			return adapter.Observation{Events: []model.UsageEvent{
				cycleEvent("evt-a-"+at.Format(time.RFC3339), "evt-tool", at, 400),
				cycleEvent("evt-b-"+at.Format(time.RFC3339), "evt-tool", at.Add(time.Hour), 800),
			}}
		},
	}
	// An aggregate source: its running totals grow each cycle, so the collector
	// materialises a delta event through ApplySnapshot.
	aggregates := &fakeAdapter{
		id:    "agg-tool",
		class: model.Aggregate,
		emit: func(call int) adapter.Observation {
			return adapter.Observation{Snapshots: []model.AggregateSnapshot{{
				Tool:         "agg-tool",
				Key:          "sess-1",
				Model:        "m-agg",
				Project:      "/w/agg",
				SessionID:    "sess-1",
				ObservedTime: base.Add(time.Duration(call) * time.Hour),
				InputTokens:  int64(100 * (call + 1)),
				TotalTokens:  int64(100 * (call + 1)),
			}}}
		},
	}
	reg := adapter.NewRegistry(events, aggregates)

	for cycle := 0; cycle < 3; cycle++ {
		stats, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if len(stats.Errors) != 0 {
			t.Fatalf("cycle %d errors: %v", cycle, stats.Errors)
		}
		if stats.RollupRebuilt {
			t.Fatalf("cycle %d rebuilt the rollup; incremental maintenance must keep it in step", cycle)
		}
		assertCycleRollupMatchesLedger(t, st, "hour", "tool")
		assertCycleRollupMatchesLedger(t, st, "day", "tool", "model")
		assertCycleRollupMatchesLedger(t, st, "project")
	}
}

// TestCycleRebuildsRollupThatFellBehind is the startup consistency check seen
// from the collector: a database whose ledger grew without the rollup (the
// state a v4 migration leaves, reproduced here by emptying the table) is
// repaired by the next pass, before that pass appends anything.
func TestCycleRebuildsRollupThatFellBehind(t *testing.T) {
	st := realStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC)

	if _, err := st.InsertEvents(ctx, []model.UsageEvent{
		cycleEvent("pre-1", "evt-tool", base, 500),
		cycleEvent("pre-2", "evt-tool", base.Add(2*time.Hour), 700),
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	clearRollup(t, st)

	ad := &fakeAdapter{
		id:    "evt-tool",
		class: model.EventLevel,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Events: []model.UsageEvent{
				cycleEvent("post-1", "evt-tool", base.Add(4*time.Hour), 900),
			}}
		},
	}
	stats, err := RunOnce(ctx, adapter.NewRegistry(ad), st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if !stats.RollupRebuilt {
		t.Fatalf("cycle did not rebuild a rollup that had fallen behind the ledger")
	}
	if stats.EventsInserted != 1 {
		t.Fatalf("inserted=%d want 1", stats.EventsInserted)
	}
	// The rebuild covered the pre-existing rows and the pass's own delta landed
	// on top of it: both must be present exactly once.
	assertCycleRollupMatchesLedger(t, st, "hour")

	// And the next pass finds nothing to repair.
	stats, err = RunOnce(ctx, adapter.NewRegistry(ad), st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if stats.RollupRebuilt {
		t.Fatalf("second cycle rebuilt an in-step rollup")
	}
}

// clearRollup empties the derived table behind the store's back, standing in
// for the empty table the v4 migration creates. It goes through a second raw
// handle because the store offers no way to damage its own rollup - which is
// the correct API and an inconvenient test.
func clearRollup(t *testing.T, st *store.Ledger) {
	t.Helper()
	stats, err := st.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	execSQL(t, stats.Path, []string{`DELETE FROM usage_rollup`})
}
