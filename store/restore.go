package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// PreparedRestore is a verified current-schema staging database. The input
// backup remains untouched; Cleanup removes only the staging file.
type PreparedRestore struct {
	SourcePath   string       `json:"source_path"`
	StagePath    string       `json:"stage_path"`
	SnapshotTime time.Time    `json:"snapshot_time"`
	Verification Verification `json:"verification"`
}

// Cleanup removes the staging database and any transient sidecars.
func (p *PreparedRestore) Cleanup() {
	if p != nil && p.StagePath != "" {
		removeBackupPartial(p.StagePath)
	}
}

// RestoreResult reports the result of applying a prepared full snapshot.
type RestoreResult struct {
	SourcePath            string       `json:"source_path"`
	TargetPath            string       `json:"target_path"`
	SafetyBackupPath      string       `json:"safety_backup_path,omitempty"`
	CorruptQuarantinePath string       `json:"corrupt_quarantine_path,omitempty"`
	SnapshotTime          time.Time    `json:"snapshot_time"`
	Verification          Verification `json:"verification"`
	TargetUsable          bool         `json:"target_usable"`
	RolledBack            bool         `json:"rolled_back"`
	EventsMayBeAbsent     bool         `json:"events_may_be_absent"`
}

// PrepareRestore copies the input into a staging database beside the target,
// verifies it, migrates only the staging copy when needed, and repairs only its
// derived rollup. It does not touch the live target.
func PrepareRestore(ctx context.Context, backupPath, targetPath string) (*PreparedRestore, error) {
	backupAbsolute, targetAbsolute, info, err := validateRestorePaths(backupPath, targetPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(targetAbsolute), 0o700); err != nil {
		return nil, fmt.Errorf("store: create restore target directory %s: %w", filepath.Dir(targetAbsolute), err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(targetAbsolute), ".restore-stage-*.db")
	if err != nil {
		return nil, fmt.Errorf("store: create restore staging path: %w", err)
	}
	stage := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(stage)
		return nil, fmt.Errorf("store: close restore staging path %s: %w", stage, err)
	}
	if err := os.Remove(stage); err != nil {
		return nil, fmt.Errorf("store: prepare restore staging path %s: %w", stage, err)
	}
	plan := &PreparedRestore{
		SourcePath:   backupAbsolute,
		StagePath:    stage,
		SnapshotTime: info.ModTime().UTC(),
	}
	failed := true
	defer func() {
		if failed {
			plan.Cleanup()
		}
	}()

	copied, err := Backup(ctx, backupAbsolute, stage)
	if err != nil {
		return nil, fmt.Errorf("store: stage restore input %s: %w", backupAbsolute, err)
	}
	if copied.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("%w: restore input %s records schema v%d, this binary's is v%d",
			ErrSchemaNewer, backupAbsolute, copied.SchemaVersion, SchemaVersion)
	}
	if copied.SchemaVersion < 1 {
		return nil, fmt.Errorf("store: restore input %s has no recognized aiusage schema", backupAbsolute)
	}
	if err := makePreparedStageCurrent(ctx, plan, copied.SchemaVersion); err != nil {
		return nil, err
	}
	verification, err := Verify(ctx, stage)
	if err != nil {
		return nil, fmt.Errorf("store: verify prepared restore %s: %w", stage, err)
	}
	if verification.State != VerificationOK {
		return nil, fmt.Errorf("store: prepared restore is %s: %s", verification.State, verification.Reason)
	}
	plan.Verification = verification
	failed = false
	return plan, nil
}

func validateRestorePaths(backupPath, targetPath string) (string, string, os.FileInfo, error) {
	if backupPath == "" || targetPath == "" {
		return "", "", nil, fmt.Errorf("store: restore input and target paths are required")
	}
	backupAbsolute, err := filepath.Abs(backupPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("store: resolve restore input %s: %w", backupPath, err)
	}
	info, err := os.Stat(backupAbsolute)
	if err != nil {
		return "", "", nil, fmt.Errorf("store: stat restore input %s: %w", backupAbsolute, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", nil, fmt.Errorf("store: restore input is not a regular file: %s", backupAbsolute)
	}
	targetAbsolute, err := filepath.Abs(targetPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("store: resolve restore target %s: %w", targetPath, err)
	}
	if filepath.Clean(backupAbsolute) == filepath.Clean(targetAbsolute) {
		return "", "", nil, fmt.Errorf("store: restore input and target are the same path: %s", targetAbsolute)
	}
	if targetInfo, err := os.Stat(targetAbsolute); err == nil && os.SameFile(info, targetInfo) {
		return "", "", nil, fmt.Errorf("store: restore input and target resolve to the same file: %s", targetAbsolute)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", nil, fmt.Errorf("store: inspect restore target %s: %w", targetAbsolute, err)
	}
	return backupAbsolute, targetAbsolute, info, nil
}

func makePreparedStageCurrent(ctx context.Context, plan *PreparedRestore, version int) error {
	db, err := openRestoreWritable(ctx, plan.StagePath, "rw")
	if err != nil {
		return err
	}
	if err := ensureSchema(ctx, db, plan.StagePath); err != nil {
		if version < SchemaVersion {
			err = migrationFailure(ctx, db, plan.StagePath, plan.SourcePath, err)
		}
		db.Close()
		return err
	}
	ledger := &Ledger{Reader: &Reader{db: db, path: plan.StagePath}}
	if _, err := ledger.EnsureRollup(ctx); err != nil {
		db.Close()
		return fmt.Errorf("store: repair staged restore rollup: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("store: close prepared restore %s: %w", plan.StagePath, err)
	}
	return nil
}

// ApplyRestore replaces the target through SQLite's Online Backup API. The
// caller must stop collection and hold the collection lock for the whole call.
func ApplyRestore(ctx context.Context, plan *PreparedRestore, targetPath string, replace bool) (RestoreResult, error) {
	return applyRestore(ctx, plan, targetPath, replace, restoreHooks{
		transfer:   restoreOnline,
		postVerify: Verify,
	})
}

type restoreHooks struct {
	transfer   func(context.Context, string, string) error
	postVerify func(context.Context, string) (Verification, error)
}

func applyRestore(ctx context.Context, plan *PreparedRestore, targetPath string, replace bool, hooks restoreHooks) (RestoreResult, error) {
	result := RestoreResult{EventsMayBeAbsent: true}
	if plan == nil || plan.StagePath == "" {
		return result, fmt.Errorf("store: restore plan is missing")
	}
	targetAbsolute, err := filepath.Abs(targetPath)
	if err != nil {
		return result, fmt.Errorf("store: resolve restore target %s: %w", targetPath, err)
	}
	result.SourcePath = plan.SourcePath
	result.TargetPath = targetAbsolute
	result.SnapshotTime = plan.SnapshotTime

	stageVerification, err := Verify(ctx, plan.StagePath)
	if err != nil {
		return result, fmt.Errorf("store: reverify restore staging database: %w", err)
	}
	if stageVerification.State != VerificationOK || stageVerification.SchemaVersion != SchemaVersion {
		return result, fmt.Errorf("store: restore staging database changed after preparation: %s", stageVerification.Reason)
	}

	targetInfo, statErr := os.Stat(targetAbsolute)
	targetExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return result, fmt.Errorf("store: inspect restore target %s: %w", targetAbsolute, statErr)
	}
	if targetExists && !targetInfo.Mode().IsRegular() {
		return result, fmt.Errorf("store: restore target is not a regular file: %s", targetAbsolute)
	}
	if targetExists && !replace {
		return result, fmt.Errorf("store: restore target exists; pass --replace to replace %s", targetAbsolute)
	}

	corruptQuarantine := ""
	if targetExists {
		liveVerification, verifyErr := Verify(ctx, targetAbsolute)
		if verifyErr != nil {
			return result, fmt.Errorf("store: verify live target before restore: %w", verifyErr)
		}
		if liveVerification.State == VerificationCorrupt {
			quarantined, quarantineErr := Quarantine(ctx, targetAbsolute)
			if quarantineErr != nil {
				return result, fmt.Errorf("store: quarantine corrupt target before restore: %w", quarantineErr)
			}
			corruptQuarantine = quarantined.QuarantinePath
			result.CorruptQuarantinePath = corruptQuarantine
		} else {
			safetyPath, pathErr := nextSafetyBackupPath(targetAbsolute, liveVerification.SchemaVersion, time.Now().UTC())
			if pathErr != nil {
				result.TargetUsable = liveVerification.Compatible
				return result, pathErr
			}
			safety, backupErr := Backup(ctx, targetAbsolute, safetyPath)
			if backupErr != nil {
				result.TargetUsable = liveVerification.Compatible
				return result, fmt.Errorf("store: pre-restore safety backup failed; live target was not changed: %w", backupErr)
			}
			result.SafetyBackupPath = safety.Path
		}
	}

	if hooks.transfer == nil {
		hooks.transfer = restoreOnline
	}
	if err := hooks.transfer(ctx, plan.StagePath, targetAbsolute); err != nil {
		return failAppliedRestore(ctx, result, targetAbsolute, targetExists, corruptQuarantine,
			fmt.Errorf("store: copy prepared restore into target: %w", err))
	}
	if err := makeBackupStandalone(ctx, targetAbsolute); err != nil {
		return failAppliedRestore(ctx, result, targetAbsolute, targetExists, corruptQuarantine, err)
	}
	if hooks.postVerify == nil {
		hooks.postVerify = Verify
	}
	verification, err := hooks.postVerify(ctx, targetAbsolute)
	if err != nil {
		return failAppliedRestore(ctx, result, targetAbsolute, targetExists, corruptQuarantine,
			fmt.Errorf("store: post-restore verification failed: %w", err))
	}
	if verification.State != VerificationOK {
		return failAppliedRestore(ctx, result, targetAbsolute, targetExists, corruptQuarantine,
			fmt.Errorf("store: post-restore verification is %s: %s", verification.State, verification.Reason))
	}
	if err := representativeRestoreReads(ctx, targetAbsolute); err != nil {
		return failAppliedRestore(ctx, result, targetAbsolute, targetExists, corruptQuarantine, err)
	}
	result.Verification = verification
	result.TargetUsable = true
	return result, nil
}

func nextSafetyBackupPath(target string, version int, now time.Time) (string, error) {
	directory := filepath.Join(filepath.Dir(target), "backups")
	versionLabel := "unknown"
	if version > 0 {
		versionLabel = fmt.Sprintf("%d", version)
	}
	stem := fmt.Sprintf("pre-restore-v%s-%s", versionLabel, now.UTC().Format("20060102T150405Z"))
	for sequence := 1; ; sequence++ {
		name := stem + ".db"
		if sequence > 1 {
			name = fmt.Sprintf("%s-%d.db", stem, sequence)
		}
		candidate := filepath.Join(directory, name)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("store: inspect safety backup path %s: %w", candidate, err)
		}
	}
}

type onlineRestoreConn interface {
	NewRestore(string) (*sqlite.Backup, error)
}

func restoreOnline(ctx context.Context, source, target string) error {
	db, err := openRestoreWritable(ctx, target, "rwc")
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		restoreConn, ok := driverConn.(onlineRestoreConn)
		if !ok {
			return fmt.Errorf("SQLite driver does not expose the Online Restore API")
		}
		restore, err := restoreConn.NewRestore(sqliteFileURI(source, true))
		if err != nil {
			return err
		}
		return stepOnlineRestore(ctx, restore)
	})
}

func openRestoreWritable(ctx context.Context, path, mode string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve writable database %s: %w", path, err)
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	u := url.URL{Scheme: "file", Path: slashed}
	query := u.Query()
	query.Set("mode", mode)
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open writable database %s: %w", absolute, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open writable database %s: %w", absolute, err)
	}
	return db, nil
}

func representativeRestoreReads(ctx context.Context, path string) error {
	reader, err := OpenReadOnly(path)
	if err != nil {
		return fmt.Errorf("store: reopen restored target: %w", err)
	}
	defer reader.Close()
	if _, err := reader.SummarizeRollup(ctx, Filter{}); err != nil {
		return fmt.Errorf("store: read restored summary: %w", err)
	}
	if _, err := reader.ListEvents(ctx, Filter{}, WithKeyset(0, 1)); err != nil {
		return fmt.Errorf("store: read restored export projection: %w", err)
	}
	return nil
}

func assessFailedRestore(ctx context.Context, result RestoreResult, target string, targetExisted bool, cause error) (RestoreResult, error) {
	if !targetExisted {
		removeBackupPartial(target)
		return result, cause
	}
	verification, err := Verify(ctx, target)
	if err == nil && verification.Compatible && (verification.State == VerificationOK || verification.State == VerificationRepairable) {
		result.TargetUsable = true
		result.Verification = verification
	}
	return result, cause
}

func failAppliedRestore(ctx context.Context, result RestoreResult, target string, targetExisted bool, corruptQuarantine string, cause error) (RestoreResult, error) {
	if corruptQuarantine == "" {
		return rollbackFailedRestore(ctx, result, target, targetExisted, cause)
	}
	if err := restoreQuarantinedBundle(target, corruptQuarantine); err != nil {
		return result, fmt.Errorf("%v; restoring the original corrupt SQLite bundle from %s also failed: %w", cause, corruptQuarantine, err)
	}
	result.CorruptQuarantinePath = ""
	return result, fmt.Errorf("%v; restored the original corrupt SQLite bundle and left collection stopped", cause)
}

func restoreQuarantinedBundle(target, quarantinePath string) error {
	var originals []string
	for _, original := range []string{target, target + "-wal", target + "-shm"} {
		moved := filepath.Join(quarantinePath, filepath.Base(original))
		if _, err := os.Lstat(moved); err == nil {
			originals = append(originals, original)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect quarantined bundle member %s: %w", moved, err)
		}
	}
	if len(originals) == 0 {
		return fmt.Errorf("quarantine %s contains no SQLite bundle", quarantinePath)
	}
	removeBackupPartial(target)
	if err := rollbackQuarantineMoves(originals, quarantinePath, os.Rename); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

func rollbackFailedRestore(ctx context.Context, result RestoreResult, target string, targetExisted bool, cause error) (RestoreResult, error) {
	if result.SafetyBackupPath == "" {
		return assessFailedRestore(ctx, result, target, targetExisted, cause)
	}
	if err := restoreOnline(ctx, result.SafetyBackupPath, target); err != nil {
		return result, fmt.Errorf("%v; rollback from safety backup %s also failed: %w", cause, result.SafetyBackupPath, err)
	}
	if err := makeBackupStandalone(ctx, target); err != nil {
		return result, fmt.Errorf("%v; safety backup was copied back but could not be finalized: %w", cause, err)
	}
	verification, err := Verify(ctx, target)
	if err != nil || !verification.Compatible || (verification.State != VerificationOK && verification.State != VerificationRepairable) {
		if err == nil {
			err = verification.StateError()
		}
		return result, fmt.Errorf("%v; rollback from safety backup did not produce a usable target: %w", cause, err)
	}
	result.TargetUsable = true
	result.RolledBack = true
	result.Verification = verification
	return result, fmt.Errorf("%v; restored the verified safety backup %s", cause, result.SafetyBackupPath)
}

// QuarantineResult describes an explicit reset that preserved the old SQLite
// bundle before creating a fresh verified database.
type QuarantineResult struct {
	TargetPath                    string       `json:"target_path"`
	QuarantinePath                string       `json:"quarantine_path"`
	Verification                  Verification `json:"verification"`
	TargetUsable                  bool         `json:"target_usable"`
	HistoricalCompletenessUnknown bool         `json:"historical_completeness_unknown"`
}

// Quarantine moves the database, WAL, and SHM as one recoverable bundle, then
// creates a fresh current database. The caller must stop collection and hold
// the collection lock.
func Quarantine(ctx context.Context, targetPath string) (QuarantineResult, error) {
	return quarantine(ctx, targetPath, quarantineHooks{rename: os.Rename})
}

type quarantineHooks struct {
	rename func(string, string) error
}

func quarantine(ctx context.Context, targetPath string, hooks quarantineHooks) (QuarantineResult, error) {
	result := QuarantineResult{HistoricalCompletenessUnknown: true}
	target, err := filepath.Abs(targetPath)
	if err != nil {
		return result, fmt.Errorf("store: resolve quarantine target %s: %w", targetPath, err)
	}
	result.TargetPath = target
	info, err := os.Stat(target)
	if err != nil {
		return result, fmt.Errorf("store: stat quarantine target %s: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("store: quarantine target is not a regular file: %s", target)
	}
	quarantinePath, err := nextQuarantinePath(target, time.Now().UTC())
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(quarantinePath, 0o700); err != nil {
		return result, fmt.Errorf("store: create quarantine directory %s: %w", quarantinePath, err)
	}
	result.QuarantinePath = quarantinePath
	if hooks.rename == nil {
		hooks.rename = os.Rename
	}

	var bundle []string
	for _, path := range []string{target, target + "-wal", target + "-shm"} {
		if _, err := os.Lstat(path); err == nil {
			bundle = append(bundle, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			os.Remove(quarantinePath)
			return result, fmt.Errorf("store: inspect quarantine bundle member %s: %w", path, err)
		}
	}
	moved := make([]string, 0, len(bundle))
	for _, source := range bundle {
		destination := filepath.Join(quarantinePath, filepath.Base(source))
		if err := hooks.rename(source, destination); err != nil {
			rollbackErr := rollbackQuarantineMoves(moved, quarantinePath, hooks.rename)
			if rollbackErr != nil {
				return result, fmt.Errorf("store: preserve quarantine bundle member %s: %v; rollback failed: %w", source, err, rollbackErr)
			}
			return result, fmt.Errorf("store: preserve quarantine bundle member %s: %w", source, err)
		}
		moved = append(moved, source)
	}
	if err := syncDirectory(quarantinePath); err != nil {
		rollbackErr := rollbackQuarantineMoves(bundle, quarantinePath, hooks.rename)
		if rollbackErr != nil {
			return result, fmt.Errorf("store: sync quarantine bundle: %v; rollback failed: %w", err, rollbackErr)
		}
		return result, fmt.Errorf("store: sync quarantine bundle: %w", err)
	}

	ledger, err := Open(target)
	if err == nil {
		err = ledger.Close()
	}
	if err == nil {
		result.Verification, err = Verify(ctx, target)
		if err == nil && result.Verification.State != VerificationOK {
			err = result.Verification.StateError()
		}
	}
	if err != nil {
		removeBackupPartial(target)
		rollbackErr := rollbackQuarantineMoves(bundle, quarantinePath, hooks.rename)
		if rollbackErr != nil {
			return result, fmt.Errorf("store: create fresh database after quarantine: %v; original bundle rollback failed: %w", err, rollbackErr)
		}
		return result, fmt.Errorf("store: create fresh database after quarantine: %w", err)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		return result, fmt.Errorf("store: sync database directory after quarantine: %w", err)
	}
	result.TargetUsable = true
	return result, nil
}

func nextQuarantinePath(target string, now time.Time) (string, error) {
	directory := filepath.Join(filepath.Dir(target), "quarantine")
	stem := strings.TrimSuffix(filepath.Base(target), filepath.Ext(target)) + "-" + now.UTC().Format("20060102T150405Z")
	for sequence := 1; ; sequence++ {
		name := stem
		if sequence > 1 {
			name = fmt.Sprintf("%s-%d", stem, sequence)
		}
		candidate := filepath.Join(directory, name)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("store: inspect quarantine path %s: %w", candidate, err)
		}
	}
}

func rollbackQuarantineMoves(originals []string, quarantinePath string, rename func(string, string) error) error {
	var rollbackErrors []string
	for i := len(originals) - 1; i >= 0; i-- {
		original := originals[i]
		moved := filepath.Join(quarantinePath, filepath.Base(original))
		if _, err := os.Lstat(moved); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := rename(moved, original); err != nil {
			rollbackErrors = append(rollbackErrors, err.Error())
		}
	}
	if len(rollbackErrors) == 0 {
		_ = os.Remove(quarantinePath)
		return nil
	}
	return errors.New(strings.Join(rollbackErrors, "; "))
}
