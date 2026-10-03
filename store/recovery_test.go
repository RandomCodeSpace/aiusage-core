package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestVerifyHealthyDatabaseWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	ledger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.InsertEvents(context.Background(), []model.UsageEvent{
		ev("verify-healthy", model.ToolCodex, time.Unix(1_750_000_000, 0), 12),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	before := snapshotFile(t, path)
	got, err := Verify(context.Background(), path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.State != VerificationOK || !got.Compatible || !got.IntegrityOK || !got.ForeignKeysOK || !got.ApplicationSchemaOK || got.RollupStale {
		t.Fatalf("verification = %+v, want healthy current database", got)
	}
	if got.RowCounts["usage_events"] != 1 {
		t.Fatalf("usage row count = %d, want 1", got.RowCounts["usage_events"])
	}
	after := snapshotFile(t, path)
	if before != after {
		t.Fatalf("Verify mutated database: before=%+v after=%+v", before, after)
	}
}

func TestVerifyRecognizedOlderSchemas(t *testing.T) {
	for version := 1; version < SchemaVersion; version++ {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			got, err := Verify(context.Background(), legacyDB(t, version))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.State != VerificationIncompatible || got.SchemaVersion != version || !got.ApplicationSchemaOK || got.Compatible {
				t.Fatalf("verification = %+v, want sound recognized v%d", got, version)
			}
			if got.RowCounts["usage_events"] != 1 {
				t.Fatalf("usage row count = %d, want 1", got.RowCounts["usage_events"])
			}
			if got.StateError() == nil {
				t.Fatal("incompatible verification must produce a non-zero state error")
			}
		})
	}
}

func TestVerifyDistinguishesRecoveryStates(t *testing.T) {
	t.Run("newer", func(t *testing.T) {
		path := closedFreshStore(t)
		db := rawDB(t, path)
		newer := SchemaVersion + 1
		if _, err := db.Exec(`UPDATE schema_meta SET value=? WHERE key='schema_version'`, strconv.Itoa(newer)); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		got, err := Verify(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != VerificationIncompatible || got.SchemaVersion != newer {
			t.Fatalf("verification = %+v, want incompatible newer schema", got)
		}
		if !errors.Is(got.StateError(), ErrSchemaNewer) {
			t.Fatalf("StateError = %v, want ErrSchemaNewer", got.StateError())
		}
	})

	t.Run("rollup drift", func(t *testing.T) {
		path := closedFreshStore(t)
		db := rawDB(t, path)
		if _, err := db.Exec(`
			INSERT INTO usage_events
			(dedup_key, tool, event_time_unix, observed_time_unix, total_tokens)
			VALUES ('drift', 'codex', 1750000000, 1750000000, 1)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		got, err := Verify(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != VerificationRepairable || !got.RollupStale || !got.Compatible {
			t.Fatalf("verification = %+v, want repairable rollup drift", got)
		}
	})

	t.Run("missing append-only trigger", func(t *testing.T) {
		path := closedFreshStore(t)
		db := rawDB(t, path)
		if _, err := db.Exec(`DROP TRIGGER trg_events_no_delete`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		got, err := Verify(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != VerificationCorrupt || !strings.Contains(got.Reason, "trg_events_no_delete") {
			t.Fatalf("verification = %+v, want missing-trigger corruption", got)
		}
	})

	t.Run("foreign key violation", func(t *testing.T) {
		path := closedFreshStore(t)
		db := rawDB(t, path)
		if _, err := db.Exec(`
			PRAGMA foreign_keys=OFF;
			CREATE TABLE verify_parent (id INTEGER PRIMARY KEY);
			CREATE TABLE verify_child (parent_id INTEGER REFERENCES verify_parent(id));
			INSERT INTO verify_child(parent_id) VALUES (42);`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		got, err := Verify(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != VerificationCorrupt || got.ForeignKeysOK || len(got.ForeignKeyErrors) != 1 {
			t.Fatalf("verification = %+v, want foreign-key corruption", got)
		}
	})

	t.Run("physical corruption", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-a-database.db")
		if err := os.WriteFile(path, []byte("this is not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Verify(context.Background(), path)
		if err != nil {
			t.Fatalf("Verify returned operational error for readable corrupt file: %v", err)
		}
		if got.State != VerificationCorrupt || len(got.IntegrityErrors) == 0 {
			t.Fatalf("verification = %+v, want physical corruption", got)
		}
	})
}

func TestVerifyDetectsAuthoritativeSchemaDamage(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{name: "missing table", sql: `DROP TABLE source_checkpoints`, want: "missing table source_checkpoints"},
		{name: "missing index", sql: `DROP INDEX idx_activity_event_time`, want: "missing index idx_activity_event_time"},
		{
			name: "wrong index columns",
			sql: `DROP INDEX idx_activity_tool_name;
				CREATE INDEX idx_activity_tool_name ON activity_events(name, tool)`,
			want: "idx_activity_tool_name has columns",
		},
		{
			name: "conditional append-only trigger",
			sql: `DROP TRIGGER trg_events_no_update;
				CREATE TRIGGER trg_events_no_update BEFORE UPDATE ON usage_events WHEN 0
				BEGIN SELECT RAISE(ABORT, 'append-only'); END`,
			want: "does not enforce UPDATE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := closedFreshStore(t)
			db := rawDB(t, path)
			if _, err := db.Exec(tt.sql); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := Verify(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != VerificationCorrupt || !strings.Contains(got.Reason, tt.want) {
				t.Fatalf("verification = %+v, want corruption containing %q", got, tt.want)
			}
		})
	}
}

func TestVerifyRejectsInvalidPathsWithoutCreatingThem(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.db")
	for _, tt := range []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "missing", path: missing},
		{name: "directory", path: directory},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Verify(context.Background(), tt.path); err == nil {
				t.Fatalf("Verify(%q) succeeded", tt.path)
			}
		})
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Verify created missing path: %v", err)
	}

	path := closedFreshStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Verify(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify cancelled error = %v", err)
	}
}

func TestVerificationStateErrorsAndMalformedMetadata(t *testing.T) {
	if err := (Verification{State: VerificationOK}).StateError(); err != nil {
		t.Fatalf("ok StateError = %v", err)
	}
	if err := (Verification{State: VerificationRepairable, Reason: "rollup drift"}).StateError(); err == nil {
		t.Fatal("repairable StateError returned nil")
	}

	t.Run("invalid version value", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid-version.db")
		db := rawDB(t, path)
		if _, err := db.Exec(`CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
			INSERT INTO schema_meta(key, value) VALUES ('schema_version', 'not-a-version')`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := Verify(context.Background(), path)
		if err != nil || got.State != VerificationIncompatible || !strings.Contains(got.Reason, "not recognized") {
			t.Fatalf("invalid version verification = %+v err=%v", got, err)
		}
	})

	t.Run("unreadable metadata shape", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unreadable-metadata.db")
		db := rawDB(t, path)
		if _, err := db.Exec(`CREATE TABLE schema_meta (key TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := Verify(context.Background(), path)
		if err != nil || got.State != VerificationCorrupt || !strings.Contains(got.Reason, "metadata is unreadable") {
			t.Fatalf("unreadable metadata verification = %+v err=%v", got, err)
		}
	})
}

func TestBackupIncludesLiveWALAndPublishesStandalone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "usage.db")
	destination := filepath.Join(dir, "backups", "usage-backup.db")
	ledger, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if _, err := ledger.InsertEvents(ctx, []model.UsageEvent{
		ev("wal-only", model.ToolCodex, time.Unix(1_750_000_000, 0), 99),
	}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(source + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("source WAL missing or empty: info=%v err=%v", info, err)
	}

	result, err := Backup(ctx, source, destination)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if result.Path != destination || result.State != VerificationOK || result.RowCounts["usage_events"] != 1 {
		t.Fatalf("backup result = %+v", result)
	}
	if result.SizeBytes <= 0 || len(result.SHA256) != 64 {
		t.Fatalf("backup identity = size %d sha %q", result.SizeBytes, result.SHA256)
	}
	for _, sidecar := range []string{destination + "-wal", destination + "-shm"} {
		if _, err := os.Stat(sidecar); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("published backup depends on sidecar %s: %v", sidecar, err)
		}
	}
	if digest := snapshotFile(t, destination).Digest; digest != result.SHA256 {
		t.Fatalf("published digest = %s, result = %s", digest, result.SHA256)
	}

	reader, err := OpenReadOnly(destination)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	events, err := reader.ListEvents(ctx, Filter{})
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].DedupKey != "wal-only" {
		t.Fatalf("backup events = %+v, want WAL-only row", events)
	}
	if _, err := ledger.InsertEvents(ctx, []model.UsageEvent{
		ev("collector-continues", model.ToolCodex, time.Unix(1_750_000_001, 0), 1),
	}); err != nil {
		t.Fatalf("source stopped accepting writes after live backup: %v", err)
	}
}

func TestBackupRefusalsPublishNoFinal(t *testing.T) {
	source := closedFreshStore(t)

	t.Run("existing destination", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "existing.db")
		if err := os.WriteFile(destination, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Backup(context.Background(), source, destination); err == nil {
			t.Fatal("Backup overwrote an existing destination")
		}
		got, err := os.ReadFile(destination)
		if err != nil || string(got) != "keep" {
			t.Fatalf("existing destination changed: %q err=%v", got, err)
		}
	})

	t.Run("same file", func(t *testing.T) {
		if _, err := Backup(context.Background(), source, source); err == nil || !strings.Contains(err.Error(), "same path") {
			t.Fatalf("Backup same file error = %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "cancelled.db")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Backup(ctx, source, destination); !errors.Is(err, context.Canceled) {
			t.Fatalf("Backup cancellation error = %v", err)
		}
		if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancelled backup published final: %v", err)
		}
	})

	t.Run("failed verification", func(t *testing.T) {
		broken := closedFreshStore(t)
		db := rawDB(t, broken)
		if _, err := db.Exec(`DROP TRIGGER trg_events_no_update`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), "broken.db")
		if _, err := Backup(context.Background(), broken, destination); err == nil || !strings.Contains(err.Error(), "verification failed") {
			t.Fatalf("Backup broken schema error = %v", err)
		}
		if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed verification published final: %v", err)
		}
	})

	t.Run("disk full while publishing", func(t *testing.T) {
		directory := t.TempDir()
		destination := filepath.Join(directory, "disk-full.db")
		_, err := backupWithHooks(context.Background(), source, destination, nil, backupHooks{
			syncFile: func(string) error { return syscall.ENOSPC },
		})
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("Backup disk-full error = %v, want ENOSPC", err)
		}
		if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disk-full backup published final: %v", err)
		}
		partials, err := filepath.Glob(filepath.Join(directory, ".disk-full.db.partial-*"))
		if err != nil || len(partials) != 0 {
			t.Fatalf("disk-full backup left partials %v: %v", partials, err)
		}
	})

	t.Run("busy exhaustion", func(t *testing.T) {
		directory := t.TempDir()
		destination := filepath.Join(directory, "busy.db")
		_, err := backupWithHooks(context.Background(), source, destination, nil, backupHooks{
			copyOnline: func(context.Context, string, string, func(int64)) error {
				return codedSQLiteError(sqlite3.SQLITE_BUSY)
			},
		})
		if err == nil || !isBusySQLiteError(err) {
			t.Fatalf("Backup busy error = %v", err)
		}
		if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("busy backup published final: %v", err)
		}
		partials, err := filepath.Glob(filepath.Join(directory, ".busy.db.partial-*"))
		if err != nil || len(partials) != 0 {
			t.Fatalf("busy backup left partials %v: %v", partials, err)
		}
	})

	t.Run("cancellation before publish", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "cancel-before-publish.db")
		ctx, cancel := context.WithCancel(context.Background())
		_, err := backupWithHooks(ctx, source, destination, nil, backupHooks{
			syncFile: func(string) error {
				cancel()
				return nil
			},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Backup late cancellation error = %v", err)
		}
		if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("late cancellation published final: %v", err)
		}
	})

	t.Run("destination race", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "raced.db")
		_, err := backupWithHooks(context.Background(), source, destination, nil, backupHooks{
			syncFile: func(path string) error {
				if err := syncFile(path); err != nil {
					return err
				}
				return os.WriteFile(destination, []byte("winner"), 0o600)
			},
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("Backup destination race error = %v", err)
		}
		contents, readErr := os.ReadFile(destination)
		if readErr != nil || string(contents) != "winner" {
			t.Fatalf("Backup changed raced destination: %q err=%v", contents, readErr)
		}
	})
}

func TestBackupRejectsInvalidPaths(t *testing.T) {
	source := closedFreshStore(t)
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.db")
	for _, tt := range []struct {
		name        string
		source      string
		destination string
	}{
		{name: "empty source", source: "", destination: filepath.Join(directory, "empty-source.db")},
		{name: "empty destination", source: source, destination: ""},
		{name: "missing source", source: missing, destination: filepath.Join(directory, "missing-source.db")},
		{name: "directory source", source: directory, destination: filepath.Join(directory, "directory-source.db")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Backup(context.Background(), tt.source, tt.destination); err == nil {
				t.Fatal("Backup succeeded")
			}
		})
	}

	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(context.Background(), source, filepath.Join(blockedParent, "backup.db")); err == nil {
		t.Fatal("Backup succeeded with a file as destination directory")
	}

	hardlink := filepath.Join(t.TempDir(), "source-link.db")
	if err := os.Link(source, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(context.Background(), source, hardlink); err == nil || !strings.Contains(err.Error(), "same file") {
		t.Fatalf("Backup hard-link refusal = %v", err)
	}
}

func TestBackupReportsProgress(t *testing.T) {
	source := closedFreshStore(t)
	destination := filepath.Join(t.TempDir(), "progress.db")
	var updates []BackupProgress
	if _, err := BackupWithProgress(context.Background(), source, destination, func(progress BackupProgress) {
		updates = append(updates, progress)
	}); err != nil {
		t.Fatal(err)
	}

	wantPhases := []string{BackupPhaseCopying, BackupPhaseVerifying, BackupPhasePublishing}
	phaseIndex := 0
	var copiedBatches int64
	for _, update := range updates {
		if update.Phase == BackupPhaseCopying && update.Batches > copiedBatches {
			copiedBatches = update.Batches
		}
		if phaseIndex < len(wantPhases) && update.Phase == wantPhases[phaseIndex] {
			phaseIndex++
		}
	}
	if phaseIndex != len(wantPhases) || copiedBatches == 0 {
		t.Fatalf("progress = %+v, want ordered phases %v and a completed copy batch", updates, wantPhases)
	}
}

func TestBackupPreservesSoundIncompatibleSchemas(t *testing.T) {
	t.Run("newer", func(t *testing.T) {
		source := closedFreshStore(t)
		db := rawDB(t, source)
		newer := SchemaVersion + 1
		if _, err := db.Exec(`UPDATE schema_meta SET value=? WHERE key='schema_version'`, strconv.Itoa(newer)); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), "newer.db")
		result, err := Backup(context.Background(), source, destination)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != VerificationIncompatible || result.SchemaVersion != newer || result.Compatible {
			t.Fatalf("newer backup = %+v", result)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "unknown.db")
		db := rawDB(t, source)
		if _, err := db.Exec(`CREATE TABLE another_application (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), "unknown.db")
		result, err := Backup(context.Background(), source, destination)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != VerificationIncompatible || result.SchemaVersion != 0 || result.Compatible {
			t.Fatalf("unknown backup = %+v", result)
		}
	})
}

func TestOnlineTransferBusyPolicy(t *testing.T) {
	t.Run("exhaustion", func(t *testing.T) {
		calls := 0
		finished := 0
		stepper := onlineStepperFunc(func(int32) (bool, error) {
			calls++
			return false, codedSQLiteError(sqlite3.SQLITE_BUSY)
		})
		err := stepOnlineTransferWithPolicy(context.Background(), stepper, func() error {
			finished++
			return nil
		}, nil, onlineTransferPolicy{pagesPerStep: 1, maxBusyRetries: 2, retryDelay: time.Nanosecond})
		if err == nil || !isBusySQLiteError(err) {
			t.Fatalf("busy exhaustion error = %v", err)
		}
		if calls != 3 || finished != 1 {
			t.Fatalf("busy exhaustion calls=%d finish=%d, want 3 and 1", calls, finished)
		}
	})

	t.Run("cancellation during retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		finished := 0
		stepper := onlineStepperFunc(func(int32) (bool, error) {
			cancel()
			return false, codedSQLiteError(sqlite3.SQLITE_BUSY)
		})
		err := stepOnlineTransferWithPolicy(ctx, stepper, func() error {
			finished++
			return nil
		}, nil, onlineTransferPolicy{pagesPerStep: 1, maxBusyRetries: 2, retryDelay: time.Hour})
		if !errors.Is(err, context.Canceled) || finished != 1 {
			t.Fatalf("cancelled retry error=%v finish=%d, want context cancellation and one finish", err, finished)
		}
	})
}

func TestOnlineTransferCompletionAndErrors(t *testing.T) {
	t.Run("multiple batches", func(t *testing.T) {
		steps := 0
		batches := 0
		finished := 0
		stepper := onlineStepperFunc(func(pages int32) (bool, error) {
			if pages != 7 {
				t.Fatalf("pages = %d, want 7", pages)
			}
			steps++
			return steps == 1, nil
		})
		err := stepOnlineTransferWithPolicy(context.Background(), stepper, func() error {
			finished++
			return nil
		}, func() { batches++ }, onlineTransferPolicy{pagesPerStep: 7, maxBusyRetries: 1, retryDelay: time.Nanosecond})
		if err != nil || steps != 2 || batches != 2 || finished != 1 {
			t.Fatalf("transfer err=%v steps=%d batches=%d finish=%d", err, steps, batches, finished)
		}
	})

	t.Run("finish error", func(t *testing.T) {
		want := errors.New("injected finish failure")
		err := stepOnlineTransferWithPolicy(context.Background(), onlineStepperFunc(func(int32) (bool, error) {
			return false, nil
		}), func() error { return want }, nil, onlineTransferPolicy{pagesPerStep: 1})
		if !errors.Is(err, want) {
			t.Fatalf("finish error = %v", err)
		}
	})

	t.Run("non-busy step error wins", func(t *testing.T) {
		want := errors.New("injected step failure")
		err := stepOnlineTransferWithPolicy(context.Background(), onlineStepperFunc(func(int32) (bool, error) {
			return false, want
		}), func() error { return errors.New("secondary finish failure") }, nil, onlineTransferPolicy{pagesPerStep: 1})
		if !errors.Is(err, want) {
			t.Fatalf("step error = %v", err)
		}
	})
}

type onlineStepperFunc func(int32) (bool, error)

func (f onlineStepperFunc) Step(pages int32) (bool, error) {
	return f(pages)
}

type codedSQLiteError int

func (e codedSQLiteError) Error() string { return "injected SQLite error" }
func (e codedSQLiteError) Code() int     { return int(e) }

func TestMigrationPreflightCreatesVerifiedBackup(t *testing.T) {
	path := legacyDB(t, 3)
	ledger, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy database: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), "backups",
		fmt.Sprintf("pre-migration-v3-to-v%d-*.db", SchemaVersion)))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("pre-migration backups = %v, want exactly one", backups)
	}
	backupVerification, err := Verify(context.Background(), backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if backupVerification.State != VerificationIncompatible || backupVerification.SchemaVersion != 3 || backupVerification.RowCounts["usage_events"] != 1 {
		t.Fatalf("pre-migration backup = %+v, want verified v3 snapshot", backupVerification)
	}
	current, err := Verify(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if current.SchemaVersion != SchemaVersion || (current.State != VerificationOK && current.State != VerificationRepairable) {
		t.Fatalf("migrated database = %+v", current)
	}
}

func TestMigrationPreflightFailureLeavesOriginalSchemaUntouched(t *testing.T) {
	path := legacyDB(t, 3)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "backups"), []byte("blocks directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}

	ledger, err := Open(path)
	if ledger != nil {
		ledger.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "before any schema statement") {
		t.Fatalf("Open error = %v, want pre-migration backup refusal", err)
	}
	db := rawDB(t, path)
	version, versionErr := readSchemaVersion(context.Background(), db)
	if versionErr != nil || version != 3 {
		t.Fatalf("schema after failed preflight = v%d err=%v, want v3", version, versionErr)
	}
	hasRollup, tableErr := tableExists(context.Background(), db, "usage_rollup")
	if tableErr != nil || hasRollup {
		t.Fatalf("failed preflight created v4 table: present=%t err=%v", hasRollup, tableErr)
	}
}

func TestMigrationPreflightRejectsDamagedOlderSchema(t *testing.T) {
	path := legacyDB(t, 5)
	db := rawDB(t, path)
	if _, err := db.Exec(`DROP TRIGGER trg_activity_no_delete`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ledger, err := Open(path)
	if ledger != nil {
		ledger.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "before any schema statement") || !strings.Contains(err.Error(), "trg_activity_no_delete") {
		t.Fatalf("Open damaged v5 error = %v", err)
	}
	db = rawDB(t, path)
	version, versionErr := readSchemaVersion(context.Background(), db)
	if versionErr != nil || version != 5 {
		t.Fatalf("schema after failed verification = v%d err=%v, want v5", version, versionErr)
	}
	backups, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), "backups", "*.db"))
	if globErr != nil || len(backups) != 0 {
		t.Fatalf("damaged schema produced backups=%v err=%v", backups, globErr)
	}
}

func TestFailedMigrationRetainsCommittedStampAndRestorableSnapshot(t *testing.T) {
	path := legacyDB(t, 3)
	originalMigrations := migrations
	t.Cleanup(func() { migrations = originalMigrations })
	brokenMigrations := append([]migration(nil), originalMigrations...)
	for i := range brokenMigrations {
		if brokenMigrations[i].version == 5 {
			brokenMigrations[i] = migration{version: 5, statements: []string{
				`CREATE TABLE should_roll_back (value INTEGER)`,
				`INSERT INTO missing_migration_table VALUES (1)`,
			}}
		}
	}
	migrations = brokenMigrations

	ledger, openErr := Open(path)
	if ledger != nil {
		ledger.Close()
	}
	if openErr == nil {
		t.Fatal("Open succeeded with injected v5 migration failure")
	}
	for _, want := range []string{"migration v5", "surviving schema is v4", "aiusage db verify", "aiusage --db", "db restore"} {
		if !strings.Contains(openErr.Error(), want) {
			t.Fatalf("migration error missing %q: %v", want, openErr)
		}
	}

	db := rawDB(t, path)
	version, err := readSchemaVersion(context.Background(), db)
	if err != nil || version != 4 {
		t.Fatalf("surviving schema = v%d err=%v, want last committed v4", version, err)
	}
	rolledBackTable, err := tableExists(context.Background(), db, "should_roll_back")
	if err != nil || rolledBackTable {
		t.Fatalf("failed v5 transaction table present=%t err=%v", rolledBackTable, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), "backups",
		fmt.Sprintf("pre-migration-v3-to-v%d-*.db", SchemaVersion)))
	if err != nil || len(backups) != 1 {
		t.Fatalf("retained snapshots = %v err=%v, want one", backups, err)
	}
	snapshot, err := Verify(context.Background(), backups[0])
	if err != nil || snapshot.State != VerificationIncompatible || snapshot.SchemaVersion != 3 {
		t.Fatalf("retained snapshot = %+v err=%v", snapshot, err)
	}

	// Restore the clean snapshot after removing the injected migration fault.
	// This proves the retained file is recovery material, not merely present.
	migrations = originalMigrations
	restored := filepath.Join(t.TempDir(), "restored.db")
	plan, err := PrepareRestore(context.Background(), backups[0], restored)
	if err != nil {
		t.Fatalf("prepare retained snapshot: %v", err)
	}
	defer plan.Cleanup()
	result, err := ApplyRestore(context.Background(), plan, restored, false)
	if err != nil || !result.TargetUsable || result.Verification.State != VerificationOK {
		t.Fatalf("restore retained snapshot = %+v err=%v", result, err)
	}
}

func TestMigrationVerificationDiagnostics(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if err := verifyCompletedMigration(context.Background(), missing, "snapshot.db"); err == nil || !strings.Contains(err.Error(), "post-migration verification failed") {
		t.Fatalf("missing migrated database error = %v", err)
	}

	newer := closedFreshStore(t)
	db := rawDB(t, newer)
	if _, err := db.Exec(`UPDATE schema_meta SET value=? WHERE key='schema_version'`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifyCompletedMigration(context.Background(), newer, "snapshot.db"); err == nil || !strings.Contains(err.Error(), "post-migration verification is incompatible") {
		t.Fatalf("incompatible migrated database error = %v", err)
	}
}

func TestRecoveryFilesystemHelperFailures(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if _, err := RecordedSchemaVersion(context.Background(), missing); err == nil {
		t.Fatal("RecordedSchemaVersion accepted missing path")
	}
	if err := makeBackupStandalone(context.Background(), missing); err == nil {
		t.Fatal("makeBackupStandalone accepted missing path")
	}
	if err := syncFile(missing); err == nil {
		t.Fatal("syncFile accepted missing path")
	}
	if err := syncDirectory(missing); err == nil {
		t.Fatal("syncDirectory accepted missing path")
	}
	if _, _, err := fileIdentity(context.Background(), missing); err == nil {
		t.Fatal("fileIdentity accepted missing path")
	}

	standalone := filepath.Join(dir, "standalone.db")
	if err := os.WriteFile(standalone, []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(standalone+"-wal", []byte("sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureStandaloneBackup(standalone); err == nil || !strings.Contains(err.Error(), "unexpected sidecar") {
		t.Fatalf("ensureStandaloneBackup sidecar error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fileIdentity(ctx, standalone); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fileIdentity error = %v", err)
	}
	if _, err := resolvedPathsEqual(missing, filepath.Join(dir, "destination.db")); err == nil {
		t.Fatal("resolvedPathsEqual accepted missing source")
	}
	if got := sqliteFileURI("relative database.db", true); !strings.HasPrefix(got, "file:/") || !strings.Contains(got, "mode=ro") {
		t.Fatalf("relative SQLite URI = %s", got)
	}
}

func TestPreMigrationBackupPathAvoidsCollision(t *testing.T) {
	database := filepath.Join(t.TempDir(), "usage.db")
	now := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	first, err := nextPreMigrationBackupPath(database, 3, SchemaVersion, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(first), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := nextPreMigrationBackupPath(database, 3, SchemaVersion, now)
	if err != nil || second != strings.TrimSuffix(first, ".db")+"-2.db" {
		t.Fatalf("second pre-migration path = %s err=%v", second, err)
	}
}

type fileSnapshot struct {
	Digest  string
	Mode    os.FileMode
	ModTime int64
	Size    int64
}

func snapshotFile(t *testing.T, path string) fileSnapshot {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return fileSnapshot{
		Digest:  hex.EncodeToString(digest[:]),
		Mode:    info.Mode().Perm(),
		ModTime: info.ModTime().UnixNano(),
		Size:    info.Size(),
	}
}

func closedFreshStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	ledger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
