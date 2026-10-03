// SQLite-backed implementation of the two handles. Pure Go via
// modernc.org/sqlite (CGO_ENABLED=0). The append-only guarantee is enforced by
// schema.sql (UNIQUE(dedup_key) + no-UPDATE/no-DELETE triggers); this file only
// ever appends to usage_events (INSERT .. ON CONFLICT(dedup_key) DO NOTHING)
// and upserts mutable accumulator and code-change state.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

//go:embed schema.sql
var schemaSQL string

// SchemaVersion is the schema version this binary creates fresh databases at
// and can open. Bump when the schema changes, keep schema.sql describing the
// full latest schema, and add the matching step to migrations (migrate.go).
const SchemaVersion = 9

// Reader is the READ handle: every query, summary, listing and ranking this
// package answers, and NOT ONE METHOD THAT WRITES. That absence is the point.
// The append-only ledger used to be defended at three depths - the schema's
// no-UPDATE/no-DELETE triggers, the mode=ro connection, and a flag this package
// checked at the top of each write method - all three of them at RUNTIME, on a
// handle whose type advertised InsertEvents to anyone holding it. A consumer
// given one of these cannot call a write, cannot compile a program that calls a
// write, and needs no test to prove it did not (issue #72, decision 2 and 8).
//
// It is returned by OpenReadOnly and embedded in Ledger, so the collector holds
// one handle carrying both halves while every reporting surface can be handed
// the read half alone. Consumers that want a fake declare their own narrow
// interface over the methods they actually call - this package exports no fat
// one to implement.
type Reader struct {
	db   *sql.DB
	path string
}

// Ledger is the FULL handle: a Reader plus the appends. It is what Open returns
// and what the collector holds, and it is the only type in this package that
// can add a row to any of the three append-only tables, upsert accumulator
// state or code-change snapshots, or rebuild the derived rollup.
//
// The Reader is embedded by pointer so a caller that only reads can be handed
// l.Reader by name rather than by an interface conversion, and so the two
// handles are never two connections to one file.
type Ledger struct{ *Reader }

// Open opens (creating if absent) the database at path with WAL and
// busy_timeout=5000 pragmas, then reads the recorded schema version before
// touching anything: same version opens as-is, older versions run the ordered
// migrations (migrate.go), and a newer version is refused so an older binary
// can never stamp it backwards. The handle is read/write because the collector
// appends to it; all reporting paths only issue SELECTs.
func Open(path string) (*Ledger, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}
	preMigrationBackup, err := prepareMigration(context.Background(), path)
	if err != nil {
		return nil, err
	}

	// modernc driver name is "sqlite". Pragmas applied via the DSN run on every
	// pooled connection; the schema is managed once by ensureSchema below.
	// synchronous=NORMAL is the WAL-recommended durability level (the default
	// FULL fsyncs every commit; NORMAL only at checkpoints).
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve database path: %w", err)
	}
	dsn := sqliteFileURI(absolute, false)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	if err := ensureSchema(context.Background(), db, path); err != nil {
		if preMigrationBackup != "" {
			err = migrationFailure(context.Background(), db, path, preMigrationBackup, err)
		}
		db.Close()
		return nil, err
	}
	if preMigrationBackup != "" {
		if err := verifyCompletedMigration(context.Background(), path, preMigrationBackup); err != nil {
			db.Close()
			return nil, err
		}
	}

	// The raw column holds transcript content, so the DB must be owner-only.
	// SQLite creates WAL/SHM sidecars with the main DB file's mode, so tightening
	// the DB here also covers sidecars created later; existing sidecars are fixed
	// directly. Best-effort: doctor surfaces perms that could not be repaired.
	restrictPerms(path)

	return &Ledger{Reader: &Reader{db: db, path: path}}, nil
}

// OpenReadOnly opens an EXISTING database for reading only (issue #60): the
// connection is mode=ro, no schema is created, no migration is run, and no file
// mode is touched. It returns a Reader, which HAS NO WRITE METHOD, so "this
// process cannot write the ledger" is a property of the type its caller holds
// rather than a promise checked somewhere further down.
//
// The connection is still opened mode=ro plus query_only(1) - defence in depth,
// the same DSN every adapter uses over an agent's own database. The compile-time
// absence covers this package's own API; the pragmas cover a future statement
// issued from inside this package, which is where a write would have to come
// from now that no caller can ask for one.
//
// A schema version that differs from this binary's in EITHER direction is
// refused. Migrating would be a write, and a reader that quietly serves a
// schema it does not understand is worse than one that will not start.
func OpenReadOnly(path string) (*Reader, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	// No journal_mode pragma: WAL is a property of the file, and setting it on
	// a read-only connection fails. busy_timeout still matters - readers wait
	// out a writer's commit instead of erroring.
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve database path: %w", err)
	}
	dsn := sqliteFileURI(absolute, true)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s read-only: %w", path, err)
	}

	ctx := context.Background()
	hasMeta, err := tableExists(ctx, db, "schema_meta")
	if err != nil {
		db.Close()
		return nil, err
	}
	version := 0
	if hasMeta {
		version, err = readSchemaVersion(ctx, db)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("store: read schema version: %w", err)
		}
	}
	if version != SchemaVersion {
		db.Close()
		// A NEWER database is the same refusal Open gives and carries the same
		// sentinel: whichever handle a caller asked for, the answer is to
		// upgrade the binary. An OLDER one is deliberately NOT ErrSchemaNewer -
		// the instruction is the opposite, and it is the plain message below.
		if version > SchemaVersion {
			return nil, fmt.Errorf(
				"%w: %s records schema v%d, this binary's is v%d; a read-only handle never migrates, so run a matching aiusage build",
				ErrSchemaNewer, path, version, SchemaVersion)
		}
		return nil, fmt.Errorf(
			"store: %s records schema v%d, this binary's is v%d; a read-only handle never migrates, so open it read-write once (or run a matching aiusage build) before serving it",
			path, version, SchemaVersion)
	}

	return &Reader{db: db, path: path}, nil
}

// restrictPerms chmods the DB and its WAL/SHM sidecars to 0600. Missing
// sidecars and chmod failures are ignored (see Open).
func restrictPerms(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(p, 0o600)
	}
}

func ensureParentDir(path string) error {
	dir := dirOf(path)
	if dir == "" || dir == "." {
		return nil
	}
	// 0700: the DB inside holds transcript content (raw column).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create dir %s: %w", dir, err)
	}
	return nil
}

// dirOf returns the directory component of a path without importing path-style
// assumptions beyond the OS separator handled by the caller's filepath usage.
func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// Close releases the database handle.
func (s *Reader) Close() error { return s.db.Close() }

// InsertEvents appends events idempotently in a single transaction. Returns the
// count of rows actually inserted (new dedup keys). Existing dedup keys are
// ignored; rows are never updated or deleted. A row that fails its own insert
// (CHECK violation, empty dedup key) is skipped and reported in the returned
// error while the rest of the batch still commits — one poison row must not
// abort a batch that is re-read and retried every cycle.
func (l *Ledger) InsertEvents(ctx context.Context, events []model.UsageEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	inserted, skipErr, err := insertEventsTx(ctx, tx, events)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	return inserted, skipErr
}

// insertEventsTx runs the idempotent event insert inside an existing
// transaction, so ApplySnapshot (and future collection-scoped writes such as
// cycle checkpoints) can combine it with other statements atomically.
// ON CONFLICT(dedup_key) DO NOTHING keeps the dedup ignore silent while a
// CHECK violation still errors (blanket OR IGNORE would swallow it). Per-row
// CHECK failures are skipped and summarised in skipErr. Every other insert
// failure is a batch error, after which the transaction must not be committed.
//
// The derived rollup's delta is written here, from the rows that were ACTUALLY
// inserted (dedup collisions contribute nothing), inside the caller's
// transaction. Every event that reaches the ledger passes through this
// function, so that placement is what makes "a crash cannot land events
// without the matching rollup delta" true rather than aspirational. A rollup
// failure is returned as err, not skipErr: the batch must roll back, since
// committing events whose delta was lost is exactly the state being prevented.
func insertEventsTx(ctx context.Context, tx *sql.Tx, events []model.UsageEvent) (inserted int, skipErr, err error) {
	if len(events) == 0 {
		return 0, nil, nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO usage_events (
			dedup_key, tool, model, session_id, project,
			event_time_unix, observed_time_unix,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			reasoning_tokens, total_tokens,
			request_id, message_id, source_path, kind, raw,
			provider, service_tier, cost_micro_usd, price_source
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(dedup_key) DO NOTHING`)
	if err != nil {
		return 0, nil, fmt.Errorf("store: prepare insert: %w", err)
	}
	defer stmt.Close()

	var (
		skips = rowSkips{table: tableUsageEvents}
		roll  rollupDelta
	)
	for _, e := range events {
		if err := ctx.Err(); err != nil {
			return inserted, nil, err
		}
		if e.DedupKey == "" {
			skips.add(e.DedupKey, fmt.Errorf("store: event with empty dedup key (tool=%s)", e.Tool))
			continue
		}
		kind := e.Kind
		if kind == "" {
			kind = model.KindUsage
		}
		res, execErr := stmt.ExecContext(ctx,
			e.DedupKey, e.Tool, e.Model, e.SessionID, e.Project,
			e.EventTime.UTC().Unix(), observedUnix(e),
			e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens,
			e.ReasoningTokens, e.TotalTokens,
			e.RequestID, e.MessageID, e.SourcePath, string(kind), nullString(e.Raw),
			e.Provider, e.ServiceTier, nullCost(e), e.PriceSource,
		)
		if execErr != nil {
			rowErr := fmt.Errorf("store: insert event %s: %w", e.DedupKey, execErr)
			if !isSkippableRowError(execErr) {
				return inserted, nil, rowErr
			}
			skips.add(e.DedupKey, rowErr)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
			id, idErr := res.LastInsertId()
			if idErr != nil {
				return inserted, nil, fmt.Errorf("store: row id of event %s: %w", e.DedupKey, idErr)
			}
			roll.add(e, id)
		}
	}
	if err := roll.apply(ctx, tx); err != nil {
		return inserted, nil, err
	}
	return inserted, skips.err(len(events)), nil
}

// applySnapshotFault, when non-nil, runs between the event insert and the
// state upsert inside ApplySnapshot's transaction. Test-only seam simulating a
// crash in the window that used to double count (events committed, state not).
var applySnapshotFault func() error

// ApplySnapshot atomically appends an aggregate cell's delta events and records
// its new accumulator state in ONE transaction, so a crash can never persist the
// events without the state (the next cycle would re-derive the same delta under
// a fresh dedup key — a permanent double count).
//
// When events is non-empty but every dedup key already exists, the state write
// is SKIPPED: an unchanged baseline lets the next poll re-derive the colliding
// delta instead of dropping it. A non-nil cp is upserted under the same
// condition and in the same transaction, so a checkpoint can never claim data
// whose state write was skipped or rolled back — a collided delta stays
// re-derivable only while neither baseline nor checkpoint advances. Returns the
// number of events actually inserted.
func (l *Ledger) ApplySnapshot(ctx context.Context, events []model.UsageEvent, state model.AggregateSnapshot, cp *model.SourceCheckpoint) (int, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	inserted, skipErr, err := insertEventsTx(ctx, tx, events)
	if err != nil {
		return 0, err
	}
	if skipErr != nil {
		// A skipped delta event must not advance the baseline: rolling back
		// keeps the delta re-derivable next cycle.
		return 0, fmt.Errorf("store: snapshot rolled back: %v", skipErr)
	}
	if applySnapshotFault != nil {
		if err := applySnapshotFault(); err != nil {
			return 0, err
		}
	}
	if len(events) == 0 || inserted > 0 {
		if err := upsertState(ctx, tx, state); err != nil {
			return 0, err
		}
		if cp != nil {
			if err := upsertCheckpoint(ctx, tx, *cp); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	return inserted, nil
}

// ApplyEvents appends usage events (same idempotent semantics as InsertEvents)
// and upserts the source checkpoint in ONE transaction. A checkpoint persisted
// outside the event transaction could outrun the events it claims — a crash
// between the two commits would then skip data forever. A nil cp degrades to a
// plain event insert; empty events with a non-nil cp writes just the checkpoint.
//
// It is the events-only shorthand for ApplyBatch. Per-row skips (poison rows)
// still commit alongside the checkpoint: they are permanent CHECK violations a
// re-read cannot fix, so holding the checkpoint back would only re-parse them
// forever.
func (l *Ledger) ApplyEvents(ctx context.Context, events []model.UsageEvent, cp *model.SourceCheckpoint) (int, error) {
	res, err := l.ApplyObservation(ctx, events, nil, cp)
	return res.Events, err
}

// ApplyObservation appends usage events, appends agent activity rows, and
// upserts the source checkpoint — all in ONE transaction. It is ApplyBatch
// without the turn contexts.
//
// The single transaction is the point. Activity rows reference usage rows by
// dedup key, and the checkpoint gates the re-read of both: splitting them across
// transactions would let a crash advance the checkpoint past activity that never
// landed, losing it permanently, or leave a call pointing at a usage row that
// rolled back. Activity is appended AFTER the events so the row it names already
// exists.
//
// Activity has the same idempotence as events (ON CONFLICT(dedup_key) DO
// NOTHING) and the same per-row skip behaviour, so the returned counts stay
// meaningful when the error is non-nil.
func (l *Ledger) ApplyObservation(ctx context.Context, events []model.UsageEvent, activity []model.ActivityEvent, cp *model.SourceCheckpoint) (Applied, error) {
	return l.ApplyBatch(ctx, ObservationBatch{Events: events, Activity: activity, Checkpoint: cp})
}

// ApplyBatch is ApplyObservation plus turn contexts and code-change snapshots.
// It uses the same transaction and ordering (events first, so
// the rows naming them by dedup key find them present), same idempotence: a turn
// context conflicts on (usage dedup key, dimension) and does nothing, which is
// what stops a re-read serving a turn's cost twice.
func (l *Ledger) ApplyBatch(ctx context.Context, b ObservationBatch) (Applied, error) {
	events, activity, ctxs, cp := b.Events, b.Activity, b.TurnContexts, b.Checkpoint
	if len(events) == 0 && len(activity) == 0 && len(ctxs) == 0 && len(b.CodeChanges) == 0 && cp == nil {
		return Applied{}, nil
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Applied{}, fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	var out Applied
	inserted, skipErr, err := insertEventsTx(ctx, tx, events)
	out.Events = inserted
	if err != nil {
		return Applied{}, err
	}
	actInserted, actSkipErr, err := insertActivityTx(ctx, tx, activity)
	out.Activity = actInserted
	if err != nil {
		return Applied{}, err
	}
	ctxInserted, ctxSkipErr, err := insertTurnContextsTx(ctx, tx, ctxs)
	out.TurnContexts = ctxInserted
	if err != nil {
		return Applied{}, err
	}
	out.CodeChanges, err = upsertCodeChangesTx(ctx, tx, b.CodeChanges)
	if err != nil {
		return Applied{}, err
	}
	if cp != nil {
		if err := upsertCheckpoint(ctx, tx, *cp); err != nil {
			return Applied{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Applied{}, fmt.Errorf("store: commit: %w", err)
	}
	// Join preserves each table's committed skip report. errors.As finds the
	// first matching report; callers wanting every report traverse the joined
	// children. The message includes usage, activity, then turn context skips.
	return out, errors.Join(skipErr, actSkipErr, ctxSkipErr)
}

// Checkpoint returns the stored incremental state for (tool, sourcePath), or
// nil when none has been recorded.
func (s *Reader) Checkpoint(ctx context.Context, tool, sourcePath string) (*model.SourceCheckpoint, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tool, source_path, size_bytes, mtime_ns, read_offset, watermark, COALESCE(state,'')
		FROM source_checkpoints WHERE tool=? AND source_path=?`, tool, sourcePath)

	var out model.SourceCheckpoint
	err := row.Scan(&out.Tool, &out.SourcePath, &out.Size, &out.MTimeNS, &out.Offset, &out.Watermark, &out.State)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: checkpoint %s/%s: %w", tool, sourcePath, err)
	}
	return &out, nil
}

func upsertCheckpoint(ctx context.Context, db execer, cp model.SourceCheckpoint) error {
	if cp.Tool == "" || cp.SourcePath == "" {
		return fmt.Errorf("store: checkpoint missing tool/source path (%q/%q)", cp.Tool, cp.SourcePath)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO source_checkpoints (
			tool, source_path, size_bytes, mtime_ns, read_offset, watermark, state
		) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(tool, source_path) DO UPDATE SET
			size_bytes=excluded.size_bytes, mtime_ns=excluded.mtime_ns,
			read_offset=excluded.read_offset, watermark=excluded.watermark,
			state=excluded.state`,
		cp.Tool, cp.SourcePath, cp.Size, cp.MTimeNS, cp.Offset, cp.Watermark, nullString(cp.State),
	)
	if err != nil {
		return fmt.Errorf("store: upsert checkpoint %s/%s: %w", cp.Tool, cp.SourcePath, err)
	}
	return nil
}

// observedUnix returns the observed timestamp in UTC seconds, falling back to
// the event time when ObservedTime is unset.
func observedUnix(e model.UsageEvent) int64 {
	if e.ObservedTime.IsZero() {
		return e.EventTime.UTC().Unix()
	}
	return e.ObservedTime.UTC().Unix()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullCost binds the stamped cost, or SQL NULL when the event is unpriced.
// Binding 0 instead would assert the request was free.
func nullCost(e model.UsageEvent) any {
	if c, ok := e.Cost(); ok {
		return c
	}
	return nil
}

// LastState returns the latest observed counters for the (tool, key) accumulator
// cell, or nil if none has been recorded.
func (s *Reader) LastState(ctx context.Context, tool, key string) (*model.AggregateSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tool, acc_key, model, session_id, project, observed_time_unix,
		       input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		       reasoning_tokens, total_tokens, source_path, COALESCE(raw,'')
		FROM aggregate_state WHERE tool=? AND acc_key=?`, tool, key)

	var (
		out      model.AggregateSnapshot
		observed int64
	)
	err := row.Scan(
		&out.Tool, &out.Key, &out.Model, &out.SessionID, &out.Project, &observed,
		&out.InputTokens, &out.OutputTokens, &out.CacheCreationTokens, &out.CacheReadTokens,
		&out.ReasoningTokens, &out.TotalTokens, &out.SourcePath, &out.Raw,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: last state %s/%s: %w", tool, key, err)
	}
	out.ObservedTime = time.Unix(observed, 0).UTC()
	return &out, nil
}

// UpsertState records the latest observed counters for (tool, key), replacing
// any previous value. This is mutable accumulator state, not history.
func (l *Ledger) UpsertState(ctx context.Context, st model.AggregateSnapshot) error {
	return upsertState(ctx, l.db, st)
}

// execer is the ExecContext subset shared by *sql.DB and *sql.Tx, so the state
// upsert can run standalone (autocommit) or inside ApplySnapshot's transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func upsertState(ctx context.Context, db execer, st model.AggregateSnapshot) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO aggregate_state (
			tool, acc_key, model, session_id, project, observed_time_unix,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			reasoning_tokens, total_tokens, source_path, raw
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(tool, acc_key) DO UPDATE SET
			model=excluded.model, session_id=excluded.session_id, project=excluded.project,
			observed_time_unix=excluded.observed_time_unix,
			input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			cache_creation_tokens=excluded.cache_creation_tokens,
			cache_read_tokens=excluded.cache_read_tokens,
			reasoning_tokens=excluded.reasoning_tokens, total_tokens=excluded.total_tokens,
			source_path=excluded.source_path, raw=excluded.raw`,
		st.Tool, st.Key, st.Model, st.SessionID, st.Project, st.ObservedTime.UTC().Unix(),
		st.InputTokens, st.OutputTokens, st.CacheCreationTokens, st.CacheReadTokens,
		st.ReasoningTokens, st.TotalTokens, st.SourcePath, nullString(st.Raw),
	)
	if err != nil {
		return fmt.Errorf("store: upsert state %s/%s: %w", st.Tool, st.Key, err)
	}
	return nil
}

// Summarize aggregates usage matching Filter, grouped per Filter.GroupBy. Time
// dimensions (hour/day/week/month) are bucketed in the local timezone so "today"
// matches the wall clock; categorical dimensions group by their stored value.
func (s *Reader) Summarize(ctx context.Context, f Filter) (*Summary, error) {
	if rollupRangeAligned(f.Since, time.Time{}) {
		sum, current, err := s.summarizeCurrentRollup(ctx, f)
		if err != nil {
			return nil, err
		}
		if current {
			return sum, nil
		}
	}
	return s.summarizeLedger(ctx, f)
}

// summarizeLedger is Summarize's direct authoritative path. Tests compare it
// field for field with the accelerated path. Unaligned starts use this path;
// aligned starts can combine complete rollup buckets with exact ending events.
func (s *Reader) summarizeLedger(ctx context.Context, f Filter) (*Summary, error) {
	where, args := buildWhere(f)

	groupExprs := make([]string, 0, len(f.GroupBy))
	for _, dim := range f.GroupBy {
		expr, err := groupExpr(dim)
		if err != nil {
			return nil, err
		}
		groupExprs = append(groupExprs, expr)
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	for _, ge := range groupExprs {
		sb.WriteString(ge)
		sb.WriteString(", ")
	}
	sb.WriteString(`COUNT(*) AS events,
		COUNT(DISTINCT CASE WHEN session_id <> '' THEN session_id END),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(cost_micro_usd),0),
		COALESCE(SUM(CASE WHEN cost_micro_usd IS NULL THEN 1 ELSE 0 END),0),
		` + computedCostCountSQL("cost_micro_usd", "price_source") + `
		FROM usage_events`)
	sb.WriteString(where)
	if len(groupExprs) > 0 {
		sb.WriteString(" GROUP BY ")
		sb.WriteString(strings.Join(groupExprs, ", "))
		sb.WriteString(" ORDER BY ")
		sb.WriteString(strings.Join(groupExprs, ", "))
	}

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: summarize: %w", err)
	}
	defer rows.Close()

	sum := &Summary{GroupBy: append([]string{}, f.GroupBy...)}
	for rows.Next() {
		keyVals := make([]string, len(f.GroupBy))
		dest := make([]any, 0, len(f.GroupBy)+10)
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var b Bucket
		dest = append(dest, &b.Events, &b.Sessions, &b.Input, &b.Output, &b.CacheCreation, &b.CacheRead, &b.Reasoning, &b.Total,
			&b.CostMicroUSD, &b.UnpricedEvents, &b.ComputedCostEvents)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan summary row: %w", err)
		}
		if len(f.GroupBy) > 0 {
			b.Keys = make(map[string]string, len(f.GroupBy))
			b.OrderedKeys = append([]string{}, f.GroupBy...)
			for i, dim := range f.GroupBy {
				b.Keys[dim] = keyVals[i]
			}
		}
		sum.Buckets = append(sum.Buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: summary rows: %w", err)
	}

	// Grand total: derived from the single pass instead of re-running the full
	// aggregate. Ungrouped, the one result row IS the total; grouped, the
	// summable fields add across buckets and only the distinct session count
	// (which does not add across buckets) needs its own narrow query.
	if len(f.GroupBy) == 0 {
		if len(sum.Buckets) == 1 {
			sum.Totals = sum.Buckets[0]
		}
		return sum, nil
	}
	for _, b := range sum.Buckets {
		sum.Totals.Events += b.Events
		sum.Totals.Input += b.Input
		sum.Totals.Output += b.Output
		sum.Totals.CacheCreation += b.CacheCreation
		sum.Totals.CacheRead += b.CacheRead
		sum.Totals.Reasoning += b.Reasoning
		sum.Totals.Total += b.Total
		sum.Totals.CostMicroUSD += b.CostMicroUSD
		sum.Totals.UnpricedEvents += b.UnpricedEvents
		sum.Totals.ComputedCostEvents += b.ComputedCostEvents
	}
	if len(sum.Buckets) > 0 {
		n, err := s.distinctSessions(ctx, where, args)
		if err != nil {
			return nil, err
		}
		sum.Totals.Sessions = n
	}
	return sum, nil
}

// UnpricedGroups returns the token totals of the matching rows that have NO
// stamped cost, grouped by Filter.GroupBy plus (tool, model, provider,
// service_tier). It exists so cost surfaces can value historical and
// unpriceable rows from the CURRENT price table without materialising one bucket
// per event; an empty result means every matching row is stamped.
//
// The grouping columns are appended AFTER the caller's dimensions so the
// returned Keys align one-to-one with Summarize's buckets.
func (s *Reader) UnpricedGroups(ctx context.Context, f Filter) ([]UnpricedGroup, error) {
	if rollupRangeAligned(f.Since, f.Until) {
		groups, current, err := s.unpricedGroupsCurrentRollup(ctx, f)
		if err != nil {
			return nil, err
		}
		if current {
			return groups, nil
		}
	}
	return s.unpricedGroupsLedger(ctx, f)
}

func (s *Reader) unpricedGroupsLedger(ctx context.Context, f Filter) ([]UnpricedGroup, error) {
	where, args := buildWhere(f)
	if where == "" {
		where = " WHERE cost_micro_usd IS NULL"
	} else {
		where += " AND cost_micro_usd IS NULL"
	}

	groupExprs := make([]string, 0, len(f.GroupBy)+4)
	for _, dim := range f.GroupBy {
		expr, err := groupExpr(dim)
		if err != nil {
			return nil, err
		}
		groupExprs = append(groupExprs, expr)
	}
	cols := append(append([]string{}, groupExprs...), "tool", "model", "provider", "service_tier")

	q := "SELECT " + strings.Join(cols, ", ") + `,
		COUNT(*),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		COALESCE(SUM(reasoning_tokens),0)
		FROM usage_events` + where +
		" GROUP BY " + strings.Join(cols, ", ") +
		" ORDER BY " + strings.Join(cols, ", ")

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: unpriced groups: %w", err)
	}
	defer rows.Close()

	var out []UnpricedGroup
	for rows.Next() {
		keyVals := make([]string, len(f.GroupBy))
		dest := make([]any, 0, len(f.GroupBy)+10)
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var g UnpricedGroup
		dest = append(dest, &g.Tool, &g.Model, &g.Provider, &g.ServiceTier,
			&g.Events, &g.Input, &g.Output, &g.CacheCreation, &g.CacheRead, &g.Reasoning)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan unpriced group: %w", err)
		}
		if len(f.GroupBy) > 0 {
			g.Keys = make(map[string]string, len(f.GroupBy))
			g.OrderedKeys = append([]string{}, f.GroupBy...)
			for i, dim := range f.GroupBy {
				g.Keys[dim] = keyVals[i]
			}
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: unpriced group rows: %w", err)
	}
	return out, nil
}

// distinctSessions counts distinct non-empty session ids over the filtered set.
func (s *Reader) distinctSessions(ctx context.Context, where string, args []any) (int64, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN session_id <> '' THEN session_id END)
		FROM usage_events`+where, args...)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: distinct sessions: %w", err)
	}
	return n, nil
}

// eventColumnsNoRaw is ListEvents' default projection: every column of
// usage_events EXCEPT raw. Naming them is not cosmetic - raw holds the audit
// payload, and on a real ledger it is the overwhelming majority of the bytes
// (rows appended before the allow-list landed carry whole transcript lines).
// A SELECT that dragged it along would move tens of megabytes through the
// driver on every call, for callers that discard it.
const eventColumnsNoRaw = `id, tool, model, session_id, project, event_time_unix, observed_time_unix,
	input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
	reasoning_tokens, total_tokens, request_id, message_id, source_path, kind,
	dedup_key, provider, service_tier, cost_micro_usd, price_source`

// ListEvents returns events matching Filter, ordered by event_time then row id
// (a total order, which keyset pagination will need). The projection names its
// columns and EXCLUDES raw; pass WithRaw to include it (export --include-raw
// only). WithKeyset switches it to one page of an id-ordered walk;
// WithEventTimeKeyset pages without changing the default total order.
func (s *Reader) ListEvents(ctx context.Context, f Filter, opts ...ListOption) ([]model.UsageEvent, error) {
	var lo listOptions
	for _, fn := range opts {
		fn(&lo)
	}
	where, args := buildWhere(f)
	cols := eventColumnsNoRaw
	if lo.includeRaw {
		cols += ", COALESCE(raw,'')"
	}
	order := " ORDER BY event_time_unix, id"
	limit := ""
	if lo.eventTimeKeyset {
		if lo.afterID > 0 {
			// SQLite indexes carry the rowid after the declared columns, so the
			// existing event_time index can seek this row-value cursor directly.
			// The equivalent OR form forced extra scans on every export page.
			cursor := "(event_time_unix, id) > (?, ?)"
			if where == "" {
				where = " WHERE " + cursor
			} else {
				where += " AND " + cursor
			}
			afterUnix := lo.afterEventTime.UTC().Unix()
			args = append(args, afterUnix, lo.afterID)
		}
		if lo.limit > 0 {
			limit = " LIMIT ?"
			args = append(args, lo.limit)
		}
	} else if lo.keyset {
		if lo.afterID > 0 {
			if where == "" {
				where = " WHERE id > ?"
			} else {
				where += " AND id > ?"
			}
			args = append(args, lo.afterID)
		}
		order = " ORDER BY id"
		if lo.limit > 0 {
			limit = " LIMIT ?"
			args = append(args, lo.limit)
		}
	}
	q := `SELECT ` + cols + ` FROM usage_events` + where + order + limit

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()

	var out []model.UsageEvent
	if lo.limit > 0 {
		out = make([]model.UsageEvent, 0, lo.limit)
	}
	for rows.Next() {
		e, err := scanUsageEvent(rows, lo.includeRaw)
		if err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: event rows: %w", err)
	}
	return out, nil
}

func scanUsageEvent(rows *sql.Rows, includeRaw bool) (model.UsageEvent, error) {
	var e model.UsageEvent
	var eventUnix, observedUnix int64
	var kind string
	var cost sql.NullInt64
	dest := []any{
		&e.ID, &e.Tool, &e.Model, &e.SessionID, &e.Project, &eventUnix, &observedUnix,
		&e.InputTokens, &e.OutputTokens, &e.CacheCreationTokens, &e.CacheReadTokens,
		&e.ReasoningTokens, &e.TotalTokens, &e.RequestID, &e.MessageID, &e.SourcePath, &kind,
		&e.DedupKey, &e.Provider, &e.ServiceTier, &cost, &e.PriceSource,
	}
	if includeRaw {
		dest = append(dest, &e.Raw)
	}
	if err := rows.Scan(dest...); err != nil {
		return e, err
	}
	e.EventTime = time.Unix(eventUnix, 0).UTC()
	e.ObservedTime = time.Unix(observedUnix, 0).UTC()
	e.Kind = model.EventKind(kind)
	if cost.Valid {
		e.SetCost(cost.Int64, e.PriceSource)
	}
	return e, nil
}

// IngestWatermark returns the observed time of the NEWEST row in the ledger, or
// the zero time when the ledger is empty. It is the "how far has collection
// got" token a serving process publishes: it moves when a cycle appends and
// stands still when one appends nothing, which is exactly what a client
// invalidating its queries needs to know.
//
// It reads the last row by id rather than taking MAX(observed_time_unix). id is
// an INTEGER PRIMARY KEY, so the newest row is one index step, while the max
// over an unindexed column is a full scan of a 300k-row ledger on every meta
// poll. The two agree because observed time is stamped at insert and inserts
// are id-ordered.
//
// This is deliberately NOT on the Store interface: it serves the read-only web
// surface, and the collector has no use for it.
func (s *Reader) IngestWatermark(ctx context.Context) (time.Time, error) {
	var observed sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT observed_time_unix FROM usage_events ORDER BY id DESC LIMIT 1`).Scan(&observed)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: read ingest watermark: %w", err)
	}
	if !observed.Valid {
		return time.Time{}, nil
	}
	return time.Unix(observed.Int64, 0).UTC(), nil
}

// SourceStats returns per-tool stored stats for the `sources` command.
// LastEventTimes reports the newest event_time per tool over the whole
// ledger. It is the freshness question ("when did codex last write?") answered
// without SourceStats' full-table aggregate, which on a 370k-row ledger costs
// seconds. The recursive CTE is SQLite's loose index scan: each step seeks the
// next distinct tool through idx_events_tool_time, and each MAX is one seek to
// the end of that tool's range in the same index, so the cost is a handful of
// B-tree probes however many rows the ledger holds.
func (s *Reader) LastEventTimes(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE tools(tool) AS (
			SELECT MIN(tool) FROM usage_events
			UNION ALL
			SELECT (SELECT MIN(tool) FROM usage_events WHERE tool > tools.tool)
			FROM tools WHERE tools.tool IS NOT NULL
		)
		SELECT tool, (SELECT MAX(event_time_unix) FROM usage_events WHERE tool = tools.tool)
		FROM tools WHERE tool IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: last event times: %w", err)
	}
	defer rows.Close()
	out := make(map[string]time.Time)
	for rows.Next() {
		var tool string
		var last sql.NullInt64
		if err := rows.Scan(&tool, &last); err != nil {
			return nil, fmt.Errorf("store: scan last event time: %w", err)
		}
		if last.Valid {
			out[tool] = time.Unix(last.Int64, 0).UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: last event time rows: %w", err)
	}
	return out, nil
}

func (s *Reader) SourceStats(ctx context.Context) ([]SourceStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tool,
			COUNT(*) AS events,
			COUNT(DISTINCT CASE WHEN session_id <> '' THEN session_id END) AS sessions,
			COALESCE(SUM(total_tokens),0) AS total,
			MIN(event_time_unix), MAX(event_time_unix), MAX(observed_time_unix)
		FROM usage_events
		GROUP BY tool
		ORDER BY total DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: source stats: %w", err)
	}
	defer rows.Close()

	var out []SourceStat
	for rows.Next() {
		var (
			st                   SourceStat
			first, last, lastObs sql.NullInt64
		)
		if err := rows.Scan(&st.Tool, &st.Events, &st.Sessions, &st.Total, &first, &last, &lastObs); err != nil {
			return nil, fmt.Errorf("store: scan source stat: %w", err)
		}
		if first.Valid {
			st.FirstEvent = time.Unix(first.Int64, 0).UTC()
		}
		if last.Valid {
			st.LastEvent = time.Unix(last.Int64, 0).UTC()
		}
		if lastObs.Valid {
			st.LastObserved = time.Unix(lastObs.Int64, 0).UTC()
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: source stat rows: %w", err)
	}

	models, err := s.modelsByTool(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Models = models[out[i].Tool]
	}
	return out, nil
}

// modelsByTool returns the sorted distinct non-empty model ids per tool in one
// query, replacing the previous per-tool round trip.
func (s *Reader) modelsByTool(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT tool, model FROM usage_events
		WHERE model <> '' ORDER BY tool, model`)
	if err != nil {
		return nil, fmt.Errorf("store: distinct models: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var tool, m string
		if err := rows.Scan(&tool, &m); err != nil {
			return nil, fmt.Errorf("store: scan model: %w", err)
		}
		out[tool] = append(out[tool], m)
	}
	return out, rows.Err()
}

// Stats returns whole-database diagnostics for the `doctor` command.
func (s *Reader) Stats(ctx context.Context) (DBStats, error) {
	st := DBStats{Path: s.path, SchemaVersion: SchemaVersion}

	var (
		earliest, latest sql.NullInt64
	)
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COUNT(DISTINCT tool),
			COUNT(DISTINCT CASE WHEN model <> '' THEN model END),
			MIN(event_time_unix), MAX(event_time_unix)
		FROM usage_events`)
	if err := row.Scan(&st.Events, &st.DistinctTools, &st.DistinctModel, &earliest, &latest); err != nil {
		return DBStats{}, fmt.Errorf("store: stats: %w", err)
	}
	if earliest.Valid {
		st.EarliestEvent = time.Unix(earliest.Int64, 0).UTC()
	}
	if latest.Valid {
		st.LatestEvent = time.Unix(latest.Int64, 0).UTC()
	}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM aggregate_state`).Scan(&st.Snapshots); err != nil {
		return DBStats{}, fmt.Errorf("store: snapshot count: %w", err)
	}

	if v, err := readSchemaVersion(ctx, s.db); err == nil && v > 0 {
		st.SchemaVersion = v
	}

	if fi, err := os.Stat(s.path); err == nil {
		st.SizeBytes = fi.Size()
	}
	return st, nil
}
