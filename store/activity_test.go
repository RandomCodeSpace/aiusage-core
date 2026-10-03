package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// act builds an activity row tied to a usage event's dedup key.
func act(dedup, name string, kind model.ActivityKind, et time.Time, usageKey string, seq, calls int) model.ActivityEvent {
	return model.ActivityEvent{
		Tool:          model.ToolClaudeCode,
		Kind:          kind,
		Name:          name,
		SessionID:     "s",
		Project:       "/p",
		Model:         "m",
		EventTime:     et,
		UsageDedupKey: usageKey,
		MessageID:     usageKey,
		TurnSeq:       seq,
		CallsInTurn:   calls,
		DedupKey:      dedup,
	}
}

// TestActivityAttributionNeverExceedsLedger is THE invariant of this table.
//
// One assistant turn emits several tool calls against a SINGLE usage object.
// The obvious-and-wrong implementation copies the turn's tokens onto each call,
// which multiplies a 3-call turn's cost by three; the join in
// SummarizeActivity is written to divide by calls_in_turn precisely so it
// cannot. This seeds a window whose turns have 1, 3 and 5 calls and asserts the
// attributed total is at most the ledger's total for the same window — the
// direction that must NEVER be violated — and that it is not trivially zero,
// which would satisfy the bound by attributing nothing at all.
func TestActivityAttributionNeverExceedsLedger(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// Three turns, deliberately with different call counts. Token totals are
	// chosen so the integer division does NOT come out even (1000/3, 777/5):
	// an implementation that multiplied would overshoot by thousands.
	turns := []struct {
		key   string
		total int64
		calls int
	}{
		{"u-single", 500, 1},
		{"u-triple", 1000, 3},
		{"u-quint", 777, 5},
	}

	var (
		events   []model.UsageEvent
		activity []model.ActivityEvent
		want     int64
	)
	for i, tn := range turns {
		when := base.Add(time.Duration(i) * time.Minute)
		e := ev(tn.key, model.ToolClaudeCode, when, tn.total)
		e.InputTokens = tn.total
		cost := int64(tn.total) * 10
		e.SetCost(cost, "test")
		events = append(events, e)
		want += tn.total
		for c := range tn.calls {
			activity = append(activity, act(
				fmt.Sprintf("a-%s-%d", tn.key, c), "Bash", model.ActivityTool,
				when, tn.key, c, tn.calls))
		}
	}

	applied, err := st.ApplyObservation(ctx, events, activity, nil)
	if err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}
	if applied.Events != len(events) || applied.Activity != len(activity) {
		t.Fatalf("applied = %+v, want %d events / %d activity", applied, len(events), len(activity))
	}

	window := Filter{Since: base.Add(-time.Hour), Until: base.Add(time.Hour)}
	sum, err := st.Summarize(ctx, window)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Totals.Total != want {
		t.Fatalf("ledger total = %d, want %d", sum.Totals.Total, want)
	}

	af := ActivityFilter{Since: window.Since, Until: window.Until}
	asum, err := st.SummarizeActivity(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}

	// The invariant, stated in the direction that matters.
	if asum.Totals.AttributedTotal > sum.Totals.Total {
		t.Fatalf("attributed tokens %d EXCEED the ledger's %d for the same window: "+
			"a turn's cost is being counted once per call in it",
			asum.Totals.AttributedTotal, sum.Totals.Total)
	}
	if asum.Totals.AttributedCostMicroUSD > sum.Totals.CostMicroUSD {
		t.Fatalf("attributed cost %d EXCEEDS the ledger's %d for the same window",
			asum.Totals.AttributedCostMicroUSD, sum.Totals.CostMicroUSD)
	}

	// Not trivially satisfied: the bound must hold because the split is right,
	// not because nothing was attributed. Integer division floors each share, so
	// the exact expectation is the sum of floor(total/calls)*calls.
	wantAttributed := int64(500) + (1000/3)*3 + (777/5)*5
	if asum.Totals.AttributedTotal != wantAttributed {
		t.Fatalf("attributed tokens = %d, want %d (floor share per call, summed)",
			asum.Totals.AttributedTotal, wantAttributed)
	}
	if asum.Totals.Calls != int64(len(activity)) {
		t.Fatalf("calls = %d, want %d", asum.Totals.Calls, len(activity))
	}
	if asum.Totals.UnattributedCalls != 0 {
		t.Fatalf("unattributed calls = %d, want 0 (every call names a stored usage row)",
			asum.Totals.UnattributedCalls)
	}
}

// TestAttributionSurvivesAStaleCallsInTurnStamp is the invariant re-stated for
// the case an append-only table cannot fix by rewriting: a row whose
// calls_in_turn is WRONG and can never be corrected.
//
// It is not hypothetical. Claude Code streams one response across several
// transcript records, and the claudecode adapter used to emit one row per turn
// stamped calls_in_turn=1 for turns that had three calls. Appending the missing
// two is the recovery — their keys never collided with the stored one — but the
// row already in the table keeps its 1 forever. Dividing by the STAMP would
// then give one turn's tokens out as 1/1 + 1/3 + 1/3, i.e. 167% of what it
// cost: an overstatement, which is the one direction this table promises never
// to go. Counting the rows that share the usage key removes the possibility.
//
// This seeds exactly that shape — one stale row, two correct siblings — and
// asserts the turn is attributed AT MOST once, not one and two thirds.
func TestAttributionSurvivesAStaleCallsInTurnStamp(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	const total = int64(900)
	e := ev("u-streamed", model.ToolClaudeCode, base, total)
	e.InputTokens = total
	e.SetCost(total*10, "test")

	// The row written before the union fix: it believed it was the turn's only
	// call. Its stamp is a lie the table cannot be told to forget.
	stale := act("a-stale", "Bash", model.ActivityTool, base, "u-streamed", 0, 1)
	// The rows the re-read appends once the adapter counts across records.
	rest := []model.ActivityEvent{
		act("a-new-1", "Read", model.ActivityTool, base, "u-streamed", 1, 3),
		act("a-new-2", "Edit", model.ActivityTool, base, "u-streamed", 2, 3),
	}

	if _, err := st.ApplyObservation(ctx, []model.UsageEvent{e},
		append([]model.ActivityEvent{stale}, rest...), nil); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}

	window := Filter{Since: base.Add(-time.Hour), Until: base.Add(time.Hour)}
	sum, err := st.Summarize(ctx, window)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	asum, err := st.SummarizeActivity(ctx, ActivityFilter{Since: window.Since, Until: window.Until})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}

	if asum.Totals.Calls != 3 {
		t.Fatalf("calls = %d, want 3", asum.Totals.Calls)
	}
	if asum.Totals.AttributedTotal > sum.Totals.Total {
		t.Fatalf("attributed tokens %d EXCEED the ledger's %d: a stale calls_in_turn stamp "+
			"is being trusted over the rows that actually share the usage row",
			asum.Totals.AttributedTotal, sum.Totals.Total)
	}
	if asum.Totals.AttributedCostMicroUSD > sum.Totals.CostMicroUSD {
		t.Fatalf("attributed cost %d EXCEEDS the ledger's %d", asum.Totals.AttributedCostMicroUSD,
			sum.Totals.CostMicroUSD)
	}
	// Three rows share the usage row, so each takes a third whatever it was
	// stamped with — and the shares add back up to the turn.
	if want := (total / 3) * 3; asum.Totals.AttributedTotal != want {
		t.Fatalf("attributed tokens = %d, want %d (an equal third to each of the three rows)",
			asum.Totals.AttributedTotal, want)
	}

	// Per-name, the stale row must not out-earn its siblings either: a ranking
	// built on the stamp would show Bash at three times Read and Edit.
	top, err := st.TopActivity(ctx, ActivityFilter{
		Since: window.Since, Until: window.Until, GroupBy: []string{"name"},
	}, ActivityByTokens, 10)
	if err != nil {
		t.Fatalf("TopActivity: %v", err)
	}
	for _, b := range top {
		if got, want := b.AttributedTotal, total/3; got != want {
			t.Errorf("name %q attributed %d tokens, want %d — every call of the turn takes "+
				"the same share regardless of what it was stamped with",
				b.Keys["name"], got, want)
		}
	}
}

// TestActivityUnjoinedCallsCostNothing pins the other half of the invariant: a
// call whose source gives no usage join (codex function calls, hooks) is still
// counted as a call, contributes no tokens, and is reported as unattributed
// rather than silently reading as free.
func TestActivityUnjoinedCallsCostNothing(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	when := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)

	e := ev("u-1", model.ToolClaudeCode, when, 300)
	joined := act("a-joined", "Read", model.ActivityTool, when, "u-1", 0, 1)
	orphan := act("a-orphan", "exec", model.ActivityTool, when, "", 0, 1)
	orphan.Tool = model.ToolCodex
	hook := act("a-hook", "stop_hook_summary", model.ActivityHook, when, "", 0, 1)

	if _, err := st.ApplyObservation(ctx, []model.UsageEvent{e},
		[]model.ActivityEvent{joined, orphan, hook}, nil); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}

	sum, err := st.SummarizeActivity(ctx, ActivityFilter{})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if sum.Totals.Calls != 3 {
		t.Fatalf("calls = %d, want 3 (an unjoinable call still happened)", sum.Totals.Calls)
	}
	if sum.Totals.UnattributedCalls != 2 {
		t.Fatalf("unattributed = %d, want 2", sum.Totals.UnattributedCalls)
	}
	if sum.Totals.AttributedTotal != 300 {
		t.Fatalf("attributed = %d, want 300 (only the joined call carries tokens)",
			sum.Totals.AttributedTotal)
	}
}

// TestActivityGroupingAndRanking covers the read API the TUI will call:
// frequency by name, and the cost ranking that answers "which skill is
// expensive" in one query.
func TestActivityGroupingAndRanking(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	when := time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC)

	// Two turns: a cheap one that called Bash twice, and an expensive one that
	// invoked a skill once. Frequency ranks Bash first; cost ranks the skill.
	cheap := ev("u-cheap", model.ToolClaudeCode, when, 100)
	cheap.SetCost(1_000, "test")
	pricey := ev("u-pricey", model.ToolClaudeCode, when, 90_000)
	pricey.SetCost(900_000, "test")

	activity := []model.ActivityEvent{
		act("a1", "Bash", model.ActivityTool, when, "u-cheap", 0, 2),
		act("a2", "Bash", model.ActivityTool, when, "u-cheap", 1, 2),
		act("a3", "artifact-design", model.ActivitySkill, when, "u-pricey", 0, 1),
	}
	if _, err := st.ApplyObservation(ctx, []model.UsageEvent{cheap, pricey}, activity, nil); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}

	byName, err := st.SummarizeActivity(ctx, ActivityFilter{GroupBy: []string{"name"}})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	got := map[string]int64{}
	for _, b := range byName.Buckets {
		got[b.Keys["name"]] = b.Calls
	}
	if got["Bash"] != 2 || got["artifact-design"] != 1 {
		t.Fatalf("call counts by name = %v, want Bash:2 artifact-design:1", got)
	}

	byKind, err := st.SummarizeActivity(ctx, ActivityFilter{GroupBy: []string{"kind"}})
	if err != nil {
		t.Fatalf("SummarizeActivity by kind: %v", err)
	}
	kinds := map[string]int64{}
	for _, b := range byKind.Buckets {
		kinds[b.Keys["kind"]] = b.Calls
	}
	if kinds["tool"] != 2 || kinds["skill"] != 1 {
		t.Fatalf("call counts by kind = %v, want tool:2 skill:1", kinds)
	}

	top, err := st.TopActivity(ctx, ActivityFilter{GroupBy: []string{"name"}}, ActivityByCost, 1)
	if err != nil {
		t.Fatalf("TopActivity: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("TopActivity returned %d buckets, want 1", len(top))
	}
	if top[0].Keys["name"] != "artifact-design" {
		t.Fatalf("most expensive name = %q, want artifact-design", top[0].Keys["name"])
	}
	if top[0].AttributedCostMicroUSD != 900_000 {
		t.Fatalf("attributed cost = %d, want 900000", top[0].AttributedCostMicroUSD)
	}

	// Ranking by calls picks the other one, which is the whole point of having
	// both metrics: frequency and expense are different questions.
	byCalls, err := st.TopActivity(ctx, ActivityFilter{GroupBy: []string{"name"}}, ActivityByCalls, 1)
	if err != nil {
		t.Fatalf("TopActivity by calls: %v", err)
	}
	if len(byCalls) != 1 || byCalls[0].Keys["name"] != "Bash" {
		t.Fatalf("most-called name = %+v, want Bash", byCalls)
	}
}

// TestActivityPerSessionBreakdown covers the per-session view: calls by name
// within one session, with the other session excluded.
func TestActivityPerSessionBreakdown(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	when := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)

	mine := act("a1", "Read", model.ActivityTool, when, "", 0, 1)
	mine.SessionID = "sess-a"
	theirs := act("a2", "Write", model.ActivityTool, when, "", 0, 1)
	theirs.SessionID = "sess-b"

	if _, err := st.ApplyObservation(ctx, nil, []model.ActivityEvent{mine, theirs}, nil); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}

	sum, err := st.SummarizeActivity(ctx, ActivityFilter{
		Sessions: []string{"sess-a"}, GroupBy: []string{"name"},
	})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if len(sum.Buckets) != 1 || sum.Buckets[0].Keys["name"] != "Read" {
		t.Fatalf("session breakdown = %+v, want only Read", sum.Buckets)
	}
	if sum.Totals.Sessions != 1 {
		t.Fatalf("distinct sessions = %d, want 1", sum.Totals.Sessions)
	}
}

// TestActivityAppendOnly pins the triggers: activity is history on the same
// terms as usage, so neither UPDATE nor DELETE is available even to a buggy
// path inside this package.
func TestActivityAppendOnly(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	when := time.Now().UTC()

	if _, err := st.ApplyObservation(ctx, nil,
		[]model.ActivityEvent{act("a1", "Bash", model.ActivityTool, when, "", 0, 1)}, nil); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}

	if _, err := st.db.ExecContext(ctx, `UPDATE activity_events SET name='x'`); err == nil {
		t.Fatal("UPDATE on activity_events succeeded; the append-only trigger is missing")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE refused with %v, want the append-only abort", err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM activity_events`); err == nil {
		t.Fatal("DELETE on activity_events succeeded; the append-only trigger is missing")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE refused with %v, want the append-only abort", err)
	}
}

// TestActivityDedupIsIdempotent verifies a re-read inserts nothing new. The
// claude-code adapter re-parses every transcript of a root whenever any file
// under it changes, so this is the steady state, not an edge case.
func TestActivityDedupIsIdempotent(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	when := time.Now().UTC()
	rows := []model.ActivityEvent{act("a1", "Bash", model.ActivityTool, when, "", 0, 1)}

	first, err := st.ApplyObservation(ctx, nil, rows, nil)
	if err != nil || first.Activity != 1 {
		t.Fatalf("first apply = %+v err=%v, want 1 activity", first, err)
	}
	second, err := st.ApplyObservation(ctx, nil, rows, nil)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second.Activity != 0 {
		t.Fatalf("re-applying the same rows inserted %d, want 0", second.Activity)
	}
}

// TestActivityUsageCountsTrackOnlyCommittedRows pins the derived divisor's
// write contract. Duplicate, malformed and unjoinable activity rows must not
// change a count that every attribution query trusts.
func TestActivityUsageCountsTrackOnlyCommittedRows(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	when := time.Date(2026, 5, 5, 8, 0, 0, 0, time.UTC)

	initial := []model.ActivityEvent{
		act("count-a", "Read", model.ActivityTool, when, "usage-a", 0, 2),
		act("count-b", "Edit", model.ActivityTool, when, "usage-a", 1, 2),
		act("unjoined", "stop_hook", model.ActivityHook, when, "", 0, 1),
	}
	res, err := st.ApplyObservation(ctx, nil, initial, nil)
	if err != nil || res.Activity != 3 {
		t.Fatalf("initial apply = %+v err=%v, want 3 activity", res, err)
	}
	assertActivityUsageCount(t, st, "usage-a", 2)
	assertActivityUsageCountRows(t, st, 1)

	res, err = st.ApplyObservation(ctx, nil, initial, nil)
	if err != nil || res.Activity != 0 {
		t.Fatalf("duplicate apply = %+v err=%v, want 0 activity", res, err)
	}
	assertActivityUsageCount(t, st, "usage-a", 2)

	bad := act("bad-count", "", model.ActivityTool, when, "usage-a", 0, 1)
	good := act("count-c", "Bash", model.ActivityTool, when, "usage-a", 0, 1)
	res, err = st.ApplyObservation(ctx, nil, []model.ActivityEvent{bad, good}, nil)
	if err == nil || res.Activity != 1 {
		t.Fatalf("mixed apply = %+v err=%v, want 1 activity and a row error", res, err)
	}
	assertActivityUsageCount(t, st, "usage-a", 3)
	assertActivityUsageCountRows(t, st, 1)
}

func assertActivityUsageCount(t *testing.T, st *Ledger, key string, want int64) {
	t.Helper()
	var got int64
	if err := st.db.QueryRow(`
		SELECT activity_count FROM activity_usage_counts WHERE usage_dedup_key=?`, key).Scan(&got); err != nil {
		t.Fatalf("read activity count for %s: %v", key, err)
	}
	if got != want {
		t.Fatalf("activity count for %s = %d, want %d", key, got, want)
	}
}

func assertActivityUsageCountRows(t *testing.T, st *Ledger, want int64) {
	t.Helper()
	var got int64
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM activity_usage_counts`).Scan(&got); err != nil {
		t.Fatalf("count activity usage keys: %v", err)
	}
	if got != want {
		t.Fatalf("activity usage count rows = %d, want %d", got, want)
	}
}

// TestActivityRejectsMalformedRows checks the CHECK constraints do their job
// and that a poison row is skipped rather than aborting the batch it rides in —
// the same contract usage_events has, because activity is re-derived every
// cycle and one bad row must not stall the rest forever.
func TestActivityRejectsMalformedRows(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	when := time.Now().UTC()

	bad := []model.ActivityEvent{
		act("bad-kind", "Bash", model.ActivityKind("verb"), when, "", 0, 1),
		act("bad-name", "", model.ActivityTool, when, "", 0, 1),
		act("bad-seq", "Bash", model.ActivityTool, when, "", 3, 2), // seq past the turn
	}
	good := act("good", "Bash", model.ActivityTool, when, "", 0, 1)

	res, err := st.ApplyObservation(ctx, nil, append(bad, good), nil)
	if err == nil {
		t.Fatal("expected a skip error naming the rejected rows")
	}
	if res.Activity != 1 {
		t.Fatalf("inserted %d, want 1 (the good row must still commit)", res.Activity)
	}
	rows, err := st.ListActivity(ctx, ActivityFilter{})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(rows) != 1 || rows[0].DedupKey != "good" {
		t.Fatalf("stored rows = %+v, want only the good one", rows)
	}
}

// TestActivityRefusesUnknownGroupDimension pins the refusal: an unknown
// dimension is a caller bug, and answering it from a column that does not exist
// is not on offer.
func TestActivityRefusesUnknownGroupDimension(t *testing.T) {
	st := openTemp(t)
	if _, err := st.SummarizeActivity(context.Background(),
		ActivityFilter{GroupBy: []string{"provider"}}); err == nil {
		t.Fatal("expected a refusal for an unknown activity group dimension")
	}
}

// TestTopActivityNeedsADimension: ranking one grand-total bucket is not a
// ranking, and silently returning it would look like an answer.
func TestTopActivityNeedsADimension(t *testing.T) {
	st := openTemp(t)
	if _, err := st.TopActivity(context.Background(), ActivityFilter{}, ActivityByCost, 5); err == nil {
		t.Fatal("expected TopActivity to refuse a filter with no group dimension")
	}
}

// TestActivityQueriesHonourContext: the TUI cancels superseded load flights, so
// a query that ignored ctx would keep burning CPU for a result nobody wants.
func TestActivityQueriesHonourContext(t *testing.T) {
	st := openTemp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := st.SummarizeActivity(ctx, ActivityFilter{GroupBy: []string{"name"}}); err == nil {
		t.Fatal("SummarizeActivity ignored a cancelled context")
	}
	if _, err := st.TopActivity(ctx, ActivityFilter{GroupBy: []string{"name"}}, ActivityByCalls, 5); err == nil {
		t.Fatal("TopActivity ignored a cancelled context")
	}
}

// TestMigrateV4ToV5CreatesActivityLedger opens a real v4 database, migrates it,
// and checks the two things a migration must never get wrong: the existing
// ledger is intact and readable, and the new table actually works.
func TestMigrateV4ToV5CreatesActivityLedger(t *testing.T) {
	ctx := context.Background()
	path := legacyDB(t, 4)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("migrate v4 database: %v", err)
	}
	defer st.Close()

	v, err := readSchemaVersion(ctx, st.db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	// Opening a v4 database runs every step from v5 up to SchemaVersion, so this
	// asserts the activity ledger exists after that whole chain rather than
	// after one step. The v5 statements are the only ones that can create it.
	if v != SchemaVersion {
		t.Fatalf("post-migration version = %d, want %d", v, SchemaVersion)
	}

	// The pre-existing ledger row survived and still reads back correctly.
	events, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].DedupKey != "legacy-1" || events[0].TotalTokens != 30 {
		t.Fatalf("ledger after migration = %+v, want the seeded legacy-1 row intact", events)
	}

	// The new table is empty and usable.
	rows, err := st.ListActivity(ctx, ActivityFilter{})
	if err != nil {
		t.Fatalf("ListActivity on a migrated database: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("migrated activity ledger holds %d rows, want 0 (v5 creates it empty)", len(rows))
	}
	res, err := st.ApplyObservation(ctx, nil,
		[]model.ActivityEvent{act("a1", "Bash", model.ActivityTool, time.Now().UTC(), "", 0, 1)}, nil)
	if err != nil || res.Activity != 1 {
		t.Fatalf("insert into migrated activity ledger = %+v err=%v, want 1", res, err)
	}
}

// TestActivityTableMatchesFreshSchema guards the one thing two DDL copies
// always eventually do: drift. The v5 migration's table, indexes and triggers
// and schema.sql's must match, or a migrated database and a fresh one would
// carry different tables under the same version stamp.
func TestActivityTableMatchesFreshSchema(t *testing.T) {
	fresh := openTemp(t)
	migrated, err := Open(legacyDB(t, 4))
	if err != nil {
		t.Fatalf("migrate v4 db: %v", err)
	}
	defer migrated.Close()

	if a, b := activityColumnSpec(t, fresh), activityColumnSpec(t, migrated); a != b {
		t.Fatalf("activity_events drifted between schema.sql and the v5 migration:\nfresh:    %s\nmigrated: %s", a, b)
	}
	if a, b := activityObjectSpec(t, fresh), activityObjectSpec(t, migrated); a != b {
		t.Fatalf("activity indexes/triggers drifted between schema.sql and the v5 migration:\nfresh:    %s\nmigrated: %s", a, b)
	}
}

// activityColumnSpec renders activity_events' column names, types, NOT NULL
// flags and primary-key positions as one comparable string.
func activityColumnSpec(t *testing.T, st *Ledger) string {
	t.Helper()
	rows, err := st.db.Query(
		`SELECT name, type, "notnull", pk FROM pragma_table_info('activity_events') ORDER BY cid`)
	if err != nil {
		t.Fatalf("read activity columns: %v", err)
	}
	defer rows.Close()
	var spec string
	for rows.Next() {
		var name, typ string
		var notNull, pk int
		if err := rows.Scan(&name, &typ, &notNull, &pk); err != nil {
			t.Fatalf("scan activity column: %v", err)
		}
		spec += fmt.Sprintf("%s:%s:%d:%d;", name, typ, notNull, pk)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("activity column rows: %v", err)
	}
	if spec == "" {
		t.Fatal("activity_events has no columns")
	}
	return spec
}

// activityObjectSpec renders the names of every index and trigger attached to
// activity_events. Columns matching is not enough: an index that only one of
// the two paths creates is a query plan that only one of them gets, and a
// trigger only one of them creates is a table that is append-only on one
// machine and not on the other.
func activityObjectSpec(t *testing.T, st *Ledger) string {
	t.Helper()
	rows, err := st.db.Query(
		`SELECT type, name FROM sqlite_master
		 WHERE tbl_name='activity_events' AND type IN ('index','trigger')
		   AND name NOT LIKE 'sqlite_%'
		 ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read activity objects: %v", err)
	}
	defer rows.Close()
	var spec string
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatalf("scan activity object: %v", err)
		}
		spec += typ + ":" + name + ";"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("activity object rows: %v", err)
	}
	if spec == "" {
		t.Fatal("activity_events has no indexes or triggers")
	}
	return spec
}
