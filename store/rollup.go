// Derived rollup of usage_events (issue #59). The ledger stays the only
// history; this table is a summary that exists so time-bucketed reporting stops
// scanning 360k rows to return 24 numbers. Every row is reproducible from
// usage_events, nothing reads it as a source of truth, and it may be dropped
// and rebuilt at any time.
//
// Two rules keep it honest:
//
//   - It is keyed by the UTC 15-MINUTE bucket an event falls in, never by local
//     time. Rolling up by local time would bake the writing machine's calendar
//     into stored data; the local fold happens on READ, in SQL, exactly the way
//     query.go folds event_time_unix. The width is 15 minutes and not an hour
//     because every real-world UTC offset is a whole number of quarter hours,
//     while half-hour zones (Asia/Kolkata at +05:30 among them) split an hour
//     bucket across two local buckets - an hourly key would silently move the
//     first half hour of every local day into the previous one. Resolution
//     BELOW the bucket width requires ledger events. Exact ending buckets
//     are read separately; unaligned starts use the ledger for the whole query.
//   - Its deltas are written inside the transaction that appends the events
//     (insertEventsTx), so a crash cannot land events without the matching
//     delta. The watermark below catches the one case that discipline cannot:
//     a rollup created empty by the migration, or left behind by a write that
//     predates it.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// rollupV4TableDDL is frozen migration history. Version 4 did not carry the
// dimensions version 8 needs, so old files must first reach the layout their
// recorded version promises before migration 8 replaces this derived table.
const rollupV4TableDDL = `CREATE TABLE IF NOT EXISTS usage_rollup (
  bucket_start_unix     INTEGER NOT NULL,
  tool                  TEXT    NOT NULL,
  model                 TEXT    NOT NULL DEFAULT '',
  project               TEXT    NOT NULL DEFAULT '',
  input_tokens          INTEGER NOT NULL DEFAULT 0,
  output_tokens         INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens          INTEGER NOT NULL DEFAULT 0,
  events                INTEGER NOT NULL DEFAULT 0,
  cost_micro_usd        INTEGER NOT NULL DEFAULT 0,
  unpriced_events       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (bucket_start_unix, tool, model, project)
) WITHOUT ROWID`

// rollupTableDDL is the current v8 rollup. schema.sql carries the same table
// and TestRollupTableMatchesFreshSchema compares fresh and migrated databases.
// The deeper key makes every public summary measure exactly derivable while
// keeping the immutable usage ledger as the only source of truth.
const rollupTableDDL = `CREATE TABLE IF NOT EXISTS usage_rollup (
  bucket_start_unix     INTEGER NOT NULL,
  tool                  TEXT    NOT NULL,
  model                 TEXT    NOT NULL DEFAULT '',
  project               TEXT    NOT NULL DEFAULT '',
  session_id            TEXT    NOT NULL DEFAULT '',
  provider              TEXT    NOT NULL DEFAULT '',
  service_tier          TEXT    NOT NULL DEFAULT '',
  price_class           TEXT    NOT NULL,
  input_tokens          INTEGER NOT NULL DEFAULT 0,
  output_tokens         INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens          INTEGER NOT NULL DEFAULT 0,
  events                INTEGER NOT NULL DEFAULT 0,
  cost_micro_usd        INTEGER NOT NULL DEFAULT 0,
  unpriced_events       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (
    bucket_start_unix, tool, model, project, session_id,
    provider, service_tier, price_class
  ),
  CHECK (price_class IN ('unpriced','computed','vendor'))
) WITHOUT ROWID`

const (
	priceClassUnpriced = "unpriced"
	priceClassComputed = "computed"
	priceClassVendor   = "vendor"
)

// rollupWatermarkKey names the schema_meta row holding the highest
// usage_events.id folded into the rollup. schema_meta is bookkeeping, not a
// data table: it already carries the version stamp, and the watermark is the
// same kind of fact about the file rather than about usage.
const rollupWatermarkKey = "rollup_watermark"

// rollupBucketSeconds is the rollup's bucket width: 15 minutes. Every UTC
// offset in use is a multiple of it, which is what makes the local fold on read
// exact for every zone rather than only for whole-hour ones.
const rollupBucketSeconds = 900

// bucketStartUnix floors a UTC unix second to the start of its bucket. It uses
// floor division rather than Go's truncating one so a pre-epoch timestamp lands
// in the bucket that contains it; bucketStartSQL is the same arithmetic in SQL,
// so the Go-side delta and the SQL-side rebuild bucket every event identically.
func bucketStartUnix(sec int64) int64 {
	return sec - ((sec%rollupBucketSeconds)+rollupBucketSeconds)%rollupBucketSeconds
}

// bucketStartSQL is bucketStartUnix as a SQL expression over event_time_unix.
const bucketStartSQL = `(event_time_unix - ((event_time_unix % 900) + 900) % 900)`

// rollupPriceClassSQL is eventPriceClass expressed over usage_events. Both use
// the same closed vendor vocabulary, so rebuilding cannot move a row between
// provenance classes compared with the incremental path.
var rollupPriceClassSQL = `CASE
	WHEN cost_micro_usd IS NULL THEN '` + priceClassUnpriced + `'
	WHEN ` + vendorPriceSourceSQL("price_source") + ` THEN '` + priceClassVendor + `'
	ELSE '` + priceClassComputed + `' END`

// rollupKey is one v8 rollup row's identity.
type rollupKey struct {
	bucket      int64
	tool        string
	model       string
	project     string
	session     string
	provider    string
	serviceTier string
	priceClass  string
}

// rollupCell accumulates the measures of one rollup row.
type rollupCell struct {
	input         int64
	output        int64
	cacheCreation int64
	cacheRead     int64
	reasoning     int64
	total         int64
	events        int64
	costMicroUSD  int64
	unpriced      int64
}

// rollupDelta is the change one event batch makes to the rollup, folded in
// memory so a batch touching the same bucket repeatedly costs one upsert.
type rollupDelta struct {
	cells map[rollupKey]*rollupCell
	maxID int64
}

// add folds one inserted event (and the row id it was assigned) into the delta.
func (d *rollupDelta) add(e model.UsageEvent, id int64) {
	d.adjust(e, id, 1)
}

// adjust also supports moving a previously unpriced event between price classes.
// Negative cells must be checked against the stored rollup before apply.
func (d *rollupDelta) adjust(e model.UsageEvent, id, sign int64) {
	if d.cells == nil {
		d.cells = make(map[rollupKey]*rollupCell)
	}
	k := rollupKey{
		bucket:      bucketStartUnix(e.EventTime.UTC().Unix()),
		tool:        e.Tool,
		model:       e.Model,
		project:     e.Project,
		session:     e.SessionID,
		provider:    e.Provider,
		serviceTier: e.ServiceTier,
		priceClass:  eventPriceClass(e),
	}
	c := d.cells[k]
	if c == nil {
		c = &rollupCell{}
		d.cells[k] = c
	}
	c.input += sign * e.InputTokens
	c.output += sign * e.OutputTokens
	c.cacheCreation += sign * e.CacheCreationTokens
	c.cacheRead += sign * e.CacheReadTokens
	c.reasoning += sign * e.ReasoningTokens
	c.total += sign * e.TotalTokens
	c.events += sign
	if cost, ok := e.Cost(); ok {
		c.costMicroUSD += sign * cost
	} else {
		c.unpriced += sign
	}
	if id > d.maxID {
		d.maxID = id
	}
}

func eventPriceClass(e model.UsageEvent) string {
	if _, ok := e.Cost(); !ok {
		return priceClassUnpriced
	}
	if model.PriceProvenance(e.PriceSource) == model.CostVendor {
		return priceClassVendor
	}
	return priceClassComputed
}

func (d *rollupDelta) empty() bool { return len(d.cells) == 0 }

// rollupUpsertSQL adds a delta onto the matching rollup row, creating it when
// the bucket/tool/model/project cell is new. The bare column names on the right
// of the SET are the stored row's current values.
const rollupUpsertSQL = `
	INSERT INTO usage_rollup (
		bucket_start_unix, tool, model, project, session_id,
		provider, service_tier, price_class,
		input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		reasoning_tokens, total_tokens, events, cost_micro_usd, unpriced_events
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(
		bucket_start_unix, tool, model, project, session_id,
		provider, service_tier, price_class
	) DO UPDATE SET
		input_tokens          = input_tokens          + excluded.input_tokens,
		output_tokens         = output_tokens         + excluded.output_tokens,
		cache_creation_tokens = cache_creation_tokens + excluded.cache_creation_tokens,
		cache_read_tokens     = cache_read_tokens     + excluded.cache_read_tokens,
		reasoning_tokens      = reasoning_tokens      + excluded.reasoning_tokens,
		total_tokens          = total_tokens          + excluded.total_tokens,
		events                = events                + excluded.events,
		cost_micro_usd        = cost_micro_usd        + excluded.cost_micro_usd,
		unpriced_events       = unpriced_events       + excluded.unpriced_events`

// apply writes the delta and advances the watermark inside the caller's
// transaction. It must never run in a transaction the events did not commit in:
// the whole point is that a crash cannot separate the two.
func (d *rollupDelta) apply(ctx context.Context, tx *sql.Tx) error {
	if d.empty() {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, rollupUpsertSQL)
	if err != nil {
		return fmt.Errorf("store: prepare rollup upsert: %w", err)
	}
	defer stmt.Close()

	for k, c := range d.cells {
		if _, err := stmt.ExecContext(ctx,
			k.bucket, k.tool, k.model, k.project, k.session,
			k.provider, k.serviceTier, k.priceClass,
			c.input, c.output, c.cacheCreation, c.cacheRead,
			c.reasoning, c.total, c.events, c.costMicroUSD, c.unpriced,
		); err != nil {
			return fmt.Errorf("store: rollup upsert (bucket=%d tool=%s): %w", k.bucket, k.tool, err)
		}
	}
	return setRollupWatermark(ctx, tx, d.maxID)
}

// setRollupWatermark records the highest ledger row id folded into the rollup.
// It never moves backwards: ids are AUTOINCREMENT, so a lower value can only
// come from a stale caller.
func setRollupWatermark(ctx context.Context, db execer, id int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO schema_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value =
			CASE WHEN CAST(excluded.value AS INTEGER) > CAST(schema_meta.value AS INTEGER)
			     THEN excluded.value ELSE schema_meta.value END`,
		rollupWatermarkKey, strconv.FormatInt(id, 10))
	if err != nil {
		return fmt.Errorf("store: record rollup watermark: %w", err)
	}
	return nil
}

// rollupWatermark reads the recorded watermark, or -1 when none is recorded
// (never 0: an empty ledger legitimately has watermark 0, and the two states
// must not be confused).
func rollupWatermark(ctx context.Context, q rowQuerier) (int64, error) {
	var v string
	err := q.QueryRowContext(ctx,
		`SELECT value FROM schema_meta WHERE key=?`, rollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return -1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read rollup watermark: %w", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: unrecognized rollup watermark %q", v)
	}
	return n, nil
}

// RebuildRollup drops and recreates the whole rollup from usage_events in one
// transaction, so a reader never sees a half-built summary. It is the
// definition of the table's contents: any disagreement between the rollup and
// the ledger is resolved by running it, never by correcting the ledger.
func (l *Ledger) RebuildRollup(ctx context.Context) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin rollup rebuild: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_rollup`); err != nil {
		return fmt.Errorf("store: clear rollup: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_rollup (
			bucket_start_unix, tool, model, project, session_id,
			provider, service_tier, price_class,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			reasoning_tokens, total_tokens, events, cost_micro_usd, unpriced_events
		)
		SELECT `+bucketStartSQL+`, tool, model, project, session_id,
			provider, service_tier, `+rollupPriceClassSQL+`,
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(total_tokens),0),
			COUNT(*),
			COALESCE(SUM(cost_micro_usd),0),
			COALESCE(SUM(CASE WHEN cost_micro_usd IS NULL THEN 1 ELSE 0 END),0)
		FROM usage_events
		GROUP BY 1, 2, 3, 4, 5, 6, 7, 8`); err != nil {
		return fmt.Errorf("store: fill rollup: %w", err)
	}

	var maxID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM usage_events`).Scan(&maxID); err != nil {
		return fmt.Errorf("store: read ledger watermark: %w", err)
	}
	// The rebuild is authoritative about what it covered, including downwards
	// (a rollup rebuilt from a shorter ledger must not keep a higher mark), so
	// it writes the value directly instead of through the monotonic upsert.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO schema_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		rollupWatermarkKey, strconv.FormatInt(maxID, 10)); err != nil {
		return fmt.Errorf("store: record rollup watermark: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit rollup rebuild: %w", err)
	}
	return nil
}

// EnsureRollup brings the usage rollup and activity usage counts back in step
// with their ledgers, and reports whether either derived table was rebuilt.
//
// The usage rollup has two checks for different failures. The watermark (highest
// ledger id folded in) catches the empty rollup a v4 migration leaves behind
// and any write that skipped the delta. The event count catches the rollup that
// tracked the newest ids but lost older ones - a rollup filled from a partial
// ledger would otherwise pass the watermark check forever.
// Activity counts are compared by key and count in both directions, so moving
// counts between keys cannot conceal drift behind an unchanged total.
func (l *Ledger) EnsureRollup(ctx context.Context) (bool, error) {
	stale, err := l.RollupStale(ctx)
	if err != nil {
		return false, err
	}
	if stale {
		if err := l.RebuildRollup(ctx); err != nil {
			return false, err
		}
	}
	activityStale, err := l.activityUsageCountsStale(ctx)
	if err != nil {
		return stale, err
	}
	if activityStale {
		if err := l.rebuildActivityUsageCounts(ctx); err != nil {
			return stale, err
		}
	}
	return stale || activityStale, nil
}

// RollupStale reports whether the usage rollup disagrees with the usage ledger,
// the question EnsureRollup asks before rebuilding that table. It cannot
// repair the table. It is exported for the READ-ONLY serving path: a
// process that cannot write still has to know that the summary it would answer
// from covers nothing, so it can go to the ledger instead of serving the zeros
// of a rollup a migration created empty.
//
// Cheap in the common case and cheapest when the answer is yes: a watermark
// that disagrees with MAX(id) returns before the two aggregate queries run. The
// caller is still expected to cache the verdict rather than ask per request.
func (s *Reader) RollupStale(ctx context.Context) (bool, error) {
	mark, err := rollupWatermark(ctx, s.db)
	if err != nil {
		return false, err
	}
	if mark < 0 {
		// No watermark recorded: the rollup covers nothing so far. That is the
		// truth on a fresh database as much as on one the v4 migration just
		// touched, and an empty ledger satisfies it - which is why it compares
		// as 0 instead of forcing a rebuild that would find nothing to do.
		mark = 0
	}
	var maxID int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM usage_events`).Scan(&maxID); err != nil {
		return false, fmt.Errorf("store: read ledger watermark: %w", err)
	}
	if mark != maxID {
		return true, nil
	}

	var ledgerEvents, rollupEvents int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events`).Scan(&ledgerEvents); err != nil {
		return false, fmt.Errorf("store: count ledger events: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(events),0) FROM usage_rollup`).Scan(&rollupEvents); err != nil {
		return false, fmt.Errorf("store: count rollup events: %w", err)
	}
	return ledgerEvents != rollupEvents, nil
}

// SummarizeRollup answers a bucket query from the derived rollup instead of the
// ledger. It is the fast path behind time-bucketed reporting: identical inputs
// must yield identical numbers to Summarize over the same range, which
// TestRollupMatchesLedger pins through store queries on both sides.
// Callers must first check RollupStale and use Summarize when it is true, or
// rebuild through EnsureRollup on a writable handle. This method trusts the
// derived contents and does not validate out-of-band edits.
//
// The public contract remains the one version 4 exposed: Since/Until are
// snapped OUTWARD to whole UTC buckets (15 minutes), because
//
//	a bucket is the finest thing the table knows. The snapped bounds come back
//	in the result so a caller can label what it actually got instead of
//	implying it asked for it.
func (s *Reader) SummarizeRollup(ctx context.Context, f Filter) (*RollupSummary, error) {
	since, until := snapRollupRange(f.Since, f.Until)
	sum, _, err := s.summarizeRollup(ctx, f, since, until, false)
	if err != nil {
		return nil, err
	}
	return &RollupSummary{
		GroupBy: sum.GroupBy,
		Buckets: sum.Buckets,
		Totals:  sum.Totals,
		Since:   since,
		Until:   until,
	}, nil
}

// rollupCurrentSQL is embedded in accelerated aggregate statements so checking
// the watermark does not add a round trip. A missing watermark means zero,
// which is current only for an empty ledger.
const rollupCurrentSQL = `(COALESCE((
	SELECT CAST(value AS INTEGER) FROM schema_meta WHERE key='rollup_watermark'
),0) = (SELECT COALESCE(MAX(id),0) FROM usage_events))`

func rollupRangeAligned(since, until time.Time) bool {
	aligned := func(t time.Time) bool {
		return t.IsZero() || (t.Nanosecond() == 0 && bucketStartUnix(t.UTC().Unix()) == t.UTC().Unix())
	}
	return aligned(since) && aligned(until)
}

func (s *Reader) summarizeCurrentRollup(ctx context.Context, f Filter) (*Summary, bool, error) {
	return s.summarizeRollup(ctx, f, f.Since, f.Until, true)
}

// summarizeRollup answers from the v8 derived table, adding ending events for
// an exact live upper bound. requireCurrent embeds the
// watermark check into the aggregate so Summarize keeps its existing one-query
// ungrouped and two-query grouped contract. A grouped query with no rows falls
// back to the ledger: that is correct for both an empty current result and a
// stale table, and avoids a separate status query on the hot path.
func (s *Reader) summarizeRollup(ctx context.Context, f Filter, since, until time.Time, requireCurrent bool) (*Summary, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("store: begin rollup summary snapshot: %w", err)
	}
	defer tx.Rollback()
	groupExprs := make([]string, 0, len(f.GroupBy))
	for _, dim := range f.GroupBy {
		expr, err := rollupGroupExpr(dim)
		if err != nil {
			return nil, false, err
		}
		groupExprs = append(groupExprs, expr)
	}

	from := "usage_rollup"
	where, args := buildRollupWhere(f, since, until)
	if requireCurrent && !rollupRangeAligned(time.Time{}, until) {
		from, args = exactEndingRollupSource(f)
		where = ""
	}
	statusExpr := "1"
	if requireCurrent {
		statusExpr = rollupCurrentSQL
		if where == "" {
			where = " WHERE " + rollupCurrentSQL
		} else {
			where += " AND " + rollupCurrentSQL
		}
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	for _, ge := range groupExprs {
		sb.WriteString(ge)
		sb.WriteString(", ")
	}
	sb.WriteString(`COALESCE(SUM(events),0),
		COUNT(DISTINCT CASE WHEN session_id <> '' THEN session_id END),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(cost_micro_usd),0), COALESCE(SUM(unpriced_events),0),
		COALESCE(SUM(CASE WHEN price_class='computed' THEN events ELSE 0 END),0), `)
	sb.WriteString(statusExpr)
	sb.WriteString(" FROM " + from)
	sb.WriteString(where)
	if len(groupExprs) > 0 {
		sb.WriteString(" GROUP BY ")
		sb.WriteString(strings.Join(groupExprs, ", "))
		sb.WriteString(" ORDER BY ")
		sb.WriteString(strings.Join(groupExprs, ", "))
	}

	rows, err := tx.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: summarize rollup: %w", err)
	}
	defer rows.Close()

	out := &Summary{
		GroupBy: append([]string{}, f.GroupBy...),
	}
	current := !requireCurrent
	for rows.Next() {
		keyVals := make([]string, len(f.GroupBy))
		dest := make([]any, 0, len(f.GroupBy)+12)
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var b Bucket
		var status int
		dest = append(dest, &b.Events, &b.Sessions, &b.Input, &b.Output, &b.CacheCreation, &b.CacheRead,
			&b.Reasoning, &b.Total, &b.CostMicroUSD, &b.UnpricedEvents, &b.ComputedCostEvents, &status)
		if err := rows.Scan(dest...); err != nil {
			return nil, false, fmt.Errorf("store: scan rollup row: %w", err)
		}
		current = status != 0
		if len(f.GroupBy) > 0 {
			b.Keys = make(map[string]string, len(f.GroupBy))
			b.OrderedKeys = append([]string{}, f.GroupBy...)
			for i, dim := range f.GroupBy {
				b.Keys[dim] = keyVals[i]
			}
		}
		out.Buckets = append(out.Buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: rollup rows: %w", err)
	}
	if requireCurrent && !current {
		return out, false, nil
	}

	// Ungrouped, the single result row IS the total. Grouped, every measure but
	// distinct sessions adds across buckets; that one gets the same narrow
	// second query the authoritative ledger path uses.
	if len(f.GroupBy) == 0 {
		if len(out.Buckets) == 1 {
			out.Totals = out.Buckets[0]
		}
		return out, current, nil
	}
	for _, b := range out.Buckets {
		out.Totals.Events += b.Events
		out.Totals.Input += b.Input
		out.Totals.Output += b.Output
		out.Totals.CacheCreation += b.CacheCreation
		out.Totals.CacheRead += b.CacheRead
		out.Totals.Reasoning += b.Reasoning
		out.Totals.Total += b.Total
		out.Totals.CostMicroUSD += b.CostMicroUSD
		out.Totals.UnpricedEvents += b.UnpricedEvents
		out.Totals.ComputedCostEvents += b.ComputedCostEvents
	}
	if len(out.Buckets) > 0 {
		n, err := distinctRollupSessions(ctx, tx, from, where, args)
		if err != nil {
			return nil, false, err
		}
		out.Totals.Sessions = n
	}
	return out, current, nil
}

// exactEndingRollupSource keeps a live snapshot's upper bound exact. Complete
// 15-minute buckets use the rollup; only the final partial bucket reads events.
// Both branches retain session IDs so distinct counts remain correct across
// their boundary. The caller checks the rollup watermark for the whole query.
func exactEndingRollupSource(f Filter) (string, []any) {
	boundary := time.Unix(bucketStartUnix(f.Until.Unix()), 0).UTC()
	rollupWhere, args := buildRollupWhere(f, f.Since, boundary)
	tail := f
	if tail.Since.IsZero() || tail.Since.Before(boundary) {
		tail.Since = boundary
	}
	eventWhere, eventArgs := buildWhere(tail)
	args = append(args, eventArgs...)
	const dimensions = "tool, model, project, session_id, provider, service_tier, "
	const tokens = "input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, reasoning_tokens, total_tokens, "
	from := "(SELECT bucket_start_unix, " + dimensions + tokens +
		"events, cost_micro_usd, unpriced_events, price_class FROM usage_rollup" + rollupWhere +
		" UNION ALL SELECT event_time_unix AS bucket_start_unix, " + dimensions + tokens +
		"1 AS events, COALESCE(cost_micro_usd,0) AS cost_micro_usd, " +
		"CASE WHEN cost_micro_usd IS NULL THEN 1 ELSE 0 END AS unpriced_events, " +
		rollupPriceClassSQL + " AS price_class FROM usage_events" + eventWhere + ")"
	return from, args
}

func distinctRollupSessions(ctx context.Context, q rowQuerier, from, where string, args []any) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN session_id <> '' THEN session_id END)
		FROM `+from+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: distinct rollup sessions: %w", err)
	}
	return n, nil
}

// unpricedGroupsCurrentRollup is UnpricedGroups' exact aligned-range fast
// path. The enriched key carries every field a display-time price lookup needs,
// so no component is inferred from a priced neighbour.
func (s *Reader) unpricedGroupsCurrentRollup(ctx context.Context, f Filter) ([]UnpricedGroup, bool, error) {
	groupExprs := make([]string, 0, len(f.GroupBy))
	for _, dim := range f.GroupBy {
		expr, err := rollupGroupExpr(dim)
		if err != nil {
			return nil, false, err
		}
		groupExprs = append(groupExprs, expr)
	}
	where, args := buildRollupWhere(f, f.Since, f.Until)
	condition := "price_class='" + priceClassUnpriced + "' AND " + rollupCurrentSQL
	if where == "" {
		where = " WHERE " + condition
	} else {
		where += " AND " + condition
	}
	cols := append(append([]string{}, groupExprs...), "tool", "model", "provider", "service_tier")
	q := "SELECT " + strings.Join(cols, ", ") + `,
		COALESCE(SUM(events),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		COALESCE(SUM(reasoning_tokens),0), ` + rollupCurrentSQL + `
		FROM usage_rollup` + where +
		" GROUP BY " + strings.Join(cols, ", ") +
		" ORDER BY " + strings.Join(cols, ", ")

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: unpriced rollup groups: %w", err)
	}
	defer rows.Close()

	var out []UnpricedGroup
	for rows.Next() {
		keyVals := make([]string, len(f.GroupBy))
		dest := make([]any, 0, len(f.GroupBy)+11)
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var g UnpricedGroup
		var current int
		dest = append(dest, &g.Tool, &g.Model, &g.Provider, &g.ServiceTier,
			&g.Events, &g.Input, &g.Output, &g.CacheCreation, &g.CacheRead, &g.Reasoning, &current)
		if err := rows.Scan(dest...); err != nil {
			return nil, false, fmt.Errorf("store: scan unpriced rollup group: %w", err)
		}
		if current == 0 {
			return nil, false, nil
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
		return nil, false, fmt.Errorf("store: unpriced rollup rows: %w", err)
	}
	if len(out) > 0 {
		return out, true, nil
	}

	// No matching unpriced row is a valid answer on a current rollup and is
	// indistinguishable from a stale empty table in the grouped query above.
	// Resolve that uncommon edge with one scalar read rather than charging every
	// summary an extra round trip.
	var current int
	if err := s.db.QueryRowContext(ctx, "SELECT "+rollupCurrentSQL).Scan(&current); err != nil {
		return nil, false, fmt.Errorf("store: check unpriced rollup watermark: %w", err)
	}
	return nil, current != 0, nil
}

// snapRollupRange widens the requested bounds to the whole UTC buckets that
// contain them, so the answer covers at least what was asked for. Zero bounds
// stay zero (open).
func snapRollupRange(since, until time.Time) (time.Time, time.Time) {
	var lo, hi time.Time
	if !since.IsZero() {
		lo = time.Unix(bucketStartUnix(since.UTC().Unix()), 0).UTC()
	}
	if !until.IsZero() {
		u := until.UTC().Unix()
		b := bucketStartUnix(u)
		if b != u {
			b += rollupBucketSeconds
		}
		hi = time.Unix(b, 0).UTC()
	}
	return lo, hi
}

// buildRollupWhere is buildWhere against the rollup's columns: the time bounds
// compare against bucket_start_unix (already snapped by the caller) and the
// categorical filters cover only the dimensions the rollup keeps.
func buildRollupWhere(f Filter, since, until time.Time) (string, []any) {
	var conds []string
	var args []any

	if !since.IsZero() {
		conds = append(conds, "bucket_start_unix >= ?")
		args = append(args, since.Unix())
	}
	if !until.IsZero() {
		conds = append(conds, "bucket_start_unix < ?")
		args = append(args, until.Unix())
	}
	addIn := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		conds = append(conds, col+" IN ("+strings.Join(ph, ",")+")")
	}
	addIn("tool", f.Tools)
	addIn("model", f.Models)
	addIn("provider", f.Providers)
	addIn("project", f.Projects)
	addIn("session_id", f.Sessions)

	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}
