package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

var provRef = time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)

// seedProvenance inserts one row per price source plus one unpriced row, and
// returns the store. The mix is the real one: a vendor stamp, a vendor FAMILY
// stamp, two ladder rungs and a row nothing could price.
func seedProvenance(t *testing.T) (*Ledger, context.Context) {
	t.Helper()
	st := openTemp(t)
	ctx := context.Background()

	mk := func(key, tool, source string, micro int64) model.UsageEvent {
		e := ev(key, tool, provRef, 100)
		if source != "" {
			e.SetCost(micro, source)
		}
		return e
	}
	events := []model.UsageEvent{
		mk("vendor-exact", model.ToolCopilot, "copilot-nano-aiu", 1_000_000),
		mk("vendor-family", model.ToolGoose, "goose-provider_reported", 2_000_000),
		mk("vendor-family-bare", model.ToolGoose, "goose", 3_000_000),
		mk("computed-litellm", model.ToolClaudeCode, "litellm-2026-08-14", 4_000_000),
		mk("computed-long", model.ToolClaudeCode, "litellm-2026-08-14+long-context", 5_000_000),
		mk("unpriced", model.ToolCodex, "", 0),
	}
	if n, err := st.InsertEvents(ctx, events); err != nil || n != len(events) {
		t.Fatalf("insert = %d,%v want %d,nil", n, err, len(events))
	}
	return st, ctx
}

// A summed cost has to be able to say how much of it this project estimated.
// The count is of PRICED rows only: an unpriced row has no price to have a
// provenance, and counting it as computed would double-report it against
// UnpricedEvents.
func TestSummarizeCountsComputedCostRows(t *testing.T) {
	st, ctx := seedProvenance(t)

	sum, err := st.Summarize(ctx, Filter{})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got, want := sum.Totals.ComputedCostEvents, int64(2); got != want {
		t.Errorf("ComputedCostEvents = %d, want %d (the two ladder rungs)", got, want)
	}
	if got, want := sum.Totals.UnpricedEvents, int64(1); got != want {
		t.Errorf("UnpricedEvents = %d, want %d", got, want)
	}
	if got, want := sum.Totals.Events, int64(6); got != want {
		t.Errorf("Events = %d, want %d", got, want)
	}
	// The three vendor rows are neither computed nor unpriced.
	if vendor := sum.Totals.Events - sum.Totals.ComputedCostEvents - sum.Totals.UnpricedEvents; vendor != 3 {
		t.Errorf("vendor-priced rows = %d, want 3", vendor)
	}
}

// Grouped, the count adds across buckets exactly the way the cost it qualifies
// does — otherwise a grand total would be marked estimated while none of its
// parts were, or the reverse.
func TestComputedCostCountAddsAcrossBuckets(t *testing.T) {
	st, ctx := seedProvenance(t)

	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	var summed int64
	byTool := map[string]int64{}
	for _, b := range sum.Buckets {
		summed += b.ComputedCostEvents
		byTool[b.Keys["tool"]] = b.ComputedCostEvents
	}
	if summed != sum.Totals.ComputedCostEvents {
		t.Errorf("buckets sum to %d computed rows, totals say %d", summed, sum.Totals.ComputedCostEvents)
	}
	if got := byTool[model.ToolClaudeCode]; got != 2 {
		t.Errorf("claude-code computed rows = %d, want 2", got)
	}
	if got := byTool[model.ToolGoose]; got != 0 {
		t.Errorf("goose computed rows = %d, want 0 — its adapter reports the price", got)
	}
	if got := byTool[model.ToolCopilot]; got != 0 {
		t.Errorf("copilot computed rows = %d, want 0 — its adapter reports the price", got)
	}
}

// The vendor predicate is built from model's vocabulary. If the two ever part
// company the count silently flips its meaning, so this asserts the SQL names
// every stamp model declares.
func TestVendorPredicateNamesEveryDeclaredStamp(t *testing.T) {
	sql := vendorPriceSourceSQL("price_source")
	for _, v := range model.VendorPriceSources() {
		if !strings.Contains(sql, "'"+v+"'") {
			t.Errorf("vendor predicate does not name %q:\n%s", v, sql)
		}
	}
	for _, f := range model.VendorPriceSourceFamilies() {
		if !strings.Contains(sql, "'"+f+"-%'") {
			t.Errorf("vendor predicate does not carry family %q:\n%s", f, sql)
		}
	}
}

// The activity ledger's attributed cost gets the same qualifier, counted per
// CALL — the level the figure it qualifies is summed at.
func TestActivityCountsComputedCostCalls(t *testing.T) {
	st, ctx := seedProvenance(t)

	acts := []model.ActivityEvent{
		{Tool: model.ToolClaudeCode, Kind: model.ActivityTool, Name: "Bash",
			EventTime: provRef, DedupKey: "a1", UsageDedupKey: "computed-litellm", CallsInTurn: 1},
		{Tool: model.ToolCopilot, Kind: model.ActivityTool, Name: "view",
			EventTime: provRef, DedupKey: "a2", UsageDedupKey: "vendor-exact", CallsInTurn: 1},
		{Tool: model.ToolCodex, Kind: model.ActivityTool, Name: "exec",
			EventTime: provRef, DedupKey: "a3", UsageDedupKey: "", CallsInTurn: 1},
	}
	if _, err := st.ApplyObservation(ctx, nil, acts, nil); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}

	sum, err := st.SummarizeActivity(ctx, ActivityFilter{})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if got, want := sum.Totals.ComputedCostCalls, int64(1); got != want {
		t.Errorf("ComputedCostCalls = %d, want %d", got, want)
	}
	if got, want := sum.Totals.UnattributedCalls, int64(1); got != want {
		t.Errorf("UnattributedCalls = %d, want %d — the codex call joins nothing", got, want)
	}
}

// Turn contexts get it too, counted per turn.
func TestTurnContextCountsComputedCostTurns(t *testing.T) {
	st, ctx := seedProvenance(t)

	ctxs := []model.TurnContext{
		{Tool: model.ToolClaudeCode, Dimension: model.DimensionAgent, Value: "Explore",
			UsageDedupKey: "computed-litellm", EventTime: provRef},
		{Tool: model.ToolClaudeCode, Dimension: model.DimensionAgent, Value: "Explore",
			UsageDedupKey: "computed-long", EventTime: provRef},
		{Tool: model.ToolCopilot, Dimension: model.DimensionAgent, Value: "task",
			UsageDedupKey: "vendor-exact", EventTime: provRef},
	}
	if _, err := st.ApplyBatch(ctx, ObservationBatch{TurnContexts: ctxs}); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}

	sum, err := st.SummarizeTurnContext(ctx, model.DimensionAgent, ActivityFilter{GroupBy: []string{"value"}})
	if err != nil {
		t.Fatalf("SummarizeTurnContext: %v", err)
	}
	if got, want := sum.Totals.ComputedCostTurns, int64(2); got != want {
		t.Errorf("ComputedCostTurns = %d, want %d", got, want)
	}
	for _, b := range sum.Buckets {
		switch b.Keys["value"] {
		case "Explore":
			if b.ComputedCostTurns != 2 {
				t.Errorf("Explore computed turns = %d, want 2", b.ComputedCostTurns)
			}
		case "task":
			if b.ComputedCostTurns != 0 {
				t.Errorf("task computed turns = %d, want 0 — copilot reports its own price", b.ComputedCostTurns)
			}
		}
	}
}
