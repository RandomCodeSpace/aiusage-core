package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestPrepareRestoreMigratesStageWithoutChangingInput(t *testing.T) {
	input := legacyDB(t, 3)
	before := snapshotFile(t, input)
	target := filepath.Join(t.TempDir(), "live.db")

	plan, err := PrepareRestore(context.Background(), input, target)
	if err != nil {
		t.Fatalf("PrepareRestore: %v", err)
	}
	defer plan.Cleanup()
	if plan.Verification.State != VerificationOK || plan.Verification.SchemaVersion != SchemaVersion {
		t.Fatalf("prepared restore = %+v", plan.Verification)
	}
	if after := snapshotFile(t, input); after != before {
		t.Fatalf("preparing an older backup changed the input: before=%+v after=%+v", before, after)
	}
	inputVerification, err := Verify(context.Background(), input)
	if err != nil || inputVerification.SchemaVersion != 3 || inputVerification.State != VerificationIncompatible {
		t.Fatalf("input after staged migration = %+v err=%v", inputVerification, err)
	}
}

func TestPrepareRestoreMigrationFailureLeavesInputAndTargetUntouched(t *testing.T) {
	input := legacyDB(t, 3)
	inputBefore := snapshotFile(t, input)
	target := filepath.Join(t.TempDir(), "live.db")
	seedSimpleRestoreStore(t, target, "live-before")
	targetBefore := snapshotFile(t, target)

	originalMigrations := migrations
	t.Cleanup(func() { migrations = originalMigrations })
	brokenMigrations := append([]migration(nil), originalMigrations...)
	for i := range brokenMigrations {
		if brokenMigrations[i].version == 4 {
			brokenMigrations[i] = migration{version: 4, statements: []string{
				`CREATE TABLE stage_change_must_roll_back (value INTEGER)`,
				`INSERT INTO missing_stage_table VALUES (1)`,
			}}
		}
	}
	migrations = brokenMigrations

	plan, err := PrepareRestore(context.Background(), input, target)
	if plan != nil {
		plan.Cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "migration v4") {
		t.Fatalf("PrepareRestore migration failure = %v", err)
	}
	if after := snapshotFile(t, input); after != inputBefore {
		t.Fatalf("failed staged migration changed input: before=%+v after=%+v", inputBefore, after)
	}
	if after := snapshotFile(t, target); after != targetBefore {
		t.Fatalf("failed staged migration changed target: before=%+v after=%+v", targetBefore, after)
	}
	stages, globErr := filepath.Glob(filepath.Join(filepath.Dir(target), ".restore-stage-*.db"))
	if globErr != nil || len(stages) != 0 {
		t.Fatalf("failed staged migration left stages=%v err=%v", stages, globErr)
	}
}

func TestPrepareRestoreRejectsNewerAndUnknownInputs(t *testing.T) {
	t.Run("newer", func(t *testing.T) {
		input := closedFreshStore(t)
		db := rawDB(t, input)
		if _, err := db.Exec(`UPDATE schema_meta SET value=? WHERE key='schema_version'`, SchemaVersion+1); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := snapshotFile(t, input)
		plan, err := PrepareRestore(context.Background(), input, filepath.Join(t.TempDir(), "target.db"))
		if plan != nil {
			plan.Cleanup()
		}
		if !errors.Is(err, ErrSchemaNewer) {
			t.Fatalf("newer restore error = %v", err)
		}
		if after := snapshotFile(t, input); after != before {
			t.Fatalf("newer restore changed input: before=%+v after=%+v", before, after)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		input := filepath.Join(t.TempDir(), "unknown.db")
		db := rawDB(t, input)
		if _, err := db.Exec(`CREATE TABLE another_application (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		plan, err := PrepareRestore(context.Background(), input, filepath.Join(t.TempDir(), "target.db"))
		if plan != nil {
			plan.Cleanup()
		}
		if err == nil || !strings.Contains(err.Error(), "no recognized aiusage schema") {
			t.Fatalf("unknown restore error = %v", err)
		}
	})
}

func TestPrepareRestoreRejectsInvalidPaths(t *testing.T) {
	valid := closedFreshStore(t)
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.db")
	for _, tt := range []struct {
		name   string
		input  string
		target string
	}{
		{name: "empty input", input: "", target: filepath.Join(directory, "target.db")},
		{name: "empty target", input: valid, target: ""},
		{name: "missing input", input: missing, target: filepath.Join(directory, "target.db")},
		{name: "directory input", input: directory, target: filepath.Join(directory, "target.db")},
		{name: "same path", input: valid, target: valid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := PrepareRestore(context.Background(), tt.input, tt.target)
			if plan != nil {
				plan.Cleanup()
			}
			if err == nil {
				t.Fatal("PrepareRestore succeeded")
			}
		})
	}

	hardlink := filepath.Join(t.TempDir(), "same-file.db")
	if err := os.Link(valid, hardlink); err != nil {
		t.Fatal(err)
	}
	if plan, err := PrepareRestore(context.Background(), valid, hardlink); err == nil {
		plan.Cleanup()
		t.Fatal("PrepareRestore accepted hard-linked target")
	}

	blockedParent := filepath.Join(t.TempDir(), "blocked-parent")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if plan, err := PrepareRestore(context.Background(), valid, filepath.Join(blockedParent, "target.db")); err == nil {
		plan.Cleanup()
		t.Fatal("PrepareRestore accepted target under a file")
	}
}

func TestApplyRestorePreservesCompleteSnapshotAndSafetyBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	snapshotDB := filepath.Join(dir, "snapshot-source.db")
	seedCompleteRestoreStore(t, snapshotDB, "restored")
	input := filepath.Join(dir, "input-backup.db")
	if _, err := Backup(ctx, snapshotDB, input); err != nil {
		t.Fatal(err)
	}
	inputBefore := snapshotFile(t, input)

	target := filepath.Join(dir, "live.db")
	seedSimpleRestoreStore(t, target, "live-before")
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := ApplyRestore(ctx, plan, target, true)
	if err != nil {
		t.Fatalf("ApplyRestore: %v", err)
	}
	if !result.TargetUsable || result.RolledBack || result.SafetyBackupPath == "" || !result.EventsMayBeAbsent {
		t.Fatalf("restore result = %+v", result)
	}
	if after := snapshotFile(t, input); after != inputBefore {
		t.Fatalf("restore changed input backup: before=%+v after=%+v", inputBefore, after)
	}
	verification, err := Verify(ctx, target)
	if err != nil || verification.State != VerificationOK {
		t.Fatalf("restored target = %+v err=%v", verification, err)
	}
	wantCounts := map[string]int64{
		"usage_events":       2,
		"activity_events":    1,
		"usage_turn_context": 1,
		"aggregate_state":    1,
		"source_checkpoints": 1,
	}
	for table, want := range wantCounts {
		if got := verification.RowCounts[table]; got != want {
			t.Errorf("%s rows = %d, want %d", table, got, want)
		}
	}
	assertEventKeys(t, target, []string{"restored-aggregate", "restored-observation"})
	assertEventKeys(t, result.SafetyBackupPath, []string{"live-before"})
}

func TestApplyRestoreRequiresReplaceBeforeChangingTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "source")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedSimpleRestoreStore(t, target, "target")
	before := snapshotFile(t, target)
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	if _, err := ApplyRestore(ctx, plan, target, false); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("ApplyRestore without --replace = %v", err)
	}
	if after := snapshotFile(t, target); after != before {
		t.Fatalf("replace refusal changed target: before=%+v after=%+v", before, after)
	}
}

func TestApplyRestoreRejectsMissingOrChangedPlan(t *testing.T) {
	if _, err := ApplyRestore(context.Background(), nil, filepath.Join(t.TempDir(), "target.db"), false); err == nil {
		t.Fatal("ApplyRestore accepted nil plan")
	}

	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "source")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareRestore(ctx, input, filepath.Join(dir, "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	db := rawDB(t, plan.StagePath)
	if _, err := db.Exec(`DROP TRIGGER trg_events_no_delete`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyRestore(ctx, plan, filepath.Join(dir, "target.db"), false); err == nil || !strings.Contains(err.Error(), "changed after preparation") {
		t.Fatalf("changed plan error = %v", err)
	}
}

func TestApplyRestoreRejectsDirectoryTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "source")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target-directory")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if _, err := ApplyRestore(ctx, plan, target, true); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory target error = %v", err)
	}
}

func TestApplyRestoreDefaultsHooksAndRejectsUninspectableTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "source")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	preparedTarget := filepath.Join(dir, "prepared-target.db")
	plan, err := PrepareRestore(ctx, input, preparedTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	actualTarget := filepath.Join(dir, "actual-target.db")
	result, err := applyRestore(ctx, plan, actualTarget, false, restoreHooks{})
	if err != nil || !result.TargetUsable {
		t.Fatalf("default restore hooks = %+v err=%v", result, err)
	}

	blockedParent := filepath.Join(dir, "blocked-parent")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyRestore(ctx, plan, filepath.Join(blockedParent, "target.db"), false, restoreHooks{}); err == nil || !strings.Contains(err.Error(), "inspect restore target") {
		t.Fatalf("uninspectable restore target error = %v", err)
	}
}

func TestApplyRestoreTransferFailureLeavesLiveTargetUsable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "source")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedSimpleRestoreStore(t, target, "original")
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := applyRestore(ctx, plan, target, true, restoreHooks{
		transfer: func(context.Context, string, string) error { return errors.New("injected disk full") },
	})
	if err == nil || !strings.Contains(err.Error(), "injected disk full") {
		t.Fatalf("transfer failure = %v", err)
	}
	if !result.TargetUsable || result.SafetyBackupPath == "" {
		t.Fatalf("failed transfer result = %+v", result)
	}
	assertEventKeys(t, target, []string{"original"})
}

func TestApplyRestoreTransferFailureRemovesNewTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "source")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "new-target.db")
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := applyRestore(ctx, plan, target, false, restoreHooks{
		transfer: func(_ context.Context, _, target string) error {
			if writeErr := os.WriteFile(target, []byte("partial"), 0o600); writeErr != nil {
				return writeErr
			}
			return errors.New("injected interruption")
		},
	})
	if err == nil || result.TargetUsable {
		t.Fatalf("new-target interruption = %+v err=%v", result, err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("interrupted new target remains: %v", statErr)
	}
}

func TestApplyRestorePostVerifyFailureRollsBackSafetyBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "replacement")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedSimpleRestoreStore(t, target, "original")
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := applyRestore(ctx, plan, target, true, restoreHooks{
		transfer: restoreOnline,
		postVerify: func(context.Context, string) (Verification, error) {
			return Verification{State: VerificationCorrupt, Reason: "injected post-copy failure"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected post-copy failure") {
		t.Fatalf("post-copy failure = %v", err)
	}
	if !result.TargetUsable || !result.RolledBack || result.SafetyBackupPath == "" {
		t.Fatalf("rollback result = %+v", result)
	}
	assertEventKeys(t, target, []string{"original"})
}

func TestApplyRestorePostVerifyErrorRollsBackSafetyBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "replacement")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedSimpleRestoreStore(t, target, "original")
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := applyRestore(ctx, plan, target, true, restoreHooks{
		postVerify: func(context.Context, string) (Verification, error) {
			return Verification{}, errors.New("injected verification I/O error")
		},
	})
	if err == nil || !result.TargetUsable || !result.RolledBack {
		t.Fatalf("post-verify error result = %+v err=%v", result, err)
	}
	assertEventKeys(t, target, []string{"original"})
}

func TestApplyRestoreSafetyBackupFailureLeavesTargetUsable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "replacement")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedSimpleRestoreStore(t, target, "original")
	if err := os.WriteFile(filepath.Join(dir, "backups"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := ApplyRestore(ctx, plan, target, true)
	if err == nil || !result.TargetUsable || !strings.Contains(err.Error(), "safety backup") {
		t.Fatalf("safety backup failure = %+v err=%v", result, err)
	}
	assertEventKeys(t, target, []string{"original"})
}

func TestApplyRestoreCanReplaceCorruptTargetWithoutSafetyCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "replacement")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(target, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := ApplyRestore(ctx, plan, target, true)
	if err != nil || !result.TargetUsable || result.SafetyBackupPath != "" || result.CorruptQuarantinePath == "" {
		t.Fatalf("replace corrupt target = %+v err=%v", result, err)
	}
	quarantined, readErr := os.ReadFile(filepath.Join(result.CorruptQuarantinePath, filepath.Base(target)))
	if readErr != nil || string(quarantined) != "not sqlite" {
		t.Fatalf("quarantined corrupt target = %q err=%v", quarantined, readErr)
	}
	assertEventKeys(t, target, []string{"replacement"})
}

func TestApplyRestoreFailureRestoresOriginalCorruptBundle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "replacement")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(target, []byte("original corrupt bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+"-wal", []byte("original wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := applyRestore(ctx, plan, target, true, restoreHooks{
		transfer: func(context.Context, string, string) error {
			return errors.New("injected restore interruption")
		},
	})
	if err == nil || result.TargetUsable || result.CorruptQuarantinePath != "" || !strings.Contains(err.Error(), "restored the original corrupt SQLite bundle") {
		t.Fatalf("corrupt rollback result = %+v err=%v", result, err)
	}
	for path, want := range map[string]string{
		target:          "original corrupt bytes",
		target + "-wal": "original wal",
	} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || string(got) != want {
			t.Fatalf("restored corrupt bundle %s = %q err=%v", path, got, readErr)
		}
	}
}

func TestRollbackFailureDiagnostics(t *testing.T) {
	ctx := context.Background()
	t.Run("missing safety backup", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "target.db")
		seedSimpleRestoreStore(t, target, "target")
		result, err := rollbackFailedRestore(ctx, RestoreResult{
			SafetyBackupPath: filepath.Join(t.TempDir(), "missing.db"),
		}, target, true, errors.New("original restore failure"))
		if err == nil || result.TargetUsable || !strings.Contains(err.Error(), "rollback") {
			t.Fatalf("missing safety rollback = %+v err=%v", result, err)
		}
	})

	t.Run("incompatible safety backup", func(t *testing.T) {
		dir := t.TempDir()
		safety := filepath.Join(dir, "safety.db")
		seedSimpleRestoreStore(t, safety, "safety")
		db := rawDB(t, safety)
		if _, err := db.Exec(`UPDATE schema_meta SET value=? WHERE key='schema_version'`, SchemaVersion+1); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "target.db")
		seedSimpleRestoreStore(t, target, "target")
		result, err := rollbackFailedRestore(ctx, RestoreResult{SafetyBackupPath: safety}, target, true, errors.New("original restore failure"))
		if err == nil || result.TargetUsable || !strings.Contains(err.Error(), "did not produce a usable target") {
			t.Fatalf("incompatible safety rollback = %+v err=%v", result, err)
		}
	})
}

func TestCorruptRestoreRollbackFailureDiagnostics(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.db")
	result, err := failAppliedRestore(context.Background(), RestoreResult{}, target, true, filepath.Join(t.TempDir(), "missing-quarantine"), errors.New("restore failed"))
	if err == nil || result.TargetUsable || !strings.Contains(err.Error(), "restoring the original corrupt SQLite bundle") {
		t.Fatalf("corrupt rollback diagnostic = %+v err=%v", result, err)
	}
	if err := restoreQuarantinedBundle(target, t.TempDir()); err == nil || !strings.Contains(err.Error(), "contains no SQLite bundle") {
		t.Fatalf("empty quarantine restore error = %v", err)
	}
}

func TestQuarantinePreservesBundleAndCreatesFreshDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := filepath.Join(dir, "usage.db")
	seedSimpleRestoreStore(t, target, "history")
	if err := os.WriteFile(target+"-wal", []byte("wal evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+"-shm", []byte("shm evidence"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Quarantine(ctx, target)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if !result.TargetUsable || !result.HistoricalCompletenessUnknown || result.Verification.State != VerificationOK {
		t.Fatalf("quarantine result = %+v", result)
	}
	if result.Verification.RowCounts["usage_events"] != 0 {
		t.Fatalf("fresh database has %d usage rows", result.Verification.RowCounts["usage_events"])
	}
	for path, want := range map[string]string{
		filepath.Join(result.QuarantinePath, "usage.db-wal"): "wal evidence",
		filepath.Join(result.QuarantinePath, "usage.db-shm"): "shm evidence",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("quarantined %s = %q err=%v, want %q", path, got, err, want)
		}
	}
	assertEventKeys(t, filepath.Join(result.QuarantinePath, "usage.db"), []string{"history"})
}

func TestQuarantineMoveFailureRollsBackWholeBundle(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "usage.db")
	seedSimpleRestoreStore(t, target, "history")
	if err := os.WriteFile(target+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainBefore := snapshotFile(t, target)

	renameCalls := 0
	_, err := quarantine(context.Background(), target, quarantineHooks{
		rename: func(old, new string) error {
			renameCalls++
			if renameCalls == 2 {
				return errors.New("injected move failure")
			}
			return os.Rename(old, new)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected move failure") {
		t.Fatalf("quarantine failure = %v", err)
	}
	if after := snapshotFile(t, target); after != mainBefore {
		t.Fatalf("failed quarantine changed main database: before=%+v after=%+v", mainBefore, after)
	}
	if got, err := os.ReadFile(target + "-wal"); err != nil || string(got) != "wal" {
		t.Fatalf("failed quarantine lost WAL: %q err=%v", got, err)
	}
}

func TestQuarantineRejectsInvalidTargets(t *testing.T) {
	if _, err := Quarantine(context.Background(), filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("Quarantine accepted missing target")
	}
	directory := t.TempDir()
	if _, err := Quarantine(context.Background(), directory); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Quarantine directory error = %v", err)
	}
}

func TestQuarantineReportsRollbackFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "usage.db")
	seedSimpleRestoreStore(t, target, "history")
	if err := os.WriteFile(target+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	moveCalls := 0
	_, err := quarantine(context.Background(), target, quarantineHooks{
		rename: func(old, new string) error {
			moveCalls++
			switch moveCalls {
			case 1:
				return os.Rename(old, new)
			case 2:
				return errors.New("injected bundle move failure")
			default:
				return errors.New("injected rollback failure")
			}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "rollback failed") || !strings.Contains(err.Error(), "injected rollback failure") {
		t.Fatalf("quarantine rollback failure = %v", err)
	}
}

func TestQuarantineFreshCreationFailureRestoresOriginal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "usage.db")
	seedSimpleRestoreStore(t, target, "history")
	_, err := quarantine(context.Background(), target, quarantineHooks{
		rename: func(old, new string) error {
			if err := os.Rename(old, new); err != nil {
				return err
			}
			if old == target {
				return os.Mkdir(target, 0o700)
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "create fresh database") {
		t.Fatalf("fresh creation failure = %v", err)
	}
	assertEventKeys(t, target, []string{"history"})
}

func TestRecoveryPathsAvoidCollisions(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "usage.db")
	now := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)

	firstSafety, err := nextSafetyBackupPath(target, SchemaVersion, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(firstSafety), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstSafety, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondSafety, err := nextSafetyBackupPath(target, SchemaVersion, now)
	if err != nil || secondSafety != strings.TrimSuffix(firstSafety, ".db")+"-2.db" {
		t.Fatalf("second safety path = %s err=%v", secondSafety, err)
	}

	firstQuarantine, err := nextQuarantinePath(target, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(firstQuarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	secondQuarantine, err := nextQuarantinePath(target, now)
	if err != nil || secondQuarantine != firstQuarantine+"-2" {
		t.Fatalf("second quarantine path = %s err=%v", secondQuarantine, err)
	}
}

func seedSimpleRestoreStore(t *testing.T, path, key string) {
	t.Helper()
	ledger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.InsertEvents(context.Background(), []model.UsageEvent{
		ev(key, model.ToolCodex, time.Unix(1_750_000_000, 0), 10),
	}); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedCompleteRestoreStore(t *testing.T, path, prefix string) {
	t.Helper()
	ctx := context.Background()
	ledger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_750_000_000, 0)
	aggregateEvent := ev(prefix+"-aggregate", model.ToolHermes, at, 20)
	snapshot := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "session", Model: "m", SessionID: "session",
		ObservedTime: at, InputTokens: 20, TotalTokens: 20, SourcePath: "/source/session",
	}
	checkpoint := &model.SourceCheckpoint{
		Tool: model.ToolHermes, SourcePath: "/source/session", Size: 100, Offset: 100, State: `{"baseline":20}`,
	}
	if _, err := ledger.ApplySnapshot(ctx, []model.UsageEvent{aggregateEvent}, snapshot, checkpoint); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	observation := ev(prefix+"-observation", model.ToolClaudeCode, at.Add(time.Second), 30)
	batch := ObservationBatch{
		Events: []model.UsageEvent{observation},
		Activity: []model.ActivityEvent{{
			Tool: model.ToolClaudeCode, Kind: model.ActivityTool, Name: "Read", EventTime: at.Add(time.Second),
			ObservedTime: at.Add(time.Second), UsageDedupKey: observation.DedupKey, CallsInTurn: 1, DedupKey: prefix + "-activity",
		}},
		TurnContexts: []model.TurnContext{{
			UsageDedupKey: observation.DedupKey, Tool: model.ToolClaudeCode, Dimension: model.DimensionSkill,
			Value: "restore-test", EventTime: at.Add(time.Second), ObservedTime: at.Add(time.Second),
		}},
	}
	if _, err := ledger.ApplyBatch(ctx, batch); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertEventKeys(t *testing.T, path string, want []string) {
	t.Helper()
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.ListEvents(context.Background(), Filter{})
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(events))
	for i := range events {
		got[i] = events[i].DedupKey
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event keys = %v, want %v", got, want)
	}
}
