package collect

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// activityObs builds an observation of one turn that cost `total` tokens and
// contained `calls` tool calls sharing that one usage record.
func activityObs(key string, total int64, calls int, when time.Time) adapter.Observation {
	e := model.UsageEvent{
		Tool: model.ToolClaudeCode, Model: "m", SessionID: "s",
		EventTime: when, InputTokens: total, TotalTokens: total,
		MessageID: key, DedupKey: key, Kind: model.KindUsage,
	}
	obs := adapter.Observation{Events: []model.UsageEvent{e}}
	for i := range calls {
		obs.Activity = append(obs.Activity, model.ActivityEvent{
			Tool: model.ToolClaudeCode, Kind: model.ActivityTool, Name: "Bash",
			SessionID: "s", EventTime: when,
			UsageDedupKey: key, MessageID: key,
			TurnSeq: i, CallsInTurn: calls,
			DedupKey: key + "|act|" + string(rune('0'+i)),
		})
	}
	return obs
}

// TestCycleStoresActivityAlongsideUsage: a cycle carries both ledgers, counts
// them separately, and is idempotent on a re-read.
func TestCycleStoresActivityAlongsideUsage(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	obs := activityObs("u-1", 900, 3, refDay.Add(time.Hour))
	reg := adapter.NewRegistry(&fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation { return obs },
	})

	stats, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.EventsInserted != 1 {
		t.Fatalf("events inserted = %d, want 1", stats.EventsInserted)
	}
	if stats.ActivitySeen != 3 || stats.ActivityInserted != 3 {
		t.Fatalf("activity seen/inserted = %d/%d, want 3/3", stats.ActivitySeen, stats.ActivityInserted)
	}

	// A second pass re-reads the same source: nothing new in either ledger.
	stats2, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if stats2.ActivityInserted != 0 || stats2.EventsInserted != 0 {
		t.Fatalf("re-read inserted %d events / %d activity, want 0/0",
			stats2.EventsInserted, stats2.ActivityInserted)
	}
}

// TestCycleStoresTurnContextsAndNoAttributionInflates is the pipeline-level
// statement of the whole feature.
//
// It runs a cycle over turns that overlap in every awkward way — a multi-call
// turn under two contexts at once, a turn under a context that calls nothing at
// all, and a turn under nothing — and asserts EVERY attribution independently
// against the ledger the same pass stored. None may exceed it, and none may be
// zero, which is the way a bound gets satisfied by attributing nothing.
//
// The agent dimension is the load-bearing addition. u-1 carries both a skill and
// an agent, so its cost appears in FULL under each — the partition rule — and a
// query that forgot to pin the dimension would report 2500 tokens against a
// 1800-token ledger.
func TestCycleStoresTurnContextsAndNoAttributionInflates(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	when := refDay.Add(time.Hour)

	turnCtx := func(key string, dim model.TurnDimension, value string) model.TurnContext {
		return model.TurnContext{
			UsageDedupKey: key, Tool: model.ToolClaudeCode,
			Dimension: dim, Value: value, SessionID: "s", EventTime: when,
		}
	}

	// u-1: 1000 tokens, 2 calls, skill "alpha" AND agent "worker" -> calls take 500 each
	// u-2:  500 tokens, 0 calls, skill "alpha"                    -> invisible to activity
	// u-3:  300 tokens, 1 call,  no context at all
	obs := activityObs("u-1", 1000, 2, when)
	obs.TurnContexts = append(obs.TurnContexts,
		turnCtx("u-1", model.DimensionSkill, "alpha"),
		turnCtx("u-1", model.DimensionAgent, "worker"),
	)
	ctxOnly := activityObs("u-2", 500, 0, when)
	ctxOnly.TurnContexts = append(ctxOnly.TurnContexts, turnCtx("u-2", model.DimensionSkill, "alpha"))
	plain := activityObs("u-3", 300, 1, when)

	obs.Events = append(obs.Events, ctxOnly.Events...)
	obs.Events = append(obs.Events, plain.Events...)
	obs.Activity = append(obs.Activity, plain.Activity...)
	obs.TurnContexts = append(obs.TurnContexts, ctxOnly.TurnContexts...)

	reg := adapter.NewRegistry(&fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation { return obs },
	})

	stats, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Three ROWS from two turns: the counters count (turn, dimension) pairs.
	if stats.TurnContextsSeen != 3 || stats.TurnContextsInserted != 3 {
		t.Fatalf("turn contexts seen/inserted = %d/%d, want 3/3",
			stats.TurnContextsSeen, stats.TurnContextsInserted)
	}
	if stats.EventsInserted != 3 || stats.ActivityInserted != 3 {
		t.Fatalf("events/activity inserted = %d/%d, want 3/3",
			stats.EventsInserted, stats.ActivityInserted)
	}

	const ledger int64 = 1800 // 1000 + 500 + 300

	window := store.ActivityFilter{Since: when.Add(-time.Hour), Until: when.Add(time.Hour)}

	got := map[string]int64{}
	// alpha owns u-1 and u-2 whole: 1500. worker owns u-1 whole: 1000. u-3
	// belongs to nothing. Note 1500 + 1000 = 2500 > 1800: two partitions of one
	// budget, which is exactly why no query may span them.
	for _, want := range []struct {
		dim    model.TurnDimension
		turns  int64
		tokens int64
	}{
		{model.DimensionSkill, 2, 1500},
		{model.DimensionAgent, 1, 1000},
	} {
		sum, err := st.SummarizeTurnContext(ctx, want.dim, window)
		if err != nil {
			t.Fatalf("SummarizeTurnContext(%s): %v", want.dim, err)
		}
		if sum.Totals.TotalTokens != want.tokens {
			t.Fatalf("%s-attributed tokens = %d, want %d", want.dim, sum.Totals.TotalTokens, want.tokens)
		}
		if sum.Totals.Turns != want.turns {
			t.Fatalf("%s turns = %d, want %d (the tool-less turn must count)",
				want.dim, sum.Totals.Turns, want.turns)
		}
		got[string(want.dim)] = sum.Totals.TotalTokens
	}

	acts, err := st.SummarizeActivity(ctx, window)
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	// u-1 splits 500/500, u-3 contributes 300, u-2 has no calls at all.
	if acts.Totals.AttributedTotal != 1300 {
		t.Fatalf("call-attributed tokens = %d, want 1300", acts.Totals.AttributedTotal)
	}
	got["call"] = acts.Totals.AttributedTotal

	for name, n := range got {
		if n > ledger {
			t.Fatalf("%s attribution %d exceeds the ledger's %d", name, n, ledger)
		}
		if n == 0 {
			t.Fatalf("%s attribution is zero; the bound would hold vacuously", name)
		}
	}

	// A second pass re-reads the same source: a turn's context is not recorded
	// twice on any axis, so its cost is not served twice either.
	stats2, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if stats2.TurnContextsInserted != 0 {
		t.Fatalf("re-read inserted %d turn contexts, want 0", stats2.TurnContextsInserted)
	}
}

// TestCycleActivityNeverOutRunsTheLedger is the collector-level statement of
// the table's invariant: whatever a pass stores, the attributed tokens over a
// window cannot exceed the usage the same window recorded.
func TestCycleActivityNeverOutRunsTheLedger(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	reg := adapter.NewRegistry(&fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(call int) adapter.Observation {
			// Turns with different call counts, including the multi-call turn
			// that would inflate a naive implementation.
			switch call {
			case 0:
				return activityObs("u-1", 900, 3, refDay.Add(time.Hour))
			default:
				return activityObs("u-2", 500, 1, refDay.Add(2*time.Hour))
			}
		},
	})

	for range 2 {
		if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}

	since, until := refDay, refDay.Add(24*time.Hour)
	ledger := windowTotal(t, st, since, until)
	acts, err := st.SummarizeActivity(ctx, store.ActivityFilter{Since: since, Until: until})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if acts.Totals.AttributedTotal > ledger {
		t.Fatalf("attributed %d tokens over a window the ledger recorded %d for",
			acts.Totals.AttributedTotal, ledger)
	}
	if acts.Totals.Calls != 4 {
		t.Fatalf("calls = %d, want 4", acts.Totals.Calls)
	}
	// 3*(900/3) + 1*(500/1) = 1400, and the ledger holds 1400.
	if acts.Totals.AttributedTotal != ledger {
		t.Fatalf("attributed %d, want the full %d (every turn here has calls)",
			acts.Totals.AttributedTotal, ledger)
	}
}

// TestActivityHasNoRawToStrip: privacy.no_raw drops audit payloads, and
// activity has none by construction. The cycle must still collect it.
func TestActivityHasNoRawToStrip(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	reg := adapter.NewRegistry(&fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			obs := activityObs("u-1", 100, 2, refDay.Add(time.Hour))
			obs.Events[0].Raw = `{"usage":{"input_tokens":100}}`
			return obs
		},
	})

	stats, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}, WithoutRaw())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.ActivityInserted != 2 {
		t.Fatalf("activity inserted = %d, want 2 under no_raw", stats.ActivityInserted)
	}
	if st.events[0].Raw != "" {
		t.Fatalf("no_raw left a raw payload on the usage event: %q", st.events[0].Raw)
	}
}
