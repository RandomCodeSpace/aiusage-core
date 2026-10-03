package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// turnCtx builds a turn-context row for a usage event's dedup key.
func turnCtx(usageKey string, dim model.TurnDimension, value string, et time.Time) model.TurnContext {
	return model.TurnContext{
		UsageDedupKey: usageKey,
		Tool:          model.ToolClaudeCode,
		Dimension:     dim,
		Value:         value,
		SessionID:     "s",
		Project:       "/p",
		Model:         "m",
		EventTime:     et,
	}
}

// turnFixture seeds the window every attribution test below reasons about. It is
// built to contain each case that could break the arithmetic at once:
//
//	u1  1000 tok / 10000 micro  agent=worker skill=alpha
//	                            TWO tool calls (multi-call turn under a context)
//	u2   500 tok /  5000 micro  agent=worker skill=alpha
//	                            ZERO tool calls (context-only turn: invisible to
//	                            the activity ledger)
//	u3   300 tok /  3000 micro  skill=beta mcp_tool=browser_eval
//	                            mcp_server=ruflo plugin=mattpocock
//	                            ONE tool call (FOUR dimensions on one turn)
//	u4   200 tok /  2000 micro  no context at all
//	                            ONE tool call (turn outside every partition)
//
// Ledger for the window: 2000 tokens, 20000 micro-USD.
//
// Per-dimension expectations, each of which is a COMPLETE answer over its own
// axis and none of which may be added to another:
//
//	agent      worker 1500 / 15000                    (2 turns)
//	skill      alpha  1500 / 15000, beta 300 / 3000   (3 turns)
//	mcp_tool   browser_eval 300 / 3000                (1 turn)
//	mcp_server ruflo 300 / 3000                       (1 turn)
//	plugin     mattpocock 300 / 3000                  (1 turn)
//	tool calls 1500 / 15000 (divided shares)          (4 calls)
func turnFixture(t *testing.T, st *Ledger, base time.Time) {
	t.Helper()
	ctx := context.Background()

	type turn struct {
		key   string
		total int64
		ctxs  map[model.TurnDimension]string
		calls []string
	}
	turns := []turn{
		{"u1", 1000, map[model.TurnDimension]string{
			model.DimensionAgent: "worker", model.DimensionSkill: "alpha",
		}, []string{"Bash", "Read"}},
		{"u2", 500, map[model.TurnDimension]string{
			model.DimensionAgent: "worker", model.DimensionSkill: "alpha",
		}, nil},
		{"u3", 300, map[model.TurnDimension]string{
			model.DimensionSkill:     "beta",
			model.DimensionMCPTool:   "browser_eval",
			model.DimensionMCPServer: "ruflo",
			model.DimensionPlugin:    "mattpocock",
		}, []string{"Bash"}},
		{"u4", 200, nil, []string{"Grep"}},
	}

	var (
		events   []model.UsageEvent
		activity []model.ActivityEvent
		ctxs     []model.TurnContext
	)
	for i, tn := range turns {
		when := base.Add(time.Duration(i) * time.Minute)
		e := ev(tn.key, model.ToolClaudeCode, when, tn.total)
		e.SetCost(tn.total*10, "test")
		events = append(events, e)
		for c, name := range tn.calls {
			activity = append(activity, act(
				fmt.Sprintf("a-%s-%d", tn.key, c), name, model.ActivityTool,
				when, tn.key, c, len(tn.calls)))
		}
		// Iterate the closed vocabulary rather than the map so the emitted order
		// is fixed and a failure reproduces.
		for _, dim := range model.TurnDimensions() {
			if v, ok := tn.ctxs[dim]; ok {
				ctxs = append(ctxs, turnCtx(tn.key, dim, v, when))
			}
		}
	}

	applied, err := st.ApplyBatch(ctx, ObservationBatch{
		Events: events, Activity: activity, TurnContexts: ctxs,
	})
	if err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	if applied.Events != 4 || applied.Activity != 4 || applied.TurnContexts != 8 {
		t.Fatalf("applied = %+v, want 4 events / 4 activity / 8 turn contexts", applied)
	}
}

// TestEveryDimensionStaysUnderTheLedger is THE invariant of this feature, and
// the reason the dimension is an argument rather than a filter.
//
// SIX attributions now read the same dollars: activity_events splits a turn's
// cost among the calls that shared it, and usage_turn_context assigns the whole
// turn to one value on each of five axes. EACH must stay inside the window's
// real ledger total ON ITS OWN, and none may be inflated by another's existence.
//
// The exact expected values are asserted, not just the bound. A bound alone is
// satisfied by attributing nothing at all, which is the failure mode that would
// silently make every agent and skill look free.
//
// Note what the numbers do NOT do: the six sum to 57000, nearly three times the
// ledger's 20000. That is not a bug, it is the definition of a partition — "which
// agent ran", "which skill was active", "which server answered", "which tool was
// called" are complete answers to different questions over one budget. No query
// in this package can produce that 57000, and this test exists to keep it that
// way.
func TestEveryDimensionStaysUnderTheLedger(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	turnFixture(t, st, base)

	since, until := base.Add(-time.Hour), base.Add(time.Hour)

	ledger, err := st.Summarize(ctx, Filter{Since: since, Until: until})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if ledger.Totals.Total != 2000 || ledger.Totals.CostMicroUSD != 20000 {
		t.Fatalf("ledger = %d tok / %d micro, want 2000 / 20000",
			ledger.Totals.Total, ledger.Totals.CostMicroUSD)
	}

	af := ActivityFilter{Since: since, Until: until}

	// Every dimension, independently: exact values first, then the ceiling.
	for _, want := range []struct {
		dim    model.TurnDimension
		turns  int64
		tokens int64
		cost   int64
	}{
		{model.DimensionAgent, 2, 1500, 15000},
		{model.DimensionSkill, 3, 1800, 18000},
		{model.DimensionMCPTool, 1, 300, 3000},
		{model.DimensionMCPServer, 1, 300, 3000},
		{model.DimensionPlugin, 1, 300, 3000},
	} {
		sum, err := st.SummarizeTurnContext(ctx, want.dim, af)
		if err != nil {
			t.Fatalf("SummarizeTurnContext(%s): %v", want.dim, err)
		}
		if sum.Dimension != want.dim {
			t.Fatalf("summary echoed dimension %q, want %q", sum.Dimension, want.dim)
		}
		if sum.Totals.Turns != want.turns {
			t.Errorf("%s turns = %d, want %d", want.dim, sum.Totals.Turns, want.turns)
		}
		if sum.Totals.TotalTokens != want.tokens {
			t.Errorf("%s tokens = %d, want %d", want.dim, sum.Totals.TotalTokens, want.tokens)
		}
		if sum.Totals.CostMicroUSD != want.cost {
			t.Errorf("%s cost = %d, want %d", want.dim, sum.Totals.CostMicroUSD, want.cost)
		}
		// The ceiling, stated separately from the exact values so a future change
		// to the fixture still trips on the invariant itself.
		if sum.Totals.TotalTokens > ledger.Totals.Total {
			t.Errorf("%s attributed %d tokens, over the ledger's %d",
				want.dim, sum.Totals.TotalTokens, ledger.Totals.Total)
		}
		if sum.Totals.CostMicroUSD > ledger.Totals.CostMicroUSD {
			t.Errorf("%s attributed %d micro, over the ledger's %d",
				want.dim, sum.Totals.CostMicroUSD, ledger.Totals.CostMicroUSD)
		}
	}

	// The sixth partition: divided shares. u1's 1000 splits 500/500 across its
	// two calls, u2 contributes nothing (no calls at all), u3 300, u4 200.
	acts, err := st.SummarizeActivity(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if acts.Totals.AttributedTotal != 1500 || acts.Totals.AttributedCostMicroUSD != 15000 {
		t.Fatalf("call attribution = %d tok / %d micro, want 1500 / 15000",
			acts.Totals.AttributedTotal, acts.Totals.AttributedCostMicroUSD)
	}
	if acts.Totals.AttributedTotal > ledger.Totals.Total {
		t.Errorf("call attribution %d exceeds ledger %d", acts.Totals.AttributedTotal, ledger.Totals.Total)
	}
}

// TestOneTurnCarriesEveryDimensionAtFullCost is the shape a shared "skill"
// column could never have held, and the arithmetic that makes the partition rule
// necessary rather than stylistic.
//
// One turn carries all five attributions at once. It must produce FIVE rows, and
// each dimension's total must independently equal the turn's WHOLE cost — not a
// fifth of it. A divided share would be the wrong answer to every one of the
// five questions: the turn did not spend a fifth of its tokens as
// workflow-subagent, it spent all of them.
func TestOneTurnCarriesEveryDimensionAtFullCost(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	e := ev("u-all", model.ToolClaudeCode, base, 700)
	e.SetCost(7000, "test")

	values := map[model.TurnDimension]string{
		model.DimensionAgent:     "workflow-subagent",
		model.DimensionSkill:     "adhd",
		model.DimensionMCPTool:   "browser_eval",
		model.DimensionMCPServer: "ruflo",
		model.DimensionPlugin:    "mattpocock-skills",
	}
	var ctxs []model.TurnContext
	for _, dim := range model.TurnDimensions() {
		ctxs = append(ctxs, turnCtx("u-all", dim, values[dim], base))
	}

	applied, err := st.ApplyBatch(ctx, ObservationBatch{
		Events: []model.UsageEvent{e}, TurnContexts: ctxs,
	})
	if err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	if applied.TurnContexts != 5 {
		t.Fatalf("inserted %d contexts for one turn, want 5 (one per dimension)", applied.TurnContexts)
	}

	for _, dim := range model.TurnDimensions() {
		sum, err := st.SummarizeTurnContext(ctx, dim, ActivityFilter{GroupBy: []string{"value"}})
		if err != nil {
			t.Fatalf("SummarizeTurnContext(%s): %v", dim, err)
		}
		if len(sum.Buckets) != 1 {
			t.Fatalf("%s produced %d buckets, want 1: %+v", dim, len(sum.Buckets), sum.Buckets)
		}
		b := sum.Buckets[0]
		if b.Keys["value"] != values[dim] {
			t.Errorf("%s bucket value = %q, want %q", dim, b.Keys["value"], values[dim])
		}
		if b.Turns != 1 {
			t.Errorf("%s turns = %d, want 1", dim, b.Turns)
		}
		// The point of the test: the FULL cost on every axis, never a share.
		if b.TotalTokens != 700 || b.CostMicroUSD != 7000 {
			t.Errorf("%s = %d tok / %d micro, want the turn's whole 700 / 7000 (a divided share would be 140 / 1400)",
				dim, b.TotalTokens, b.CostMicroUSD)
		}
	}
}

// TestTurnContextsLeaveActivityTotalsUntouched is the structural half of the
// partition claim, and the one a shared table would have failed.
//
// Had turn context been another activity_events kind, every attributed turn
// would have gained up to five rows and SummarizeActivity — which no caller
// filters by kind — would have counted it five more times. Here the numbers are
// taken with the contexts absent and again with them present, and they must be
// identical.
func TestTurnContextsLeaveActivityTotalsUntouched(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	af := ActivityFilter{Since: base.Add(-time.Hour), Until: base.Add(time.Hour)}

	// Same fixture, once without the turn contexts and once with.
	bare := openTemp(t)
	withCtx := openTemp(t)
	turnFixture(t, withCtx, base)

	rows, err := withCtx.ListActivity(ctx, ActivityFilter{})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	evs, err := withCtx.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if _, err := bare.ApplyObservation(ctx, evs, rows, nil); err != nil {
		t.Fatalf("seed bare store: %v", err)
	}

	a, err := bare.SummarizeActivity(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeActivity (bare): %v", err)
	}
	b, err := withCtx.SummarizeActivity(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeActivity (with contexts): %v", err)
	}
	// ActivityBucket holds a map, so compare the metrics field by field.
	for _, m := range []struct {
		name string
		a, b int64
	}{
		{"calls", a.Totals.Calls, b.Totals.Calls},
		{"sessions", a.Totals.Sessions, b.Totals.Sessions},
		{"input", a.Totals.AttributedInput, b.Totals.AttributedInput},
		{"output", a.Totals.AttributedOutput, b.Totals.AttributedOutput},
		{"total", a.Totals.AttributedTotal, b.Totals.AttributedTotal},
		{"cost", a.Totals.AttributedCostMicroUSD, b.Totals.AttributedCostMicroUSD},
		{"unattributed", a.Totals.UnattributedCalls, b.Totals.UnattributedCalls},
		{"unpriced", a.Totals.UnpricedCalls, b.Totals.UnpricedCalls},
	} {
		if m.a != m.b {
			t.Fatalf("turn contexts changed activity %s: without=%d with=%d", m.name, m.a, m.b)
		}
	}
	if a.Totals.AttributedCostMicroUSD != 15000 {
		t.Fatalf("activity cost = %d, want 15000; a zero would satisfy the equality trivially",
			a.Totals.AttributedCostMicroUSD)
	}
}

// TestTurnContextCountsCallLessTurns covers the 41.8% of real skill records that
// call no tool at all — and, for the agent dimension, the overwhelming majority
// of turns, which never appear in the activity ledger under their agent at all.
// They emit no activity row, so a context COLUMN on activity_events would have
// had nowhere to record them and their cost would have vanished from every
// per-context answer.
func TestTurnContextCountsCallLessTurns(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	turnFixture(t, st, base)

	af := ActivityFilter{
		Since: base.Add(-time.Hour), Until: base.Add(time.Hour),
		Values: []string{"alpha"},
	}
	sum, err := st.SummarizeTurnContext(ctx, model.DimensionSkill, af)
	if err != nil {
		t.Fatalf("SummarizeTurnContext: %v", err)
	}
	// u1 (with calls) AND u2 (context-only) — 1000+500 tokens, 10000+5000 micro.
	if sum.Totals.Turns != 2 {
		t.Fatalf("alpha turns = %d, want 2 (the context-only turn must count)", sum.Totals.Turns)
	}
	if sum.Totals.TotalTokens != 1500 || sum.Totals.CostMicroUSD != 15000 {
		t.Fatalf("alpha = %d tok / %d micro, want 1500 / 15000", sum.Totals.TotalTokens, sum.Totals.CostMicroUSD)
	}

	// The activity ledger sees only u1's two calls for alpha's turns, which is
	// exactly the blind spot this table fills.
	acts, err := st.SummarizeActivity(ctx, ActivityFilter{
		Since: af.Since, Until: af.Until, Names: []string{"Bash", "Read"},
	})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if acts.Totals.Calls != 3 {
		t.Fatalf("activity calls = %d, want 3; the fixture's context-only turn must NOT appear here", acts.Totals.Calls)
	}
}

// TestTurnContextIsOnePerTurnPerDimension pins the constraint the whole
// no-double-count argument rests on. The source cannot express two skills for one
// record, and the table cannot store them: a second context for the same (usage
// row, dimension) is absorbed by the primary key, so even a buggy adapter
// emitting both an outer and an inner skill for one turn cannot make that turn
// cost twice on that axis — while a value on a DIFFERENT axis still lands.
//
// This is also how NESTING behaves. A skill may invoke another, and once the
// inner one runs the transcript names only the inner skill — so cost lands on
// the innermost active skill and the outer one is not credited with what its
// callee spent.
func TestTurnContextIsOnePerTurnPerDimension(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	e := ev("u-nested", model.ToolClaudeCode, base, 900)
	e.SetCost(9000, "test")

	// The outer skill is recorded first; a later sighting claiming the inner one
	// for the SAME turn must not add a second helping of its cost.
	applied, err := st.ApplyBatch(ctx, ObservationBatch{
		Events:       []model.UsageEvent{e},
		TurnContexts: []model.TurnContext{turnCtx("u-nested", model.DimensionSkill, "outer", base)},
	})
	if err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	if applied.TurnContexts != 1 {
		t.Fatalf("first apply inserted %d contexts, want 1", applied.TurnContexts)
	}

	again, err := st.ApplyBatch(ctx, ObservationBatch{
		TurnContexts: []model.TurnContext{
			turnCtx("u-nested", model.DimensionSkill, "inner", base),
			turnCtx("u-nested", model.DimensionSkill, "outer", base),
			// A different AXIS for the same turn is not a conflict: it answers a
			// different question and must still land.
			turnCtx("u-nested", model.DimensionAgent, "worker", base),
		},
	})
	if err != nil {
		t.Fatalf("ApplyBatch (repeat): %v", err)
	}
	if again.TurnContexts != 1 {
		t.Fatalf("re-applying contexts inserted %d, want 1 (only the new dimension)", again.TurnContexts)
	}

	sum, err := st.SummarizeTurnContext(ctx, model.DimensionSkill, ActivityFilter{GroupBy: []string{"skill"}})
	if err != nil {
		t.Fatalf("SummarizeTurnContext: %v", err)
	}
	if len(sum.Buckets) != 1 || sum.Buckets[0].Keys["skill"] != "outer" {
		t.Fatalf("buckets = %+v, want exactly one for the first-recorded skill", sum.Buckets)
	}
	if sum.Totals.CostMicroUSD != 9000 {
		t.Fatalf("skill cost = %d, want 9000 (the turn counted exactly once)", sum.Totals.CostMicroUSD)
	}

	// The agent axis carries the same turn's FULL cost, in parallel.
	agents, err := st.SummarizeTurnContext(ctx, model.DimensionAgent, ActivityFilter{})
	if err != nil {
		t.Fatalf("SummarizeTurnContext(agent): %v", err)
	}
	if agents.Totals.CostMicroUSD != 9000 {
		t.Fatalf("agent cost = %d, want 9000", agents.Totals.CostMicroUSD)
	}
}

// TestTopTurnContextRanksByCost checks the "which agent/skill is expensive"
// query answers with the value that actually cost most, capped in SQL, on every
// dimension rather than only on the one that shipped first.
func TestTopTurnContextRanksByCost(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	turnFixture(t, st, base)

	top, err := st.TopTurnContext(ctx,
		model.DimensionSkill, ActivityFilter{GroupBy: []string{"value"}}, ActivityByCost, 1)
	if err != nil {
		t.Fatalf("TopTurnContext: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("got %d rows, want 1", len(top))
	}
	if top[0].Keys["value"] != "alpha" || top[0].CostMicroUSD != 15000 {
		t.Fatalf("top skill = %+v, want alpha at 15000", top[0])
	}

	agents, err := st.TopTurnContext(ctx,
		model.DimensionAgent, ActivityFilter{GroupBy: []string{"agent"}}, ActivityByTokens, 5)
	if err != nil {
		t.Fatalf("TopTurnContext(agent): %v", err)
	}
	if len(agents) != 1 || agents[0].Keys["agent"] != "worker" || agents[0].TotalTokens != 1500 {
		t.Fatalf("top agent = %+v, want worker at 1500 tokens", agents)
	}

	if _, err := st.TopTurnContext(ctx, model.DimensionSkill, ActivityFilter{}, ActivityByCost, 1); err == nil {
		t.Fatal("TopTurnContext with no group dimension should refuse")
	}
}

// TestSkillCostDelegatesToTheSkillDimension is the compatibility check for the
// two skill-named entry points. They must be the SAME answer as the general
// query pinned to the skill dimension — not a second implementation that could
// drift into a second number for one question.
func TestSkillCostDelegatesToTheSkillDimension(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	turnFixture(t, st, base)

	f := ActivityFilter{GroupBy: []string{"skill"}}
	viaSkill, err := st.SummarizeSkillCost(ctx, f)
	if err != nil {
		t.Fatalf("SummarizeSkillCost: %v", err)
	}
	viaDim, err := st.SummarizeTurnContext(ctx, model.DimensionSkill, f)
	if err != nil {
		t.Fatalf("SummarizeTurnContext: %v", err)
	}
	// TurnContextBucket holds a map, so compare the metrics field by field.
	for _, m := range []struct {
		name string
		a, b int64
	}{
		{"turns", viaSkill.Totals.Turns, viaDim.Totals.Turns},
		{"tokens", viaSkill.Totals.TotalTokens, viaDim.Totals.TotalTokens},
		{"cost", viaSkill.Totals.CostMicroUSD, viaDim.Totals.CostMicroUSD},
		{"sessions", viaSkill.Totals.Sessions, viaDim.Totals.Sessions},
		{"unjoined", viaSkill.Totals.UnjoinedTurns, viaDim.Totals.UnjoinedTurns},
		{"unpriced", viaSkill.Totals.UnpricedTurns, viaDim.Totals.UnpricedTurns},
	} {
		if m.a != m.b {
			t.Fatalf("SummarizeSkillCost %s = %d, the skill dimension says %d", m.name, m.a, m.b)
		}
	}
	if viaSkill.Totals.CostMicroUSD != 18000 {
		t.Fatalf("skill cost = %d, want 18000; equal zeroes would satisfy the check trivially",
			viaSkill.Totals.CostMicroUSD)
	}

	topSkill, err := st.TopSkillCost(ctx, f, ActivityByCost, 1)
	if err != nil {
		t.Fatalf("TopSkillCost: %v", err)
	}
	topDim, err := st.TopTurnContext(ctx, model.DimensionSkill, f, ActivityByCost, 1)
	if err != nil {
		t.Fatalf("TopTurnContext: %v", err)
	}
	if len(topSkill) != 1 || len(topDim) != 1 || topSkill[0].CostMicroUSD != topDim[0].CostMicroUSD {
		t.Fatalf("TopSkillCost %+v differs from TopTurnContext %+v", topSkill, topDim)
	}
}

// TestTurnContextRefusesDimensionMixing is the partition invariant as an API
// contract. Every one of these would, if honoured, put two partitions of the same
// dollars in one result set or filter one partition by another's vocabulary, and
// every one of them is refused rather than quietly ignored — an empty result or a
// mislabelled column reads as a fact, and a returned error does not.
func TestTurnContextRefusesDimensionMixing(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	turnFixture(t, st, base)

	for _, tc := range []struct {
		name string
		dim  model.TurnDimension
		f    ActivityFilter
	}{
		// The one grouping that would concatenate two partitions outright.
		{"group by dimension", model.DimensionSkill, ActivityFilter{GroupBy: []string{"dimension"}}},
		// Grouping one dimension's rows under another dimension's heading.
		{"skill query grouped by agent", model.DimensionSkill, ActivityFilter{GroupBy: []string{"agent"}}},
		{"agent query grouped by skill", model.DimensionAgent, ActivityFilter{GroupBy: []string{"skill"}}},
		{"plugin query grouped by mcp_server", model.DimensionPlugin, ActivityFilter{GroupBy: []string{"mcp_server"}}},
		// A dimension outside the closed vocabulary is a typo, and a typo must
		// not silently return "that cost nothing".
		{"unknown dimension", model.TurnDimension("agnet"), ActivityFilter{}},
		{"empty dimension", model.TurnDimension(""), ActivityFilter{}},
		// The activity ledger's call-level vocabulary, which would need a join
		// that re-introduces the multi-call double count.
		{"kinds", model.DimensionSkill, ActivityFilter{Kinds: []string{"tool"}}},
		{"names", model.DimensionSkill, ActivityFilter{Names: []string{"Bash"}}},
		{"group by kind", model.DimensionSkill, ActivityFilter{GroupBy: []string{"kind"}}},
		{"group by name", model.DimensionSkill, ActivityFilter{GroupBy: []string{"name"}}},
		{"group by unknown", model.DimensionSkill, ActivityFilter{GroupBy: []string{"nope"}}},
		// Skills is the skill dimension's own filter and means nothing elsewhere.
		{"skills filter on the agent dimension", model.DimensionAgent, ActivityFilter{Skills: []string{"alpha"}}},
	} {
		if _, err := st.SummarizeTurnContext(ctx, tc.dim, tc.f); err == nil {
			t.Errorf("SummarizeTurnContext accepted %s, want refusal", tc.name)
		}
		if _, err := st.TopTurnContext(ctx, tc.dim, tc.f, ActivityByCost, 1); err == nil {
			t.Errorf("TopTurnContext accepted %s, want refusal", tc.name)
		}
	}

	// The activity queries keep their full vocabulary: Values/Skills are inert
	// there, not errors, because they name a dimension that table does not have.
	if _, err := st.SummarizeActivity(ctx, ActivityFilter{
		Kinds: []string{"tool"}, Names: []string{"Bash"},
		Skills: []string{"alpha"}, Values: []string{"worker"},
	}); err != nil {
		t.Fatalf("SummarizeActivity must still accept its own filters: %v", err)
	}
}

// TestTurnContextAppendOnly holds the table to the same terms as the two
// ledgers: an observation of an immutable transcript line is never rewritten.
func TestTurnContextAppendOnly(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	turnFixture(t, st, base)

	if _, err := st.db.ExecContext(ctx,
		`UPDATE usage_turn_context SET value='rewritten'`); err == nil {
		t.Fatal("UPDATE on usage_turn_context succeeded; the append-only trigger is missing")
	}
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM usage_turn_context`); err == nil {
		t.Fatal("DELETE on usage_turn_context succeeded; the append-only trigger is missing")
	}
}

// TestTurnContextRejectsBadRows checks the CHECK constraints reach the caller as
// a skip rather than being swallowed: ON CONFLICT DO NOTHING is scoped to the
// primary key, and a blanket OR IGNORE would have hidden every one of these.
//
// The unknown dimension is the important one. It is what stops an adapter
// inventing a sixth axis by writing a new string — the read API's per-dimension
// refusals are only as good as the vocabulary the storage layer closes.
func TestTurnContextRejectsBadRows(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	e := ev("u-bad", model.ToolClaudeCode, base, 100)
	res, err := st.ApplyBatch(ctx, ObservationBatch{
		Events: []model.UsageEvent{e},
		TurnContexts: []model.TurnContext{
			{UsageDedupKey: "u-bad", Tool: model.ToolClaudeCode, Dimension: model.DimensionSkill, Value: "", EventTime: base},
			{UsageDedupKey: "u-bad", Tool: model.ToolClaudeCode, Dimension: "workspace", Value: "x", EventTime: base},
			turnCtx("u-bad", model.DimensionSkill, "real", base),
		},
	})
	// The bad rows are reported, not swallowed — and the good one still
	// committed, so poison rows cannot cost a batch that is re-derived every
	// cycle.
	if err == nil {
		t.Fatal("bad turn contexts were accepted; the CHECK constraints are missing or swallowed")
	}
	if !strings.Contains(err.Error(), "skipped 2 of 3") {
		t.Fatalf("error = %v, want a per-row skip report", err)
	}
	if res.TurnContexts != 1 {
		t.Fatalf("inserted %d contexts, want 1 (the empty value and unknown dimension must be skipped)", res.TurnContexts)
	}
	if res.Events != 1 {
		t.Fatalf("inserted %d events, want 1 (the batch must still commit)", res.Events)
	}
}

// TestMigrateV6ToV7FoldsSkillContext verifies the step against a database
// stamped at v6, and covers the three things v7 does that no earlier step did.
//
// It CREATES usage_turn_context empty — no backfill is possible, since the facts
// live only on the source transcripts and the no-UPDATE trigger forbids adding
// them to stored rows regardless.
//
// It DROPS usage_skill_context, which it subsumes. That is only defensible
// because the table is derived: its rows are this table's dimension='skill'
// partition, re-derived from the transcripts on the next pass, the way
// usage_rollup is re-derived from the ledger.
//
// And it CLEARS the claude-code checkpoint, which is what makes "re-derived on
// the next pass" true rather than aspirational: the adapter opens no file when
// its size+mtime manifest is unchanged, so without this an idle machine would
// keep an empty table forever.
func TestMigrateV6ToV7FoldsSkillContext(t *testing.T) {
	ctx := context.Background()
	st, err := Open(legacyDB(t, 6))
	if err != nil {
		t.Fatalf("migrate v6 database: %v", err)
	}
	defer st.Close()

	v, err := readSchemaVersion(ctx, st.db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("post-migration version = %d, want %d", v, SchemaVersion)
	}

	// The pre-existing LEDGER row survived untouched. That is the line between
	// the two kinds of table: history is never dropped, derived data is.
	events, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].DedupKey != "legacy-1" || events[0].TotalTokens != 30 {
		t.Fatalf("ledger after migration = %+v, want the seeded legacy-1 row intact", events)
	}

	// usage_skill_context is gone, triggers and all.
	gone, err := tableExists(ctx, st.db, "usage_skill_context")
	if err != nil {
		t.Fatalf("check usage_skill_context: %v", err)
	}
	if gone {
		t.Fatal("usage_skill_context survived v7; two tables would then answer the same question")
	}

	// The checkpoint that would have suppressed the re-derivation is cleared.
	cp, err := st.Checkpoint(ctx, model.ToolClaudeCode, "/home/x/.claude")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if cp != nil {
		t.Fatalf("claude-code checkpoint survived v7 (%+v); the dropped rows would never come back", cp)
	}

	// The new table exists, is queryable on every dimension, and is empty.
	for _, dim := range model.TurnDimensions() {
		sum, err := st.SummarizeTurnContext(ctx, dim, ActivityFilter{})
		if err != nil {
			t.Fatalf("SummarizeTurnContext(%s) on a migrated database: %v", dim, err)
		}
		if sum.Totals.Turns != 0 {
			t.Fatalf("migrated %s partition holds %d turns, want 0 (v7 creates the table empty)", dim, sum.Totals.Turns)
		}
	}

	// And it accepts new rows, so the migration produced a usable table rather
	// than merely a present one.
	when := time.Unix(1750000100, 0).UTC()
	e := ev("fresh-1", model.ToolClaudeCode, when, 42)
	if _, err := st.ApplyBatch(ctx, ObservationBatch{
		Events: []model.UsageEvent{e},
		TurnContexts: []model.TurnContext{
			turnCtx("fresh-1", model.DimensionAgent, "workflow-subagent", when),
			turnCtx("fresh-1", model.DimensionSkill, "alpha", when),
		},
	}); err != nil {
		t.Fatalf("write to migrated turn-context table: %v", err)
	}
}

// TestTurnContextTableMatchesFreshSchema guards the drift two DDL copies always
// eventually produce: the v7 migration's table, indexes and triggers and
// schema.sql's must match, or a migrated database and a fresh one would carry
// different tables under the same version stamp.
func TestTurnContextTableMatchesFreshSchema(t *testing.T) {
	fresh := openTemp(t)
	// From v5, so the fixture also exercises the create-then-drop the v6 step
	// still performs on the way through.
	migrated, err := Open(legacyDB(t, 5))
	if err != nil {
		t.Fatalf("migrate v5 db: %v", err)
	}
	defer migrated.Close()

	if a, b := turnColumnSpec(t, fresh), turnColumnSpec(t, migrated); a != b {
		t.Fatalf("usage_turn_context drifted between schema.sql and the v7 migration:\nfresh:    %s\nmigrated: %s", a, b)
	}
	if a, b := turnObjectSpec(t, fresh), turnObjectSpec(t, migrated); a != b {
		t.Fatalf("turn-context indexes/triggers drifted between schema.sql and the v7 migration:\nfresh:    %s\nmigrated: %s", a, b)
	}
	// A fresh database must not carry the retired table either, or the two
	// spellings of "skill cost" would reappear on new installs only.
	ctx := context.Background()
	if present, err := tableExists(ctx, fresh.db, "usage_skill_context"); err != nil || present {
		t.Fatalf("fresh schema.sql still creates usage_skill_context (present=%v err=%v)", present, err)
	}
}

// turnColumnSpec renders usage_turn_context's column names, types, NOT NULL
// flags and primary-key positions as one comparable string.
func turnColumnSpec(t *testing.T, st *Ledger) string {
	t.Helper()
	rows, err := st.db.Query(
		`SELECT name, type, "notnull", pk FROM pragma_table_info('usage_turn_context') ORDER BY cid`)
	if err != nil {
		t.Fatalf("read turn context columns: %v", err)
	}
	defer rows.Close()
	var spec string
	for rows.Next() {
		var name, typ string
		var notNull, pk int
		if err := rows.Scan(&name, &typ, &notNull, &pk); err != nil {
			t.Fatalf("scan turn context column: %v", err)
		}
		spec += fmt.Sprintf("%s:%s:%d:%d;", name, typ, notNull, pk)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("turn context column rows: %v", err)
	}
	if spec == "" {
		t.Fatal("usage_turn_context has no columns")
	}
	// The COMPOSITE primary key is the whole no-double-count argument: the key
	// alone would allow one context per turn across all axes, and no key at all
	// would allow two agents for one turn. Assert both halves rather than
	// trusting the comparison to notice their absence on both sides.
	for _, want := range []string{"usage_dedup_key:TEXT:1:1;", "dimension:TEXT:1:2;"} {
		if !strings.Contains(spec, want) {
			t.Fatalf("primary key is not (usage_dedup_key, dimension); missing %q in %s", want, spec)
		}
	}
	return spec
}

// turnObjectSpec renders the names of every index and trigger attached to
// usage_turn_context.
func turnObjectSpec(t *testing.T, st *Ledger) string {
	t.Helper()
	rows, err := st.db.Query(
		`SELECT type, name FROM sqlite_master WHERE tbl_name='usage_turn_context'
		 AND type IN ('index','trigger') ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read turn context objects: %v", err)
	}
	defer rows.Close()
	var spec string
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatalf("scan turn context object: %v", err)
		}
		spec += typ + ":" + name + ";"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("turn context object rows: %v", err)
	}
	return spec
}
