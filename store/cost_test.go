package store

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

var costRef = time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)

// TestDedupKeyStoredVerbatim is the regression guard on the one thing the v3
// insert must not disturb. Dedup keys are the only defence against re-ingesting
// a user's whole history as duplicates, so the stored bytes must equal the
// adapter's bytes exactly, including punctuation, unicode and separators.
func TestDedupKeyStoredVerbatim(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	keys := []string{
		"opencode|msg_01H8XK",
		"agg|hermes|sess-1|1786000000123456789",
		"claude-code|/home/u/.claude/projects/a b/c.jsonl|42",
		"codex|9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"copilot|span:0123456789abcdef|gen_ai.client.token.usage",
		"unicode|café|é中",
	}
	events := make([]model.UsageEvent, 0, len(keys))
	for i, k := range keys {
		e := ev(k, model.ToolCodex, costRef.Add(time.Duration(i)*time.Minute), 10)
		events = append(events, e)
	}
	if n, err := st.InsertEvents(ctx, events); err != nil || n != len(keys) {
		t.Fatalf("insert = %d,%v want %d,nil", n, err, len(keys))
	}

	got, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	stored := make(map[string]bool, len(got))
	for _, e := range got {
		stored[e.DedupKey] = true
	}
	for _, k := range keys {
		if !stored[k] {
			t.Errorf("dedup key %q was not stored byte-for-byte; stored set: %v", k, stored)
		}
	}

	// Re-inserting the same keys must still be a no-op after the v3 columns.
	if n, err := st.InsertEvents(ctx, events); err != nil || n != 0 {
		t.Fatalf("re-insert = %d,%v want 0,nil (dedup broken)", n, err)
	}
}

// TestCostRoundTripAndNullSemantics checks a stamped cost survives storage
// exactly and an unstamped one reads back as unpriced rather than zero.
func TestCostRoundTripAndNullSemantics(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	priced := ev("priced", model.ToolClaudeCode, costRef, 100)
	priced.Provider = model.ProviderAnthropic
	priced.ServiceTier = "standard"
	priced.SetCost(1_234_567, "litellm-2026-08-09")

	unpriced := ev("unpriced", model.ToolCopilot, costRef.Add(time.Minute), 100)
	unpriced.Provider = model.ProviderGitHub

	if _, err := st.InsertEvents(ctx, []model.UsageEvent{priced, unpriced}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	evs, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byKey := map[string]model.UsageEvent{}
	for _, e := range evs {
		byKey[e.DedupKey] = e
	}

	p := byKey["priced"]
	if c, ok := p.Cost(); !ok || c != 1_234_567 {
		t.Errorf("priced cost = %d,%v want 1234567,true", c, ok)
	}
	if p.PriceSource != "litellm-2026-08-09" || p.Provider != model.ProviderAnthropic || p.ServiceTier != "standard" {
		t.Errorf("priced row = %+v, want the stamped provider/tier/source", p)
	}

	u := byKey["unpriced"]
	if c, ok := u.Cost(); ok || c != 0 {
		t.Errorf("unpriced cost = %d,%v want 0,false (NULL, never a stored zero)", c, ok)
	}
	if u.PriceSource != "" || u.Provider != model.ProviderGitHub {
		t.Errorf("unpriced row = %+v, want empty price source and the stored provider", u)
	}
}

// TestSummarizeCostAndUnpricedCounts verifies the aggregate carries the stamped
// cost sum plus the count of rows that still need display pricing, both per
// bucket and in the grand total.
func TestSummarizeCostAndUnpricedCounts(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	a := ev("a", model.ToolClaudeCode, costRef, 10)
	a.SetCost(500, "embedded-2026-08-09")
	b := ev("b", model.ToolClaudeCode, costRef.Add(time.Minute), 10)
	b.SetCost(250, "embedded-2026-08-09")
	c := ev("c", model.ToolCodex, costRef.Add(2*time.Minute), 10)

	if _, err := st.InsertEvents(ctx, []model.UsageEvent{a, b, c}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(sum.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(sum.Buckets))
	}
	for _, bk := range sum.Buckets {
		switch bk.Keys["tool"] {
		case model.ToolClaudeCode:
			if bk.CostMicroUSD != 750 || bk.UnpricedEvents != 0 {
				t.Errorf("claude bucket cost=%d unpriced=%d want 750,0", bk.CostMicroUSD, bk.UnpricedEvents)
			}
		case model.ToolCodex:
			if bk.CostMicroUSD != 0 || bk.UnpricedEvents != 1 {
				t.Errorf("codex bucket cost=%d unpriced=%d want 0,1", bk.CostMicroUSD, bk.UnpricedEvents)
			}
		}
	}
	if sum.Totals.CostMicroUSD != 750 || sum.Totals.UnpricedEvents != 1 {
		t.Errorf("totals cost=%d unpriced=%d want 750,1", sum.Totals.CostMicroUSD, sum.Totals.UnpricedEvents)
	}

	// Ungrouped: the single result row IS the total.
	sum, err = st.Summarize(ctx, Filter{})
	if err != nil {
		t.Fatalf("summarize ungrouped: %v", err)
	}
	if sum.Totals.CostMicroUSD != 750 || sum.Totals.UnpricedEvents != 1 {
		t.Errorf("ungrouped totals cost=%d unpriced=%d want 750,1", sum.Totals.CostMicroUSD, sum.Totals.UnpricedEvents)
	}
}

// TestUnpricedGroupsAggregatesByModel checks the display-pricing query: only
// NULL-cost rows are returned, they are split by the attributes a price lookup
// needs, and they carry the caller's grouping keys so the result folds back
// into the matching bucket.
func TestUnpricedGroupsAggregatesByModel(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	stamped := ev("stamped", model.ToolClaudeCode, costRef, 10)
	stamped.Model = "claude-sonnet-4-6"
	stamped.SetCost(999, "embedded-2026-08-09")

	mk := func(key, tool, m, provider string, in, out int64) model.UsageEvent {
		e := ev(key, tool, costRef, in+out)
		e.Model = m
		e.Provider = provider
		e.InputTokens = in
		e.OutputTokens = out
		return e
	}
	events := []model.UsageEvent{
		stamped,
		mk("u1", model.ToolCodex, "gpt-5", model.ProviderOpenAI, 100, 10),
		mk("u2", model.ToolCodex, "gpt-5", model.ProviderOpenAI, 200, 20),
		mk("u3", model.ToolGemini, "gemini-3-flash-preview", model.ProviderGoogle, 50, 5),
	}
	if _, err := st.InsertEvents(ctx, events); err != nil {
		t.Fatalf("insert: %v", err)
	}

	groups, err := st.UnpricedGroups(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("unpriced groups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d (%+v), want 2 — the stamped row must be excluded", len(groups), groups)
	}
	for _, g := range groups {
		switch g.Model {
		case "gpt-5":
			if g.Events != 2 || g.Input != 300 || g.Output != 30 {
				t.Errorf("gpt-5 group = %+v, want 2 events / 300 in / 30 out", g)
			}
			if g.Provider != model.ProviderOpenAI || g.Keys["tool"] != model.ToolCodex {
				t.Errorf("gpt-5 group identity = %+v", g)
			}
		case "gemini-3-flash-preview":
			if g.Events != 1 || g.Keys["tool"] != model.ToolGemini {
				t.Errorf("gemini group = %+v, want 1 event under the gemini tool key", g)
			}
		default:
			t.Errorf("unexpected group %+v", g)
		}
	}

	// Ungrouped: no keys, one row per (tool, model, provider, tier).
	groups, err = st.UnpricedGroups(ctx, Filter{})
	if err != nil {
		t.Fatalf("unpriced groups ungrouped: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("ungrouped groups = %d, want 2", len(groups))
	}
	for _, g := range groups {
		if len(g.Keys) != 0 {
			t.Errorf("ungrouped group carries keys: %+v", g)
		}
	}
}
