package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func modernDB(t *testing.T) (*sql.DB, adapter.Source) {
	t.Helper()
	path := writeDBWithParts(t, t.TempDir(), nil, nil)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	execSource(t, db, `ALTER TABLE message ADD COLUMN time_created INTEGER`)
	execSource(t, db, `ALTER TABLE message ADD COLUMN time_updated INTEGER`)
	return db, adapter.Source{Tool: model.ToolOpenCode, Path: path, Meta: map[string]string{"kind": kindDB}}
}

func execSource(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func modernData(role string, completed, total int64) string {
	return fmt.Sprintf(`{"role":%q,"modelID":"gpt-5","time":{"created":1730000000000,"completed":%d},"tokens":{"input":%d}}`, role, completed, total)
}

func addModernMessage(t *testing.T, db *sql.DB, rowid int64, id, session, raw string) {
	t.Helper()
	execSource(t, db, `INSERT INTO message(rowid,id,session_id,data,time_created,time_updated) VALUES(?,?,?,?,?,?)`, rowid, id, session, raw, 1730000000000, 1730000000000)
}

func addModernPart(t *testing.T, db *sql.DB, id, message string) {
	t.Helper()
	execSource(t, db, `INSERT INTO part(id,message_id,time_created,data) VALUES(?,?,?,?)`, id, message, 1730000000000, toolPartData("read"))
}

func incremental(t *testing.T, src adapter.Source, cp *model.SourceCheckpoint) adapter.Observation {
	t.Helper()
	obs, err := (Adapter{}).CollectIncremental(context.Background(), src, cp)
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func wantPending(t *testing.T, cp *model.SourceCheckpoint, watermark int64, pending ...int64) {
	t.Helper()
	if cp == nil || cp.Watermark != watermark {
		t.Fatalf("checkpoint = %+v, want watermark %d", cp, watermark)
	}
	var state dbState
	if err := json.Unmarshal([]byte(cp.State), &state); err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 || !reflect.DeepEqual(state.Pending, pending) {
		t.Fatalf("state = %+v, want version 1 pending %v", state, pending)
	}
}

func applyObservation(t *testing.T, ledger *store.Ledger, obs adapter.Observation, events, activity int) {
	t.Helper()
	got, err := ledger.ApplyBatch(context.Background(), store.ObservationBatch{Events: obs.Events, Activity: obs.Activity, Checkpoint: obs.Checkpoint})
	if err != nil || got.Events != events || got.Activity != activity {
		t.Fatalf("ApplyBatch = %+v, %v; want %d usage and %d activity", got, err, events, activity)
	}
}

func TestIncrementalInPlaceCompletion(t *testing.T) {
	for _, intermediate := range []int64{0, 10} {
		t.Run(fmt.Sprint(intermediate), func(t *testing.T) {
			db, src := modernDB(t)
			execSource(t, db, `PRAGMA journal_mode=WAL`)
			execSource(t, db, `PRAGMA wal_autocheckpoint=0`)
			addModernMessage(t, db, 1, "m1", "session", modernData("assistant", 0, intermediate))
			addModernPart(t, db, "early", "m1")
			ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			first := incremental(t, src, nil)
			wantPending(t, first.Checkpoint, 1, 1)
			if len(first.Events) != 0 || len(first.Activity) != 0 {
				t.Fatalf("unfinished message emitted %+v", first)
			}
			applyObservation(t, ledger, first, 0, 0)
			addModernPart(t, db, "late1", "m1")
			addModernPart(t, db, "late2", "m1")
			execSource(t, db, `UPDATE message SET data=?, time_updated=time_updated+1 WHERE id='m1'`, modernData("assistant", 1730000000010, 21800))
			second := incremental(t, src, first.Checkpoint)
			wantPending(t, second.Checkpoint, 1)
			if len(second.Events) != 1 || second.Events[0].TotalTokens != 21800 || second.Events[0].DedupKey != "opencode|m1" || len(second.Activity) != 3 {
				t.Fatalf("completed observation = %+v", second)
			}
			keys := map[string]bool{"opencode|part|early": true, "opencode|part|late1": true, "opencode|part|late2": true}
			for _, a := range second.Activity {
				if a.UsageDedupKey != "opencode|m1" || a.CallsInTurn != 3 || !keys[a.DedupKey] {
					t.Fatalf("activity identity/count = %+v", a)
				}
				delete(keys, a.DedupKey)
			}
			applyObservation(t, ledger, second, 1, 3)
			stored, err := ledger.Checkpoint(context.Background(), src.Tool, src.Path)
			if err != nil {
				t.Fatal(err)
			}
			wantPending(t, stored, 1)
			idle := incremental(t, src, stored)
			if len(idle.Events) != 0 || len(idle.Activity) != 0 || idle.Checkpoint != nil {
				t.Fatalf("idle replay = %+v", idle)
			}
			applyObservation(t, ledger, incremental(t, src, nil), 0, 0)
		})
	}
}

func TestIncompleteOldRowsDoNotBlockNewMessages(t *testing.T) {
	db, src := modernDB(t)
	for _, row := range []int64{9, 11} {
		// Bad creation time does not poison an unfinished message.
		addModernMessage(t, db, row, fmt.Sprint(row), "old", strings.Replace(modernData("assistant", 0, 1), `"created":1730000000000`, `"created":"unfinished"`, 1))
		addModernPart(t, db, fmt.Sprint(row), fmt.Sprint(row))
	}
	addModernMessage(t, db, 12, "user", "other", modernData("user", 0, 0))
	addModernMessage(t, db, 13, "zero", "other", modernData("assistant", 1, 0))
	addModernPart(t, db, "zero", "zero")
	addModernMessage(t, db, 14, "new", "new-session", modernData("assistant", 1, 22))
	addModernPart(t, db, "new", "new")
	first := incremental(t, src, nil)
	wantPending(t, first.Checkpoint, 14, 9, 11)
	if len(first.Events) != 1 || first.Events[0].MessageID != "new" || first.Events[0].SessionID != "new-session" || len(first.Activity) != 2 {
		t.Fatalf("new completed messages = %+v", first)
	}
	for _, a := range first.Activity {
		if a.MessageID == "zero" && (a.UsageDedupKey != "" || a.EventTime.IsZero()) {
			t.Fatalf("zero-usage activity = %+v", a)
		}
	}
	idle := incremental(t, src, first.Checkpoint)
	if idle.Checkpoint != nil || len(idle.Events) != 0 || len(idle.Activity) != 0 {
		t.Fatalf("abandoned rows caused replay: %+v", idle)
	}
	// Successful absence removes only the disappeared pending identity.
	execSource(t, db, `DELETE FROM message WHERE rowid=11`)
	removed := incremental(t, src, first.Checkpoint)
	wantPending(t, removed.Checkpoint, 14, 9)
	execSource(t, db, `UPDATE message SET data=? WHERE rowid=9`, modernData("assistant", 1, 99))
	completed := incremental(t, src, removed.Checkpoint)
	wantPending(t, completed.Checkpoint, 14)
	if len(completed.Events) != 1 || completed.Events[0].DedupKey != "opencode|9" || len(completed.Activity) != 1 || completed.Activity[0].DedupKey != "opencode|part|9" {
		t.Fatalf("old pending completion = %+v", completed)
	}
}

func TestLegacyCheckpointRecoveryOnce(t *testing.T) {
	db, src := modernDB(t)
	addModernMessage(t, db, 1, "partial", "s", modernData("assistant", 1, 200))
	addModernMessage(t, db, 2, "absent", "s", modernData("assistant", 1, 300))
	addModernPart(t, db, "p1", "partial")
	addModernPart(t, db, "p2", "absent")
	ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	old := &model.SourceCheckpoint{Tool: src.Tool, SourcePath: src.Path, Watermark: 2}
	partial, ok, err := buildEvent([]byte(modernData("assistant", 1, 50)), "partial", "s", src.Path)
	if err != nil || !ok {
		t.Fatalf("partial fixture: %v", err)
	}
	applyObservation(t, ledger, adapter.Observation{Events: []model.UsageEvent{partial}, Checkpoint: old}, 1, 0)
	recovered := incremental(t, src, old)
	wantPending(t, recovered.Checkpoint, 2)
	// No commit, then replay: the missing marker must keep recovery eligible.
	again := incremental(t, src, old)
	if !reflect.DeepEqual(recovered, again) {
		t.Fatal("recovery changed before checkpoint commit")
	}
	// Force checkpoint validation failure after tentative event/activity inserts.
	badCheckpoint := *recovered.Checkpoint
	badCheckpoint.SourcePath = ""
	if _, err := ledger.ApplyObservation(context.Background(), recovered.Events, recovered.Activity, &badCheckpoint); err == nil {
		t.Fatal("invalid checkpoint unexpectedly committed")
	}
	stored, err := ledger.Checkpoint(context.Background(), src.Tool, src.Path)
	if err != nil || !reflect.DeepEqual(stored, old) {
		t.Fatalf("failed recovery changed checkpoint: %+v, %v", stored, err)
	}
	applyObservation(t, ledger, recovered, 1, 2)
	applyObservation(t, ledger, incremental(t, src, nil), 0, 0)
	rows, err := ledger.ListEvents(context.Background(), store.Filter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("ledger rows = %+v, %v", rows, err)
	}
	for _, row := range rows {
		if row.DedupKey == "opencode|partial" && row.TotalTokens != 50 {
			t.Fatalf("existing partial history changed: %+v", row)
		}
	}
	idle := incremental(t, src, recovered.Checkpoint)
	if len(idle.Events) != 0 || len(idle.Activity) != 0 || idle.Checkpoint != nil {
		t.Fatalf("marker failed to end recovery: %+v", idle)
	}
}

func TestModernCheckpointStateRequiresValidMarker(t *testing.T) {
	db, src := modernDB(t)
	addModernMessage(t, db, 1, "m1", "s", modernData("assistant", 1, 1))
	for _, state := range []string{`{`, `{}`, `null`, `{"pending":[]}`, `{"version":2}`, `{"version":1,"pending":[0]}`, `{"version":1,"pending":[2]}`, `{"version":1,"pending":[1,1]}`, `{"version":1,"pending":[1,0]}`} {
		t.Run(state, func(t *testing.T) {
			cp := &model.SourceCheckpoint{Watermark: 1, State: state}
			before := *cp
			obs, err := (Adapter{}).CollectIncremental(context.Background(), src, cp)
			if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil || len(obs.Events) != 0 || *cp != before {
				t.Fatalf("invalid state accepted: %+v, %v", obs, err)
			}
		})
	}
}

func TestModernReadFailuresHoldCheckpoint(t *testing.T) {
	for _, failure := range []string{"role", "json", "tokens", "completion", "part-query", "part-json", "part-scan", "missing-part", "partial-schema", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			db, src := modernDB(t)
			addModernMessage(t, db, 1, "m1", "s", modernData("assistant", 0, 0))
			cp := incremental(t, src, nil).Checkpoint
			before := *cp
			execSource(t, db, `UPDATE message SET data=?`, modernData("assistant", 1, 10))
			addModernPart(t, db, "p1", "m1")
			ctx := context.Background()
			switch failure {
			case "role":
				execSource(t, db, `UPDATE message SET data=?`, strings.Replace(modernData("assistant", 1, 10), `"role":"assistant",`, "", 1))
			case "json":
				execSource(t, db, `UPDATE message SET data='{'`)
			case "tokens":
				execSource(t, db, `UPDATE message SET data=?`, strings.Replace(modernData("assistant", 1, 10), `"input":10`, `"input":"bad"`, 1))
			case "completion":
				execSource(t, db, `UPDATE message SET data=?`, strings.Replace(modernData("assistant", 1, 10), `"completed":1`, `"completed":"bad"`, 1))
			case "part-query":
				execSource(t, db, `ALTER TABLE part RENAME COLUMN data TO wrong_data`)
			case "part-json":
				execSource(t, db, `UPDATE part SET data='{'`)
			case "part-scan":
				execSource(t, db, `UPDATE part SET time_created='bad'`)
			case "missing-part":
				execSource(t, db, `ALTER TABLE part RENAME TO saved_part`)
			case "partial-schema":
				execSource(t, db, `ALTER TABLE message RENAME COLUMN time_updated TO wrong_updated`)
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			obs, err := (Adapter{}).CollectIncremental(ctx, src, cp)
			if err == nil || obs.Checkpoint != nil || *cp != before || len(obs.Activity) != 0 {
				t.Fatalf("failed read consumed pending identity: %+v, %v", obs, err)
			}
			if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation error: %v", err)
			}
			// Restore the producer state and replay the unchanged pending ID.
			switch failure {
			case "part-query":
				execSource(t, db, `ALTER TABLE part RENAME COLUMN wrong_data TO data`)
			case "missing-part":
				execSource(t, db, `ALTER TABLE saved_part RENAME TO part`)
			case "partial-schema":
				execSource(t, db, `ALTER TABLE message RENAME COLUMN wrong_updated TO time_updated`)
			}
			execSource(t, db, `UPDATE message SET data=?`, modernData("assistant", 1, 10))
			execSource(t, db, `UPDATE part SET data=?,time_created=1730000000000`, toolPartData("read"))
			retry := incremental(t, src, cp)
			wantPending(t, retry.Checkpoint, 1)
			if len(retry.Events) != 1 || len(retry.Activity) != 1 {
				t.Fatalf("repaired pending row was lost: %+v", retry)
			}
		})
	}
}

func TestMalformedTimestampsKeepValidNeighbors(t *testing.T) {
	for _, surface := range []string{"legacy", "modern", "json"} {
		for _, created := range []string{"0", "-1", `"bad"`, "null"} {
			t.Run(surface+"/"+created, func(t *testing.T) {
				valid := modernData("assistant", 1, 5)
				bad := strings.Replace(valid, `"created":1730000000000`, `"created":`+created, 1)
				var src adapter.Source
				if surface == "modern" {
					db, s := modernDB(t)
					src = s
					addModernMessage(t, db, 1, "before", "s", valid)
					addModernMessage(t, db, 2, "bad", "s", bad)
					addModernMessage(t, db, 3, "after", "s", valid)
				} else if surface == "legacy" {
					path := writeDB(t, t.TempDir(), [][3]string{{"before", "s", valid}, {"bad", "s", bad}, {"after", "s", valid}})
					src = adapter.Source{Path: path, Meta: map[string]string{"kind": kindDB}}
				} else {
					dir := t.TempDir()
					for id, raw := range map[string]string{"before": valid, "bad": bad, "after": valid} {
						raw = strings.Replace(raw, "{", `{"id":"`+id+`",`, 1)
						if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(raw), 0600); err != nil {
							t.Fatal(err)
						}
					}
					src = adapter.Source{Path: dir, Meta: map[string]string{"kind": kindJSON}}
				}
				obs, err := (Adapter{}).Collect(context.Background(), src)
				if !errors.Is(err, errInvalidTimestamp) || errors.Is(err, adapter.ErrSourceFormat) || !strings.Contains(err.Error(), "timestamp") || len(obs.Events) != 2 {
					t.Fatalf("malformed time/valid neighbors = %+v, %v", obs, err)
				}
				for _, event := range obs.Events {
					if event.MessageID == "bad" || event.EventTime.IsZero() {
						t.Fatalf("undated usage escaped: %+v", event)
					}
				}
				if surface != "json" && (obs.Checkpoint == nil || obs.Checkpoint.Watermark != 3) {
					t.Fatalf("completed malformed record blocked valid progress: %+v", obs.Checkpoint)
				}
			})
		}
	}
}

func TestActivityUsesTheMessageSnapshot(t *testing.T) {
	db, src := modernDB(t)
	execSource(t, db, `PRAGMA journal_mode=WAL`)
	execSource(t, db, `PRAGMA wal_autocheckpoint=0`)
	addModernMessage(t, db, 1, "m1", "s", modernData("assistant", 1, 10))
	addModernPart(t, db, "first", "m1")
	reader, err := sql.Open("sqlite", "file:"+src.Path+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tx, err := reader.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRow(`SELECT data FROM message WHERE rowid=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	addModernPart(t, db, "after-snapshot", "m1")
	activity, err := collectActivity(context.Background(), tx, src, nil, []int64{1}, true)
	if err != nil || len(activity) != 1 || activity[0].DedupKey != "opencode|part|first" {
		t.Fatalf("activity escaped message snapshot: %+v, %v", activity, err)
	}
}
