package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// VerificationState is the stable result of a read-only database check.
type VerificationState string

const (
	VerificationOK           VerificationState = "ok"
	VerificationRepairable   VerificationState = "repairable"
	VerificationIncompatible VerificationState = "incompatible"
	VerificationCorrupt      VerificationState = "corrupt"
)

// Verification describes a complete read-only check of one database.
type Verification struct {
	Path                string            `json:"path"`
	State               VerificationState `json:"state"`
	Reason              string            `json:"reason,omitempty"`
	SchemaVersion       int               `json:"schema_version"`
	Compatible          bool              `json:"compatible"`
	IntegrityOK         bool              `json:"integrity_ok"`
	ForeignKeysOK       bool              `json:"foreign_keys_ok"`
	ApplicationSchemaOK bool              `json:"application_schema_ok"`
	// RollupChecked covers both derived usage rollup and activity counts.
	RollupChecked bool `json:"rollup_checked"`
	// RollupStale is true when either derived table disagrees with its ledger.
	RollupStale      bool             `json:"rollup_stale"`
	IntegrityErrors  []string         `json:"integrity_errors,omitempty"`
	ForeignKeyErrors []string         `json:"foreign_key_errors,omitempty"`
	SchemaErrors     []string         `json:"schema_errors,omitempty"`
	RowCounts        map[string]int64 `json:"row_counts,omitempty"`
}

// StateError turns a completed non-ok verification into an error suitable for
// a command exit. Newer schemas preserve ErrSchemaNewer for callers that branch
// on it.
func (v Verification) StateError() error {
	if v.State == VerificationOK {
		return nil
	}
	if v.SchemaVersion > SchemaVersion {
		return fmt.Errorf("%w: %s", ErrSchemaNewer, v.Reason)
	}
	return errors.New(v.Reason)
}

// RecordedSchemaVersion reads only the version stamp from an existing
// database. It returns 0 for an unversioned or unrecognized layout and never
// creates or migrates the file.
func RecordedSchemaVersion(ctx context.Context, path string) (int, error) {
	db, absolute, err := openRecoveryReadOnly(ctx, path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	version, _, err := recordedSchemaVersion(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("store: read schema version from %s: %w", absolute, err)
	}
	return version, nil
}

// prepareMigration verifies and snapshots a recognized older database before
// Open creates a writable SQLite connection. Its caller must already own the
// process-level collection lock; store owns the database mechanics, while the
// command/lifecycle layer owns cross-process collection coordination.
func prepareMigration(ctx context.Context, path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: inspect database before open %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", nil
	}
	version, err := RecordedSchemaVersion(ctx, path)
	if err != nil {
		return "", err
	}
	if version > SchemaVersion {
		return "", fmt.Errorf("%w: %s records v%d, this binary's is v%d; upgrade aiusage to open it",
			ErrSchemaNewer, path, version, SchemaVersion)
	}
	if version < 1 || version == SchemaVersion {
		return "", nil
	}

	verification, err := Verify(ctx, path)
	if err != nil {
		return "", fmt.Errorf("store: pre-migration verification of %s failed before any schema statement: %w", path, err)
	}
	if verification.State != VerificationIncompatible || !verification.ApplicationSchemaOK {
		return "", fmt.Errorf("store: pre-migration verification of %s failed before any schema statement: %s", path, verification.Reason)
	}
	destination, err := nextPreMigrationBackupPath(path, version, SchemaVersion, time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("store: choose pre-migration backup path before any schema statement: %w", err)
	}
	backup, err := Backup(ctx, path, destination)
	if err != nil {
		return "", fmt.Errorf("store: pre-migration backup from v%d to v%d failed before any schema statement: %w", version, SchemaVersion, err)
	}
	if backup.SchemaVersion != version || backup.State != VerificationIncompatible {
		return "", fmt.Errorf("store: pre-migration backup %s did not preserve schema v%d before any schema statement", backup.Path, version)
	}
	return backup.Path, nil
}

func nextPreMigrationBackupPath(database string, from, to int, now time.Time) (string, error) {
	absolute, err := filepath.Abs(database)
	if err != nil {
		return "", fmt.Errorf("resolve database path %s: %w", database, err)
	}
	directory := filepath.Join(filepath.Dir(absolute), "backups")
	stem := fmt.Sprintf("pre-migration-v%d-to-v%d-%s", from, to, now.UTC().Format("20060102T150405Z"))
	for sequence := 1; ; sequence++ {
		name := stem + ".db"
		if sequence > 1 {
			name = fmt.Sprintf("%s-%d.db", stem, sequence)
		}
		candidate := filepath.Join(directory, name)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("inspect backup path %s: %w", candidate, err)
		}
	}
}

func migrationFailure(ctx context.Context, db *sql.DB, path, backup string, migrationErr error) error {
	version := 0
	if recorded, err := readSchemaVersion(ctx, db); err == nil {
		version = recorded
	}
	return fmt.Errorf(
		"store: migration of %s failed; surviving schema is v%d and verified snapshot is %s; "+
			"verify with `aiusage db verify %s`, retry with `aiusage --db %s once`, or restore with `aiusage --db %s db restore %s --replace`: %w",
		path, version, backup, path, path, path, backup, migrationErr)
}

func verifyCompletedMigration(ctx context.Context, path, backup string) error {
	verification, err := Verify(ctx, path)
	if err != nil {
		return fmt.Errorf("store: migrated %s to v%d but post-migration verification failed; verified pre-migration snapshot retained at %s: %w",
			path, SchemaVersion, backup, err)
	}
	if verification.State != VerificationOK && verification.State != VerificationRepairable {
		return fmt.Errorf("store: migrated %s to v%d but post-migration verification is %s; verified pre-migration snapshot retained at %s: %s",
			path, SchemaVersion, verification.State, backup, verification.Reason)
	}
	return nil
}

// BackupResult describes a verified, atomically published online backup.
type BackupResult struct {
	Path          string            `json:"path"`
	SizeBytes     int64             `json:"size_bytes"`
	SHA256        string            `json:"sha256"`
	SchemaVersion int               `json:"schema_version"`
	Compatible    bool              `json:"compatible"`
	State         VerificationState `json:"state"`
	RowCounts     map[string]int64  `json:"row_counts,omitempty"`
	Verification  Verification      `json:"verification"`
}

// BackupProgress reports coarse phases and completed Online Backup batches.
// Callers may omit the callback when they do not need interactive progress.
type BackupProgress struct {
	Phase   string `json:"phase"`
	Batches int64  `json:"batches"`
}

const (
	BackupPhaseCopying    = "copying"
	BackupPhaseVerifying  = "verifying"
	BackupPhasePublishing = "publishing"
)

// Verify checks an existing database without creating, migrating, repairing,
// checkpointing, or changing permissions.
func Verify(ctx context.Context, path string) (Verification, error) {
	db, absolute, err := openRecoveryReadOnly(ctx, path)
	result := Verification{Path: absolute, RowCounts: map[string]int64{}}
	if err != nil {
		if isCorruptSQLiteError(err) {
			result.State = VerificationCorrupt
			result.Reason = "SQLite integrity check could not read the database: " + err.Error()
			result.IntegrityErrors = []string{err.Error()}
			return result, nil
		}
		return Verification{}, err
	}
	defer db.Close()

	integrityErrors, err := integrityCheck(ctx, db)
	if err != nil {
		if ctx.Err() != nil {
			return Verification{}, ctx.Err()
		}
		if isCorruptSQLiteError(err) {
			result.State = VerificationCorrupt
			result.Reason = "SQLite integrity check could not read the database: " + err.Error()
			result.IntegrityErrors = []string{err.Error()}
			return result, nil
		}
		return Verification{}, fmt.Errorf("store: verify integrity of %s: %w", absolute, err)
	}
	result.IntegrityErrors = integrityErrors
	result.IntegrityOK = len(integrityErrors) == 0
	if !result.IntegrityOK {
		result.State = VerificationCorrupt
		result.Reason = "SQLite integrity_check failed: " + strings.Join(integrityErrors, "; ")
		return result, nil
	}

	foreignKeyErrors, err := foreignKeyCheck(ctx, db)
	if err != nil {
		if ctx.Err() != nil {
			return Verification{}, ctx.Err()
		}
		if isBusySQLiteError(err) {
			return Verification{}, fmt.Errorf("store: verify foreign keys of %s: %w", absolute, err)
		}
		result.State = VerificationCorrupt
		result.Reason = "SQLite foreign_key_check could not inspect the database: " + err.Error()
		result.ForeignKeyErrors = []string{err.Error()}
		return result, nil
	}
	result.ForeignKeyErrors = foreignKeyErrors
	result.ForeignKeysOK = len(foreignKeyErrors) == 0
	if !result.ForeignKeysOK {
		result.State = VerificationCorrupt
		result.Reason = "SQLite foreign_key_check failed: " + strings.Join(foreignKeyErrors, "; ")
		return result, nil
	}

	version, versionReason, err := recordedSchemaVersion(ctx, db)
	if err != nil {
		if ctx.Err() != nil || isBusySQLiteError(err) {
			return Verification{}, fmt.Errorf("store: read schema version from %s: %w", absolute, err)
		}
		result.State = VerificationCorrupt
		result.Reason = "application schema metadata is unreadable: " + err.Error()
		result.SchemaErrors = []string{err.Error()}
		return result, nil
	}
	result.SchemaVersion = version
	if version < 1 || version > SchemaVersion {
		result.State = VerificationIncompatible
		result.Reason = versionReason
		if result.Reason == "" {
			result.Reason = fmt.Sprintf("schema v%d is newer than this binary's v%d", version, SchemaVersion)
		}
		return result, nil
	}
	result.Compatible = version == SchemaVersion

	schemaErrors, err := verifyRecognizedSchema(ctx, db, version)
	if err != nil {
		if ctx.Err() != nil || isBusySQLiteError(err) {
			return Verification{}, fmt.Errorf("store: verify schema of %s: %w", absolute, err)
		}
		schemaErrors = append(schemaErrors, err.Error())
	}
	result.SchemaErrors = schemaErrors
	result.ApplicationSchemaOK = len(schemaErrors) == 0
	if !result.ApplicationSchemaOK {
		result.State = VerificationCorrupt
		result.Reason = "required application schema is damaged: " + strings.Join(schemaErrors, "; ")
		return result, nil
	}

	counts, err := availableRowCounts(ctx, db, version)
	if err != nil {
		if ctx.Err() != nil || isBusySQLiteError(err) {
			return Verification{}, fmt.Errorf("store: count rows in %s: %w", absolute, err)
		}
		result.State = VerificationCorrupt
		result.Reason = "required application table is unreadable: " + err.Error()
		result.SchemaErrors = []string{err.Error()}
		return result, nil
	}
	result.RowCounts = counts

	if version != SchemaVersion {
		result.State = VerificationIncompatible
		result.Reason = fmt.Sprintf("schema v%d is recognized but this binary requires v%d; preserve it or migrate a staged copy", version, SchemaVersion)
		return result, nil
	}

	reader := &Reader{db: db, path: absolute}
	stale, err := reader.RollupStale(ctx)
	var activityStale bool
	if err == nil {
		activityStale, err = reader.activityUsageCountsStale(ctx)
	}
	if err != nil {
		if ctx.Err() != nil || isBusySQLiteError(err) {
			return Verification{}, fmt.Errorf("store: verify rollup in %s: %w", absolute, err)
		}
		result.State = VerificationCorrupt
		result.Reason = "derived rollup could not be checked: " + err.Error()
		result.SchemaErrors = []string{err.Error()}
		return result, nil
	}
	result.RollupChecked = true
	result.RollupStale = stale || activityStale
	if result.RollupStale {
		result.State = VerificationRepairable
		result.Reason = "derived rollup does not match the ledger; run aiusage once or let the collector rebuild it"
		if activityStale {
			result.Reason = "derived activity usage counts do not match the activity ledger; run aiusage once or let the collector rebuild them"
			if stale {
				result.Reason = "derived usage rollup and activity usage counts do not match their ledgers; run aiusage once or let the collector rebuild them"
			}
		}
		return result, nil
	}

	result.State = VerificationOK
	result.Reason = "database integrity, schema, append-only protections, and rollup agree"
	return result, nil
}

// Backup creates a complete SQLite snapshot through the driver's Online Backup
// API, verifies it, then publishes it without overwriting an existing path.
func Backup(ctx context.Context, source, destination string) (BackupResult, error) {
	return backupWithHooks(ctx, source, destination, nil, backupHooks{})
}

// BackupWithProgress is Backup with coarse progress notifications for an
// interactive command. The callback runs synchronously and must return
// promptly.
func BackupWithProgress(ctx context.Context, source, destination string, report func(BackupProgress)) (BackupResult, error) {
	return backupWithHooks(ctx, source, destination, report, backupHooks{})
}

type backupHooks struct {
	copyOnline func(context.Context, string, string, func(int64)) error
	syncFile   func(string) error
}

func backupWithHooks(ctx context.Context, source, destination string, report func(BackupProgress), hooks backupHooks) (BackupResult, error) {
	if err := ctx.Err(); err != nil {
		return BackupResult{}, err
	}
	sourcePath, destinationPath, err := validateBackupPaths(source, destination)
	if err != nil {
		return BackupResult{}, err
	}

	destinationDir := filepath.Dir(destinationPath)
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		return BackupResult{}, fmt.Errorf("store: create backup directory %s: %w", destinationDir, err)
	}
	if same, err := resolvedPathsEqual(sourcePath, destinationPath); err != nil {
		return BackupResult{}, err
	} else if same {
		return BackupResult{}, fmt.Errorf("store: backup source and destination resolve to the same file: %s", sourcePath)
	}

	tmp, err := os.CreateTemp(destinationDir, "."+filepath.Base(destinationPath)+".partial-*")
	if err != nil {
		return BackupResult{}, fmt.Errorf("store: create backup partial in %s: %w", destinationDir, err)
	}
	partial := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(partial)
		return BackupResult{}, fmt.Errorf("store: close backup partial %s: %w", partial, err)
	}
	if err := os.Remove(partial); err != nil {
		return BackupResult{}, fmt.Errorf("store: prepare backup partial %s: %w", partial, err)
	}
	defer removeBackupPartial(partial)

	reportBackupProgress(report, BackupProgress{Phase: BackupPhaseCopying})
	if hooks.copyOnline == nil {
		hooks.copyOnline = copyOnline
	}
	if err := hooks.copyOnline(ctx, sourcePath, partial, func(batches int64) {
		reportBackupProgress(report, BackupProgress{Phase: BackupPhaseCopying, Batches: batches})
	}); err != nil {
		return BackupResult{}, fmt.Errorf("store: online backup %s: %w", sourcePath, err)
	}
	reportBackupProgress(report, BackupProgress{Phase: BackupPhaseVerifying})
	if err := makeBackupStandalone(ctx, partial); err != nil {
		return BackupResult{}, err
	}
	if err := os.Chmod(partial, 0o600); err != nil {
		return BackupResult{}, fmt.Errorf("store: restrict backup partial %s: %w", partial, err)
	}

	verification, err := Verify(ctx, partial)
	if err != nil {
		return BackupResult{}, fmt.Errorf("store: verify backup partial %s: %w", partial, err)
	}
	if verification.State == VerificationCorrupt {
		return BackupResult{}, fmt.Errorf("store: backup verification failed: %s", verification.Reason)
	}
	if err := ensureStandaloneBackup(partial); err != nil {
		return BackupResult{}, err
	}
	reportBackupProgress(report, BackupProgress{Phase: BackupPhasePublishing})
	if hooks.syncFile == nil {
		hooks.syncFile = syncFile
	}
	if err := hooks.syncFile(partial); err != nil {
		return BackupResult{}, fmt.Errorf("store: sync backup partial %s: %w", partial, err)
	}

	size, digest, err := fileIdentity(ctx, partial)
	if err != nil {
		return BackupResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return BackupResult{}, err
	}
	if err := os.Link(partial, destinationPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return BackupResult{}, fmt.Errorf("store: backup destination already exists: %s", destinationPath)
		}
		return BackupResult{}, fmt.Errorf("store: publish backup %s: %w", destinationPath, err)
	}
	if err := os.Remove(partial); err != nil {
		_ = os.Remove(destinationPath)
		return BackupResult{}, fmt.Errorf("store: remove published backup partial %s: %w", partial, err)
	}
	if err := syncDirectory(destinationDir); err != nil {
		_ = os.Remove(destinationPath)
		return BackupResult{}, fmt.Errorf("store: sync backup directory %s: %w", destinationDir, err)
	}

	verification.Path = destinationPath
	return BackupResult{
		Path:          destinationPath,
		SizeBytes:     size,
		SHA256:        digest,
		SchemaVersion: verification.SchemaVersion,
		Compatible:    verification.Compatible,
		State:         verification.State,
		RowCounts:     verification.RowCounts,
		Verification:  verification,
	}, nil
}

func reportBackupProgress(report func(BackupProgress), progress BackupProgress) {
	if report != nil {
		report(progress)
	}
}

func openRecoveryReadOnly(ctx context.Context, path string) (*sql.DB, string, error) {
	if path == "" {
		return nil, "", fmt.Errorf("store: empty database path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("store: resolve database path %s: %w", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, absolute, fmt.Errorf("store: stat database %s: %w", absolute, err)
	}
	if !info.Mode().IsRegular() {
		return nil, absolute, fmt.Errorf("store: database path is not a regular file: %s", absolute)
	}

	dsn := sqliteFileURI(absolute, true)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, absolute, fmt.Errorf("store: open %s read-only: %w", absolute, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, absolute, fmt.Errorf("store: open %s read-only: %w", absolute, err)
	}
	return db, absolute, nil
}

// sqliteFileURI encodes an absolute filesystem path without treating literal
// question marks, fragments or percent signs as URI syntax.
func sqliteFileURI(path string, readOnly bool) string {
	u := sqlitePathURL(path)
	q := u.Query()
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	u.RawQuery = q.Encode()
	return u.String()
}

func sqlitePathURL(path string) url.URL {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return url.URL{Scheme: "file", Path: slashed}
}

func integrityCheck(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var failures []string
	rowCount := 0
	for rows.Next() {
		rowCount++
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		if line != "ok" {
			failures = append(failures, line)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if rowCount == 0 {
		return []string{"integrity_check returned no result"}, nil
	}
	return failures, nil
}

func foreignKeyCheck(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var failures []string
	for rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKeyID int64
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			return nil, err
		}
		row := "without-rowid"
		if rowID.Valid {
			row = strconv.FormatInt(rowID.Int64, 10)
		}
		failures = append(failures, fmt.Sprintf("table %s row %s references %s (foreign key %d)", table, row, parent, foreignKeyID))
	}
	return failures, rows.Err()
}

func recordedSchemaVersion(ctx context.Context, db *sql.DB) (int, string, error) {
	hasMeta, err := tableExists(ctx, db, "schema_meta")
	if err != nil {
		return 0, "", err
	}
	if !hasMeta {
		return 0, "schema metadata is absent; this is not a recognized aiusage schema", nil
	}
	var raw string
	err = db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, "schema_version is absent; this is not a recognized aiusage schema", nil
	}
	if err != nil {
		return 0, "", err
	}
	version, err := strconv.Atoi(raw)
	if err != nil || version < 1 {
		return 0, fmt.Sprintf("schema_version %q is not recognized by this binary", raw), nil
	}
	return version, "", nil
}

type requiredTable struct {
	name    string
	columns []string
}

type requiredIndex struct {
	name    string
	table   string
	columns []string
}

type requiredTrigger struct {
	name  string
	table string
	event string
}

func verifyRecognizedSchema(ctx context.Context, db *sql.DB, version int) ([]string, error) {
	tables, indexes, triggers := schemaRequirements(version)
	var failures []string
	for _, required := range tables {
		exists, err := schemaObjectExists(ctx, db, "table", required.name, required.name)
		if err != nil {
			return failures, err
		}
		if !exists {
			failures = append(failures, "missing table "+required.name)
			continue
		}
		columns, err := columnSet(ctx, db, required.name)
		if err != nil {
			return failures, err
		}
		for _, column := range required.columns {
			if !columns[column] {
				failures = append(failures, fmt.Sprintf("table %s is missing column %s", required.name, column))
			}
		}
	}
	for _, required := range indexes {
		exists, err := schemaObjectExists(ctx, db, "index", required.name, required.table)
		if err != nil {
			return failures, err
		}
		if !exists {
			failures = append(failures, "missing index "+required.name)
			continue
		}
		columns, err := indexColumns(ctx, db, required.name)
		if err != nil {
			return failures, err
		}
		if strings.Join(columns, ",") != strings.Join(required.columns, ",") {
			failures = append(failures, fmt.Sprintf("index %s has columns (%s), want (%s)", required.name, strings.Join(columns, ", "), strings.Join(required.columns, ", ")))
		}
	}
	for _, required := range triggers {
		var ddl string
		err := db.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=? AND tbl_name=?`,
			required.name, required.table).Scan(&ddl)
		if err == sql.ErrNoRows {
			failures = append(failures, "missing append-only trigger "+required.name)
			continue
		}
		if err != nil {
			return failures, err
		}
		normalized := strings.ToUpper(strings.Join(strings.Fields(ddl), " "))
		guard := "BEFORE " + required.event + " ON " + strings.ToUpper(required.table)
		if !strings.Contains(normalized, guard) || !strings.Contains(normalized, "RAISE(ABORT") || strings.Contains(normalized, " WHEN ") {
			failures = append(failures, "append-only trigger "+required.name+" does not enforce "+required.event)
		}
	}
	return failures, nil
}

func schemaRequirements(version int) ([]requiredTable, []requiredIndex, []requiredTrigger) {
	usageColumns := eventColumns[:len(eventColumns)-4]
	if version >= 3 {
		usageColumns = eventColumns
	}
	tables := []requiredTable{
		{"schema_meta", []string{"key", "value"}},
		{"usage_events", usageColumns},
		{"aggregate_state", []string{"tool", "acc_key", "model", "session_id", "project", "observed_time_unix", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "reasoning_tokens", "total_tokens", "source_path", "raw"}},
	}
	// The five usage_events indexes in today's fresh schema were never part of
	// the historical migration chain. A database legitimately upgraded from v1
	// can therefore lack them at every later version. They are performance aids,
	// not authoritative objects, and rejecting their absence would mislabel a
	// valid database as corrupt.
	var indexes []requiredIndex
	triggers := []requiredTrigger{
		{"trg_events_no_update", "usage_events", "UPDATE"},
		{"trg_events_no_delete", "usage_events", "DELETE"},
	}
	if version >= 2 {
		tables = append(tables, requiredTable{"source_checkpoints", []string{"tool", "source_path", "size_bytes", "mtime_ns", "read_offset", "watermark", "state"}})
	}
	if version >= 4 {
		rollupColumns := []string{"bucket_start_unix", "tool", "model", "project", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "reasoning_tokens", "total_tokens", "events", "cost_micro_usd", "unpriced_events"}
		if version >= 8 {
			rollupColumns = []string{"bucket_start_unix", "tool", "model", "project", "session_id", "provider", "service_tier", "price_class", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "reasoning_tokens", "total_tokens", "events", "cost_micro_usd", "unpriced_events"}
		}
		tables = append(tables, requiredTable{"usage_rollup", rollupColumns})
	}
	if version >= 5 {
		tables = append(tables, requiredTable{"activity_events", []string{"id", "dedup_key", "tool", "kind", "name", "session_id", "project", "model", "event_time_unix", "observed_time_unix", "usage_dedup_key", "message_id", "request_id", "turn_seq", "calls_in_turn", "source_path"}})
		indexes = append(indexes,
			requiredIndex{"idx_activity_event_time", "activity_events", []string{"event_time_unix"}},
			requiredIndex{"idx_activity_tool_name", "activity_events", []string{"tool", "name"}},
			requiredIndex{"idx_activity_name_time", "activity_events", []string{"name", "event_time_unix"}},
			requiredIndex{"idx_activity_kind_time", "activity_events", []string{"kind", "event_time_unix"}},
			requiredIndex{"idx_activity_session", "activity_events", []string{"session_id"}},
			requiredIndex{"idx_activity_usage_key", "activity_events", []string{"usage_dedup_key"}},
		)
		triggers = append(triggers,
			requiredTrigger{"trg_activity_no_update", "activity_events", "UPDATE"},
			requiredTrigger{"trg_activity_no_delete", "activity_events", "DELETE"},
		)
	}
	if version == 6 {
		tables = append(tables, requiredTable{"usage_skill_context", []string{"usage_dedup_key", "tool", "skill", "session_id", "project", "model", "event_time_unix", "observed_time_unix", "source_path"}})
		indexes = append(indexes,
			requiredIndex{"idx_skillctx_skill_time", "usage_skill_context", []string{"skill", "event_time_unix"}},
			requiredIndex{"idx_skillctx_event_time", "usage_skill_context", []string{"event_time_unix"}},
			requiredIndex{"idx_skillctx_session", "usage_skill_context", []string{"session_id"}},
		)
		triggers = append(triggers,
			requiredTrigger{"trg_skillctx_no_update", "usage_skill_context", "UPDATE"},
			requiredTrigger{"trg_skillctx_no_delete", "usage_skill_context", "DELETE"},
		)
	}
	if version >= 7 {
		tables = append(tables, requiredTable{"usage_turn_context", []string{"usage_dedup_key", "dimension", "value", "tool", "session_id", "project", "model", "event_time_unix", "observed_time_unix", "source_path"}})
		indexes = append(indexes,
			requiredIndex{"idx_turnctx_dim_value_time", "usage_turn_context", []string{"dimension", "value", "event_time_unix"}},
			requiredIndex{"idx_turnctx_dim_time", "usage_turn_context", []string{"dimension", "event_time_unix"}},
			requiredIndex{"idx_turnctx_session", "usage_turn_context", []string{"session_id"}},
		)
		triggers = append(triggers,
			requiredTrigger{"trg_turnctx_no_update", "usage_turn_context", "UPDATE"},
			requiredTrigger{"trg_turnctx_no_delete", "usage_turn_context", "DELETE"},
		)
	}
	if version >= 8 {
		tables = append(tables, requiredTable{"activity_usage_counts", []string{"usage_dedup_key", "activity_count"}})
	}
	if version >= 9 {
		tables = append(tables, requiredTable{"code_changes", []string{"tool", "change_id", "session_id", "project", "known", "lines_added", "lines_removed", "updated_at_unix_ms", "observed_time_unix"}})
		indexes = append(indexes, requiredIndex{"idx_code_changes_session", "code_changes", []string{"tool", "session_id", "project"}})
	}
	return tables, indexes, triggers
}

func schemaObjectExists(ctx context.Context, db *sql.DB, kind, name, table string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type=? AND name=? AND tbl_name=?`,
		kind, name, table).Scan(&count)
	return count == 1, err
}

func indexColumns(ctx context.Context, db *sql.DB, index string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func availableRowCounts(ctx context.Context, db *sql.DB, version int) (map[string]int64, error) {
	tables := []string{"schema_meta", "usage_events", "aggregate_state"}
	if version >= 2 {
		tables = append(tables, "source_checkpoints")
	}
	if version >= 4 {
		tables = append(tables, "usage_rollup")
	}
	if version >= 5 {
		tables = append(tables, "activity_events")
	}
	if version == 6 {
		tables = append(tables, "usage_skill_context")
	}
	if version >= 7 {
		tables = append(tables, "usage_turn_context")
	}
	if version >= 8 {
		tables = append(tables, "activity_usage_counts")
	}
	if version >= 9 {
		tables = append(tables, "code_changes")
	}
	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = count
	}
	return counts, nil
}

func validateBackupPaths(source, destination string) (string, string, error) {
	if source == "" {
		return "", "", fmt.Errorf("store: empty backup source path")
	}
	if destination == "" {
		return "", "", fmt.Errorf("store: empty backup destination path")
	}
	sourcePath, err := filepath.Abs(source)
	if err != nil {
		return "", "", fmt.Errorf("store: resolve backup source %s: %w", source, err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return "", "", fmt.Errorf("store: stat backup source %s: %w", sourcePath, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("store: backup source is not a regular file: %s", sourcePath)
	}
	destinationPath, err := filepath.Abs(destination)
	if err != nil {
		return "", "", fmt.Errorf("store: resolve backup destination %s: %w", destination, err)
	}
	if filepath.Clean(sourcePath) == filepath.Clean(destinationPath) {
		return "", "", fmt.Errorf("store: backup source and destination are the same path: %s", sourcePath)
	}
	if _, err := os.Lstat(destinationPath); err == nil {
		if destinationInfo, statErr := os.Stat(destinationPath); statErr == nil && os.SameFile(info, destinationInfo) {
			return "", "", fmt.Errorf("store: backup source and destination resolve to the same file: %s", sourcePath)
		}
		return "", "", fmt.Errorf("store: backup destination already exists: %s", destinationPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("store: inspect backup destination %s: %w", destinationPath, err)
	}
	return sourcePath, destinationPath, nil
}

func resolvedPathsEqual(source, destination string) (bool, error) {
	resolvedSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return false, fmt.Errorf("store: resolve backup source %s: %w", source, err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return false, fmt.Errorf("store: resolve backup destination directory %s: %w", filepath.Dir(destination), err)
	}
	resolvedDestination := filepath.Join(resolvedParent, filepath.Base(destination))
	return filepath.Clean(resolvedSource) == filepath.Clean(resolvedDestination), nil
}

type onlineBackupConn interface {
	NewBackup(string) (*sqlite.Backup, error)
}

func copyOnline(ctx context.Context, source, destination string, onBatch func(int64)) error {
	db, _, err := openRecoveryReadOnly(ctx, source)
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
		backupConn, ok := driverConn.(onlineBackupConn)
		if !ok {
			return fmt.Errorf("SQLite driver does not expose the Online Backup API")
		}
		u := sqlitePathURL(destination)
		backup, err := backupConn.NewBackup(u.String())
		if err != nil {
			return err
		}
		return stepOnlineBackup(ctx, backup, onBatch)
	})
}

func stepOnlineBackup(ctx context.Context, backup *sqlite.Backup, onBatch func(int64)) (err error) {
	var batches int64
	return stepOnlineTransfer(ctx, backup, backup.Finish, func() {
		batches++
		if onBatch != nil {
			onBatch(batches)
		}
	})
}

func stepOnlineRestore(ctx context.Context, backup *sqlite.Backup) error {
	// NewRestore keeps the target on the caller-owned connection and opens the
	// source as Backup's remote connection. Finish closes that source and leaves
	// the target connection alive for database/sql to close normally.
	return stepOnlineTransfer(ctx, backup, backup.Finish, nil)
}

type onlineStepper interface {
	Step(int32) (bool, error)
}

type onlineTransferPolicy struct {
	pagesPerStep   int32
	maxBusyRetries int
	retryDelay     time.Duration
}

var defaultOnlineTransferPolicy = onlineTransferPolicy{
	pagesPerStep:   256,
	maxBusyRetries: 200,
	retryDelay:     25 * time.Millisecond,
}

func stepOnlineTransfer(ctx context.Context, backup onlineStepper, finish func() error, onBatch func()) error {
	return stepOnlineTransferWithPolicy(ctx, backup, finish, onBatch, defaultOnlineTransferPolicy)
}

func stepOnlineTransferWithPolicy(ctx context.Context, backup onlineStepper, finish func() error, onBatch func(), policy onlineTransferPolicy) (err error) {
	defer func() {
		if finishErr := finish(); err == nil && finishErr != nil {
			err = finishErr
		}
	}()

	busyRetries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		more, stepErr := backup.Step(policy.pagesPerStep)
		if stepErr == nil {
			busyRetries = 0
			if onBatch != nil {
				onBatch()
			}
			if !more {
				return nil
			}
			continue
		}
		if !isBusySQLiteError(stepErr) || busyRetries >= policy.maxBusyRetries {
			return stepErr
		}
		busyRetries++
		timer := time.NewTimer(policy.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// makeBackupStandalone changes journal mode only on the unpublished snapshot.
// Online Backup copies a WAL source's persistent journal mode into the main
// file; switching the closed snapshot to DELETE makes the final file usable
// without a WAL/SHM pair. No application row or source file is changed.
func makeBackupStandalone(ctx context.Context, path string) error {
	u, err := url.Parse(sqliteFileURI(path, true))
	if err != nil {
		return fmt.Errorf("store: build backup URI for %s: %w", path, err)
	}
	query := u.Query()
	query.Set("mode", "rw")
	query.Del("_pragma")
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("store: open backup partial %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		db.Close()
		return fmt.Errorf("store: make backup standalone %s: %w", path, err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("store: close standalone backup %s: %w", path, err)
	}
	if !strings.EqualFold(mode, "delete") {
		return fmt.Errorf("store: make backup standalone %s: SQLite kept journal mode %q", path, mode)
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: remove backup sidecar %s: %w", sidecar, err)
		}
	}
	return nil
}

func ensureStandaloneBackup(path string) error {
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			return fmt.Errorf("store: backup is not standalone; unexpected sidecar %s", sidecar)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: inspect backup sidecar %s: %w", sidecar, err)
		}
	}
	return nil
}

func removeBackupPartial(path string) {
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		_ = os.Remove(candidate)
	}
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func fileIdentity(ctx context.Context, path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("store: open backup for digest %s: %w", path, err)
	}
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	var written int64
	var copyErr error
	for {
		if err := ctx.Err(); err != nil {
			copyErr = err
			break
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
			written += int64(n)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				copyErr = readErr
			}
			break
		}
	}
	closeErr := f.Close()
	if copyErr != nil {
		return 0, "", fmt.Errorf("store: hash backup %s: %w", path, copyErr)
	}
	if closeErr != nil {
		return 0, "", fmt.Errorf("store: close backup after digest %s: %w", path, closeErr)
	}
	return written, hex.EncodeToString(hash.Sum(nil)), nil
}

func sqlitePrimaryErrorCode(err error) (int, bool) {
	var sqliteErr interface{ Code() int }
	if !errors.As(err, &sqliteErr) {
		return 0, false
	}
	return sqliteErr.Code() & 0xff, true
}

func isCorruptSQLiteError(err error) bool {
	code, ok := sqlitePrimaryErrorCode(err)
	return ok && (code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB)
}

func isBusySQLiteError(err error) bool {
	code, ok := sqlitePrimaryErrorCode(err)
	return ok && (code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED)
}
