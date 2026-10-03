package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

type rowCodeError int

func (e rowCodeError) Error() string { return fmt.Sprintf("SQLite code %d", e) }
func (e rowCodeError) Code() int     { return int(e) }

func TestRowFailureOnlyCheckMaySkip(t *testing.T) {
	for _, code := range []int{275, 19, 1555, 2067, 1299, 787, 1811, 5, 517, 6, 262, 13, 10, 266, 7, 9, 4, 8, 11, 26, 14, 1} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			if got := isSkippableRowError(fmt.Errorf("wrapped: %w", rowCodeError(code))); got != (code == 275) {
				t.Fatalf("code %d skippable = %v", code, got)
			}
		})
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("unknown"), nil} {
		if isSkippableRowError(err) {
			t.Fatalf("skippable error: %v", err)
		}
	}
}

func failureBatch() ObservationBatch {
	at := time.Unix(1_750_000_000, 0)
	return ObservationBatch{
		Events:       []model.UsageEvent{ev("u1", model.ToolClaudeCode, at, 100), ev("u2", model.ToolClaudeCode, at, 200)},
		Activity:     []model.ActivityEvent{act("a1", "Read", model.ActivityTool, at, "u1", 0, 1), act("a2", "Edit", model.ActivityTool, at, "u2", 0, 1)},
		TurnContexts: []model.TurnContext{turnCtx("u1", model.DimensionSkill, "read", at), turnCtx("u2", model.DimensionSkill, "edit", at)},
		CodeChanges:  []model.CodeChange{codeChange("c1", 4, 2), codeChange("c2", 1, 0)},
		Checkpoint:   &model.SourceCheckpoint{Tool: model.ToolClaudeCode, SourcePath: "/synthetic/source", Offset: 2},
	}
}

func execFailureSQL(t *testing.T, st *Ledger, q string) {
	t.Helper()
	if _, err := st.db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func assertBatchRolledBack(t *testing.T, st *Ledger, err error, applied Applied) {
	t.Helper()
	var skipped *SkippedRowsError
	if err == nil || errors.As(err, &skipped) || applied != (Applied{}) {
		t.Fatalf("rollback returned applied=%+v error=%v", applied, err)
	}
	for _, table := range []string{"usage_events", "activity_events", "usage_turn_context", "usage_rollup", "activity_usage_counts", "code_changes"} {
		var n int
		if err := st.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s rows=%d err=%v", table, n, err)
		}
	}
	cp, cpErr := st.Checkpoint(context.Background(), model.ToolClaudeCode, "/synthetic/source")
	if cpErr != nil || cp == nil || cp.Offset != 1 {
		t.Fatalf("checkpoint=%+v err=%v", cp, cpErr)
	}
}

func seedFailureCheckpoint(t *testing.T, st *Ledger) {
	t.Helper()
	cp := *failureBatch().Checkpoint
	cp.Offset = 1
	if _, err := st.ApplyEvents(context.Background(), nil, &cp); err != nil {
		t.Fatal(err)
	}
}

func assertBatchReplay(t *testing.T, st *Ledger) {
	t.Helper()
	ctx := context.Background()
	want := Applied{Events: 2, Activity: 2, TurnContexts: 2, CodeChanges: 2}
	if got, err := st.ApplyBatch(ctx, failureBatch()); err != nil || got != want {
		t.Fatalf("retry=%+v err=%v", got, err)
	}
	if got, err := st.ApplyBatch(ctx, failureBatch()); err != nil || got != (Applied{}) {
		t.Fatalf("repeat=%+v err=%v", got, err)
	}
	if stale, err := st.activityUsageCountsStale(ctx); err != nil || stale {
		t.Fatalf("counts after retry stale=%v err=%v", stale, err)
	}
	cp, err := st.Checkpoint(ctx, model.ToolClaudeCode, "/synthetic/source")
	if err != nil || cp == nil || cp.Offset != 2 {
		t.Fatalf("retry checkpoint=%+v err=%v", cp, err)
	}
}

func TestApplyBatchFatalFailuresRollbackAllStreams(t *testing.T) {
	for _, tc := range []struct{ name, table, condition string }{
		{"usage", "usage_events", "NEW.dedup_key='u2'"},
		{"activity", "activity_events", "NEW.dedup_key='a2'"},
		{"context", "usage_turn_context", "NEW.usage_dedup_key='u2'"},
		{"code change", "code_changes", "NEW.change_id='c2'"},
		{"checkpoint", "source_checkpoints", "NEW.read_offset=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openTemp(t)
			seedFailureCheckpoint(t, st)
			execFailureSQL(t, st, "CREATE TRIGGER fail_insert BEFORE INSERT ON "+tc.table+" WHEN "+tc.condition+" BEGIN SELECT RAISE(ABORT, 'injected failure'); END")
			batch := failureBatch()
			// A provisional CHECK skip must not turn a later rollback into partial success.
			batch.Events = append([]model.UsageEvent{ev("poison", model.ToolClaudeCode, time.Now(), -1)}, batch.Events...)
			got, err := st.ApplyBatch(context.Background(), batch)
			if err == nil || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("expected trigger failure in %s: %v", tc.table, err)
			}
			assertBatchRolledBack(t, st, err, got)
			execFailureSQL(t, st, "DROP TRIGGER fail_insert")
			assertBatchReplay(t, st)
		})
	}
}

func TestApplyBatchCommitFailureReturnsZero(t *testing.T) {
	st := openTemp(t)
	seedFailureCheckpoint(t, st)
	execFailureSQL(t, st, `CREATE TABLE fault_parent(id INTEGER PRIMARY KEY)`)
	execFailureSQL(t, st, `CREATE TABLE fault_child(id INTEGER REFERENCES fault_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	execFailureSQL(t, st, `CREATE TRIGGER fail_commit AFTER INSERT ON usage_turn_context BEGIN INSERT INTO fault_child VALUES(1); END`)
	got, err := st.ApplyBatch(context.Background(), failureBatch())
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("expected commit failure, got %v", err)
	}
	assertBatchRolledBack(t, st, err, got)
	execFailureSQL(t, st, `DROP TRIGGER fail_commit`)
	assertBatchReplay(t, st)
}

func TestInsertEventsFatalFailureReturnsZero(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) {
			st := openTemp(t)
			seedFailureCheckpoint(t, st)
			if commit {
				execFailureSQL(t, st, `CREATE TABLE fault_parent(id INTEGER PRIMARY KEY)`)
				execFailureSQL(t, st, `CREATE TABLE fault_child(id INTEGER REFERENCES fault_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
				execFailureSQL(t, st, `CREATE TRIGGER fail_insert AFTER INSERT ON usage_events BEGIN INSERT INTO fault_child VALUES(1); END`)
			} else {
				execFailureSQL(t, st, `CREATE TRIGGER fail_insert BEFORE INSERT ON usage_events WHEN NEW.dedup_key='u2' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
			}
			n, err := st.InsertEvents(context.Background(), failureBatch().Events)
			assertBatchRolledBack(t, st, err, Applied{Events: n})
			execFailureSQL(t, st, `DROP TRIGGER fail_insert`)
			assertBatchReplay(t, st)
		})
	}
}

func TestApplyBatchBusyHoldsCheckpoint(t *testing.T) {
	st := openTemp(t)
	st.db.SetMaxOpenConns(1)
	seedFailureCheckpoint(t, st)
	execFailureSQL(t, st, `PRAGMA busy_timeout=1`)
	writer, err := sql.Open("sqlite", sqliteFileURI(st.path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE schema_meta SET value=value`); err != nil {
		t.Fatal(err)
	}
	got, err := st.ApplyBatch(context.Background(), failureBatch())
	if !isBusySQLiteError(err) {
		t.Fatalf("expected real SQLite BUSY, got %v", err)
	}
	assertBatchRolledBack(t, st, err, got)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertBatchReplay(t, st)
}

func TestApplyBatchCheckSkipsCommitGoodRowsAndCheckpoint(t *testing.T) {
	st := openTemp(t)
	batch := failureBatch()
	badEvent := batch.Events[0]
	badEvent.DedupKey = "poison"
	badEvent.TotalTokens = -1
	keylessEvent := batch.Events[0]
	keylessEvent.DedupKey = ""
	batch.Events = append(batch.Events, badEvent, keylessEvent)
	badActivity := batch.Activity[0]
	badActivity.DedupKey = "poison"
	badActivity.Name = ""
	keylessActivity := batch.Activity[0]
	keylessActivity.DedupKey = ""
	batch.Activity = append(batch.Activity, badActivity, keylessActivity)
	badContext := batch.TurnContexts[0]
	badContext.UsageDedupKey = "poison"
	badContext.Value = ""
	keylessContext := batch.TurnContexts[0]
	keylessContext.UsageDedupKey = ""
	batch.TurnContexts = append(batch.TurnContexts, badContext, keylessContext)
	got, err := st.ApplyBatch(context.Background(), batch)
	if got != (Applied{Events: 2, Activity: 2, TurnContexts: 2, CodeChanges: 2}) {
		t.Fatalf("applied=%+v err=%v", got, err)
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 3 {
		t.Fatalf("missing joined skips: %v", err)
	}
	for i, table := range []string{tableUsageEvents, tableActivityEvents, tableTurnContext} {
		var skipped *SkippedRowsError
		if !errors.As(joined.Unwrap()[i], &skipped) || skipped.Table != table || skipped.Skipped() != 2 || skipped.Total != 4 {
			t.Fatalf("%s skipped=%+v", table, skipped)
		}
	}
	if cp, err := st.Checkpoint(context.Background(), batch.Checkpoint.Tool, batch.Checkpoint.SourcePath); err != nil || cp == nil || cp.Offset != 2 {
		t.Fatalf("checkpoint=%+v err=%v", cp, err)
	}
	if got, err := st.ApplyBatch(context.Background(), failureBatch()); err != nil || got != (Applied{}) {
		t.Fatalf("dedup=%+v err=%v", got, err)
	}
}

func TestApplySnapshotRejectedDeltaIsOrdinaryRollback(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprintf("fatal=%v", fatal), func(t *testing.T) {
			st := openTemp(t)
			ctx := context.Background()
			at := time.Unix(1_750_000_000, 0)
			snap := model.AggregateSnapshot{Tool: model.ToolHermes, Key: "cell", InputTokens: 100, TotalTokens: 100, ObservedTime: at}
			cp := &model.SourceCheckpoint{Tool: model.ToolHermes, SourcePath: "/synthetic/state", Offset: 1}
			if _, err := st.ApplySnapshot(ctx, []model.UsageEvent{ev("base", model.ToolHermes, at, 100)}, snap, cp); err != nil {
				t.Fatal(err)
			}
			snap.InputTokens, snap.TotalTokens, cp.Offset = 200, 200, 2
			delta := ev("delta", model.ToolHermes, at, 100)
			bad := ev("bad", model.ToolHermes, at, -1)
			if fatal {
				bad.TotalTokens, bad.InputTokens = 1, 1
				execFailureSQL(t, st, `CREATE TRIGGER fail_delta BEFORE INSERT ON usage_events WHEN NEW.dedup_key='bad' BEGIN SELECT RAISE(ABORT,'injected failure'); END`)
			}
			n, err := st.ApplySnapshot(ctx, []model.UsageEvent{delta, bad}, snap, cp)
			var skipped *SkippedRowsError
			if n != 0 || err == nil || errors.As(err, &skipped) || !strings.Contains(err.Error(), "bad") {
				t.Fatalf("rejected delta n=%d err=%v", n, err)
			}
			if state, err := st.LastState(ctx, snap.Tool, snap.Key); err != nil || state == nil || state.TotalTokens != 100 {
				t.Fatalf("baseline=%+v err=%v", state, err)
			}
			if saved, err := st.Checkpoint(ctx, cp.Tool, cp.SourcePath); err != nil || saved == nil || saved.Offset != 1 {
				t.Fatalf("checkpoint=%+v err=%v", saved, err)
			}
			if n, err := st.ApplySnapshot(ctx, []model.UsageEvent{delta}, snap, cp); err != nil || n != 1 {
				t.Fatalf("retry n=%d err=%v", n, err)
			}
			if sum, err := st.Summarize(ctx, Filter{}); err != nil || sum.Totals.Total != 200 {
				t.Fatalf("retry total=%+v err=%v", sum, err)
			}
		})
	}
}
