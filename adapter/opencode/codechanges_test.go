package opencode

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestCodeChangesMutableSnapshots(t *testing.T) {
	db, src := modernDB(t)
	execSource(t, db, `CREATE TABLE session(id TEXT PRIMARY KEY, directory TEXT NOT NULL)`)
	execSource(t, db, `INSERT INTO session VALUES('session','/recorded/project')`)
	execSource(t, db, `PRAGMA journal_mode=WAL`)
	execSource(t, db, `PRAGMA wal_autocheckpoint=0`)
	addModernMessage(t, db, 1, "user", "session", `{"role":"user"}`)
	addModernMessage(t, db, 2, "assistant", "session", modernData("assistant", 1730000000001, 17))
	first := incremental(t, src, nil)
	if len(first.Events) != 1 || len(first.CodeChanges) != 1 || first.CodeChanges[0].Known {
		t.Fatalf("initial observation = %+v", first)
	}
	wantPending(t, first.Checkpoint, 2)
	// An established usage checkpoint must not hide historical user rows or
	// a summary written after assistant completion. No checkpoint reset.
	for i, tt := range []struct {
		name, summary  string
		known          bool
		added, removed int64
	}{
		{"late summary", `{"diffs":[{"additions":8,"deletions":3,"patch":"private patch"},{"additions":5,"deletions":2}]}`, true, 13, 5},
		{"decrease", `{"diffs":[{"additions":2,"deletions":1}]}`, true, 2, 1},
		{"revert or unavailable", `{"diffs":[]}`, false, 0, 0},
		{"recorded zero", `{"diffs":[{"additions":0,"deletions":0}]}`, true, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			updated := int64(1730000000010 + i)
			execSource(t, db, `UPDATE message SET data=?,time_updated=? WHERE id='user'`, `{"role":"user","summary":`+tt.summary+`}`, updated)
			beforeDB, err := os.ReadFile(src.Path)
			if err != nil {
				t.Fatal(err)
			}
			beforeWAL, err := os.ReadFile(src.Path + "-wal")
			if err != nil {
				t.Fatal(err)
			}
			obs := incremental(t, src, first.Checkpoint)
			want := model.CodeChange{
				Tool: model.ToolOpenCode, ChangeID: "user", SessionID: "session", Project: "/recorded/project",
				Known: tt.known, LinesAdded: tt.added, LinesRemoved: tt.removed, UpdatedAt: time.UnixMilli(updated).UTC(),
			}
			if len(obs.Events) != 0 || len(obs.Activity) != 0 || obs.Checkpoint != nil || !reflect.DeepEqual(obs.CodeChanges, []model.CodeChange{want}) {
				t.Fatalf("updated observation = %+v, want code change %+v without usage replay", obs, want)
			}
			again := incremental(t, src, first.Checkpoint)
			if !reflect.DeepEqual(obs, again) {
				t.Fatalf("repeat changed snapshot: %+v then %+v", obs, again)
			}
			afterDB, err := os.ReadFile(src.Path)
			if err != nil {
				t.Fatal(err)
			}
			afterWAL, err := os.ReadFile(src.Path + "-wal")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeDB, afterDB) || !bytes.Equal(beforeWAL, afterWAL) {
				t.Fatal("collection modified source database or WAL bytes")
			}
		})
	}
}

func TestCodeChangesInvalidCountsKeepUsageCheckpoint(t *testing.T) {
	db, src := modernDB(t)
	execSource(t, db, `CREATE TABLE session(id TEXT PRIMARY KEY, directory TEXT NOT NULL)`)
	execSource(t, db, `INSERT INTO session VALUES('session','/recorded/project')`)
	tests := []struct {
		name, diffs string
		invalid     bool
	}{
		{"missing", "", false},
		{"null", "null", false},
		{"empty", "[]", false},
		{"object", `{}`, true},
		{"scalar entry", `["private content"]`, true},
		{"null entry", `[null]`, true},
		{"missing counter", `[{"additions":2}]`, true},
		{"string counter", `[{"additions":"2","deletions":0}]`, true},
		{"float counter", `[{"additions":2.0,"deletions":0}]`, true},
		{"negative", `[{"additions":2,"deletions":-1}]`, true},
		{"value overflow", `[{"additions":9223372036854775808,"deletions":0}]`, true},
		{"sum overflow", `[{"additions":9223372036854775807,"deletions":0},{"additions":1,"deletions":0}]`, true},
		{"removed overflow", `[{"additions":0,"deletions":9223372036854775807},{"additions":0,"deletions":1}]`, true},
	}
	invalid := 0
	for i, tt := range tests {
		raw := `{"role":"user"}`
		if tt.diffs != "" {
			raw = `{"role":"user","summary":{"diffs":` + tt.diffs + `}}`
		}
		addModernMessage(t, db, int64(i+1), tt.name, "session", raw)
		if tt.invalid {
			invalid++
		}
	}
	addModernMessage(t, db, int64(len(tests)+1), "valid boundary", "session", fmt.Sprintf(`{"role":"user","summary":{"diffs":[{"additions":%d,"deletions":%d}]}}`, int64(math.MaxInt64), int64(math.MaxInt64)))
	addModernMessage(t, db, int64(len(tests)+2), "assistant", "session", modernData("assistant", 1730000000001, 17))
	obs, err := (Adapter{}).CollectIncremental(context.Background(), src, nil)
	wantDiagnostic := fmt.Sprintf("opencode: invalid line-count snapshots in %d user messages", invalid)
	if err == nil || err.Error() != wantDiagnostic {
		t.Fatalf("diagnostic = %v, want %q", err, wantDiagnostic)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatal("diagnostic leaked source content")
	}
	if len(obs.Events) != 1 || obs.Events[0].TotalTokens != 17 || len(obs.CodeChanges) != len(tests)+1 {
		t.Fatalf("optional malformed counts lost usage or snapshots: %+v", obs)
	}
	wantPending(t, obs.Checkpoint, int64(len(tests)+2))
	for i, change := range obs.CodeChanges[:len(tests)] {
		if change.ChangeID != tests[i].name || change.Known || change.LinesAdded != 0 || change.LinesRemoved != 0 {
			t.Fatalf("unavailable snapshot = %+v", change)
		}
	}
	last := obs.CodeChanges[len(tests)]
	if !last.Known || last.LinesAdded != math.MaxInt64 || last.LinesRemoved != math.MaxInt64 {
		t.Fatalf("valid integer boundary = %+v", last)
	}
	// Repair remains visible after the token checkpoint has already advanced.
	execSource(t, db, `UPDATE message SET data='{"role":"user","summary":{"diffs":[{"additions":1,"deletions":2}]}}' WHERE id='negative'`)
	repaired, err := (Adapter{}).CollectIncremental(context.Background(), src, obs.Checkpoint)
	if err == nil || len(repaired.Events) != 0 || repaired.Checkpoint != nil || len(repaired.CodeChanges) != len(tests)+1 {
		t.Fatalf("repair replay = %+v, %v", repaired, err)
	}
	change := repaired.CodeChanges[9]
	if change.ChangeID != "negative" || !change.Known || change.LinesAdded != 1 || change.LinesRemoved != 2 {
		t.Fatalf("repaired snapshot = %+v", change)
	}
}

func TestCodeChangesUnsupportedSchemaKeepsUsage(t *testing.T) {
	for _, schema := range []string{"legacy", "no session", "no directory"} {
		t.Run(schema, func(t *testing.T) {
			db, src := modernDB(t)
			switch schema {
			case "legacy":
				execSource(t, db, `ALTER TABLE message DROP COLUMN time_created`)
				execSource(t, db, `ALTER TABLE message DROP COLUMN time_updated`)
			case "no directory":
				execSource(t, db, `CREATE TABLE session(id TEXT PRIMARY KEY, version TEXT)`)
			}
			execSource(t, db, `INSERT INTO message(id,session_id,data) VALUES('assistant','session',?)`, modernData("assistant", 1730000000001, 17))
			obs := incremental(t, src, nil)
			if len(obs.Events) != 1 || obs.Events[0].TotalTokens != 17 || len(obs.CodeChanges) != 0 || obs.Checkpoint == nil {
				t.Fatalf("unsupported count schema affected usage: %+v", obs)
			}
		})
	}
}
