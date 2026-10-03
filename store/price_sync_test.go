package store

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func syncTestPrice(e model.UsageEvent) (int64, string, bool) {
	return e.TotalTokens * 10, "test-prices", true
}

func priceSyncState(t *testing.T, st *Ledger) (string, string) {
	t.Helper()
	var revision, cursor string
	err := st.db.QueryRow(`SELECT
		COALESCE((SELECT value FROM schema_meta WHERE key=?),''),
		COALESCE((SELECT value FROM schema_meta WHERE key=?),'')`, priceSyncRevisionKey, priceSyncCursorKey).Scan(&revision, &cursor)
	if err != nil {
		t.Fatal(err)
	}
	return revision, cursor
}

func assertPriceSyncGuard(t *testing.T, st *Ledger) {
	t.Helper()
	for _, statement := range []string{
		`UPDATE usage_events SET input_tokens=input_tokens+1`,
		`UPDATE usage_events SET cost_micro_usd=999, price_source='changed'`,
		`DELETE FROM usage_events`,
	} {
		if _, err := st.db.Exec(statement); err == nil {
			t.Fatalf("historical mutation was allowed: %s", statement)
		}
	}
}

func TestSyncUnpricedPreservesHistoryAndMovesRollup(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Unix(1_750_000_000, 0)
	events := []model.UsageEvent{
		ev("fill", model.ToolClaudeCode, at, 100),
		ev("later", model.ToolClaudeCode, at, 200),
		ev("vendor", model.ToolClaudeCode, at, 300),
		ev("computed", model.ToolClaudeCode, at, 400),
		ev("legacy-zero", model.ToolClaudeCode, at, 500),
	}
	for i := range events {
		events[i].Project = "/p"
		events[i].Provider = "anthropic"
		events[i].ServiceTier = "batch"
		events[i].RequestID = fmt.Sprintf("r-%d", i)
		events[i].MessageID = fmt.Sprintf("m-%d", i)
		events[i].Raw = `{"audit_only":true}`
	}
	events[2].SetCost(20, "copilot-nano-aiu")
	events[3].SetCost(30, "old-prices")
	events[4].SetCost(0, "old-zero")
	batch := ObservationBatch{Events: events,
		Activity: []model.ActivityEvent{
			act("a1", "Read", model.ActivityTool, at, "fill", 0, 2),
			act("a2", "Edit", model.ActivityTool, at, "fill", 1, 2),
		},
		TurnContexts: []model.TurnContext{turnCtx("fill", model.DimensionSkill, "sample", at)},
		Checkpoint:   &model.SourceCheckpoint{Tool: model.ToolClaudeCode, SourcePath: "/source", Offset: 123},
	}
	if _, err := st.ApplyBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	before, err := st.ListEvents(ctx, Filter{}, WithRaw())
	if err != nil {
		t.Fatal(err)
	}
	price := func(e model.UsageEvent) (int64, string, bool) {
		if e.Raw != "" || e.CacheTTL != (model.CacheWriteTTL{}) {
			t.Fatal("price sync consumed non-persisted accounting or raw")
		}
		if e.DedupKey == "later" {
			return 0, "", false
		}
		return syncTestPrice(e)
	}
	if n, err := st.SyncUnpriced(ctx, "r1", price); err != nil || n != 1 {
		t.Fatalf("first sync = %d, %v", n, err)
	}
	if n, err := st.SyncUnpriced(ctx, "r1", func(model.UsageEvent) (int64, string, bool) {
		t.Fatal("completed revision rescanned historical NULL rows")
		return 0, "", false
	}); err != nil || n != 0 {
		t.Fatalf("repeat sync = %d, %v", n, err)
	}
	after, err := st.ListEvents(ctx, Filter{}, WithRaw())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := after[0].Cost(); !ok || got != 1000 || after[0].PriceSource != "test-prices" {
		t.Fatalf("filled price = %+v", after[0])
	}
	after[0].CostMicroUSD, after[0].PriceSource = before[0].CostMicroUSD, before[0].PriceSource
	if !reflect.DeepEqual(before, after) {
		t.Fatal("sync changed historical columns or an already priced row")
	}
	for _, group := range [][]string{nil, {"tool"}, {"model"}, {"provider"}, {"project"}, {"session"}} {
		assertRollupMatchesLedger(t, st, Filter{GroupBy: group})
	}
	activity, err := st.SummarizeActivity(ctx, ActivityFilter{})
	if err != nil || activity.Totals.AttributedCostMicroUSD != 1000 || activity.Totals.ComputedCostCalls != 2 || activity.Totals.UnpricedCalls != 0 {
		t.Fatalf("activity after price fill = %+v, %v", activity, err)
	}
	contexts, err := st.SummarizeTurnContext(ctx, model.DimensionSkill, ActivityFilter{})
	if err != nil || contexts.Totals.CostMicroUSD != 1000 || contexts.Totals.ComputedCostTurns != 1 || contexts.Totals.UnpricedTurns != 0 {
		t.Fatalf("context after price fill = %+v, %v", contexts, err)
	}
	assertActivityUsageCount(t, st, "fill", 2)
	if cp, err := st.Checkpoint(ctx, model.ToolClaudeCode, "/source"); err != nil || cp == nil || cp.Offset != 123 {
		t.Fatalf("source checkpoint changed: %+v, %v", cp, err)
	}
	if n, err := st.SyncUnpriced(ctx, "r2", func(e model.UsageEvent) (int64, string, bool) {
		return e.TotalTokens * 100, "r2-prices", true
	}); err != nil || n != 1 {
		t.Fatalf("new revision = %d, %v", n, err)
	}
	pricedEvents, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{1000, 20000, 20, 30, 0} {
		if got, ok := pricedEvents[i].Cost(); !ok || got != want {
			t.Fatalf("new rates changed existing price at %d: cost=%d, present=%v", i, got, ok)
		}
	}
	var unpricedCells int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM usage_rollup WHERE price_class='unpriced'`).Scan(&unpricedCells); err != nil || unpricedCells != 0 {
		t.Fatalf("emptied unpriced cells = %d, %v", unpricedCells, err)
	}
	assertRollupMatchesLedger(t, st, Filter{})
	incremental := rollupDump(t, st)
	if err := st.RebuildRollup(ctx); err != nil || rollupDump(t, st) != incremental {
		t.Fatalf("incremental price move differs from rebuild: %v", err)
	}
	assertPriceSyncGuard(t, st)
	if v, err := Verify(ctx, st.path); err != nil || v.State != VerificationOK {
		t.Fatalf("sync broke verification: %+v, %v", v, err)
	}
}

func TestSyncUnpricedCursorBatchesAndLaterIDs(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	var events []model.UsageEvent
	for i := 0; i < priceSyncBatchSize+1; i++ {
		events = append(events, ev(fmt.Sprintf("batch-%d", i), model.ToolCodex, time.Unix(1_750_000_000, 0), 1))
	}
	if _, err := st.InsertEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	if n, err := st.SyncUnpriced(ctx, "r1", syncTestPrice); err != nil || n != priceSyncBatchSize {
		t.Fatalf("first bounded batch = %d, %v", n, err)
	}
	if revision, cursor := priceSyncState(t, st); revision != "r1" || cursor != strconv.Itoa(priceSyncBatchSize) {
		t.Fatalf("partial cursor = %q, %q", revision, cursor)
	}
	// Reopen to prove the remaining batch is durable; duplicate insert attempts
	// advance SQLite's sequence and create a legitimate sparse next ID.
	path := st.path
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.InsertEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	later := ev("late", model.ToolCodex, time.Unix(1_750_000_000, 0), 2)
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{later}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.SyncUnpriced(ctx, "r1", syncTestPrice); err != nil || n != 2 {
		t.Fatalf("remaining and late sparse ID = %d, %v", n, err)
	}
	if n, err := st.SyncUnpriced(ctx, "r1", syncTestPrice); err != nil || n != 0 {
		t.Fatalf("drained batch = %d, %v", n, err)
	}
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("after-drain", model.ToolCodex, later.EventTime, 3)}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.SyncUnpriced(ctx, "r1", syncTestPrice); err != nil || n != 1 {
		t.Fatalf("same revision sees later row = %d, %v", n, err)
	}
	assertRollupMatchesLedger(t, st, Filter{})
}

func TestSyncUnpricedRejectsNonComputedOrNonpositivePrices(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		cost         int64
		ok           bool
	}{
		{"unknown", "", 1, false}, {"zero", "test", 0, true}, {"negative", "test", -1, true},
		{"no-source", "", 1, true}, {"vendor", "copilot-nano-aiu", 1, true}, {"vendor-family", "goose-provider_reported", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openTemp(t)
			ctx := context.Background()
			if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("u", model.ToolCodex, provRef, 1)}); err != nil {
				t.Fatal(err)
			}
			if n, err := st.SyncUnpriced(ctx, "r1", func(model.UsageEvent) (int64, string, bool) { return tc.cost, tc.source, tc.ok }); err != nil || n != 0 {
				t.Fatalf("invalid price accepted = %d, %v", n, err)
			}
			events, err := st.ListEvents(ctx, Filter{})
			if err != nil || events[0].CostMicroUSD != nil {
				t.Fatalf("cost changed: %+v, %v", events, err)
			}
			assertPriceSyncGuard(t, st)
		})
	}
}

func TestSyncUnpricedFailuresRollbackPriceRollupCursorAndGuard(t *testing.T) {
	for _, fault := range []struct{ name, sql string }{
		{"event", `CREATE TRIGGER fail_price AFTER UPDATE ON usage_events WHEN NEW.dedup_key='u2' BEGIN SELECT RAISE(ABORT,'price fault'); END`},
		{"rollup", `CREATE TRIGGER fail_price BEFORE INSERT ON usage_rollup WHEN NEW.price_class='computed' BEGIN SELECT RAISE(ABORT,'rollup fault'); END`},
		{"cursor", `CREATE TRIGGER fail_price BEFORE INSERT ON schema_meta WHEN NEW.key='price_sync_cursor' BEGIN SELECT RAISE(ABORT,'cursor fault'); END`},
		{"commit", `CREATE TABLE price_parent(id INTEGER PRIMARY KEY);
			CREATE TABLE price_child(id INTEGER REFERENCES price_parent(id) DEFERRABLE INITIALLY DEFERRED);
			CREATE TRIGGER fail_price AFTER UPDATE ON usage_events BEGIN INSERT INTO price_child VALUES(1); END`},
	} {
		t.Run(fault.name, func(t *testing.T) {
			st := openTemp(t)
			ctx := context.Background()
			if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("u1", model.ToolCodex, provRef, 1), ev("u2", model.ToolCodex, provRef, 2)}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.SyncUnpriced(ctx, "old", func(model.UsageEvent) (int64, string, bool) { return 0, "", false }); err != nil {
				t.Fatal(err)
			}
			beforeRollup := rollupDump(t, st)
			execFailureSQL(t, st, fault.sql)
			if n, err := st.SyncUnpriced(ctx, "new", syncTestPrice); err == nil || n != 0 {
				t.Fatalf("fault committed = %d, %v", n, err)
			}
			var priced int
			if err := st.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE cost_micro_usd IS NOT NULL`).Scan(&priced); err != nil || priced != 0 {
				t.Fatalf("partial prices = %d, %v", priced, err)
			}
			if revision, cursor := priceSyncState(t, st); revision != "old" || cursor != "2" {
				t.Fatalf("failed cursor = %q, %q", revision, cursor)
			}
			if rollupDump(t, st) != beforeRollup {
				t.Fatal("failed sync changed rollup")
			}
			assertPriceSyncGuard(t, st)
			execFailureSQL(t, st, `DROP TRIGGER fail_price`)
			if n, err := st.SyncUnpriced(ctx, "new", syncTestPrice); err != nil || n != 2 {
				t.Fatalf("retry = %d, %v", n, err)
			}
			assertPriceSyncGuard(t, st)
		})
	}
}

func TestSyncUnpricedRefusesDamagedGuardOrRollup(t *testing.T) {
	for _, damage := range []struct{ name, sql string }{
		{"missing-guard", `DROP TRIGGER trg_events_no_update`},
		{"modified-guard", `DROP TRIGGER trg_events_no_update; CREATE TRIGGER trg_events_no_update BEFORE UPDATE ON usage_events WHEN 0 BEGIN SELECT RAISE(ABORT,'disabled'); END`},
		{"stale-watermark", `UPDATE schema_meta SET value='0' WHERE key='rollup_watermark'`},
		{"missing-cell", `DELETE FROM usage_rollup`},
		{"insufficient-cell", `UPDATE usage_rollup SET total_tokens=0`},
		{"empty-with-residual", `UPDATE usage_rollup SET total_tokens=total_tokens+1`},
	} {
		t.Run(damage.name, func(t *testing.T) {
			st := openTemp(t)
			ctx := context.Background()
			if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("u", model.ToolCodex, provRef, 1)}); err != nil {
				t.Fatal(err)
			}
			execFailureSQL(t, st, damage.sql)
			before := rollupDump(t, st)
			if n, err := st.SyncUnpriced(ctx, "r1", syncTestPrice); err == nil || n != 0 {
				t.Fatalf("damaged state accepted = %d, %v", n, err)
			}
			events, err := st.ListEvents(ctx, Filter{})
			if err != nil || events[0].CostMicroUSD != nil || rollupDump(t, st) != before {
				t.Fatalf("damaged sync changed data: %v", err)
			}
			if revision, cursor := priceSyncState(t, st); revision != "" || cursor != "" {
				t.Fatalf("damaged sync advanced cursor = %q, %q", revision, cursor)
			}
		})
	}
}

func TestSyncUnpricedCancellationKeepsRevisionRetryable(t *testing.T) {
	st := openTemp(t)
	if _, err := st.InsertEvents(context.Background(), []model.UsageEvent{ev("u", model.ToolCodex, provRef, 1)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if n, err := st.SyncUnpriced(ctx, "r1", func(e model.UsageEvent) (int64, string, bool) { cancel(); return syncTestPrice(e) }); err == nil || n != 0 {
		t.Fatalf("canceled sync = %d, %v", n, err)
	}
	if revision, cursor := priceSyncState(t, st); revision != "" || cursor != "" {
		t.Fatalf("canceled sync advanced cursor = %q, %q", revision, cursor)
	}
	assertPriceSyncGuard(t, st)
	if n, err := st.SyncUnpriced(context.Background(), "r1", syncTestPrice); err != nil || n != 1 {
		t.Fatalf("retry canceled sync = %d, %v", n, err)
	}
}

func TestSyncUnpricedAcceptsConfirmedFree(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("free", model.ToolOpenCode, provRef, 100)}); err != nil {
		t.Fatal(err)
	}
	price := func(model.UsageEvent) (int64, string, bool) { return 0, "modelsdev-test+free", true }
	if n, err := st.SyncUnpriced(ctx, "free-rates", price); err != nil || n != 1 {
		t.Fatalf("free sync = %d, %v", n, err)
	}
	events, err := st.ListEvents(ctx, Filter{})
	if err != nil || len(events) != 1 || events[0].CostMicroUSD == nil || *events[0].CostMicroUSD != 0 {
		t.Fatalf("confirmed zero not stored: %+v, %v", events, err)
	}
	if n, err := st.SyncUnpriced(ctx, "free-rates", price); err != nil || n != 0 {
		t.Fatalf("repeat free sync = %d, %v", n, err)
	}
	assertPriceSyncGuard(t, st)
	var unpriced, computed, tokens int64
	if err := st.db.QueryRow(`SELECT SUM(unpriced_events), SUM(CASE WHEN price_class='computed' THEN events ELSE 0 END), SUM(total_tokens) FROM usage_rollup`).Scan(&unpriced, &computed, &tokens); err != nil || unpriced != 0 || computed != 1 || tokens != 100 {
		t.Fatalf("free rollup = unpriced %d, computed %d, tokens %d, %v", unpriced, computed, tokens, err)
	}
}
