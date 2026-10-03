package store

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// TestCheckpointRoundTrip verifies upsert-through-ApplyEvents and read-back,
// including the update path (one row per tool/source).
func TestCheckpointRoundTrip(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	if cp, err := st.Checkpoint(ctx, model.ToolCodex, "/s/a.jsonl"); err != nil || cp != nil {
		t.Fatalf("missing checkpoint = %+v err=%v want nil,nil", cp, err)
	}

	first := &model.SourceCheckpoint{
		Tool: model.ToolCodex, SourcePath: "/s/a.jsonl",
		Size: 100, MTimeNS: 42, Offset: 90, Watermark: 7, State: `{"x":1}`,
	}
	if _, err := st.ApplyEvents(ctx, nil, first); err != nil {
		t.Fatalf("apply checkpoint: %v", err)
	}
	got, err := st.Checkpoint(ctx, model.ToolCodex, "/s/a.jsonl")
	if err != nil || got == nil || *got != *first {
		t.Fatalf("round trip = %+v err=%v want %+v", got, err, first)
	}

	second := *first
	second.Size, second.Offset, second.State = 200, 180, ""
	if _, err := st.ApplyEvents(ctx, nil, &second); err != nil {
		t.Fatalf("update checkpoint: %v", err)
	}
	got, err = st.Checkpoint(ctx, model.ToolCodex, "/s/a.jsonl")
	if err != nil || got == nil || *got != second {
		t.Fatalf("updated = %+v err=%v want %+v", got, err, second)
	}
}

// TestApplyEventsAtomicWithCheckpoint: events and checkpoint land in one
// transaction, and a failing batch (empty dedup key aborts nothing — poison
// rows are per-row skips that still commit) keeps count semantics.
func TestApplyEventsAtomicWithCheckpoint(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 6, 0, 0, 0, time.UTC)

	cp := &model.SourceCheckpoint{Tool: model.ToolCodex, SourcePath: "/s/b.jsonl", Size: 10}
	n, err := st.ApplyEvents(ctx, []model.UsageEvent{ev("ck-1", model.ToolCodex, now, 5)}, cp)
	if err != nil || n != 1 {
		t.Fatalf("apply n=%d err=%v want 1,nil", n, err)
	}
	if got, _ := st.Checkpoint(ctx, model.ToolCodex, "/s/b.jsonl"); got == nil || got.Size != 10 {
		t.Fatalf("checkpoint not committed with events: %+v", got)
	}

	// A poison row is skipped and reported, but batch + checkpoint commit:
	// re-parsing cannot fix a CHECK violation, so retrying it forever is waste.
	cp2 := &model.SourceCheckpoint{Tool: model.ToolCodex, SourcePath: "/s/b.jsonl", Size: 20}
	bad := ev("", model.ToolCodex, now, 5)
	good := ev("ck-2", model.ToolCodex, now, 5)
	n, err = st.ApplyEvents(ctx, []model.UsageEvent{bad, good}, cp2)
	if err == nil || n != 1 {
		t.Fatalf("poison batch n=%d err=%v want 1,non-nil", n, err)
	}
	if got, _ := st.Checkpoint(ctx, model.ToolCodex, "/s/b.jsonl"); got == nil || got.Size != 20 {
		t.Fatalf("checkpoint after poison batch = %+v want Size 20", got)
	}
}

// TestApplyEventsRejectsUnkeyedCheckpoint: a checkpoint without tool/source
// identity would be unreadable; the write must fail loudly.
func TestApplyEventsRejectsUnkeyedCheckpoint(t *testing.T) {
	st := openTemp(t)
	if _, err := st.ApplyEvents(context.Background(), nil, &model.SourceCheckpoint{}); err == nil {
		t.Fatalf("expected error for checkpoint without tool/source path")
	}
}

// TestApplySnapshotCollisionSkipsCheckpoint: a fully-collided delta keeps the
// baseline AND the checkpoint unchanged — an advanced checkpoint would gate
// the source off before the delta was ever re-derived.
func TestApplySnapshotCollisionSkipsCheckpoint(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 6, 0, 0, 0, time.UTC)

	snap := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "cell", InputTokens: 100, TotalTokens: 100, ObservedTime: now,
	}
	cp1 := &model.SourceCheckpoint{Tool: model.ToolHermes, SourcePath: "/h/state.db", State: `{"gen":1}`}
	if n, err := st.ApplySnapshot(ctx, []model.UsageEvent{ev("agg|c1", model.ToolHermes, now, 100)}, snap, cp1); err != nil || n != 1 {
		t.Fatalf("first apply n=%d err=%v want 1,nil", n, err)
	}
	if got, _ := st.Checkpoint(ctx, model.ToolHermes, "/h/state.db"); got == nil || got.State != `{"gen":1}` {
		t.Fatalf("checkpoint not written with snapshot: %+v", got)
	}

	grown := snap
	grown.InputTokens, grown.TotalTokens = 250, 250
	cp2 := &model.SourceCheckpoint{Tool: model.ToolHermes, SourcePath: "/h/state.db", State: `{"gen":2}`}
	// Same dedup key -> full collision -> state AND checkpoint must hold.
	if n, err := st.ApplySnapshot(ctx, []model.UsageEvent{ev("agg|c1", model.ToolHermes, now, 150)}, grown, cp2); err != nil || n != 0 {
		t.Fatalf("collided apply n=%d err=%v want 0,nil", n, err)
	}
	if v, _ := st.LastState(ctx, model.ToolHermes, "cell"); v == nil || v.TotalTokens != 100 {
		t.Fatalf("collision advanced baseline: %+v", v)
	}
	if got, _ := st.Checkpoint(ctx, model.ToolHermes, "/h/state.db"); got == nil || got.State != `{"gen":1}` {
		t.Fatalf("collision advanced checkpoint: %+v", got)
	}
}
