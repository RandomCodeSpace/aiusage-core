package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// providerRef is the fixed event time the provider tests file their events at.
var providerRef = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// seedProviders inserts events across two named providers plus rows whose
// source never named one, so a provider grouping has both cases to bucket.
func seedProviders(t *testing.T, st *Ledger) {
	t.Helper()
	mk := func(key, provider string, total int64) model.UsageEvent {
		e := ev(key, model.ToolOpenCode, providerRef, total)
		e.Provider = provider
		return e
	}
	evs := []model.UsageEvent{
		mk("p1", model.ProviderAnthropic, 100),
		mk("p2", model.ProviderAnthropic, 200),
		mk("p3", model.ProviderOpenAI, 50),
		mk("p4", "", 7),
		mk("p5", "", 3),
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("seed providers: %v", err)
	}
}

// TestSummarizeGroupsByProvider is the issue #38 regression: provider was
// stored on every event and priced against, but groupExpr rejected it, so
// "what am I spending per provider" could not be asked at all. The rows whose
// provider is unknown must form their own bucket keyed by the STORED empty
// string: labelling it is the display layer's job, and a store that answered
// "unknown" here would be indistinguishable from a provider named that.
func TestSummarizeGroupsByProvider(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	seedProviders(t, st)

	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"provider"}})
	if err != nil {
		t.Fatalf("summarize by provider: %v", err)
	}
	if len(sum.Buckets) != 3 {
		t.Fatalf("buckets = %d (%+v), want 3 (anthropic, openai, unknown)", len(sum.Buckets), sum.Buckets)
	}

	got := map[string]int64{}
	for _, b := range sum.Buckets {
		if len(b.OrderedKeys) != 1 || b.OrderedKeys[0] != "provider" {
			t.Fatalf("bucket OrderedKeys = %v, want [provider]", b.OrderedKeys)
		}
		got[b.Keys["provider"]] = b.Total
	}
	want := map[string]int64{model.ProviderAnthropic: 300, model.ProviderOpenAI: 50, "": 10}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("provider %q total = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("provider buckets = %v, want exactly %v", got, want)
	}
	if sum.Totals.Total != 360 {
		t.Errorf("grand total = %d, want 360", sum.Totals.Total)
	}
}

// TestSummarizeProviderQueryCount pins the new dimension to the same SQL cost
// as every other one: a grouped pass plus the narrow distinct-session count,
// and nothing else. A provider grouping that reached for a second aggregate
// (or a per-bucket lookup) would fail here rather than ship.
func TestSummarizeProviderQueryCount(t *testing.T) {
	st := openCounting(t)
	seedProviders(t, st)
	ctx := context.Background()

	n := statementsDuring(func() {
		s, err := st.Summarize(ctx, Filter{GroupBy: []string{"provider"}})
		if err != nil {
			t.Fatalf("Summarize by provider: %v", err)
		}
		if len(s.Buckets) != 3 || s.Totals.Events != 5 {
			t.Fatalf("buckets=%d totals=%d, want 3/5", len(s.Buckets), s.Totals.Events)
		}
	})
	if n != 2 {
		t.Errorf("provider Summarize ran %d statements, want exactly 2 (grouped pass + distinct sessions)", n)
	}
}

func TestProviderFilterUsageScope(t *testing.T) {
	st := openTemp(t)
	seedProviders(t, st)
	for _, tc := range []struct {
		name          string
		providers     []string
		events, total int64
	}{
		{"all", nil, 5, 360},
		{"named", []string{model.ProviderAnthropic}, 2, 300},
		{"unknown", []string{""}, 2, 10},
		{"multiple", []string{model.ProviderOpenAI, ""}, 3, 60},
		{"missing", []string{"missing"}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, group := range []string{"model", "day", "session", "provider"} {
				f := Filter{Providers: tc.providers, Tools: []string{model.ToolOpenCode}, Models: []string{"m"}, Sessions: []string{"s"}, Since: providerRef, Until: providerRef.Add(time.Hour), GroupBy: []string{group}}
				sum, err := st.Summarize(t.Context(), f)
				if err != nil {
					t.Fatal(err)
				}
				if sum.Totals.Total != tc.total || sum.Totals.Events != tc.events {
					t.Fatalf("group %s totals = %+v, want %d events / %d tokens", group, sum.Totals, tc.events, tc.total)
				}
				assertRollupMatchesLedger(t, st, f)
				rollupPrices, err := st.UnpricedGroups(t.Context(), f)
				if err != nil {
					t.Fatal(err)
				}
				ledgerPrices, err := st.unpricedGroupsLedger(t.Context(), f)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(rollupPrices, ledgerPrices) {
					t.Fatalf("price groups differ: rollup=%+v ledger=%+v", rollupPrices, ledgerPrices)
				}
				var unpriced int64
				for _, g := range ledgerPrices {
					unpriced += g.Events
				}
				if unpriced != tc.events {
					t.Fatalf("unpriced = %d, want %d", unpriced, tc.events)
				}
				// Arbitrary second boundaries force the authoritative ledger path.
				f.Since = providerRef.Add(-time.Second)
				sum, err = st.Summarize(t.Context(), f)
				if err != nil {
					t.Fatal(err)
				}
				if sum.Totals.Total != tc.total || sum.Totals.Events != tc.events {
					t.Fatalf("ledger totals = %+v", sum.Totals)
				}
				events, err := st.ListEvents(t.Context(), f)
				if err != nil {
					t.Fatal(err)
				}
				if int64(len(events)) != tc.events {
					t.Fatalf("listed %d events, want %d", len(events), tc.events)
				}
			}
		})
	}
}

func TestProviderFilterActivityAndTurnContext(t *testing.T) {
	st := openTemp(t)
	var events []model.UsageEvent
	var activity []model.ActivityEvent
	var contexts []model.TurnContext
	for i, provider := range []string{model.ProviderAnthropic, model.ProviderOpenAI, ""} {
		key := []string{"anthropic", "openai", "unknown"}[i]
		e := ev(key, model.ToolClaudeCode, providerRef, int64((i+1)*100))
		e.Provider, e.Project = provider, "/p"
		e.SetCost(e.TotalTokens*10, "test")
		events = append(events, e)
		activity = append(activity, act("call-"+key, "Bash", model.ActivityTool, providerRef, key, 0, 1))
		contexts = append(contexts, turnCtx(key, model.DimensionSkill, "work", providerRef))
	}
	// The extra Anthropic call is outside the name filter and must still divide
	// the selected call's attributed cost. Unlinked calls and contexts must not
	// be mistaken for linked usage with a recorded empty provider.
	activity = append(activity,
		act("second", "Read", model.ActivityTool, providerRef, "anthropic", 0, 1),
		act("unlinked", "Bash", model.ActivityTool, providerRef, "", 0, 1),
		act("dangling", "Bash", model.ActivityTool, providerRef, "absent", 0, 1))
	contexts = append(contexts, turnCtx("absent", model.DimensionSkill, "work", providerRef))
	if _, err := st.ApplyBatch(t.Context(), ObservationBatch{Events: events, Activity: activity, TurnContexts: contexts}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                                      string
		providers                                 []string
		calls, attributed, turns, total, unjoined int64
	}{
		{"all", nil, 5, 550, 4, 600, 2},
		{"named", []string{model.ProviderAnthropic}, 1, 50, 1, 100, 0},
		{"unknown", []string{""}, 1, 300, 1, 300, 0},
		{"multiple", []string{model.ProviderAnthropic, ""}, 2, 350, 2, 400, 0},
		{"missing", []string{"missing"}, 0, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ActivityFilter{Providers: tc.providers, Tools: []string{model.ToolClaudeCode}, Models: []string{"m"}, Projects: []string{"/p"}, Sessions: []string{"s"}, Since: providerRef, Until: providerRef.Add(time.Hour), Names: []string{"Bash"}, GroupBy: []string{"name"}}
			sum, err := st.SummarizeActivity(t.Context(), f)
			if err != nil {
				t.Fatal(err)
			}
			if sum.Totals.Calls != tc.calls || sum.Totals.AttributedTotal != tc.attributed || sum.Totals.AttributedCostMicroUSD != tc.attributed*10 || sum.Totals.UnattributedCalls != tc.unjoined {
				t.Fatalf("activity totals = %+v", sum.Totals)
			}
			wantSessions := int64(0)
			if tc.calls > 0 {
				wantSessions = 1
			}
			if sum.Totals.Sessions != wantSessions {
				t.Fatalf("activity sessions = %d", sum.Totals.Sessions)
			}
			top, err := st.TopActivity(t.Context(), f, ActivityByCost, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(top, sum.Buckets) {
				t.Fatalf("activity ranking differs: %+v / %+v", top, sum.Buckets)
			}
			listed, err := st.ListActivity(t.Context(), f)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(listed)) != tc.calls {
				t.Fatalf("listed %d calls, want %d", len(listed), tc.calls)
			}
			f.Names, f.GroupBy = nil, []string{"value"}
			turns, err := st.SummarizeTurnContext(t.Context(), model.DimensionSkill, f)
			if err != nil {
				t.Fatal(err)
			}
			if turns.Totals.Turns != tc.turns || turns.Totals.TotalTokens != tc.total || turns.Totals.CostMicroUSD != tc.total*10 || turns.Totals.Sessions != wantSessions {
				t.Fatalf("turn totals = %+v", turns.Totals)
			}
			turnTop, err := st.TopTurnContext(t.Context(), model.DimensionSkill, f, ActivityByCost, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(turnTop, turns.Buckets) {
				t.Fatalf("turn ranking differs: %+v / %+v", turnTop, turns.Buckets)
			}
		})
	}
}
