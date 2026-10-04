package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func claudeBatch(total int64) ObservationBatch {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	e := ev("claude-message", model.ToolClaudeCode, at, total)
	e.InputTokens, e.OutputTokens = 10, total-10
	e.MessageID, e.RequestID = "message", "request"
	e.SetCost(total*10, "test-prices")
	return ObservationBatch{
		Events:       []model.UsageEvent{e},
		Activity:     []model.ActivityEvent{act("call", "Read", model.ActivityTool, at, e.DedupKey, 0, 1)},
		TurnContexts: []model.TurnContext{turnCtx(e.DedupKey, model.DimensionSkill, "read", at)},
		Checkpoint:   &model.SourceCheckpoint{Tool: model.ToolClaudeCode, SourcePath: "/claude", Offset: total},
	}
}

func assertClaudeAccounting(t *testing.T, st *Ledger, tokens, cost int64) {
	t.Helper()
	ctx := context.Background()
	for _, read := range []func(context.Context, Filter) (*Summary, error){st.Summarize, st.summarizeLedger} {
		s, err := read(ctx, Filter{})
		if err != nil || s.Totals.Events != 1 || s.Totals.Total != tokens || s.Totals.CostMicroUSD != cost {
			t.Fatalf("summary=%+v err=%v", s, err)
		}
	}
	a, err := st.SummarizeActivity(ctx, ActivityFilter{})
	if err != nil || a.Totals.Calls != 1 || a.Totals.AttributedTotal != tokens || a.Totals.AttributedCostMicroUSD != cost {
		t.Fatalf("activity=%+v err=%v", a, err)
	}
	c, err := st.SummarizeTurnContext(ctx, model.DimensionSkill, ActivityFilter{})
	if err != nil || c.Totals.Turns != 1 || c.Totals.TotalTokens != tokens || c.Totals.CostMicroUSD != cost {
		t.Fatalf("context=%+v err=%v", c, err)
	}
	assertPriceSyncGuard(t, st)
}

func TestClaudeReconciliationIsOptInAndIdempotent(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.ApplyBatch(ctx, claudeBatch(15)); err != nil {
		t.Fatal(err)
	}
	// The old API still ignores conflicting usage, even though its checkpoint advances.
	if _, err := st.ApplyBatch(ctx, claudeBatch(60)); err != nil {
		t.Fatal(err)
	}
	assertClaudeAccounting(t, st, 15, 150)
	before, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, total := range []int64{60, 60, 15} {
		applied, err := st.ReconcileClaudeBatch(ctx, claudeBatch(total))
		if err != nil || applied != (Applied{}) {
			t.Fatalf("reconcile=%+v err=%v", applied, err)
		}
		assertClaudeAccounting(t, st, 60, 600)
	}
	after, err := st.ListEvents(ctx, Filter{})
	if err != nil || len(after) != 1 || after[0].ID != before[0].ID || after[0].DedupKey != before[0].DedupKey {
		t.Fatalf("identity changed: %+v %v", after, err)
	}
}

func TestClaudeReconciliationRejectsConflictingGrowth(t *testing.T) {
	for _, mode := range []string{"counter", "model", "provider", "tier", "request", "tool", "kind", "vendor", "guard", "stale-rollup"} {
		t.Run(mode, func(t *testing.T) {
			st := openTemp(t)
			ctx := context.Background()
			initial := claudeBatch(15)
			if mode == "vendor" {
				initial.Events[0].SetCost(150, "pi-reported")
			}
			if _, err := st.ApplyBatch(ctx, initial); err != nil {
				t.Fatal(err)
			}
			incoming := claudeBatch(60)
			switch mode {
			case "counter":
				incoming.Events[0].InputTokens = 9
			case "model":
				incoming.Events[0].Model = "other"
			case "provider":
				incoming.Events[0].Provider = "other"
			case "tier":
				incoming.Events[0].ServiceTier = "other"
			case "request":
				incoming.Events[0].RequestID = "other"
			case "tool":
				incoming.Events[0].Tool = model.ToolCodex
			case "kind":
				incoming.Events[0].Kind = model.KindAdjustment
			case "guard":
				execFailureSQL(t, st, `DROP TRIGGER trg_events_no_update`)
			case "stale-rollup":
				execFailureSQL(t, st, `UPDATE schema_meta SET value='0' WHERE key='rollup_watermark'`)
			}
			before, _ := st.ListEvents(ctx, Filter{})
			applied, err := st.ReconcileClaudeBatch(ctx, incoming)
			if err == nil || applied != (Applied{}) {
				t.Fatalf("accepted conflict: %+v %v", applied, err)
			}
			after, readErr := st.ListEvents(ctx, Filter{})
			if readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("history changed: %v", readErr)
			}
			cp, err := st.Checkpoint(ctx, model.ToolClaudeCode, "/claude")
			if err != nil || cp == nil || cp.Offset != 15 {
				t.Fatalf("checkpoint=%+v %v", cp, err)
			}
		})
	}
}

func TestClaudeReconciliationUnknownReplacementCost(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.ApplyBatch(ctx, claudeBatch(15)); err != nil {
		t.Fatal(err)
	}
	incoming := claudeBatch(60)
	incoming.Events[0].CostMicroUSD, incoming.Events[0].PriceSource = nil, ""
	if _, err := st.ReconcileClaudeBatch(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	assertClaudeAccounting(t, st, 60, 0)
	summary, err := st.Summarize(ctx, Filter{})
	if err != nil || summary.Totals.UnpricedEvents != 1 {
		t.Fatalf("summary=%+v %v", summary, err)
	}
}

func TestClaudeReconciliationFailureRollsBackEverything(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.ApplyBatch(ctx, claudeBatch(15)); err != nil {
		t.Fatal(err)
	}
	execFailureSQL(t, st, `CREATE TRIGGER fail_checkpoint BEFORE UPDATE ON source_checkpoints
		BEGIN SELECT RAISE(ABORT,'injected checkpoint failure'); END`)
	applied, err := st.ReconcileClaudeBatch(ctx, claudeBatch(60))
	if err == nil || applied != (Applied{}) {
		t.Fatalf("applied=%+v err=%v", applied, err)
	}
	assertClaudeAccounting(t, st, 15, 150)
	cp, err := st.Checkpoint(ctx, model.ToolClaudeCode, "/claude")
	if err != nil || cp == nil || cp.Offset != 15 {
		t.Fatalf("checkpoint=%+v %v", cp, err)
	}
}
