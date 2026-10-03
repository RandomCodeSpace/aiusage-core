package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func seedActivityCounts(t *testing.T, st *Ledger) ObservationBatch {
	t.Helper()
	at := time.Unix(1_750_000_000, 0)
	batch := ObservationBatch{}
	for i, n := range []int{5, 2} {
		key := fmt.Sprintf("count-u%d", i)
		e := ev(key, model.ToolClaudeCode, at, int64(n*200))
		e.SetCost(int64(n*200), "test")
		batch.Events = append(batch.Events, e)
		batch.TurnContexts = append(batch.TurnContexts, turnCtx(key, model.DimensionSkill, "reader", at))
		for j := 0; j < n; j++ {
			name := "Read"
			if j%2 == 1 {
				name = "Edit"
			}
			batch.Activity = append(batch.Activity, act(fmt.Sprintf("count-a%d-%d", i, j), name, model.ActivityTool, at, key, j, n))
		}
	}
	batch.Activity = append(batch.Activity, act("unjoined", "Hook", model.ActivityHook, at, "", 0, 1))
	if _, err := st.ApplyBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	return batch
}

// The oracle derives every divisor directly from history, without consulting
// either the production detector or its chosen counts relation.
func directActivityBuckets(t *testing.T, st *Reader, f ActivityFilter, order ActivityOrder) []ActivityBucket {
	t.Helper()
	groups, err := activityGroupExprs(f)
	if err != nil {
		t.Fatal(err)
	}
	where, args := buildActivityWhere(f)
	q := "SELECT "
	if len(groups) > 0 {
		q += strings.Join(groups, ",") + ","
	}
	q += activitySelectSQL + ` FROM activity_events a
 LEFT JOIN usage_events u ON u.dedup_key=a.usage_dedup_key
 LEFT JOIN (SELECT usage_dedup_key,COUNT(*) AS activity_count FROM activity_events
 WHERE usage_dedup_key<>'' GROUP BY usage_dedup_key) c ON c.usage_dedup_key=a.usage_dedup_key` + where
	if len(groups) > 0 {
		joined := strings.Join(groups, ",")
		q += " GROUP BY " + joined + " ORDER BY "
		if order != "" {
			expr, err := order.orderExpr()
			if err != nil {
				t.Fatal(err)
			}
			q += expr + " DESC, "
		}
		q += joined
	}
	result, err := st.queryActivityBuckets(context.Background(), q, args, f.GroupBy)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertActivityCountsAnswers(t *testing.T, st *Reader) {
	t.Helper()
	ctx := context.Background()
	filters := []ActivityFilter{
		{}, {GroupBy: []string{"name"}},
		{Names: []string{"Read"}, GroupBy: []string{"name", "kind", "tool"}},
		{Sessions: []string{"s"}, Models: []string{"m"}, GroupBy: []string{"session"}},
		{Since: time.Unix(1_749_999_999, 0), Until: time.Unix(1_750_000_001, 0), Projects: []string{"/p"}, GroupBy: []string{"hour"}},
	}
	for _, f := range filters {
		sum, err := st.SummarizeActivity(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		want := directActivityBuckets(t, st, f, "")
		if !reflect.DeepEqual(sum.Buckets, want) {
			t.Fatalf("filter=%+v got=%+v direct=%+v", f, sum.Buckets, want)
		}
		if sum.Totals.AttributedTotal > 1400 || sum.Totals.AttributedCostMicroUSD > 1400 {
			t.Fatalf("inflated attribution=%+v", sum.Totals)
		}
		if len(f.GroupBy) == 0 && (sum.Totals.AttributedTotal != 1400 || sum.Totals.AttributedCostMicroUSD != 1400 || sum.Totals.UnattributedCalls != 1) {
			t.Fatalf("grand total=%+v", sum.Totals)
		}
		if len(f.GroupBy) > 0 {
			for _, order := range []ActivityOrder{ActivityByCalls, ActivityByCost, ActivityByTokens} {
				got, err := st.TopActivity(ctx, f, order, 0)
				if err != nil {
					t.Fatal(err)
				}
				if want := directActivityBuckets(t, st, f, order); !reflect.DeepEqual(got, want) {
					t.Fatalf("top filter=%+v order=%s got=%+v direct=%+v", f, order, got, want)
				}
			}
		}
	}
}

func TestActivityCountsDetectRepairAndReadFallback(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"empty", `DELETE FROM activity_usage_counts`},
		{"missing key", `DELETE FROM activity_usage_counts WHERE usage_dedup_key='count-u0'`},
		{"reduced", `UPDATE activity_usage_counts SET activity_count=1 WHERE usage_dedup_key='count-u0'`},
		{"increased", `UPDATE activity_usage_counts SET activity_count=8 WHERE usage_dedup_key='count-u0'`},
		{"extra key", `INSERT INTO activity_usage_counts VALUES('orphan',3)`},
		{"missing and extra same cardinality", `DELETE FROM activity_usage_counts WHERE usage_dedup_key='count-u0'; INSERT INTO activity_usage_counts VALUES('orphan',5)`},
		{"equal total wrong groups", `UPDATE activity_usage_counts SET activity_count=CASE usage_dedup_key WHEN 'count-u0' THEN 2 ELSE 5 END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openTemp(t)
			batch := seedActivityCounts(t, st)
			beforeEvents, err := st.ListEvents(ctx, Filter{})
			if err != nil {
				t.Fatal(err)
			}
			beforeActivity, err := st.ListActivity(ctx, ActivityFilter{})
			if err != nil {
				t.Fatal(err)
			}
			assertActivityCountsAnswers(t, st.Reader)
			execFailureSQL(t, st, tc.sql)
			if stale, err := st.activityUsageCountsStale(ctx); err != nil || !stale {
				t.Fatalf("stale=%v err=%v", stale, err)
			}
			path := st.path
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotFile(t, path)
			ro, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			assertActivityCountsAnswers(t, ro)
			if err := ro.Close(); err != nil {
				t.Fatal(err)
			}
			verified, err := Verify(ctx, path)
			if err != nil || verified.State != VerificationRepairable || !verified.RollupChecked || !verified.RollupStale || !strings.Contains(verified.Reason, "activity usage counts") {
				t.Fatalf("verification=%+v err=%v", verified, err)
			}
			if after := snapshotFile(t, path); before != after {
				t.Fatalf("read/verify mutated database: before=%+v after=%+v", before, after)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if rebuilt, err := st.EnsureRollup(ctx); err != nil || !rebuilt {
				t.Fatalf("rebuild=%v err=%v", rebuilt, err)
			}
			if rebuilt, err := st.EnsureRollup(ctx); err != nil || rebuilt {
				t.Fatalf("repeat rebuild=%v err=%v", rebuilt, err)
			}
			if applied, err := st.ApplyBatch(ctx, batch); err != nil || applied != (Applied{}) {
				t.Fatalf("dedup replay=%+v err=%v", applied, err)
			}
			afterEvents, err := st.ListEvents(ctx, Filter{})
			if err != nil {
				t.Fatal(err)
			}
			afterActivity, err := st.ListActivity(ctx, ActivityFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeEvents, afterEvents) || !reflect.DeepEqual(beforeActivity, afterActivity) {
				t.Fatal("rebuild changed authoritative history")
			}
			assertActivityCountsAnswers(t, st.Reader)
			if result, err := Verify(ctx, path); err != nil || result.State != VerificationOK || result.SchemaVersion != SchemaVersion {
				t.Fatalf("repaired verification=%+v err=%v", result, err)
			}
		})
	}
}

func TestActivityCountsEmptyAndFailedRebuild(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	if stale, err := st.activityUsageCountsStale(ctx); err != nil || stale {
		t.Fatalf("empty stale=%v err=%v", stale, err)
	}
	if sum, err := st.SummarizeActivity(ctx, ActivityFilter{}); err != nil || len(sum.Buckets) != 1 || sum.Totals.Calls != 0 {
		t.Fatalf("empty summary=%+v err=%v", sum, err)
	}
	execFailureSQL(t, st, `INSERT INTO activity_usage_counts VALUES('orphan',1)`)
	if stale, err := st.activityUsageCountsStale(ctx); err != nil || !stale {
		t.Fatalf("empty ledger with orphan stale=%v err=%v", stale, err)
	}
	if sum, err := st.SummarizeActivity(ctx, ActivityFilter{}); err != nil || len(sum.Buckets) != 1 || sum.Totals.Calls != 0 {
		t.Fatalf("empty fallback summary=%+v err=%v", sum, err)
	}
	if rebuilt, err := st.EnsureRollup(ctx); err != nil || !rebuilt {
		t.Fatalf("empty ledger orphan rebuild=%v err=%v", rebuilt, err)
	}
	seedActivityCounts(t, st)
	execFailureSQL(t, st, `UPDATE activity_usage_counts SET activity_count=1 WHERE usage_dedup_key='count-u0'`)
	execFailureSQL(t, st, `CREATE TRIGGER fail_rebuild BEFORE INSERT ON activity_usage_counts BEGIN SELECT RAISE(ABORT,'injected rebuild failure'); END`)
	if rebuilt, err := st.EnsureRollup(ctx); err == nil || rebuilt || !strings.Contains(err.Error(), "injected rebuild failure") {
		t.Fatalf("rebuild=%v err=%v", rebuilt, err)
	}
	var n, total int
	if err := st.db.QueryRow(`SELECT COUNT(*),SUM(activity_count) FROM activity_usage_counts`).Scan(&n, &total); err != nil || n != 2 || total != 3 {
		t.Fatalf("failed rebuild changed counts n=%d total=%d err=%v", n, total, err)
	}
	assertActivityCountsAnswers(t, st.Reader)
	execFailureSQL(t, st, `DROP TRIGGER fail_rebuild`)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.rebuildActivityUsageCounts(canceled); err == nil {
		t.Fatal("canceled rebuild succeeded")
	}
	if rebuilt, err := st.EnsureRollup(ctx); err != nil || !rebuilt {
		t.Fatalf("retry rebuild=%v err=%v", rebuilt, err)
	}
	assertActivityCountsAnswers(t, st.Reader)
}

func TestActivityDivisorIncludesCallsOutsideFilter(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Unix(1_750_000_000, 0)
	e := ev("shared-window", model.ToolClaudeCode, at, 300)
	e.SetCost(300, "test")
	acts := []model.ActivityEvent{
		act("before-window", "Edit", model.ActivityTool, at.Add(-time.Hour), e.DedupKey, 0, 3),
		act("in-window", "Read", model.ActivityTool, at, e.DedupKey, 1, 3),
		act("after-window", "Read", model.ActivityTool, at.Add(time.Hour), e.DedupKey, 2, 3),
	}
	if _, err := st.ApplyObservation(ctx, []model.UsageEvent{e}, acts, nil); err != nil {
		t.Fatal(err)
	}
	// Even a plausible count matching the selected row count is untrusted.
	execFailureSQL(t, st, `UPDATE activity_usage_counts SET activity_count=1`)
	filter := ActivityFilter{Since: at, Until: at.Add(time.Minute), Names: []string{"Read"}}
	sum, err := st.SummarizeActivity(ctx, filter)
	if err != nil || sum.Totals.Calls != 1 || sum.Totals.AttributedTotal != 100 || sum.Totals.AttributedCostMicroUSD != 100 {
		t.Fatalf("filtered share=%+v err=%v", sum, err)
	}
	filter.GroupBy = []string{"name"}
	rows, err := st.TopActivity(ctx, filter, ActivityByCost, 10)
	if err != nil || len(rows) != 1 || rows[0].AttributedTotal != 100 || rows[0].AttributedCostMicroUSD != 100 {
		t.Fatalf("ranked filtered share=%+v err=%v", rows, err)
	}
}
