// Schema versioning and the migration runner. Open never writes before
// reading the recorded version: fresh databases are created directly at
// SchemaVersion, older ones run the ordered additive migrations, and a newer
// database refuses to open so an older binary can never stamp a version
// backwards.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// migration is one ordered schema step bringing a database from version-1 to
// version. Authoritative ledgers are additive: trg_events_no_update blocks
// UPDATE backfill on usage_events. Derived tables may be replaced because they
// are reproducible, but the replacement still commits before the version stamp.
type migration struct {
	version    int
	statements []string
}

// migrations lists every step needed to bring an older database up to
// SchemaVersion, in ascending version order with no gaps. Fresh databases
// never run these; they are created directly at SchemaVersion from schema.sql,
// which must always describe the latest schema in full.
//
// Version ledger:
//
//	v2 — source_checkpoints table (incremental collection, issue #5)
//	v3 — cost/provider/tier columns on usage_events (issues #9, #16)
//	v4 — usage_rollup derived rollup table, 15-minute UTC buckets (issue #59)
//	v5 — activity_events ledger: tool/skill/hook invocations, joined to
//	     usage_events by dedup key for cost attribution
//	v6 — usage_skill_context: the skill a turn ran under, one row per usage
//	     event, for un-divided per-skill cost
//	v7 — usage_turn_context: all FIVE turn-attribution axes (agent, skill,
//	     mcp_tool, mcp_server, plugin) in one table keyed by (usage event,
//	     dimension); retires usage_skill_context, which it subsumes, and clears
//	     the claude-code checkpoint so the next pass re-derives both
//	v8 — enriched usage_rollup plus the activity_usage_counts derived divisor;
//	     no authoritative ledger row changes
//	v9 — mutable code_changes snapshots, independent of usage and checkpoints
var migrations = []migration{
	{version: 2, statements: []string{
		`CREATE TABLE IF NOT EXISTS source_checkpoints (
		  tool        TEXT    NOT NULL,
		  source_path TEXT    NOT NULL,
		  size_bytes  INTEGER NOT NULL DEFAULT 0,
		  mtime_ns    INTEGER NOT NULL DEFAULT 0,
		  read_offset INTEGER NOT NULL DEFAULT 0,
		  watermark   INTEGER NOT NULL DEFAULT 0,
		  state       TEXT,
		  PRIMARY KEY (tool, source_path)
		)`,
	}},
	// v3 only appends columns. Existing rows keep the SQL defaults ('' for the
	// text columns) and a NULL cost, which is exactly the "unpriced, price it
	// at display time" state — no backfill is attempted or possible, since the
	// no-UPDATE trigger forbids rewriting history.
	{version: 3, statements: []string{
		`ALTER TABLE usage_events ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_events ADD COLUMN service_tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_events ADD COLUMN cost_micro_usd INTEGER`,
		`ALTER TABLE usage_events ADD COLUMN price_source TEXT NOT NULL DEFAULT ''`,
	}},
	// v4 creates the derived rollup EMPTY and deliberately does not fill it:
	// a migration that scanned the whole ledger would hold the write lock for
	// the length of a rebuild on every upgrade. The collector detects the empty
	// rollup against the ledger watermark on its next pass and rebuilds it
	// there (EnsureRollup), where the cost is visible and non-fatal.
	{version: 4, statements: []string{rollupV4TableDDL}},
	// v5 creates the activity ledger EMPTY, and there is nothing to backfill
	// into it: activity is not derivable from usage_events (the ledger records
	// what a turn cost, never which tools it called), so the rows can only come
	// from re-reading the sources. Collection picks them up from wherever each
	// adapter's checkpoint sits — see the adapters' own notes on what that
	// means for history.
	{version: 5, statements: activityV5Statements()},
	// v6 creates the skill-context table EMPTY, and NOTHING CAN EVER BACKFILL
	// IT for rows already in the ledger. The fact it records — which skill a
	// turn ran under — exists only on the source transcript record; usage_events
	// never carried it, and the no-UPDATE trigger forbids adding it to a stored
	// row even if it had. Rows land only where a re-read of the sources reaches,
	// which each adapter's checkpoint bounds. In practice: usage already
	// collected stays permanently unattributed to any skill, and per-skill cost
	// is answerable from the moment this version ships forward, not backwards.
	// Only the CONTEXT is lost, never the cost — those turns remain in
	// usage_events in full, and every total that does not group by skill is
	// unaffected.
	{version: 6, statements: skillContextV6Statements()},
	// v7 generalises v6's one axis into five and is the only step so far that
	// DROPS anything. Both halves of that need saying.
	//
	// The drop is safe because usage_skill_context is DERIVED. Every row of it
	// is re-derived from the source transcript on the next collection pass, the
	// way usage_rollup is re-derived from the ledger; nothing originates there,
	// unlike usage_events, whose rows are the only copy of a fact the provider
	// will never repeat. Its rows are exactly this table's dimension='skill'
	// partition, and keeping both would leave two tables answering one question
	// with nothing keeping them in step — the mistake this whole change exists
	// to avoid at the schema level, not just in the query builder.
	//
	// The checkpoint reset is what makes "re-derived on the next pass" TRUE. The
	// claude-code adapter gates a root on a size+mtime manifest and opens no
	// file when nothing changed, so without this an idle machine would keep an
	// empty table and never see the four new dimensions at all. Clearing the
	// checkpoint costs one full re-parse, once; it cannot double-count anything,
	// because usage and activity rows re-derive their existing dedup keys and
	// conflict-skip. source_checkpoints is mutable working state (no
	// append-only triggers) and this is the one table where a DELETE is a
	// re-read rather than a loss.
	//
	// Like every step before it, the new table is created EMPTY and nothing
	// backfills it from usage_events: the facts exist only on the source
	// transcripts, and the no-UPDATE trigger forbids adding them to stored rows
	// regardless. Rows land only where a re-read of the sources reaches — which
	// is now the whole claude-code corpus, because of the line above.
	{version: 7, statements: turnContextV7Statements()},
	// v8 replaces only DERIVED state. usage_rollup is intentionally left empty
	// with no watermark so the next collection pass performs the visible,
	// repairable rebuild outside the migration transaction. Activity counts are
	// small and required immediately by every activity query, so they are
	// rebuilt set-wise from the append-only activity ledger here. The migration
	// runner stamps v8 only after every statement succeeds.
	{version: 8, statements: []string{
		`DROP TABLE IF EXISTS usage_rollup`,
		rollupTableDDL,
		`DELETE FROM schema_meta WHERE key='rollup_watermark'`,
		`DROP TABLE IF EXISTS activity_usage_counts`,
		activityUsageCountsTableDDL,
		rebuildActivityUsageCountsSQL,
	}},
	// Source adapters read these snapshots independently of usage checkpoints.
	// No history rewrite or checkpoint reset is needed to populate them.
	{version: 9, statements: []string{codeChangesTableDDL, codeChangesSessionIndexDDL}},
}

// ensureSchema reads the recorded schema version before touching anything and
// brings the database to SchemaVersion. path names the file in diagnostics
// only; every statement runs against db.
func ensureSchema(ctx context.Context, db *sql.DB, path string) error {
	hasMeta, err := tableExists(ctx, db, "schema_meta")
	if err != nil {
		return err
	}
	current := 0
	if hasMeta {
		current, err = readSchemaVersion(ctx, db)
		if err != nil {
			return fmt.Errorf("store: read schema version: %w", err)
		}
	}
	switch {
	case current == 0:
		// Fresh database, or a create interrupted before the version stamp.
		// Reapplying schema.sql covers only the first case: every statement in
		// it is CREATE ... IF NOT EXISTS, so a usage_events left behind by an
		// older binary keeps whatever columns it was created with — v3 appends
		// columns, and no CREATE adds them to an existing table. Stamping
		// SchemaVersion over that would claim a layout the database does not
		// have, and every insert would then fail on the placeholder count. So
		// the column set is verified before the stamp, and an incomplete table
		// is refused with the recovery instead.
		if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("store: apply schema: %w", err)
		}
		if err := verifyEventColumns(ctx, db, path); err != nil {
			return err
		}
		// Upsert is safe here: this branch only runs when no version is
		// recorded, so it can never stamp an existing version backwards.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_meta(key, value) VALUES('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			strconv.Itoa(SchemaVersion),
		); err != nil {
			return fmt.Errorf("store: record schema version: %w", err)
		}
		return nil
	case current == SchemaVersion:
		return nil
	case current < SchemaVersion:
		return applyMigrations(ctx, db, current, SchemaVersion, migrations)
	default:
		// Wrapped in ErrSchemaNewer so a caller can branch on the one open
		// failure it can do something about; the message keeps both versions,
		// which is what a person reading it needs.
		return fmt.Errorf("%w: %s records v%d, this binary's is v%d; upgrade aiusage to open it",
			ErrSchemaNewer, path, current, SchemaVersion)
	}
}

// applyMigrations runs every step in (from, to] in ascending order, refusing
// gaps so a database can never skip a version.
func applyMigrations(ctx context.Context, db *sql.DB, from, to int, steps []migration) error {
	next := from + 1
	for _, m := range steps {
		if m.version <= from {
			continue
		}
		if m.version > to {
			break
		}
		if m.version != next {
			return fmt.Errorf("store: migration to v%d missing (next step is v%d)", next, m.version)
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
		next++
	}
	if next != to+1 {
		return fmt.Errorf("store: no migration path from v%d to v%d", from, to)
	}
	return nil
}

// applyMigration runs one step in its own transaction with the version stamp
// as the last statement, so an interrupted migration never advances the
// recorded version past what actually committed.
//
// The version is re-checked INSIDE the transaction, after a write statement
// has escalated it to a write lock: two processes upgrading concurrently
// would otherwise both read the old version outside any lock and both run the
// same step (harmless for CREATE IF NOT EXISTS, corrupting for a future
// ALTER ... ADD COLUMN). The loser of the lock race re-reads the advanced
// version and skips.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration v%d: %w", m.version, err)
	}
	defer tx.Rollback()

	// No-op UPDATE: its only purpose is to take the write lock BEFORE the
	// version re-read, so the read cannot see a stale pre-upgrade snapshot.
	if _, err := tx.ExecContext(ctx,
		`UPDATE schema_meta SET value=value WHERE key='schema_version'`); err != nil {
		return fmt.Errorf("store: lock for migration v%d: %w", m.version, err)
	}
	cur, err := readSchemaVersionQ(ctx, tx)
	if err != nil {
		return fmt.Errorf("store: re-read schema version for v%d: %w", m.version, err)
	}
	if cur >= m.version {
		return nil // another process already ran this step; deferred Rollback releases the lock
	}
	if cur != m.version-1 {
		return fmt.Errorf("store: migration v%d expected database at v%d, found v%d", m.version, m.version-1, cur)
	}

	for _, stmt := range m.statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: migration v%d: %w", m.version, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.Itoa(m.version),
	); err != nil {
		return fmt.Errorf("store: stamp schema version v%d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration v%d: %w", m.version, err)
	}
	return nil
}

// eventColumns lists every column usage_events carries at SchemaVersion. It
// mirrors schema.sql and the explicit column list of the insert statement in
// sqlite.go; extend it in the same change that adds a column to the table.
var eventColumns = []string{
	"id", "dedup_key", "tool", "model", "session_id", "project",
	"event_time_unix", "observed_time_unix",
	"input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
	"reasoning_tokens", "total_tokens",
	"request_id", "message_id", "source_path", "kind", "raw",
	"provider", "service_tier", "cost_micro_usd", "price_source",
}

// verifyEventColumns refuses to stamp SchemaVersion over a usage_events that
// predates it. It only ever runs on an UNSTAMPED database (ensureSchema's
// current == 0 branch): with no recorded version there is nothing that says
// which migrations the table has seen, so guessing one and running ALTERs on a
// ledger of unknown provenance is not on offer.
//
// Both refusals leave the file untouched, but only one of them may advise
// deleting it, so the table is checked for rows rather than argued about. Empty
// is the interrupted first run the recovery assumes: nothing committed, nothing
// to lose. A short table that DOES hold events is a ledger of unknown
// provenance, and telling its owner to delete it would destroy the history this
// package exists to keep, so that case says to move it aside instead.
func verifyEventColumns(ctx context.Context, db *sql.DB, path string) error {
	have, err := columnSet(ctx, db, "usage_events")
	if err != nil {
		return err
	}
	var missing []string
	for _, c := range eventColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	empty, err := tableEmpty(ctx, db, "usage_events")
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf(
			"store: %s has a usage_events table with no recorded schema version and missing column(s) %s, "+
				"and it holds recorded events, so it is not a half-created database; "+
				"do NOT delete it - move it aside (with its -wal/-shm sidecars) to start a fresh ledger, "+
				"and keep the file: the aiusage build that wrote it can still read it",
			path, strings.Join(missing, ", "))
	}
	return fmt.Errorf(
		"store: %s has a usage_events table with no recorded schema version and missing column(s) %s; "+
			"the table is empty, so this is a half-created database from an interrupted first run - "+
			"delete it (with its -wal/-shm sidecars) and rerun",
		path, strings.Join(missing, ", "))
}

// tableEmpty reports whether a table holds no rows. EXISTS stops at the first
// row, so it stays cheap on a large ledger where COUNT(*) would scan the lot.
func tableEmpty(ctx context.Context, db *sql.DB, table string) (bool, error) {
	// The table name cannot be a placeholder; every caller passes a literal.
	var found int
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM "`+table+`")`).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: check whether %s is empty: %w", table, err)
	}
	return found == 0, nil
}

// columnSet returns the column names of a table.
func columnSet(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("store: read columns of %s: %w", table, err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: read columns of %s: %w", table, err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read columns of %s: %w", table, err)
	}
	return out, nil
}

// tableExists reports whether a table is present, so Open can read the version
// without creating anything first.
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: check table %s: %w", name, err)
	}
	return n > 0, nil
}

// rowQuerier is the QueryRowContext subset shared by *sql.DB and *sql.Tx, so
// the version read can also run inside a migration's transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readSchemaVersion returns the recorded schema version, or 0 when the row is
// absent. The schema_meta table must already exist.
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	return readSchemaVersionQ(ctx, db)
}

func readSchemaVersionQ(ctx context.Context, q rowQuerier) (int, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("unrecognized schema_version %q", v)
	}
	return n, nil
}
