package collect

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// BenchmarkRunCycle measures one steady-state collection cycle over a small
// fixed fixture: an event-level adapter re-emitting 100 already-stored events
// (the INSERT OR IGNORE dedup path) and an aggregate adapter whose snapshot no
// longer grows (the zero-delta state path). The clock is frozen so synthetic
// dedup keys stay stable and the store never grows across iterations, keeping
// the baseline flat.
func BenchmarkRunCycle(b *testing.B) {
	events := make([]model.UsageEvent, 100)
	for i := range events {
		events[i] = model.UsageEvent{
			Tool:         model.ToolClaudeCode,
			Model:        "claude-opus",
			SessionID:    "sess-bench",
			Project:      "/work/bench",
			EventTime:    refDay.Add(time.Duration(i) * time.Minute),
			InputTokens:  400,
			OutputTokens: 600,
			TotalTokens:  1000,
			DedupKey:     "cc|bench|" + strconv.Itoa(i),
			Kind:         model.KindUsage,
		}
	}
	evAd := &fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Events: events}
		},
	}
	agAd := &fakeAdapter{
		id: model.ToolHermes, class: model.Aggregate,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Snapshots: []model.AggregateSnapshot{{
				Tool: model.ToolHermes, Key: "sess-agg", SessionID: "sess-agg",
				InputTokens: 500, TotalTokens: 500,
			}}}
		},
	}
	reg := adapter.NewRegistry(evAd, agAd)
	st := newFakeStore()
	ctx := context.Background()

	restore := setNow(func() time.Time { return refDay.Add(6 * time.Hour) })
	defer restore()

	// First cycle inserts everything; iterations then measure the steady state.
	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		b.Fatalf("warm-up cycle: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
			b.Fatalf("cycle: %v", err)
		}
	}
}

// BenchmarkRunCycleUnchangedSources measures a steady-state cycle over REAL
// adapters, real fixture files and a real SQLite store when nothing changed
// since the last cycle — the checkpoint gates must reduce every source to a
// stat (or one empty watermark query), with zero parsing and zero inserts.
// The codex history is padded to hundreds of records so any regression back
// to per-cycle re-parsing is visible immediately in ns/op and allocs/op.
func BenchmarkRunCycleUnchangedSources(b *testing.B) {
	fx := setupIncrementalFixture(b)
	ctx := context.Background()

	// Pad the codex backlog: pre-checkpoint code re-parsed all of this every
	// cycle; incremental code must never touch it again.
	var pad strings.Builder
	total := int64(1750)
	for i := 0; i < 300; i++ {
		total += 10
		fmt.Fprintf(&pad,
			`{"type":"event_msg","timestamp":"2026-05-29T11:%02d:%02dZ","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}}`+"\n",
			i/60, i%60, total, total)
	}
	appendLines(b, fx.codexSession, pad.String())

	// Warm-up cycle inserts everything and writes the checkpoints.
	if s, err := RunOnce(ctx, fx.reg, fx.st, fx.dc); err != nil || len(s.Errors) > 0 {
		b.Fatalf("warm-up cycle: err=%v errors=%v", err, s.Errors)
	}

	b.ReportAllocs()
	for b.Loop() {
		s, err := RunOnce(ctx, fx.reg, fx.st, fx.dc)
		if err != nil {
			b.Fatalf("cycle: %v", err)
		}
		if s.EventsSeen != 0 || s.Snapshots != 0 || s.EventsInserted != 0 {
			b.Fatalf("steady-state cycle did work: seen=%d snapshots=%d inserted=%d",
				s.EventsSeen, s.Snapshots, s.EventsInserted)
		}
	}
}
