package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// openTemp opens a fresh SQLite store in a temp dir.
func openTemp(t *testing.T) *Ledger {
	t.Helper()
	db := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func ev(dedup, tool string, et time.Time, total int64) model.UsageEvent {
	return model.UsageEvent{
		Tool:        tool,
		Model:       "m",
		SessionID:   "s",
		EventTime:   et,
		TotalTokens: total,
		InputTokens: total,
		DedupKey:    dedup,
		Kind:        model.KindUsage,
	}
}

// TestInsertOrIgnoreIdempotent verifies re-inserting the same dedup key is a
// no-op and never errors (the append-only ON CONFLICT DO NOTHING contract).
func TestInsertOrIgnoreIdempotent(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	e := ev("k1", model.ToolCodex, time.Now(), 100)

	n, err := st.InsertEvents(ctx, []model.UsageEvent{e})
	if err != nil || n != 1 {
		t.Fatalf("first insert n=%d err=%v want 1,nil", n, err)
	}
	n, err = st.InsertEvents(ctx, []model.UsageEvent{e})
	if err != nil || n != 0 {
		t.Fatalf("second insert n=%d err=%v want 0,nil (idempotent)", n, err)
	}
}

// TestEmptyDedupKeyRejected ensures events without a dedup key are rejected.
func TestEmptyDedupKeyRejected(t *testing.T) {
	st := openTemp(t)
	e := ev("", model.ToolCodex, time.Now(), 1)
	if _, err := st.InsertEvents(context.Background(), []model.UsageEvent{e}); err == nil {
		t.Fatalf("expected error for empty dedup key")
	}
}

// TestImmutabilityUpdateForbidden proves the no-UPDATE trigger fires on the
// historical table, the core append-only guarantee at the storage layer.
func TestImmutabilityUpdateForbidden(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("k1", model.ToolCodex, time.Now(), 100)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := st.db.ExecContext(ctx, `UPDATE usage_events SET total_tokens = 0 WHERE dedup_key='k1'`)
	if err == nil {
		t.Fatalf("UPDATE on usage_events should be forbidden")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only abort, got %v", err)
	}
}

// TestImmutabilityDeleteForbidden proves the no-DELETE trigger fires.
func TestImmutabilityDeleteForbidden(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("k1", model.ToolCodex, time.Now(), 100)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := st.db.ExecContext(ctx, `DELETE FROM usage_events WHERE dedup_key='k1'`)
	if err == nil {
		t.Fatalf("DELETE on usage_events should be forbidden")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only abort, got %v", err)
	}
}

// TestDurabilitySurvivesReopenAndCompaction is the storage-layer twin of the
// collector invariant: 2,000,000 tokens written, then the DB is closed and
// reopened (simulating a daemon restart) and a re-poll inserts only NEW keys.
// A window total must never erode.
func TestDurabilitySurvivesReopenAndCompaction(t *testing.T) {
	db := filepath.Join(t.TempDir(), "usage.db")
	ctx := context.Background()
	start := time.Date(2026, 5, 29, 6, 0, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)

	st, err := Open(db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	full := make([]model.UsageEvent, 0, 20)
	for i := 0; i < 20; i++ {
		full = append(full, ev("cc|"+strconv.Itoa(i), model.ToolClaudeCode, start.Add(time.Duration(i)*30*time.Second), 100_000))
	}
	if _, err := st.InsertEvents(ctx, full); err != nil {
		t.Fatalf("insert full: %v", err)
	}
	if got := windowTot(t, st, start, end); got != 2_000_000 {
		t.Fatalf("after write total=%d want 2,000,000", got)
	}
	st.Close()

	// Reopen and re-poll a now-empty/compacted source: re-inserting the same
	// keys must be a no-op, and the historical total stays put.
	st2, err := Open(db)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if n, err := st2.InsertEvents(ctx, full); err != nil || n != 0 {
		t.Fatalf("re-poll inserted n=%d err=%v want 0,nil", n, err)
	}
	if got := windowTot(t, st2, start, end); got != 2_000_000 {
		t.Fatalf("after reopen+compaction total=%d want still 2,000,000", got)
	}
}

func windowTot(t *testing.T, st *Ledger, since, until time.Time) int64 {
	t.Helper()
	sum, err := st.Summarize(context.Background(), Filter{Since: since, Until: until})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	return sum.Totals.Total
}

// TestSummarizeGroupingAndTotals verifies grouped buckets carry keys and the
// grand total reflects the whole filtered set regardless of grouping.
func TestSummarizeGroupingAndTotals(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	in := []model.UsageEvent{
		ev("a", model.ToolClaudeCode, now, 100),
		ev("b", model.ToolClaudeCode, now, 200),
		ev("c", model.ToolCodex, now, 50),
	}
	if _, err := st.InsertEvents(ctx, in); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(sum.Buckets) != 2 {
		t.Fatalf("buckets=%d want 2", len(sum.Buckets))
	}
	if sum.Totals.Total != 350 {
		t.Fatalf("grand total=%d want 350", sum.Totals.Total)
	}
	byTool := map[string]int64{}
	for _, b := range sum.Buckets {
		if len(b.OrderedKeys) != 1 || b.OrderedKeys[0] != "tool" {
			t.Fatalf("bucket OrderedKeys=%v want [tool]", b.OrderedKeys)
		}
		byTool[b.Keys["tool"]] = b.Total
	}
	if byTool[model.ToolClaudeCode] != 300 || byTool[model.ToolCodex] != 50 {
		t.Fatalf("per-tool totals = %v", byTool)
	}
}

// TestSummarizeTimeBucketLocalDay checks day grouping produces a lexically
// sortable local-date key.
func TestSummarizeTimeBucketLocalDay(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	// Use local noon so the local-date bucket is unambiguous regardless of tz.
	day := time.Date(2026, 5, 29, 12, 0, 0, 0, time.Local)
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("d1", model.ToolCodex, day, 10)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"day"}})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(sum.Buckets) != 1 {
		t.Fatalf("buckets=%d want 1", len(sum.Buckets))
	}
	if got := sum.Buckets[0].Keys["day"]; got != "2026-05-29" {
		t.Fatalf("day bucket key=%q want 2026-05-29", got)
	}
}

// TestStateRoundTrip verifies LastState/UpsertState keep one row per cell.
func TestStateRoundTrip(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if v, err := st.LastState(ctx, model.ToolHermes, "k"); err != nil || v != nil {
		t.Fatalf("empty LastState=%v err=%v want nil,nil", v, err)
	}
	snap := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "k", SessionID: "k",
		InputTokens: 100, TotalTokens: 100, ObservedTime: time.Now(),
	}
	if err := st.UpsertState(ctx, snap); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	snap.TotalTokens = 250
	snap.InputTokens = 250
	if err := st.UpsertState(ctx, snap); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, err := st.LastState(ctx, model.ToolHermes, "k")
	if err != nil || got == nil {
		t.Fatalf("LastState=%v err=%v", got, err)
	}
	if got.TotalTokens != 250 {
		t.Fatalf("state total=%d want 250 (one row per cell)", got.TotalTokens)
	}
}

// TestApplySnapshotAtomicCrashWindow injects a failure between the event
// insert and the state upsert — the exact window where the old two-step write
// left a committed delta with a stale baseline, so the next poll re-derived
// and re-inserted the same delta forever. The whole transaction must roll
// back, and the recovery poll must materialise the delta exactly once.
func TestApplySnapshotAtomicCrashWindow(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 6, 0, 0, 0, time.UTC)

	snap := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "cell", SessionID: "cell",
		InputTokens: 900_000, TotalTokens: 900_000, ObservedTime: now,
	}
	delta := ev("agg|hermes|cell|1", model.ToolHermes, now, 900_000)

	applySnapshotFault = func() error { return errors.New("injected crash") }
	defer func() { applySnapshotFault = nil }()

	if _, err := st.ApplySnapshot(ctx, []model.UsageEvent{delta}, snap, nil); err == nil {
		t.Fatalf("expected the injected crash to surface")
	}

	// Neither side of the write may be visible after the rollback.
	if got := windowTot(t, st, time.Time{}, time.Time{}); got != 0 {
		t.Fatalf("crashed apply leaked %d tokens into usage_events", got)
	}
	if v, err := st.LastState(ctx, model.ToolHermes, "cell"); err != nil || v != nil {
		t.Fatalf("crashed apply leaked state: %+v err=%v", v, err)
	}

	// Recovery poll: the unchanged (nil) baseline re-derives the full delta
	// under a fresh dedup key; it must land exactly once, together with state.
	applySnapshotFault = nil
	delta2 := ev("agg|hermes|cell|2", model.ToolHermes, now.Add(time.Minute), 900_000)
	if n, err := st.ApplySnapshot(ctx, []model.UsageEvent{delta2}, snap, nil); err != nil || n != 1 {
		t.Fatalf("recovery apply n=%d err=%v want 1,nil", n, err)
	}
	if got := windowTot(t, st, time.Time{}, time.Time{}); got != 900_000 {
		t.Fatalf("total=%d want exactly 900,000 (no double count)", got)
	}
	v, err := st.LastState(ctx, model.ToolHermes, "cell")
	if err != nil || v == nil || v.TotalTokens != 900_000 {
		t.Fatalf("state after recovery = %+v err=%v want total 900,000", v, err)
	}
}

// TestApplySnapshotCollisionKeepsBaseline: when every supplied event collides
// on dedup_key (two polls at the same observed instant), the state write must
// be skipped so the next poll re-derives the delta instead of dropping it; an
// event-free apply (zero delta) must still advance the state.
func TestApplySnapshotCollisionKeepsBaseline(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 6, 0, 0, 0, time.UTC)

	first := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "cell",
		InputTokens: 100, TotalTokens: 100, ObservedTime: now,
	}
	if n, err := st.ApplySnapshot(ctx, []model.UsageEvent{ev("agg|k", model.ToolHermes, now, 100)}, first, nil); err != nil || n != 1 {
		t.Fatalf("first apply n=%d err=%v want 1,nil", n, err)
	}

	grown := first
	grown.InputTokens = 250
	grown.TotalTokens = 250
	n, err := st.ApplySnapshot(ctx, []model.UsageEvent{ev("agg|k", model.ToolHermes, now, 150)}, grown, nil)
	if err != nil || n != 0 {
		t.Fatalf("collided apply n=%d err=%v want 0,nil", n, err)
	}
	if v, _ := st.LastState(ctx, model.ToolHermes, "cell"); v == nil || v.TotalTokens != 100 {
		t.Fatalf("collided apply advanced state to %+v; baseline must stay at 100", v)
	}

	if n, err := st.ApplySnapshot(ctx, nil, grown, nil); err != nil || n != 0 {
		t.Fatalf("state-only apply n=%d err=%v want 0,nil", n, err)
	}
	if v, _ := st.LastState(ctx, model.ToolHermes, "cell"); v == nil || v.TotalTokens != 250 {
		t.Fatalf("state-only apply did not advance state: %+v", v)
	}
}

// TestStatsAndSourceStats checks the diagnostic aggregates.
func TestStatsAndSourceStats(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{
		ev("a", model.ToolClaudeCode, now, 100),
		ev("b", model.ToolCodex, now.Add(time.Hour), 50),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Events != 2 || stats.DistinctTools != 2 {
		t.Fatalf("stats events=%d tools=%d want 2,2", stats.Events, stats.DistinctTools)
	}
	if stats.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version=%d want %d", stats.SchemaVersion, SchemaVersion)
	}
	if stats.SizeBytes <= 0 {
		t.Fatalf("size bytes=%d want >0", stats.SizeBytes)
	}

	ss, err := st.SourceStats(ctx)
	if err != nil {
		t.Fatalf("source stats: %v", err)
	}
	if len(ss) != 2 {
		t.Fatalf("source stats len=%d want 2", len(ss))
	}
	// Ordered by total desc; claude-code (100) first.
	if ss[0].Tool != model.ToolClaudeCode || ss[0].Total != 100 {
		t.Fatalf("first source stat = %+v", ss[0])
	}
	if ss[0].Sessions != 1 {
		t.Fatalf("sessions=%d want 1", ss[0].Sessions)
	}
}

// TestOpenRestrictsPermissions verifies the privacy contract: a fresh data dir
// is created owner-only and the DB plus any WAL/SHM sidecars end up 0600 (the
// raw column holds transcript content).
func TestOpenRestrictsPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	db := filepath.Join(dir, "usage.db")
	st, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// A committed write forces the WAL sidecars into existence.
	e := ev("k-perm", model.ToolCodex, time.Now(), 1)
	if _, err := st.InsertEvents(context.Background(), []model.UsageEvent{e}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("data dir mode = %03o, want no group/other bits", perm)
	}

	fi, err = os.Stat(db)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("db mode = %03o, want 600", perm)
	}

	for _, side := range []string{db + "-wal", db + "-shm"} {
		fi, err := os.Stat(side)
		if err != nil {
			continue // sidecar not materialised; nothing to leak
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode = %03o, want no group/other bits", side, perm)
		}
	}
}

// TestInsertEventsSkipsPoisonRow: a row violating the CHECK constraint is
// skipped and reported while the rest of the batch commits — sources are
// re-read every cycle, so a whole-batch abort would retry the poison forever.
func TestInsertEventsSkipsPoisonRow(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	bad := ev("bad", model.ToolCodex, time.Now(), 5)
	bad.InputTokens = -5 // violates the CHECK constraint
	batch := []model.UsageEvent{
		ev("good-1", model.ToolCodex, time.Now(), 10),
		bad,
		ev("good-2", model.ToolCodex, time.Now(), 20),
	}

	n, err := st.InsertEvents(ctx, batch)
	if n != 2 {
		t.Fatalf("inserted = %d, want 2", n)
	}
	if err == nil || !strings.Contains(err.Error(), "skipped 1 of 3") {
		t.Fatalf("want skip report, got %v", err)
	}

	// The good rows are durably committed: re-inserting is a no-op.
	n, err = st.InsertEvents(ctx, []model.UsageEvent{ev("good-1", model.ToolCodex, time.Now(), 10)})
	if err != nil || n != 0 {
		t.Fatalf("re-insert good-1 n=%d err=%v want 0,nil", n, err)
	}
	// The poison row was NOT stored: a fixed row under the same key inserts.
	n, err = st.InsertEvents(ctx, []model.UsageEvent{ev("bad", model.ToolCodex, time.Now(), 5)})
	if err != nil || n != 1 {
		t.Fatalf("fixed row n=%d err=%v want 1,nil", n, err)
	}
}

// TestInsertEventsEmptyKeyRowSkippedNotBatch: the empty-dedup-key rejection
// still fires per row without dragging valid rows down with it.
func TestInsertEventsEmptyKeyRowSkippedNotBatch(t *testing.T) {
	st := openTemp(t)
	n, err := st.InsertEvents(context.Background(), []model.UsageEvent{
		ev("", model.ToolCodex, time.Now(), 1),
		ev("ok", model.ToolCodex, time.Now(), 2),
	})
	if n != 1 {
		t.Fatalf("inserted = %d, want 1", n)
	}
	if err == nil || !strings.Contains(err.Error(), "empty dedup key") {
		t.Fatalf("want empty-dedup-key report, got %v", err)
	}
}

// TestSummarizeDistinctSessionCounts: every summarize row (and the grand
// total) carries a store-level COUNT(DISTINCT session_id) so callers never
// materialize one bucket per session just to count them. Empty session ids
// are excluded, matching SourceStats.
func TestSummarizeDistinctSessionCounts(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Now()
	mk := func(dedup, tool, session string) model.UsageEvent {
		e := ev(dedup, tool, at, 10)
		e.SessionID = session
		return e
	}
	evs := []model.UsageEvent{
		mk("d1", "tool-a", "s1"),
		mk("d2", "tool-a", "s1"),
		mk("d3", "tool-a", "s2"),
		mk("d4", "tool-a", ""), // sessionless: never counted
		mk("d5", "tool-b", "s3"),
		mk("d6", "tool-b", "s1"), // s1 spans two group keys (tool-a AND tool-b)
	}
	if _, err := st.InsertEvents(ctx, evs); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	s, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	want := map[string]int64{"tool-a": 2, "tool-b": 2}
	for _, b := range s.Buckets {
		if got := b.Sessions; got != want[b.Keys["tool"]] {
			t.Errorf("tool %s sessions = %d, want %d", b.Keys["tool"], got, want[b.Keys["tool"]])
		}
	}
	// s1 appears under BOTH tools: summing per-bucket distinct counts would
	// give 4. The grand total must deduplicate across buckets.
	if s.Totals.Sessions != 3 {
		t.Errorf("grand total sessions = %d, want 3 (s1 must not double count across buckets)", s.Totals.Sessions)
	}

	// Ungrouped summarize carries the count too.
	s, err = st.Summarize(ctx, Filter{Tools: []string{"tool-a"}})
	if err != nil {
		t.Fatalf("Summarize(tool-a): %v", err)
	}
	if s.Totals.Sessions != 2 {
		t.Errorf("tool-a total sessions = %d, want 2", s.Totals.Sessions)
	}
}

// Every pooled connection must sync committed WAL transactions.
func TestOpenSetsSynchronousFull(t *testing.T) {
	st := openTemp(t)
	var v int
	if err := st.db.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&v); err != nil {
		t.Fatalf("pragma query: %v", err)
	}
	if v != 2 {
		t.Fatalf("synchronous = %d, want 2 (FULL)", v)
	}
	st.db.SetMaxIdleConns(0)
	if err := st.db.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("new connection synchronous = %d, want 2 (FULL)", v)
	}
}

// TestLastEventTimesMatchesSourceStats pins the loose-index-scan query to the
// full aggregate it replaces on read paths that only need freshness.
func TestLastEventTimesMatchesSourceStats(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	events := []model.UsageEvent{
		ev("a1", model.ToolCodex, base, 1),
		ev("a2", model.ToolCodex, base.Add(3*time.Hour), 1),
		ev("b1", model.ToolClaudeCode, base.Add(time.Hour), 1),
		ev("c1", "zeta", base.Add(-48*time.Hour), 1),
	}
	if _, err := st.InsertEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	got, err := st.LastEventTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := st.SourceStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(stats) {
		t.Fatalf("LastEventTimes has %d tools, SourceStats %d", len(got), len(stats))
	}
	for _, s := range stats {
		if !got[s.Tool].Equal(s.LastEvent) {
			t.Errorf("%s: LastEventTimes %v, SourceStats %v", s.Tool, got[s.Tool], s.LastEvent)
		}
	}
	empty := openTemp(t)
	if got, err := empty.LastEventTimes(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty ledger: %v, %v; want no tools and no error", got, err)
	}
}
