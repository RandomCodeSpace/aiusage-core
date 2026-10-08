package opencode

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestUnchangedDBSkipsSQLiteRead(t *testing.T) {
	for _, schema := range []string{"legacy", "modern pending"} {
		t.Run(schema, func(t *testing.T) {
			var src adapter.Source
			if schema == "legacy" {
				path := writeDB(t, t.TempDir(), [][3]string{{"m", "s", msgData("m", "s", 10)}})
				src = adapter.Source{Path: path, Meta: map[string]string{"kind": kindDB}}
			} else {
				db, modernSrc := modernDB(t)
				addModernMessage(t, db, 1, "m", "s", modernData("assistant", 0, 10))
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				src = modernSrc
			}
			first := incremental(t, src, nil)
			var state dbState
			if err := json.Unmarshal([]byte(first.Checkpoint.State), &state); err != nil || state.Files == nil {
				t.Fatalf("missing file gate: %+v, %v", state, err)
			}
			// Invalid bytes with the same stamp make any SQLite read fail. The
			// checkpoint pass must only stat the files and return without reading.
			info, err := os.Stat(src.Path)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(src.Path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt([]byte("not a database!!"), 0); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(src.Path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			before := *first.Checkpoint
			idle := incremental(t, src, first.Checkpoint)
			if len(idle.Events) != 0 || len(idle.Activity) != 0 || len(idle.CodeChanges) != 0 || idle.Checkpoint != nil || *first.Checkpoint != before {
				t.Fatalf("unchanged source accessed SQLite or changed checkpoint: %+v", idle)
			}
			if _, err := (Adapter{}).Collect(context.Background(), src); err == nil {
				t.Fatal("invalid source unexpectedly readable without checkpoint")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := (Adapter{}).CollectIncremental(ctx, src, first.Checkpoint); err != context.Canceled {
				t.Fatalf("unchanged gate ignored cancellation: %v", err)
			}
			// A same-size change still invalidates the gate through its mtime.
			changed := info.ModTime().Add(time.Second)
			if err := os.Chtimes(src.Path, changed, changed); err != nil {
				t.Fatal(err)
			}
			obs, err := (Adapter{}).CollectIncremental(context.Background(), src, first.Checkpoint)
			if err == nil || obs.Checkpoint != nil {
				t.Fatalf("changed source skipped SQLite: %+v, %v", obs, err)
			}
		})
	}
}

func TestLegacyFileCheckpointRecoversModernSchema(t *testing.T) {
	path := writeDBWithParts(t, t.TempDir(), [][3]string{{"m", "s", modernData("assistant", 0, 10)}}, nil)
	src := adapter.Source{Path: path, Meta: map[string]string{"kind": kindDB}}
	legacy := incremental(t, src, nil)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	execSource(t, db, `ALTER TABLE message ADD COLUMN time_created INTEGER`)
	execSource(t, db, `ALTER TABLE message ADD COLUMN time_updated INTEGER`)
	execSource(t, db, `UPDATE message SET data=?`, modernData("assistant", 1, 99))
	modern := incremental(t, src, legacy.Checkpoint)
	wantPending(t, modern.Checkpoint, 1)
	if len(modern.Events) != 1 || modern.Events[0].TotalTokens != 99 {
		t.Fatalf("legacy file marker suppressed modern recovery: %+v", modern)
	}
}

func TestWALRemovalInvalidatesFileGate(t *testing.T) {
	db, src := modernDB(t)
	execSource(t, db, `PRAGMA journal_mode=WAL`)
	execSource(t, db, `PRAGMA wal_autocheckpoint=0`)
	addModernMessage(t, db, 1, "m", "s", modernData("assistant", 0, 10))
	first := incremental(t, src, nil)
	// Closing the writer checkpoints and removes its WAL. Completion then
	// appears in a new WAL, even though the rowid cursor does not advance.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := statDBFiles(src.Path)
	if err != nil || files.WAL.Present {
		t.Fatalf("writer close did not remove WAL: %+v, %v", files, err)
	}
	withoutWAL := incremental(t, src, first.Checkpoint)
	wantPending(t, withoutWAL.Checkpoint, 1, 1)
	var state dbState
	if err := json.Unmarshal([]byte(withoutWAL.Checkpoint.State), &state); err != nil || (state.Files != nil && state.Files.WAL.Present) {
		t.Fatalf("removed WAL gate = %+v, %v", state, err)
	}
	db, err = sql.Open("sqlite", src.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	execSource(t, db, `UPDATE message SET data=?`, modernData("assistant", 1, 99))
	completed := incremental(t, src, withoutWAL.Checkpoint)
	wantPending(t, completed.Checkpoint, 1)
	if len(completed.Events) != 1 || completed.Events[0].TotalTokens != 99 {
		t.Fatalf("new WAL completion skipped: %+v", completed)
	}
}

var snapshotFunctionID atomic.Uint64

func TestSnapshotChangeDoesNotSaveFileGate(t *testing.T) {
	db, src := modernDB(t)
	execSource(t, db, `PRAGMA journal_mode=WAL`)
	execSource(t, db, `PRAGMA wal_autocheckpoint=0`)
	addModernMessage(t, db, 1, "m", "s", modernData("assistant", 0, 10))
	name := fmt.Sprintf("snapshot_change_%d", snapshotFunctionID.Add(1))
	var once sync.Once
	var writeErr error
	if err := sqlite.RegisterScalarFunction(name, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		once.Do(func() {
			_, writeErr = db.Exec(`UPDATE message_data SET data=?`, modernData("assistant", 1, 99))
		})
		return args[0], writeErr
	}); err != nil {
		t.Fatal(err)
	}
	// A view calls the producer write while the reader's snapshot is active.
	// This avoids sleeps or timing assumptions when exercising the stamp race.
	execSource(t, db, `ALTER TABLE message RENAME TO message_data`)
	execSource(t, db, `CREATE VIEW message AS SELECT rowid,id,session_id,`+name+`(data) AS data,time_created,time_updated FROM message_data`)
	first := incremental(t, src, nil)
	wantPending(t, first.Checkpoint, 1, 1)
	var state dbState
	if err := json.Unmarshal([]byte(first.Checkpoint.State), &state); err != nil || state.Files != nil {
		t.Fatalf("raced snapshot saved file gate: %+v, %v", state, err)
	}
	completed := incremental(t, src, first.Checkpoint)
	wantPending(t, completed.Checkpoint, 1)
	if len(completed.Events) != 1 || completed.Events[0].TotalTokens != 99 {
		t.Fatalf("snapshot race lost completion: %+v", completed)
	}
	if completed.Checkpoint.Tool != model.ToolOpenCode {
		t.Fatalf("wrong checkpoint tool: %+v", completed.Checkpoint)
	}
}
