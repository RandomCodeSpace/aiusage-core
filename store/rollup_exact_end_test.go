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

func seedExactEndingEvents(t *testing.T, st *Ledger, base time.Time) {
	t.Helper()
	var events []model.UsageEvent
	for i, offset := range []time.Duration{-time.Second, 0, 14*time.Minute + 59*time.Second, 15 * time.Minute, 15*time.Minute + 41*time.Second, 15*time.Minute + 42*time.Second, 30 * time.Minute} {
		e := rollupEvent(fmt.Sprintf("ending-%d", i), model.ToolCodex, "gpt-5", "/work", base.Add(offset), int64(i))
		if i == 2 {
			e.SetCost(0, "goose-provider_reported")
		}
		if i == 3 {
			e = rollupEvent(e.DedupKey, e.Tool, e.Model, e.Project, e.EventTime, -1)
		}
		if i == 4 {
			e.SessionID = "another-session"
			e.Model = ""
			e.Provider = ""
			e.Project = ""
		}
		events = append(events, e)
	}
	if _, err := st.InsertEvents(context.Background(), events); err != nil {
		t.Fatal(err)
	}
}

func TestExactEndingSummaryMatchesLedger(t *testing.T) {
	for _, year := range []int{1969, 2026} {
		t.Run(fmt.Sprint(year), func(t *testing.T) {
			st := openTemp(t)
			base := time.Date(year, 8, 1, 12, 0, 0, 0, time.UTC)
			seedExactEndingEvents(t, st, base)
			until := base.Add(15*time.Minute + 42*time.Second)
			var filters []Filter
			for _, dims := range [][]string{nil, {"hour"}, {"day"}, {"week"}, {"month"}, {"tool"}, {"model"}, {"provider"}, {"project"}, {"session"}, {"day", "tool", "model"}, {"session", "tool", "project"}} {
				filters = append(filters, Filter{Since: base, Until: until, GroupBy: dims})
			}
			filters = append(filters,
				Filter{Until: until},
				Filter{Since: base, Until: base.Add(time.Second)},
				Filter{Since: base, Until: base.Add(15*time.Minute + time.Nanosecond)},
				Filter{Since: base.Add(30 * time.Minute), Until: until},
				Filter{Since: base, Until: until, Tools: []string{model.ToolCodex}, Models: []string{"gpt-5"}, Providers: []string{model.ProviderOpenAI}, Projects: []string{"/work"}, Sessions: []string{"sess-" + model.ToolCodex}, GroupBy: []string{"model"}},
				Filter{Since: base, Until: until, Models: []string{""}, Providers: []string{""}, Projects: []string{""}, GroupBy: []string{"session"}},
				Filter{Since: base, Until: until, Tools: []string{"missing"}},
				Filter{Since: base, Until: until, Tools: []string{"missing"}, GroupBy: []string{"tool"}},
			)
			for _, f := range filters {
				want, err := st.summarizeLedger(context.Background(), f)
				if err != nil {
					t.Fatal(err)
				}
				got, err := st.Summarize(context.Background(), f)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("filter %+v: exact ending result differs\ngot %#v\nwant %#v", f, got, want)
				}
			}
		})
	}
}

func TestExactEndingSummaryQueriesAndStaleFallback(t *testing.T) {
	st := openCounting(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedExactEndingEvents(t, st, base)
	f := Filter{Since: base, Until: base.Add(15*time.Minute + 42*time.Second)}
	for _, grouped := range []bool{false, true} {
		if grouped {
			f.GroupBy = []string{"model"}
		}
		queries := queriesDuring(func() {
			sum, err := st.Summarize(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			if sum.Totals.Events != 4 || sum.Totals.Sessions != 2 {
				t.Fatalf("exact upper bound or distinct sessions changed: %+v", sum.Totals)
			}
		})
		want := 1
		if grouped {
			want = 2
		}
		if len(queries) != want {
			t.Fatalf("grouped=%v issued %d queries, want %d", grouped, len(queries), want)
		}
		for _, q := range queries {
			if !strings.Contains(q, "UNION ALL") {
				t.Fatalf("exact ending query bypassed acceleration: %s", q)
			}
		}
	}
	if _, err := st.db.Exec("UPDATE schema_meta SET value='0' WHERE key=?", rollupWatermarkKey); err != nil {
		t.Fatal(err)
	}
	queries := queriesDuring(func() {
		sum, err := st.Summarize(context.Background(), f)
		if err != nil || sum.Totals.Events != 4 || sum.Totals.Sessions != 2 {
			t.Fatalf("stale fallback: sum=%+v err=%v", sum, err)
		}
	})
	if len(queries) != 3 || !strings.Contains(queries[0], "UNION ALL") || strings.Contains(queries[1], "usage_rollup") {
		t.Fatalf("stale rollup did not fall back to ledger: %v", queries)
	}
}

func TestExactEndingSummaryUsesIndexedTail(t *testing.T) {
	st := openTemp(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := Filter{Since: base.AddDate(0, 0, -30), Until: base.Add(42 * time.Second)}
	from, args := exactEndingRollupSource(f)
	rows, err := st.db.Query("EXPLAIN QUERY PLAN SELECT SUM(events) FROM "+from, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "SEARCH usage_events USING INDEX idx_events_event_time (event_time_unix>? AND event_time_unix<?)") {
		t.Fatalf("partial bucket must use a bounded indexed scan: %v", plan)
	}
}
