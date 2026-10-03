package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func codeChange(id string, added, removed int64) model.CodeChange {
	return model.CodeChange{
		Tool: model.ToolOpenCode, ChangeID: id, SessionID: "session", Project: "/project",
		Known: true, LinesAdded: added, LinesRemoved: removed,
		UpdatedAt: time.UnixMilli(1_750_000_000_123), ObservedTime: time.Unix(1_750_000_010, 0),
	}
}

func TestCodeChangesLatestSnapshot(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	initial := codeChange("message", 20, 8)
	c := initial
	for _, step := range []struct {
		name   string
		change func()
		writes int
		want   CodeChangeSummary
	}{
		{"first", func() {}, 1, CodeChangeSummary{20, 8, 1, 0}},
		{"repeat observation", func() { c.ObservedTime = c.ObservedTime.Add(time.Hour) }, 0, CodeChangeSummary{20, 8, 1, 0}},
		{"undo one millisecond later", func() { c.UpdatedAt = c.UpdatedAt.Add(time.Millisecond); c.LinesAdded, c.LinesRemoved = 5, 2 }, 1, CodeChangeSummary{5, 2, 1, 0}},
		{"stale positive", func() { c = initial; c.LinesAdded = 1000 }, 0, CodeChangeSummary{5, 2, 1, 0}},
		{"equal version correction", func() { c.UpdatedAt = initial.UpdatedAt.Add(time.Millisecond); c.LinesAdded, c.LinesRemoved = 4, 2 }, 1, CodeChangeSummary{4, 2, 1, 0}},
		{"newer unknown", func() {
			c.UpdatedAt = initial.UpdatedAt.Add(2 * time.Millisecond)
			c.Known = false
			c.LinesAdded, c.LinesRemoved = 0, 0
		}, 1, CodeChangeSummary{0, 0, 0, 1}},
		{"stale known cannot restore counts", func() { c = initial }, 0, CodeChangeSummary{0, 0, 0, 1}},
		{"known zero", func() { c.UpdatedAt = initial.UpdatedAt.Add(3 * time.Millisecond); c.LinesAdded, c.LinesRemoved = 0, 0 }, 1, CodeChangeSummary{0, 0, 1, 0}},
	} {
		t.Run(step.name, func(t *testing.T) {
			step.change()
			got, err := st.ApplyBatch(ctx, ObservationBatch{CodeChanges: []model.CodeChange{c}})
			if err != nil || got != (Applied{CodeChanges: step.writes}) {
				t.Fatalf("applied=%+v err=%v", got, err)
			}
			if summary, err := st.SessionCodeChanges(ctx, c.Tool, c.SessionID, c.Project); err != nil || summary != step.want {
				t.Fatalf("summary=%+v want=%+v err=%v", summary, step.want, err)
			}
			if step.name == "repeat observation" {
				var observed int64
				if err := st.db.QueryRow(`SELECT observed_time_unix FROM code_changes`).Scan(&observed); err != nil || observed != initial.ObservedTime.Unix() {
					t.Fatalf("identical snapshot churned observation time: %d, %v", observed, err)
				}
			}
		})
	}
	if summary, err := st.Summarize(ctx, Filter{}); err != nil || summary.Totals.Total != 0 || summary.Totals.Events != 0 {
		t.Fatalf("code changes entered token accounting: %+v, %v", summary, err)
	}
}

func TestCodeChangesSessionScope(t *testing.T) {
	st := openTemp(t)
	base := codeChange("same-message", 12, 4)
	base.SessionID, base.Project = "s_%", "/p_%"
	otherProject := base
	otherProject.ChangeID, otherProject.Project, otherProject.LinesAdded = "other-project", "/p-other", 3
	unknown := base
	unknown.ChangeID, unknown.Known, unknown.LinesAdded, unknown.LinesRemoved = "unknown", false, 0, 0
	zero := base
	zero.ChangeID, zero.LinesAdded, zero.LinesRemoved = "zero", 0, 0
	otherTool := base
	otherTool.Tool, otherTool.LinesAdded = model.ToolCodex, 100
	otherSession := base
	otherSession.ChangeID, otherSession.SessionID, otherSession.LinesAdded = "other-session", "s_AB", 1000
	if got, err := st.ApplyBatch(t.Context(), ObservationBatch{CodeChanges: []model.CodeChange{base, otherProject, unknown, zero, otherTool, otherSession}}); err != nil || got.CodeChanges != 6 {
		t.Fatalf("applied=%+v err=%v", got, err)
	}
	for _, tc := range []struct {
		tool, session, project string
		want                   CodeChangeSummary
	}{
		{base.Tool, base.SessionID, base.Project, CodeChangeSummary{12, 4, 2, 1}},
		{base.Tool, base.SessionID, "", CodeChangeSummary{15, 8, 3, 1}},
		{otherTool.Tool, base.SessionID, "", CodeChangeSummary{100, 4, 1, 0}},
		{base.Tool, "missing", "", CodeChangeSummary{}},
		{base.Tool, base.SessionID, "/p", CodeChangeSummary{}},
	} {
		if got, err := st.SessionCodeChanges(t.Context(), tc.tool, tc.session, tc.project); err != nil || got != tc.want {
			t.Fatalf("scope %q/%q/%q: got=%+v want=%+v err=%v", tc.tool, tc.session, tc.project, got, tc.want, err)
		}
	}
	for _, scope := range [][2]string{{"", "session"}, {base.Tool, ""}} {
		if _, err := st.SessionCodeChanges(t.Context(), scope[0], scope[1], ""); err == nil {
			t.Fatalf("accepted incomplete scope %v", scope)
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.SessionCodeChanges(cancelled, base.Tool, base.SessionID, ""); err == nil {
		t.Fatal("canceled summary succeeded")
	}
}

func TestCodeChangesInvalidSnapshotRollsBackBatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.CodeChange)
	}{
		{"tool", func(c *model.CodeChange) { c.Tool = "" }},
		{"change ID", func(c *model.CodeChange) { c.ChangeID = "" }},
		{"session ID", func(c *model.CodeChange) { c.SessionID = "" }},
		{"source time", func(c *model.CodeChange) { c.UpdatedAt = time.Time{} }},
		{"negative added", func(c *model.CodeChange) { c.LinesAdded = -1 }},
		{"negative removed", func(c *model.CodeChange) { c.LinesRemoved = -1 }},
		{"unknown positive", func(c *model.CodeChange) { c.Known = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openTemp(t)
			seedFailureCheckpoint(t, st)
			batch := failureBatch()
			bad := codeChange("bad", 1, 1)
			tc.mutate(&bad)
			batch.CodeChanges = append(batch.CodeChanges, bad)
			got, err := st.ApplyBatch(t.Context(), batch)
			assertBatchRolledBack(t, st, err, got)
		})
	}
}

func TestCodeChangesUpdateRollsBackWithCheckpoint(t *testing.T) {
	st := openTemp(t)
	seedFailureCheckpoint(t, st)
	initial := codeChange("c1", 20, 8)
	if _, err := st.ApplyBatch(t.Context(), ObservationBatch{CodeChanges: []model.CodeChange{initial}}); err != nil {
		t.Fatal(err)
	}
	execFailureSQL(t, st, `CREATE TRIGGER fail_checkpoint BEFORE INSERT ON source_checkpoints
		WHEN NEW.read_offset=2 BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	batch := failureBatch()
	batch.CodeChanges[0].Known = false
	batch.CodeChanges[0].LinesAdded, batch.CodeChanges[0].LinesRemoved = 0, 0
	batch.CodeChanges[0].UpdatedAt = initial.UpdatedAt.Add(time.Millisecond)
	if got, err := st.ApplyBatch(t.Context(), batch); err == nil || got != (Applied{}) {
		t.Fatalf("failed batch applied=%+v err=%v", got, err)
	}
	if got, err := st.SessionCodeChanges(t.Context(), initial.Tool, initial.SessionID, ""); err != nil || got != (CodeChangeSummary{20, 8, 1, 0}) {
		t.Fatalf("rollback lost previous known snapshot: %+v, %v", got, err)
	}
	if cp, err := st.Checkpoint(t.Context(), batch.Checkpoint.Tool, batch.Checkpoint.SourcePath); err != nil || cp == nil || cp.Offset != 1 {
		t.Fatalf("failed update advanced checkpoint: %+v, %v", cp, err)
	}
	execFailureSQL(t, st, `DROP TRIGGER fail_checkpoint`)
	if got, err := st.ApplyBatch(t.Context(), batch); err != nil || got != (Applied{Events: 2, Activity: 2, TurnContexts: 2, CodeChanges: 2}) {
		t.Fatalf("retry=%+v, %v", got, err)
	}
	if got, err := st.SessionCodeChanges(t.Context(), initial.Tool, initial.SessionID, ""); err != nil || got != (CodeChangeSummary{1, 0, 1, 1}) {
		t.Fatalf("retry summary=%+v, %v", got, err)
	}
}

func TestCodeChangesFreshAndV8Migration(t *testing.T) {
	for _, migration := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "v8 migration"}[migration], func(t *testing.T) {
			var st *Ledger
			var oldUsage []model.UsageEvent
			var oldActivity []model.ActivityEvent
			var oldCheckpoint *model.SourceCheckpoint
			var oldContexts int
			if migration {
				path := legacyDB(t, 8)
				db := rawDB(t, path)
				reader := &Reader{db: db, path: path}
				var err error
				oldUsage, err = reader.ListEvents(t.Context(), Filter{}, WithRaw())
				if err != nil {
					t.Fatal(err)
				}
				oldCheckpoint = &model.SourceCheckpoint{Tool: model.ToolOpenCode, SourcePath: "/synthetic/source", Offset: 19, Watermark: 23, State: `{"cursor":7}`}
				if err := upsertCheckpoint(t.Context(), db, *oldCheckpoint); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO source_checkpoints (tool,source_path,read_offset,state)
					VALUES ('claude-code','/synthetic/claude',41,'preserved')`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO activity_events (dedup_key,tool,kind,name,event_time_unix,observed_time_unix)
					VALUES ('legacy-activity','opencode','tool','edit',1750000000,1750000000)`); err != nil {
					t.Fatal(err)
				}
				oldActivity, err = reader.ListActivity(t.Context(), ActivityFilter{})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM usage_turn_context`).Scan(&oldContexts); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO aggregate_state (tool,acc_key,observed_time_unix,total_tokens) VALUES ('opencode','baseline',1750000000,37)`); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				st, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { st.Close() })
				usage, err := st.ListEvents(t.Context(), Filter{}, WithRaw())
				if err != nil || !reflect.DeepEqual(usage, oldUsage) {
					t.Fatalf("migration changed usage: %+v, %v", usage, err)
				}
				if cp, err := st.Checkpoint(t.Context(), oldCheckpoint.Tool, oldCheckpoint.SourcePath); err != nil || !reflect.DeepEqual(cp, oldCheckpoint) {
					t.Fatalf("migration changed checkpoint: %+v, %v", cp, err)
				}
				if cp, err := st.Checkpoint(t.Context(), model.ToolClaudeCode, "/synthetic/claude"); err != nil || cp == nil || cp.Offset != 41 || cp.State != "preserved" {
					t.Fatalf("migration changed unrelated checkpoint: %+v, %v", cp, err)
				}
				activity, err := st.ListActivity(t.Context(), ActivityFilter{})
				if err != nil || !reflect.DeepEqual(activity, oldActivity) {
					t.Fatalf("migration changed activity: %+v, %v", activity, err)
				}
				var contexts, baseline int
				if err := st.db.QueryRow(`SELECT COUNT(*) FROM usage_turn_context`).Scan(&contexts); err != nil || contexts != oldContexts {
					t.Fatalf("migration changed contexts: %d, %v", contexts, err)
				}
				if err := st.db.QueryRow(`SELECT total_tokens FROM aggregate_state WHERE tool='opencode' AND acc_key='baseline'`).Scan(&baseline); err != nil || baseline != 37 {
					t.Fatalf("migration changed accumulator: %d, %v", baseline, err)
				}
			} else {
				st = openTemp(t)
			}
			if version, err := readSchemaVersion(t.Context(), st.db); err != nil || version != 9 {
				t.Fatalf("schema=%d, %v", version, err)
			}
			if got, err := st.SessionCodeChanges(t.Context(), model.ToolOpenCode, "session", ""); err != nil || got != (CodeChangeSummary{}) {
				t.Fatalf("new table=%+v, %v", got, err)
			}
			c := codeChange("first", 7, 2)
			c.ObservedTime = time.Time{}
			if got, err := st.ApplyBatch(t.Context(), ObservationBatch{CodeChanges: []model.CodeChange{c}}); err != nil || got != (Applied{CodeChanges: 1}) {
				t.Fatalf("insert=%+v, %v", got, err)
			}
			if _, err := st.EnsureRollup(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotFile(t, st.path)
			verified, err := Verify(t.Context(), st.path)
			if err != nil || verified.State != VerificationOK || verified.RowCounts["code_changes"] != 1 {
				t.Fatalf("verification=%+v, %v", verified, err)
			}
			reader, err := OpenReadOnly(st.path)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := reader.SessionCodeChanges(t.Context(), c.Tool, c.SessionID, ""); err != nil || got != (CodeChangeSummary{7, 2, 1, 0}) {
				t.Fatalf("readonly summary=%+v, %v", got, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if after := snapshotFile(t, st.path); before != after {
				t.Fatal("verify/read-only summary changed ledger")
			}
			backup, err := Backup(t.Context(), st.path, st.path+".backup")
			if err != nil || backup.RowCounts["code_changes"] != 1 {
				t.Fatalf("backup=%+v, %v", backup, err)
			}
		})
	}
}

func TestCodeChangesVerifyRequiresSchema(t *testing.T) {
	for _, tc := range []struct{ name, statement, diagnostic string }{
		{"table", `DROP TABLE code_changes`, "missing table code_changes"},
		{"column", `ALTER TABLE code_changes RENAME COLUMN updated_at_unix_ms TO old_time`, "missing column updated_at_unix_ms"},
		{"index", `DROP INDEX idx_code_changes_session`, "missing index idx_code_changes_session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openTemp(t)
			execFailureSQL(t, st, tc.statement)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotFile(t, st.path)
			got, err := Verify(t.Context(), st.path)
			if err != nil || got.State != VerificationCorrupt || !strings.Contains(got.Reason, tc.diagnostic) {
				t.Fatalf("verification=%+v, %v", got, err)
			}
			if after := snapshotFile(t, st.path); before != after {
				t.Fatal("verification repaired corrupt schema")
			}
		})
	}
}
