package collect

import (
	"context"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// rawObservation is what an adapter that stores an audit payload hands the
// collector: one event-level record and one aggregate cell, both carrying raw.
func rawObservation() adapter.Observation {
	return adapter.Observation{
		Events: []model.UsageEvent{{
			Tool: model.ToolClaudeCode, Model: "claude-opus-4-7",
			EventTime: refDay, TotalTokens: 10,
			DedupKey: "claude-code|msg-raw", Kind: model.KindUsage,
			Raw: `{"message":{"usage":{"input_tokens":10}}}`,
		}},
		Snapshots: []model.AggregateSnapshot{{
			Tool: model.ToolGemini, Key: "file|turn-1", Model: "gemini-2.5-pro",
			ObservedTime: refDay, InputTokens: 10, TotalTokens: 10,
			Raw: `{"id":"turn-1","tokens":{"input":10}}`,
		}},
	}
}

// TestWithoutRawStoresNoPayload proves config privacy.no_raw at the seam that
// enforces it: with the option set, neither the appended event nor the
// aggregate baseline nor the synthetic delta event derived from it keeps a raw
// payload, regardless of what the adapter emitted.
func TestWithoutRawStoresNoPayload(t *testing.T) {
	ad := &fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation { return rawObservation() },
	}
	st := newFakeStore()
	ctx := context.Background()

	if _, err := RunOnce(ctx, adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}, WithoutRaw()); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	evs, err := st.ListEvents(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("stored %d events, want 2 (one event-level, one aggregate delta)", len(evs))
	}
	for _, e := range evs {
		if e.Raw != "" {
			t.Errorf("event %q stored raw %q, want empty under no_raw", e.DedupKey, e.Raw)
		}
	}

	last, err := st.LastState(ctx, model.ToolGemini, "file|turn-1")
	if err != nil || last == nil {
		t.Fatalf("last state = %v, %v", last, err)
	}
	if last.Raw != "" {
		t.Errorf("aggregate_state raw = %q, want empty under no_raw", last.Raw)
	}
}

// TestDefaultKeepsRawPayload pins the default: without the switch the audit
// payload reaches both tables unchanged, so no_raw is opt-in.
func TestDefaultKeepsRawPayload(t *testing.T) {
	ad := &fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation { return rawObservation() },
	}
	st := newFakeStore()
	ctx := context.Background()

	if _, err := RunOnce(ctx, adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	evs, _ := st.ListEvents(ctx, store.Filter{})
	found := false
	for _, e := range evs {
		if e.Raw != "" {
			found = true
		}
	}
	if !found {
		t.Error("no event kept its raw payload by default")
	}
	last, err := st.LastState(ctx, model.ToolGemini, "file|turn-1")
	if err != nil || last == nil {
		t.Fatalf("last state = %v, %v", last, err)
	}
	if last.Raw == "" {
		t.Error("aggregate_state raw is empty by default, want the adapter payload")
	}
}

// TestNoRawShrinksExistingAggregateState covers the mutable half of the policy:
// a cell whose stored baseline still holds a payload from before the switch is
// rewritten without one on the next snapshot cycle, even though its counters
// did not move. usage_events, being append-only, keeps what it was written with.
func TestNoRawShrinksExistingAggregateState(t *testing.T) {
	ad := &fakeAdapter{
		id: model.ToolGemini, class: model.Aggregate,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Snapshots: rawObservation().Snapshots}
		},
	}
	st := newFakeStore()
	ctx := context.Background()

	// Cycle 1: default policy, the payload lands in aggregate_state.
	if _, err := RunOnce(ctx, adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	last, _ := st.LastState(ctx, model.ToolGemini, "file|turn-1")
	if last == nil || last.Raw == "" {
		t.Fatalf("cycle 1 left no baseline payload: %+v", last)
	}

	// Cycle 2: no_raw, identical counters. The baseline must still be rewritten.
	if _, err := RunOnce(ctx, adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}, WithoutRaw()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	last, _ = st.LastState(ctx, model.ToolGemini, "file|turn-1")
	if last == nil || last.Raw != "" {
		t.Fatalf("aggregate_state raw = %+v, want the payload dropped on the next snapshot", last)
	}
}
