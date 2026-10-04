package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// This file provides a driver-level query counter: a wrapper around the
// modernc sqlite driver that counts every SQL statement database/sql executes.
// Because the wrapper hides the optional fast-path interfaces (QueryerContext,
// ExecerContext, ...), database/sql routes everything through Prepare + Stmt,
// where Exec/Query are counted exactly once per statement execution. It pins
// the store's query-count contracts (single-pass Summarize totals, batched
// SourceStats) so an N+1 regression fails a test instead of shipping.

// stmtCount counts statement executions across all counting connections. Tests
// in this package run sequentially, so before/after diffs are race-free.
var stmtCount atomic.Int64

type countingDriver struct{ inner driver.Driver }

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: c}, nil
}

type countingConn struct{ driver.Conn }

// preparedSQL records the text of every statement prepared through the counting
// driver. Statement counts pin how MANY queries run; this pins WHAT they ask
// for, which is how the event projection can be checked to leave the raw
// column in the database rather than merely to discard it after transfer.
var preparedSQL struct {
	mu sync.Mutex
	q  []string
}

func (c countingConn) Prepare(q string) (driver.Stmt, error) {
	preparedSQL.mu.Lock()
	preparedSQL.q = append(preparedSQL.q, q)
	preparedSQL.mu.Unlock()
	s, err := c.Conn.Prepare(q)
	if err != nil {
		return nil, err
	}
	return countingStmt{Stmt: s, query: q}, nil
}

// queriesDuring returns the SQL prepared while fn executed.
func queriesDuring(fn func()) []string {
	preparedSQL.mu.Lock()
	before := len(preparedSQL.q)
	preparedSQL.mu.Unlock()

	fn()

	preparedSQL.mu.Lock()
	defer preparedSQL.mu.Unlock()
	return append([]string{}, preparedSQL.q[before:]...)
}

// countingStmt counts executions via the context interfaces, which
// database/sql prefers over the embedded legacy Exec/Query whenever they are
// present — so every statement execution passes through exactly one counter.
type countingStmt struct {
	driver.Stmt
	query string
}

// Tests are sequential. This hook lets a writer commit between reader queries.
var beforeCountedQuery func(string)

func (s countingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	stmtCount.Add(1)
	ec, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("querycount_test: inner driver stmt lacks StmtExecContext")
	}
	return ec.ExecContext(ctx, args)
}

func (s countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if beforeCountedQuery != nil {
		beforeCountedQuery(s.query)
	}
	stmtCount.Add(1)
	qc, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("querycount_test: inner driver stmt lacks StmtQueryContext")
	}
	return qc.QueryContext(ctx, args)
}

func init() {
	// sql.Open never connects, so this only resolves the registered driver.
	probe, err := sql.Open("sqlite", "probe")
	if err != nil {
		panic(fmt.Sprintf("resolve sqlite driver: %v", err))
	}
	inner := probe.Driver()
	probe.Close()
	sql.Register("sqlite-counting", countingDriver{inner: inner})
}

// openCounting opens a fresh store whose statements are counted via the
// wrapped driver, mirroring Open's DSN pragmas and schema setup.
func openCounting(t *testing.T) *Ledger {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	dsn := sqliteFileURI(path, false)
	db, err := sql.Open("sqlite-counting", dsn)
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	if err := ensureSchema(context.Background(), db, path); err != nil {
		db.Close()
		t.Fatalf("ensure schema: %v", err)
	}
	st := &Ledger{Reader: &Reader{db: db, path: path}}
	t.Cleanup(func() { st.Close() })
	return st
}

// statementsDuring returns how many SQL statements ran while fn executed.
func statementsDuring(fn func()) int64 {
	before := stmtCount.Load()
	fn()
	return stmtCount.Load() - before
}

// seedTools inserts events across three tools with models and sessions so the
// read paths under test have multiple groups to aggregate.
func seedTools(t *testing.T, st *Ledger) {
	t.Helper()
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	var evs []model.UsageEvent
	for i, tool := range []string{"tool-a", "tool-b", "tool-c"} {
		for j := 0; j < 3; j++ {
			e := ev(fmt.Sprintf("%s|%d", tool, j), tool, at.Add(time.Duration(i*3+j)*time.Minute), 100)
			e.Model = fmt.Sprintf("model-%d", j%2)
			e.SessionID = fmt.Sprintf("%s-s%d", tool, j)
			evs = append(evs, e)
		}
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestSummarizeQueryCount pins the single-pass Summarize contract at the SQL
// level: an ungrouped summarize is exactly ONE statement (the result row IS the
// grand total — re-running the aggregate for totals would be a second), and a
// grouped summarize is exactly TWO (the single grouped pass + the narrow
// distinct-session count, which cannot be summed across buckets).
func TestSummarizeQueryCount(t *testing.T) {
	st := openCounting(t)
	seedTools(t, st)
	ctx := context.Background()

	n := statementsDuring(func() {
		s, err := st.Summarize(ctx, Filter{})
		if err != nil {
			t.Fatalf("Summarize ungrouped: %v", err)
		}
		if s.Totals.Events != 9 {
			t.Fatalf("ungrouped totals events = %d, want 9", s.Totals.Events)
		}
	})
	if n != 1 {
		t.Errorf("ungrouped Summarize ran %d statements, want exactly 1 (single pass)", n)
	}

	n = statementsDuring(func() {
		s, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
		if err != nil {
			t.Fatalf("Summarize grouped: %v", err)
		}
		if len(s.Buckets) != 3 || s.Totals.Events != 9 {
			t.Fatalf("grouped buckets=%d totals=%d, want 3/9", len(s.Buckets), s.Totals.Events)
		}
	})
	if n != 2 {
		t.Errorf("grouped Summarize ran %d statements, want exactly 2 (grouped pass + distinct sessions)", n)
	}
}

func TestSummaryUsesOneSnapshotDuringCollection(t *testing.T) {
	for _, mode := range []string{"ledger", "rollup", "explicit-rollup"} {
		t.Run(mode, func(t *testing.T) {
			reader := openCounting(t)
			writer, err := Open(reader.path)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			ctx := context.Background()
			at := time.Date(2026, 10, 4, 12, 0, 1, 0, time.UTC)
			first := ev("first", "codex", at, 100)
			first.SessionID = "first-session"
			if _, err := writer.InsertEvents(ctx, []model.UsageEvent{first}); err != nil {
				t.Fatal(err)
			}
			injected := false
			beforeCountedQuery = func(query string) {
				if injected || !strings.HasPrefix(strings.TrimSpace(query), "SELECT COUNT(DISTINCT") {
					return
				}
				injected = true
				second := ev("second", "codex", at.Add(time.Second), 200)
				second.SessionID = "second-session"
				if _, err := writer.InsertEvents(ctx, []model.UsageEvent{second}); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { beforeCountedQuery = nil })
			filter := Filter{GroupBy: []string{"tool"}}
			if mode == "ledger" {
				filter.Since = at // Unaligned lower bound forces the ledger path.
			}
			var total Bucket
			if mode == "explicit-rollup" {
				summary, err := reader.SummarizeRollup(ctx, filter)
				if err != nil {
					t.Fatal(err)
				}
				total = summary.Totals
			} else {
				summary, err := reader.Summarize(ctx, filter)
				if err != nil {
					t.Fatal(err)
				}
				total = summary.Totals
			}
			if !injected {
				t.Fatal("did not exercise a commit between summary queries")
			}
			if total.Events != 1 || total.Sessions != 1 || total.Total != 100 {
				t.Fatalf("summary mixed snapshots: %+v", total)
			}
		})
	}
}

// TestSummaryAccelerationRouting proves the fast path is conditional rather
// than a semantic change: exact 15-minute bounds read the current rollup,
// arbitrary-second starts read the authoritative ledger, and a stale
// watermark causes a rollup probe followed by the ledger fallback. The same
// routing applies to the unpriced groups used for display-time costing.
func TestSummaryAccelerationRouting(t *testing.T) {
	st := openCounting(t)
	seedTools(t, st)
	ctx := context.Background()
	aligned := Filter{
		Since: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 8, 1, 12, 15, 0, 0, time.UTC),
	}
	arbitrary := aligned
	arbitrary.Since = arbitrary.Since.Add(time.Second)

	assertRoute := func(name string, queries []string, wantRollup []bool) {
		t.Helper()
		if len(queries) != len(wantRollup) {
			t.Fatalf("%s prepared %d statements, want %d:\n%s", name, len(queries), len(wantRollup), strings.Join(queries, "\n---\n"))
		}
		for i, want := range wantRollup {
			got := strings.Contains(queries[i], "FROM usage_rollup")
			if got != want {
				t.Errorf("%s statement %d rollup=%v want %v:\n%s", name, i, got, want, queries[i])
			}
		}
	}

	q := queriesDuring(func() {
		if _, err := st.Summarize(ctx, aligned); err != nil {
			t.Fatalf("aligned Summarize: %v", err)
		}
	})
	assertRoute("aligned summary", q, []bool{true})

	q = queriesDuring(func() {
		if _, err := st.Summarize(ctx, arbitrary); err != nil {
			t.Fatalf("arbitrary Summarize: %v", err)
		}
	})
	assertRoute("arbitrary summary", q, []bool{false})

	q = queriesDuring(func() {
		if _, err := st.UnpricedGroups(ctx, aligned); err != nil {
			t.Fatalf("aligned UnpricedGroups: %v", err)
		}
	})
	assertRoute("aligned unpriced", q, []bool{true})

	q = queriesDuring(func() {
		if _, err := st.UnpricedGroups(ctx, arbitrary); err != nil {
			t.Fatalf("arbitrary UnpricedGroups: %v", err)
		}
	})
	assertRoute("arbitrary unpriced", q, []bool{false})

	if _, err := st.db.ExecContext(ctx,
		`UPDATE schema_meta SET value='0' WHERE key=?`, rollupWatermarkKey); err != nil {
		t.Fatalf("stale watermark: %v", err)
	}
	q = queriesDuring(func() {
		if _, err := st.Summarize(ctx, aligned); err != nil {
			t.Fatalf("stale Summarize: %v", err)
		}
	})
	assertRoute("stale summary", q, []bool{true, false})

	q = queriesDuring(func() {
		if _, err := st.UnpricedGroups(ctx, aligned); err != nil {
			t.Fatalf("stale UnpricedGroups: %v", err)
		}
	})
	// A stale rollup yields no grouped rows, so UnpricedGroups resolves that
	// ambiguous empty result with one scalar watermark read before falling back.
	assertRoute("stale unpriced", q, []bool{true, false, false})
}

// TestActivityAttributionUsesSetBasedCounts keeps the hot attribution query
// set-based and authoritative. A correlated COUNT would rescan per call;
// trusting the persisted derived count table could inflate a damaged ledger.
func TestActivityAttributionUsesSetBasedCounts(t *testing.T) {
	st := openCounting(t)
	ctx := context.Background()
	at := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	e := ev("usage-counted", model.ToolClaudeCode, at, 900)
	acts := []model.ActivityEvent{
		act("activity-counted-a", "Read", model.ActivityTool, at, e.DedupKey, 0, 2),
		act("activity-counted-b", "Edit", model.ActivityTool, at, e.DedupKey, 1, 2),
	}
	if _, err := st.ApplyObservation(ctx, []model.UsageEvent{e}, acts, nil); err != nil {
		t.Fatalf("seed activity: %v", err)
	}

	queries := queriesDuring(func() {
		sum, err := st.SummarizeActivity(ctx, ActivityFilter{})
		if err != nil {
			t.Fatalf("SummarizeActivity: %v", err)
		}
		if sum.Totals.AttributedTotal != 900 {
			t.Fatalf("attributed total = %d, want 900", sum.Totals.AttributedTotal)
		}
	})
	if len(queries) != 1 {
		t.Fatalf("SummarizeActivity prepared %d statements, want 1:\n%s", len(queries), strings.Join(queries, "\n---\n"))
	}
	q := strings.ToLower(queries[0])
	if !strings.Contains(q, "join expected_activity_counts") || strings.Contains(q, "activity_usage_counts") {
		t.Fatalf("activity query does not use authoritative grouped counts:\n%s", queries[0])
	}
	if strings.Contains(q, "select count(*) from activity_events") {
		t.Fatalf("activity query restored a correlated divisor scan:\n%s", queries[0])
	}
}

func TestActivityFallbackKeepsOneStatement(t *testing.T) {
	st := openCounting(t)
	seedActivityCounts(t, st)
	if _, err := st.db.Exec(`DELETE FROM activity_usage_counts`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, top := range []bool{false, true} {
		queries := queriesDuring(func() {
			if top {
				rows, err := st.TopActivity(ctx, ActivityFilter{GroupBy: []string{"name"}}, ActivityByCost, 10)
				if err != nil || len(rows) != 3 {
					t.Fatalf("TopActivity rows=%d err=%v", len(rows), err)
				}
			} else {
				sum, err := st.SummarizeActivity(ctx, ActivityFilter{})
				if err != nil || sum.Totals.AttributedCostMicroUSD != 1400 {
					t.Fatalf("SummarizeActivity=%+v err=%v", sum, err)
				}
			}
		})
		if len(queries) != 1 {
			t.Fatalf("top=%v prepared %d statements, want 1", top, len(queries))
		}
	}
}

// TestSourceStatsQueryCount pins the batched SourceStats contract: exactly TWO
// statements regardless of tool count (per-tool aggregate + one distinct-models
// query), never the old 1+N per-tool round trips.
func TestSourceStatsQueryCount(t *testing.T) {
	st := openCounting(t)
	seedTools(t, st)

	n := statementsDuring(func() {
		stats, err := st.SourceStats(context.Background())
		if err != nil {
			t.Fatalf("SourceStats: %v", err)
		}
		if len(stats) != 3 {
			t.Fatalf("stats rows = %d, want 3", len(stats))
		}
		for _, s := range stats {
			if len(s.Models) != 2 {
				t.Errorf("tool %s models = %v, want 2 entries", s.Tool, s.Models)
			}
		}
	})
	if n != 2 {
		t.Errorf("SourceStats ran %d statements for 3 tools, want exactly 2 (batched, no N+1)", n)
	}
}
