package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// ---------------------------------------------------------------------------
// Timezone discipline for this file.
//
// The rollup buckets by UTC 15-minute bucket and folds to local wall clock on
// read, in SQL. SQLite's 'localtime' follows the SYSTEM zone, which a running
// test process cannot move (it is fixed when the process starts), so no test
// here computes a bucket key in Go: both sides of every comparison come out of
// a store query, exactly as TestScrubCompositionBracketsLocalDay does in
// internal/tui.
//
// The equivalence fixtures place events on exact UTC hour starts. That is not
// convenience: an event and its bucket then carry the same instant, so they
// fold to the same local bucket under any offset.
// TestRollupFoldsSubHourEventsLikeTheLedger covers the off-boundary case, and
// TestRollupFoldsSubHourEventsInKolkata runs the same comparison at +05:30 -
// the offset an hourly bucket key could not fold - by re-executing this test
// binary with TZ set, since that is the only point at which the zone can still
// be chosen.
// ---------------------------------------------------------------------------

// utcOffsetSeconds asks SQLite (not Go) for the system zone's offset at an
// instant, so the answer is the one the bucket expressions will use.
func utcOffsetSeconds(t *testing.T, st *Ledger, at time.Time) int64 {
	t.Helper()
	var local int64
	err := st.db.QueryRow(
		`SELECT CAST(strftime('%s', ?, 'unixepoch', 'localtime') AS INTEGER)`, at.UTC().Unix(),
	).Scan(&local)
	if err != nil {
		t.Fatalf("read local offset: %v", err)
	}
	return local - at.UTC().Unix()
}

// rollupEvent builds one ledger event. cost < 0 leaves it unpriced.
func rollupEvent(key, tool, mdl, project string, at time.Time, cost int64) model.UsageEvent {
	provider := model.ProviderGoogle
	switch tool {
	case model.ToolClaudeCode:
		provider = model.ProviderAnthropic
	case model.ToolCodex:
		provider = model.ProviderOpenAI
	}
	e := model.UsageEvent{
		Tool:                tool,
		Model:               mdl,
		Project:             project,
		SessionID:           "sess-" + tool,
		Provider:            provider,
		ServiceTier:         "standard",
		EventTime:           at,
		InputTokens:         11,
		OutputTokens:        7,
		CacheCreationTokens: 5,
		CacheReadTokens:     3,
		ReasoningTokens:     2,
		TotalTokens:         26,
		DedupKey:            key,
		Kind:                model.KindUsage,
	}
	if cost >= 0 {
		e.SetCost(cost, "test-table")
	}
	return e
}

// seedLedger fills a store with events spread over hours, days, weeks and
// months, across three tools / models / projects, half of them unpriced. Every
// event sits on an exact UTC hour start (see the file header).
func seedLedger(t *testing.T, st *Ledger) {
	t.Helper()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	tools := []string{model.ToolClaudeCode, model.ToolCodex, model.ToolOpenCode}
	models := []string{"claude-sonnet-4-6", "gpt-5", "grok-9"}
	projects := []string{"/w/alpha", "/w/beta", "/w/gamma"}

	var evs []model.UsageEvent
	for i := 0; i < 240; i++ {
		// Hours 0..23 of 10 days spread over three months, so day, week and
		// month buckets all have more than one member.
		at := base.Add(time.Duration(i%24)*time.Hour).
			AddDate(0, i%3, (i/24)%10)
		cost := int64(-1)
		if i%2 == 0 {
			cost = int64(100 + i)
		}
		e := rollupEvent(
			"seed-"+strconv.Itoa(i), tools[i%3], models[(i/2)%3], projects[(i/5)%3], at, cost)
		if cost >= 0 && i%4 == 0 {
			e.SetCost(cost, "goose-provider_reported")
		}
		evs = append(evs, e)
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
}

// assertRollupMatchesLedger runs the same filter against both tables and
// compares bucket for bucket. Both sides are produced by store queries, so the
// local fold is SQLite's on both sides and a mismatch means the rollup really
// disagrees with the ledger rather than that Go and SQLite disagree.
func assertRollupMatchesLedger(t *testing.T, st *Ledger, f Filter) {
	t.Helper()
	ctx := context.Background()

	want, err := st.summarizeLedger(ctx, f)
	if err != nil {
		t.Fatalf("summarize ledger %v: %v", f.GroupBy, err)
	}
	got, err := st.SummarizeRollup(ctx, f)
	if err != nil {
		t.Fatalf("summarize rollup %v: %v", f.GroupBy, err)
	}

	label := fmt.Sprintf("group=%v tools=%v models=%v projects=%v sessions=%v since=%v until=%v",
		f.GroupBy, f.Tools, f.Models, f.Projects, f.Sessions, f.Since, f.Until)

	if len(got.Buckets) != len(want.Buckets) {
		t.Fatalf("%s: rollup returned %d buckets, ledger %d", label, len(got.Buckets), len(want.Buckets))
	}
	for i := range want.Buckets {
		w, g := want.Buckets[i], got.Buckets[i]
		for _, dim := range f.GroupBy {
			if w.Keys[dim] != g.Keys[dim] {
				t.Fatalf("%s: bucket %d key %q = %q (rollup) vs %q (ledger)",
					label, i, dim, g.Keys[dim], w.Keys[dim])
			}
		}
		assertBucketMeasures(t, label+fmt.Sprintf(" bucket %d %v", i, w.Keys), w, g)
	}
	assertBucketMeasures(t, label+" totals", want.Totals, got.Totals)
}

// assertBucketMeasures compares every public summary measure. Schema 8 keeps
// the session and price-provenance dimensions needed to derive the two
// non-token measures instead of returning plausible-looking zeros.
func assertBucketMeasures(t *testing.T, label string, want, got Bucket) {
	t.Helper()
	cmp := []struct {
		name       string
		want, got_ int64
	}{
		{"events", want.Events, got.Events},
		{"sessions", want.Sessions, got.Sessions},
		{"input", want.Input, got.Input},
		{"output", want.Output, got.Output},
		{"cache_creation", want.CacheCreation, got.CacheCreation},
		{"cache_read", want.CacheRead, got.CacheRead},
		{"reasoning", want.Reasoning, got.Reasoning},
		{"total", want.Total, got.Total},
		{"cost_micro_usd", want.CostMicroUSD, got.CostMicroUSD},
		{"unpriced_events", want.UnpricedEvents, got.UnpricedEvents},
		{"computed_cost_events", want.ComputedCostEvents, got.ComputedCostEvents},
	}
	for _, c := range cmp {
		if c.want != c.got_ {
			t.Errorf("%s: %s = %d (rollup) want %d (ledger)", label, c.name, c.got_, c.want)
		}
	}
}

// TestRollupMatchesLedger is the equivalence contract: for every granularity
// and filter the rollup claims to serve, its numbers are the ledger's numbers.
// It runs on the rollup the inserts maintained incrementally - no rebuild - so
// it also pins that the in-transaction delta is complete.
func TestRollupMatchesLedger(t *testing.T) {
	st := openTemp(t)
	seedLedger(t, st)

	// Bounds on exact UTC hours so the outward snap is a no-op and the two
	// tables see the same range.
	since := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)

	filters := []Filter{
		{},
		{GroupBy: []string{"hour"}},
		{GroupBy: []string{"day"}},
		{GroupBy: []string{"week"}},
		{GroupBy: []string{"month"}},
		{GroupBy: []string{"tool"}},
		{GroupBy: []string{"model"}},
		{GroupBy: []string{"provider"}},
		{GroupBy: []string{"project"}},
		{GroupBy: []string{"session"}},
		{GroupBy: []string{"day", "tool"}},
		{GroupBy: []string{"month", "tool", "model"}},
		{Since: since, Until: until, GroupBy: []string{"day"}},
		{Since: mid, GroupBy: []string{"day", "model"}},
		{Until: mid, GroupBy: []string{"week"}},
		{Tools: []string{model.ToolCodex}, GroupBy: []string{"day"}},
		{Models: []string{"gpt-5", "grok-9"}, GroupBy: []string{"month", "model"}},
		{Projects: []string{"/w/alpha"}, GroupBy: []string{"day", "tool"}},
		{Sessions: []string{"sess-" + model.ToolCodex}, GroupBy: []string{"provider", "session"}},
		{Since: since, Until: mid, Tools: []string{model.ToolClaudeCode},
			Projects: []string{"/w/beta"}, GroupBy: []string{"day", "model"}},
		{Tools: []string{"no-such-tool"}, GroupBy: []string{"day"}},
	}
	for _, f := range filters {
		assertRollupMatchesLedger(t, st, f)
	}
}

// TestUnpricedGroupsRollupMatchesLedger covers the other cost query that now
// uses the rollup. Provider, service tier and every token component are inputs
// to display-time pricing, so equality of only the event count would still
// permit a wrong dollar result.
func TestUnpricedGroupsRollupMatchesLedger(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

	events := []model.UsageEvent{
		rollupEvent("unpriced-a", model.ToolCodex, "gpt-5", "/w/alpha", at, -1),
		rollupEvent("unpriced-b", model.ToolCodex, "gpt-5", "/w/alpha", at.Add(time.Minute), -1),
		rollupEvent("unpriced-c", model.ToolClaudeCode, "claude-sonnet-4-6", "/w/beta", at.Add(15*time.Minute), -1),
		rollupEvent("priced", model.ToolCodex, "gpt-5", "/w/alpha", at.Add(2*time.Minute), 99),
	}
	events[1].ServiceTier = "priority"
	events[2].SessionID = "another-session"
	if _, err := st.InsertEvents(ctx, events); err != nil {
		t.Fatalf("insert: %v", err)
	}

	filters := []Filter{
		{},
		{GroupBy: []string{"tool"}},
		{GroupBy: []string{"provider", "session"}},
		{Sessions: []string{"sess-" + model.ToolCodex}, GroupBy: []string{"model", "project"}},
		{Since: at, Until: at.Add(30 * time.Minute), GroupBy: []string{"provider"}},
	}
	for _, f := range filters {
		want, err := st.unpricedGroupsLedger(ctx, f)
		if err != nil {
			t.Fatalf("ledger groups %v: %v", f, err)
		}
		got, err := st.UnpricedGroups(ctx, f)
		if err != nil {
			t.Fatalf("rollup groups %v: %v", f, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("unpriced groups %v differ:\nrollup: %#v\nledger: %#v", f, got, want)
		}
	}
}

// subHourFoldBase is the instant the off-boundary fixtures start from.
var subHourFoldBase = time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)

// assertSubHourFoldMatchesLedger seeds events that deliberately miss every
// bucket boundary - jittered by minutes and seconds across two days - and
// compares every time granularity the rollup serves against the ledger. It is
// the case a coarser bucket key gets wrong: an event at 00:20 UTC belongs to a
// different local hour than one at 00:50 in a zone offset by half an hour.
func assertSubHourFoldMatchesLedger(t *testing.T, st *Ledger) {
	t.Helper()
	var evs []model.UsageEvent
	for i := 0; i < 48; i++ {
		at := subHourFoldBase.Add(time.Duration(i)*time.Hour +
			time.Duration(7+i%50)*time.Minute + time.Duration(i*13%60)*time.Second)
		evs = append(evs, rollupEvent("jitter-"+strconv.Itoa(i), model.ToolCodex, "gpt-5", "/w/alpha", at, int64(i)))
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, dim := range []string{"hour", "day", "week", "month"} {
		assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{dim}})
	}
}

// TestRollupFoldsSubHourEventsLikeTheLedger covers events that do NOT sit on a
// bucket boundary, in whatever zone the test process was started in. It skips
// only for a zone whose offset is not a whole number of 15-minute buckets,
// which no jurisdiction has used since the 1940s.
func TestRollupFoldsSubHourEventsLikeTheLedger(t *testing.T) {
	st := openTemp(t)
	if off := utcOffsetSeconds(t, st, subHourFoldBase); off%rollupBucketSeconds != 0 {
		t.Skipf("system zone is %ds from UTC, which is not a whole number of %ds buckets", off, rollupBucketSeconds)
	}
	assertSubHourFoldMatchesLedger(t, st)
}

// kolkataOffsetSeconds is +05:30, the offset this machine runs at and the one
// that made the bucket width 15 minutes instead of an hour.
const kolkataOffsetSeconds = 19800

// tzChildEnv marks the child process the zone-specific test re-executes itself
// as. Its value is never read; only its presence distinguishes the two roles.
const tzChildEnv = "AIUSAGE_TEST_TZ_CHILD"

// TestRollupFoldsSubHourEventsInKolkata pins the fold at +05:30 explicitly,
// rather than leaving it to whatever zone the machine running the suite happens
// to use. An hourly bucket key fails here and nowhere else in CI: 00:00-01:00
// UTC is 05:30-06:30 local, so half of it belongs to the previous local hour
// (and, at the start of a day, to the previous local DAY).
//
// The zone can only be chosen before the process starts - SQLite's 'localtime'
// reads it once - so the test re-executes this binary with TZ set and reports
// the child's verdict as its own.
func TestRollupFoldsSubHourEventsInKolkata(t *testing.T) {
	if os.Getenv(tzChildEnv) == "" {
		runTestInZone(t, t.Name(), "Asia/Kolkata")
		return
	}

	st := openTemp(t)
	if off := utcOffsetSeconds(t, st, subHourFoldBase); off != kolkataOffsetSeconds {
		// This test must prove the SQL offset before testing bucket equality.
		t.Fatalf("TZ=Asia/Kolkata resolved to offset %ds, not %ds; this machine has no zone database",
			off, kolkataOffsetSeconds)
	}
	assertSubHourFoldMatchesLedger(t, st)
}

// runTestInZone re-runs one test of this binary in another timezone and fails
// if the child does. A skip cannot satisfy this required timezone proof.
func runTestInZone(t *testing.T, name, zone string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(), "TZ="+zone, tzChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s under TZ=%s: %v\n%s", name, zone, err, out)
	}
	if strings.Contains(string(out), "--- SKIP") {
		t.Fatalf("%s skipped itself under TZ=%s:\n%s", name, zone, out)
	}
	if !strings.Contains(string(out), "--- PASS") {
		t.Fatalf("%s under TZ=%s did not report a pass:\n%s", name, zone, out)
	}
}

// TestRollupCountsOnlyInsertedRows pins the incremental maintenance against the
// two ways a batch does not become a row: a dedup collision and a rejected
// row. Either one folded into the rollup would inflate it permanently, and the
// ledger would have no matching row to catch it.
func TestRollupCountsOnlyInsertedRows(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 2, 2, 9, 0, 0, 0, time.UTC)

	batch := []model.UsageEvent{
		rollupEvent("dup-1", model.ToolCodex, "gpt-5", "/w/alpha", at, 500),
		rollupEvent("dup-2", model.ToolCodex, "gpt-5", "/w/alpha", at, -1),
	}
	if n, err := st.InsertEvents(ctx, batch); err != nil || n != 2 {
		t.Fatalf("first insert n=%d err=%v want 2,nil", n, err)
	}
	// Same batch again: every key collides, so nothing may reach the rollup.
	if n, err := st.InsertEvents(ctx, batch); err != nil || n != 0 {
		t.Fatalf("second insert n=%d err=%v want 0,nil", n, err)
	}

	// A CHECK violation is skipped by the row loop; the rest of the batch
	// commits and only the committed row may be counted.
	poison := rollupEvent("poison-1", model.ToolCodex, "gpt-5", "/w/alpha", at, -1)
	poison.InputTokens = -1
	good := rollupEvent("good-1", model.ToolCodex, "gpt-5", "/w/alpha", at, 250)
	if n, err := st.InsertEvents(ctx, []model.UsageEvent{poison, good}); n != 1 || err == nil {
		t.Fatalf("poison batch n=%d err=%v want 1,non-nil", n, err)
	}

	assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{"hour", "tool"}})
	if stale, err := st.RollupStale(ctx); err != nil || stale {
		t.Fatalf("rollup stale=%v err=%v after incremental maintenance; want false,nil", stale, err)
	}
}

// TestRollupMaintainedByApplyPaths covers the other two write entry points -
// ApplyEvents (event-level sources with a checkpoint) and ApplySnapshot
// (aggregate deltas plus accumulator state) - since both must carry the rollup
// delta in the SAME transaction as the events.
func TestRollupMaintainedByApplyPaths(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 2, 3, 15, 0, 0, 0, time.UTC)

	cp := &model.SourceCheckpoint{Tool: model.ToolCodex, SourcePath: "/src/a", Size: 10}
	if _, err := st.ApplyEvents(ctx, []model.UsageEvent{
		rollupEvent("ae-1", model.ToolCodex, "gpt-5", "/w/alpha", at, 40),
		rollupEvent("ae-2", model.ToolCodex, "gpt-5", "/w/beta", at.Add(time.Hour), -1),
	}, cp); err != nil {
		t.Fatalf("apply events: %v", err)
	}

	state := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "sess-1", Model: "claude-sonnet-4-6",
		Project: "/w/gamma", ObservedTime: at, TotalTokens: 26,
	}
	if _, err := st.ApplySnapshot(ctx, []model.UsageEvent{
		rollupEvent("as-1", model.ToolHermes, "claude-sonnet-4-6", "/w/gamma", at, 90),
	}, state, nil); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}

	assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{"hour", "tool", "project"}})
}

// TestRollupDeltaRollsBackWithItsEvents proves the two are one transaction: a
// snapshot that fails after the insert must leave neither the event nor the
// rollup delta behind. Without that, the rollup would count usage the ledger
// has no record of - and no rebuild would be triggered, since the watermark
// only ever moves with committed rows.
func TestRollupDeltaRollsBackWithItsEvents(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 2, 4, 1, 0, 0, 0, time.UTC)

	applySnapshotFault = func() error { return fmt.Errorf("simulated crash") }
	defer func() { applySnapshotFault = nil }()

	state := model.AggregateSnapshot{Tool: model.ToolHermes, Key: "sess-x", ObservedTime: at}
	if _, err := st.ApplySnapshot(ctx, []model.UsageEvent{
		rollupEvent("fault-1", model.ToolHermes, "m", "/w/alpha", at, 10),
	}, state, nil); err == nil {
		t.Fatalf("expected the injected fault to fail the snapshot")
	}

	var rows int64
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM usage_rollup`).Scan(&rows); err != nil {
		t.Fatalf("count rollup: %v", err)
	}
	if rows != 0 {
		t.Fatalf("rollup kept %d row(s) from a rolled-back transaction", rows)
	}
	assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{"hour"}})
}

// TestRebuildRollupRestoresTheLedgersNumbers takes a deliberately corrupted
// rollup (rows dropped, a measure inflated) and proves the rebuild is the
// repair: the ledger is never touched, the rollup is replaced wholesale.
func TestRebuildRollupRestoresTheLedgersNumbers(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	seedLedger(t, st)

	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM usage_rollup WHERE tool=?`, model.ToolCodex); err != nil {
		t.Fatalf("corrupt rollup (delete): %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE usage_rollup SET total_tokens = total_tokens + 1000`); err != nil {
		t.Fatalf("corrupt rollup (inflate): %v", err)
	}

	if err := st.RebuildRollup(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	for _, dim := range []string{"hour", "day", "month", "tool", "model", "project"} {
		assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{dim}})
	}
	if stale, err := st.RollupStale(ctx); err != nil || stale {
		t.Fatalf("post-rebuild stale=%v err=%v want false,nil", stale, err)
	}
}

// TestEnsureRollupDetectsBothKindsOfDrift covers the two checks separately: a
// watermark that lags the ledger, and a watermark that agrees while the rollup
// has lost rows. Only the second needs the event count, which is precisely why
// the count check exists.
func TestEnsureRollupDetectsBothKindsOfDrift(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	seedLedger(t, st)

	if rebuilt, err := st.EnsureRollup(ctx); err != nil || rebuilt {
		t.Fatalf("in-step rollup: rebuilt=%v err=%v want false,nil", rebuilt, err)
	}

	// (1) watermark behind: the state a v4 migration leaves.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE schema_meta SET value='0' WHERE key=?`, rollupWatermarkKey); err != nil {
		t.Fatalf("rewind watermark: %v", err)
	}
	if rebuilt, err := st.EnsureRollup(ctx); err != nil || !rebuilt {
		t.Fatalf("behind watermark: rebuilt=%v err=%v want true,nil", rebuilt, err)
	}
	assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{"day"}})

	// (2) watermark current, rollup short: a rollup filled from a partial
	// ledger passes the watermark check forever.
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM usage_rollup WHERE tool=?`, model.ToolOpenCode); err != nil {
		t.Fatalf("drop rollup rows: %v", err)
	}
	if rebuilt, err := st.EnsureRollup(ctx); err != nil || !rebuilt {
		t.Fatalf("short rollup: rebuilt=%v err=%v want true,nil", rebuilt, err)
	}
	assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{"day", "tool"}})
}

// TestMigrateV3ToV4CreatesEmptyRollup pins the migration contract: the step
// creates the table and deliberately does NOT fill it (a backfill would hold
// the write lock for a full rebuild on every upgrade), leaving the collector's
// EnsureRollup to notice and repair it.
func TestMigrateV3ToV4CreatesEmptyRollup(t *testing.T) {
	ctx := context.Background()
	path := legacyDB(t, 3)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v3 db: %v", err)
	}
	defer st.Close()

	if v, err := readSchemaVersion(ctx, st.db); err != nil || v != SchemaVersion {
		t.Fatalf("post-migration version=%d err=%v want %d,nil", v, err, SchemaVersion)
	}
	ok, err := tableExists(ctx, st.db, "usage_rollup")
	if err != nil || !ok {
		t.Fatalf("usage_rollup exists=%v err=%v want true,nil", ok, err)
	}
	var rows int64
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_rollup`).Scan(&rows); err != nil {
		t.Fatalf("count rollup: %v", err)
	}
	if rows != 0 {
		t.Fatalf("migration backfilled %d rollup row(s); it must leave the table empty", rows)
	}
	if mark, err := rollupWatermark(ctx, st.db); err != nil || mark != -1 {
		t.Fatalf("post-migration watermark=%d err=%v want -1,nil (unrecorded)", mark, err)
	}

	// The ledger still holds its legacy row, so the rollup is behind and the
	// next collection pass repairs it.
	if rebuilt, err := st.EnsureRollup(ctx); err != nil || !rebuilt {
		t.Fatalf("post-migration EnsureRollup rebuilt=%v err=%v want true,nil", rebuilt, err)
	}
	assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{"day", "tool"}})
}

// TestRollupTableMatchesFreshSchema guards the one thing two DDL copies always
// eventually do: drift. The v4 migration's table and schema.sql's must produce
// the same columns and key, or a migrated database and a fresh one would carry
// different tables under the same version stamp.
func TestRollupTableMatchesFreshSchema(t *testing.T) {
	fresh := openTemp(t)
	migratedPath := legacyDB(t, 3)
	migrated, err := Open(migratedPath)
	if err != nil {
		t.Fatalf("migrate v3 db: %v", err)
	}
	defer migrated.Close()

	if a, b := rollupColumnSpec(t, fresh), rollupColumnSpec(t, migrated); a != b {
		t.Fatalf("rollup table drifted between schema.sql and the v4 migration:\nfresh:    %s\nmigrated: %s", a, b)
	}
}

// rollupColumnSpec renders the rollup table's column names, types, NOT NULL
// flags and primary-key positions as one comparable string.
func rollupColumnSpec(t *testing.T, st *Ledger) string {
	t.Helper()
	rows, err := st.db.Query(
		`SELECT name, type, "notnull", pk FROM pragma_table_info('usage_rollup') ORDER BY cid`)
	if err != nil {
		t.Fatalf("read rollup columns: %v", err)
	}
	defer rows.Close()
	var spec string
	for rows.Next() {
		var name, typ string
		var notNull, pk int
		if err := rows.Scan(&name, &typ, &notNull, &pk); err != nil {
			t.Fatalf("scan rollup column: %v", err)
		}
		spec += fmt.Sprintf("%s:%s:%d:%d;", name, typ, notNull, pk)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rollup column rows: %v", err)
	}
	if spec == "" {
		t.Fatalf("usage_rollup has no columns")
	}
	return spec
}

// TestRollupServesEverySummaryDimension pins schema 8's deeper key. Session and
// provider used to be refused because the old rollup could not derive them;
// both now have to match the authoritative ledger, while unknown dimensions
// remain caller errors.
func TestRollupServesEverySummaryDimension(t *testing.T) {
	st := openTemp(t)
	seedLedger(t, st)
	for _, dim := range []string{"session", "provider"} {
		assertRollupMatchesLedger(t, st, Filter{GroupBy: []string{dim}})
	}
	assertRollupMatchesLedger(t, st, Filter{
		Sessions: []string{"sess-" + model.ToolCodex}, GroupBy: []string{"provider", "session"},
	})
	ctx := context.Background()
	if _, err := st.SummarizeRollup(ctx, Filter{GroupBy: []string{"nonsense"}}); err == nil {
		t.Errorf("SummarizeRollup accepted an invalid dimension")
	}
}

// TestRollupRangeSnapsOutward pins the bound contract: a request that starts
// mid-bucket is answered from the whole bucket that contains it, and the
// summary reports the range it actually covered rather than the one it was
// handed. The second half pins the RESOLUTION of that snap: a request inside a
// later quarter of the same hour must not drag in the hour's earlier events,
// which is exactly what an hour-wide bucket key would do.
func TestRollupRangeSnapsOutward(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 5, 6, 7, 0, 0, 0, time.UTC)
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{
		rollupEvent("snap-1", model.ToolCodex, "gpt-5", "/w/alpha", at, 10),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sum, err := st.SummarizeRollup(ctx, Filter{
		Since: at.Add(5 * time.Minute),
		Until: at.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("summarize rollup: %v", err)
	}
	if !sum.Since.Equal(at) || !sum.Until.Equal(at.Add(15*time.Minute)) {
		t.Fatalf("snapped range = [%s,%s) want [%s,%s)",
			sum.Since, sum.Until, at, at.Add(15*time.Minute))
	}
	if sum.Totals.Events != 1 {
		t.Fatalf("snapped range events = %d, want 1 (the bucket containing the request)", sum.Totals.Events)
	}

	later, err := st.SummarizeRollup(ctx, Filter{
		Since: at.Add(20 * time.Minute),
		Until: at.Add(40 * time.Minute),
	})
	if err != nil {
		t.Fatalf("summarize rollup (later quarters): %v", err)
	}
	if !later.Since.Equal(at.Add(15*time.Minute)) || !later.Until.Equal(at.Add(45*time.Minute)) {
		t.Fatalf("snapped range = [%s,%s) want [%s,%s)",
			later.Since, later.Until, at.Add(15*time.Minute), at.Add(45*time.Minute))
	}
	if later.Totals.Events != 0 {
		t.Fatalf("later-quarter range events = %d, want 0; the snap must be %ds wide, not an hour",
			later.Totals.Events, rollupBucketSeconds)
	}
}

// TestBucketStartUnixFloors covers the arithmetic the Go-side delta and the
// SQL-side rebuild must agree on, including before the epoch where truncating
// division would put a timestamp in the following bucket.
func TestBucketStartUnixFloors(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, 0},
		{1, 0},
		{899, 0},
		{900, 900},
		{3599, 2700},
		{3600, 3600},
		{3661, 3600},
		{-1, -900},
		{-900, -900},
		{-901, -1800},
	}
	for _, c := range cases {
		if got := bucketStartUnix(c.in); got != c.want {
			t.Errorf("bucketStartUnix(%d)=%d want %d", c.in, got, c.want)
		}
	}
}

// TestRollupSQLFloorMatchesGo proves the two implementations of the bucket
// floor agree on the same inputs - the rebuild and the incremental delta must
// bucket an event identically or a rebuild would silently move rows.
func TestRollupSQLFloorMatchesGo(t *testing.T) {
	st := openTemp(t)
	for _, sec := range []int64{0, 1, 899, 900, 3599, 3600, 3661, 1_760_000_000, -1, -901, -3601} {
		var got int64
		err := st.db.QueryRow(
			`SELECT (:t - ((:t % 900) + 900) % 900)`, sql.Named("t", sec)).Scan(&got)
		if err != nil {
			t.Fatalf("sql floor(%d): %v", sec, err)
		}
		if want := bucketStartUnix(sec); got != want {
			t.Errorf("sql floor(%d)=%d want %d (Go)", sec, got, want)
		}
	}
}

// TestRebuildIsIdempotent: running the rebuild twice must not change anything,
// which is the property that makes it a safe repair to run at any time.
func TestRebuildIsIdempotent(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	seedLedger(t, st)

	if err := st.RebuildRollup(ctx); err != nil {
		t.Fatalf("rebuild 1: %v", err)
	}
	first := rollupDump(t, st)
	if err := st.RebuildRollup(ctx); err != nil {
		t.Fatalf("rebuild 2: %v", err)
	}
	if second := rollupDump(t, st); second != first {
		t.Fatalf("rebuild is not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// rollupDump renders the whole rollup as one comparable string.
func rollupDump(t *testing.T, st *Ledger) string {
	t.Helper()
	rows, err := st.db.Query(`
		SELECT bucket_start_unix, tool, model, project, session_id, provider,
			service_tier, price_class, input_tokens, output_tokens,
			cache_creation_tokens, cache_read_tokens, reasoning_tokens, total_tokens,
			events, cost_micro_usd, unpriced_events
		FROM usage_rollup ORDER BY bucket_start_unix, tool, model, project,
			session_id, provider, service_tier, price_class`)
	if err != nil {
		t.Fatalf("dump rollup: %v", err)
	}
	defer rows.Close()
	var out string
	for rows.Next() {
		var (
			bucket                              int64
			tool, mdl, project                  string
			session, provider, tier, priceClass string
			in, outTok, cc, cr, reason, total   int64
			events, cost, unpriced              int64
		)
		if err := rows.Scan(&bucket, &tool, &mdl, &project, &session, &provider, &tier, &priceClass,
			&in, &outTok, &cc, &cr,
			&reason, &total, &events, &cost, &unpriced); err != nil {
			t.Fatalf("scan rollup row: %v", err)
		}
		out += fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s|%s|%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
			bucket, tool, mdl, project, session, provider, tier, priceClass,
			in, outTok, cc, cr, reason, total, events, cost, unpriced)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rollup dump rows: %v", err)
	}
	return out
}
