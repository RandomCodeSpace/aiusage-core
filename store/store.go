// Package store is the append-only usage ledger and the query surface over it.
//
// The database is one SQLite file driven by modernc.org/sqlite (pure Go,
// CGO_ENABLED=0). It holds the token ledger, the activity ledger, the turn
// attribution table, a derived rollup and the collector's own working state,
// and Open / OpenReadOnly are the only ways in. schema.sql always describes the
// full latest schema; older files run the ordered steps in migrate.go.
//
// # HISTORY IS APPENDED BY DEFAULT
//
// usage_events records what a harness spent, and two of the
// tables beside it are on the same terms: activity_events (one row per tool,
// skill or hook invocation) and usage_turn_context (what a turn ran under).
// Each carries BEFORE UPDATE and BEFORE DELETE triggers that RAISE(ABORT), so
// a mutation aborts in the database rather than being caught by discipline in
// this package. Rows arrive through INSERT .. ON CONFLICT(dedup_key) DO
// NOTHING - deliberately not INSERT OR IGNORE, which would also swallow CHECK
// violations - which is what makes a re-read of an unchanged source a no-op
// rather than a double count. The default correction mechanism is a NEW row
// with kind='adjustment', preserving the earlier observation.
// Explicit exceptions are SyncUnpriced (filling unknown costs) and the opt-in
// ReconcileClaudeBatch (growing Claude usage). Neither changes the insertion
// semantics of ApplyBatch, ApplyEvents or InsertEvents.
//
// aggregate_state, source_checkpoints, usage_rollup and activity_usage_counts
// are mutable working tables (schema_meta holds the version stamp and the
// rollup watermark).
// They are working state, not history: losing any of them costs a re-read or a
// rebuild, never a fact.
//
// Two consequences a caller feels. First, a write can partially succeed: a row
// that fails its own insert (CHECK violation, empty dedup key) is skipped and
// reported in the returned error while the rest of the batch commits, so the
// returned counts stay meaningful when the error is non-nil - one poison row
// must not abort a batch that is re-read every cycle. Second, one read of one
// source commits as ONE transaction (ApplyBatch): events, activity, turn
// contexts and the checkpoint together, because a checkpoint that outran its
// data would skip that data forever.
//
// # TWO HANDLES, AND THE ABSENCE THAT SEPARATES THEM
//
// Open returns a *Ledger: the full handle. It creates the file if absent,
// applies WAL, synchronous=FULL, busy_timeout=5000 and foreign_keys=ON,
// migrates an older schema, refuses a newer one (an older binary must never
// stamp a version backwards), and chmods the database and its
// WAL/SHM sidecars to 0600 because the raw column can hold transcript content.
// It is what the collector holds.
//
// OpenReadOnly returns a *Reader: an EXISTING database, opened mode=ro plus
// query_only(1), with no schema creation, no migration and no file mode
// touched. Its schema version must equal this binary's EXACTLY and is refused
// in either direction - migrating would be a write, and a reader that quietly
// serves a schema it does not understand is worse than one that will not start.
//
// A Reader HAS NO WRITE METHOD. That is the whole design (issue #72,
// decisions 2 and 8): the append-only guarantee used to be defended by a flag
// this package checked at the top of each write, on a handle whose type still
// advertised InsertEvents to whoever held it, so "a serving process cannot
// write the ledger" was a promise kept at runtime. It is now a property of the
// type - a program that calls a write through a read handle does not compile,
// and there is no test to write about it. Ledger EMBEDS *Reader, so the
// collector holds one handle carrying both halves and hands l.Reader to
// anything that only queries. EnsureRollup and RebuildRollup are writes and
// live on Ledger; RollupStale is a read and lives on Reader, since a serving
// process still has to know that the summary it would answer from covers
// nothing.
//
// This package exports NO fat store interface. A consumer that wants a fake
// declares its own interface over the methods it actually calls (collect.Store
// and tui.DataSource are the two in this repository), which keeps the seam
// beside the code that depends on it and lets a method be added here without
// breaking an implementation nobody in this module wrote.
//
// # TWO ERRORS WORTH BRANCHING ON, AND NO MORE
//
// A sentinel is a promise: once it exists, code outside this module tests for
// it and it can never be retired quietly. So this package exports one where a
// caller can do something different because of it, and nothing where it cannot
// (issue #72, decision 6). Everything else is a plain wrapped error carrying a
// message a person reads.
//
// ErrSchemaNewer, matched with errors.Is, is the database written by a NEWER
// build than the binary opening it. Both handles refuse it - Open will not
// migrate a schema it does not understand, OpenReadOnly will not serve one -
// and the answer is the same either way: upgrade. An OLDER database is
// deliberately not this error; Open migrates it, and through the read handle it
// asks for the opposite action.
//
// SkippedRowsError, matched with errors.As, is the partial success described
// above: a non-nil error WHOSE COUNTS ARE STILL TRUE. It names the table, how
// many rows were offered, and every row that was refused with its own dedup key
// and cause, so a caller can log what was rejected instead of parsing a
// message. Unwrap reaches the first row's cause, so errors.Is still finds the
// CHECK violation or driver error underneath. A caller that treats a non-nil
// error from a batch write as "nothing happened" under-reports a pass that
// mostly worked.
//
// # A READ HANDLE IS SAFE AGAINST A LIVE COLLECTOR
//
// This is a promise of the API, not an accident of the implementation, and it
// is not withdrawn without a major version. A read handle may be open, and
// queried, while a collector writes the same file: WAL means readers do not
// block the writer and the writer does not block readers, so a report never
// waits on a collection pass and never sees half of one - each source's pass
// commits atomically.
//
// The honest sentence: a read can still fail with SQLITE_BUSY, because WAL has
// moments (crash recovery, a checkpoint that restarts the log) where a reader
// needs a lock a writer holds, and the wait before it gives up is the
// busy_timeout - fixed at 5000ms in both DSNs, not configurable. A read is
// idempotent, so a retry is always safe. One case sits outside the promise: the
// schema version is checked once, at open, so a NEWER collector migrating the
// file underneath a live read handle is not covered; reopen it.
//
// # ONE DIMENSION PER QUERY, ALWAYS
//
// Cost is partitioned six ways: the five turn-context dimensions
// (model.TurnDimensions - agent, skill, mcp_tool, mcp_server, plugin) plus
// activity_events' per-call attribution. Each is honest alone and meaningless
// summed, the way cost-by-region and cost-by-product are two views of one
// budget. A turn commonly carries three or four contexts at once and EVERY row
// names the turn's full cost, so a dimension-blind join counts the same tokens
// once per context the turn held: measured on a real ledger, 6.213bn tokens and
// $6,023.52 reported for turns that cost 4.984bn and $4,700.90, a 28.1%
// overstatement from one missing predicate.
//
// So the dimension is a REQUIRED ARGUMENT of SummarizeTurnContext and
// TopTurnContext, never a filter field a caller could leave unset. It is
// validated against the closed vocabulary before any SQL exists, and the WHERE
// builder writes the dimension predicate unconditionally, so no argument and no
// combination of empty filters produces a statement without it. Grouping by
// "dimension" is refused by name (it is exactly the operation that concatenates
// two partitions), as is grouping by any dimension other than the one queried;
// ActivityFilter's Kinds and Names are refused because honouring them means
// joining activity_events, where a turn with two matching calls joins twice.
// No query here reads two dimensions, or both this table and activity_events,
// in one statement - and a caller must not add two results together either.
//
// Within one dimension the join is 1:1 (PRIMARY KEY (usage_dedup_key,
// dimension) against a UNIQUE dedup_key), so a bucket's cost is a plain SUM
// with no divisor. The activity queries are the opposite case and DO divide:
// one assistant turn commonly emits several calls against a single usage
// object, so each call takes the turn's counts divided by the number of rows
// naming that turn - counted in the table, integer, non-negative, which bounds
// the shares by the turn's real total by construction. Calls whose usage row is
// absent are reported as unattributed: unknown, never free. Rows with no
// stamped cost are likewise unpriced and never $0 - a bucket carries
// UnpricedEvents so a caller knows its CostMicroUSD is an understatement until
// those are display-priced.
//
// # WithRaw IS AN AUDIT PAYLOAD, NOT A DATA SOURCE
//
// usage_events.raw exists to answer "what did the provider actually say", and
// adapters now build it from an explicit allow-list of usage/model/identity
// fields. Rows appended BEFORE that allow-list landed can hold whole transcript
// lines, and append-only means they are never rewritten - that history is
// exactly as it was stored. The default ListEvents projection therefore names
// its columns and excludes raw; WithRaw is the explicit opt-in, and the CLI
// passes it for export --include-raw and nothing else. The gate that matters is
// upstream of every reader: privacy.no_raw drops the column at collection, and
// what was never stored cannot be projected.
package store

import (
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// Filter selects and groups usage for reporting.
type Filter struct {
	Since time.Time // inclusive lower bound on event_time (zero = open)
	Until time.Time // exclusive upper bound on event_time (zero = open)

	Tools     []string // restrict to these tools (empty = all)
	Models    []string // restrict to these models (empty = all)
	Providers []string // restrict to recorded providers (empty = all; [""] = unknown)
	Projects  []string // restrict to these projects (empty = all)
	Sessions  []string // restrict to these sessions (empty = all)

	// GroupBy lists grouping dimensions, applied in order. Valid values:
	// "hour","day","week","month","tool","model","provider","project",
	// "session". A provider bucket keyed by the empty string is the one
	// holding the rows whose source never named a billing provider.
	// Empty means a single grand-total bucket.
	GroupBy []string
}

// Bucket is one grouped row of summarised usage.
type Bucket struct {
	// Keys maps each GroupBy dimension to its value for this bucket
	// (e.g. {"day":"2026-05-29","tool":"codex"}). Ordered via OrderedKeys.
	Keys        map[string]string
	OrderedKeys []string // dimension names in GroupBy order
	Events      int64
	// Sessions is the distinct non-empty session_id count within the group,
	// computed store-level (COUNT DISTINCT) so callers never have to
	// materialize one bucket per session just to count them. Distinct counts
	// do not add across buckets.
	Sessions      int64
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
	Reasoning     int64
	Total         int64
	// CostMicroUSD sums the costs stamped at collect time (millionths of USD).
	// Rows with no stamped cost contribute nothing to it — they are counted in
	// UnpricedEvents instead and must be display-priced separately, so a bucket
	// with UnpricedEvents > 0 is an UNDERSTATEMENT until that is folded in.
	CostMicroUSD int64
	// UnpricedEvents counts rows in the bucket whose cost_micro_usd is NULL
	// (collected before v3, or a model the pricing ladder could not price).
	UnpricedEvents int64
	// ComputedCostEvents counts the PRICED rows of the bucket whose cost this
	// project derived from a public rate card rather than reading off the
	// harness (model.PriceProvenance). It is what lets a surface say the sum is
	// an estimate: zero means every dollar in CostMicroUSD is one a vendor
	// reported.
	ComputedCostEvents int64
}

// UnpricedGroup aggregates the rows of one bucket that carry NO stamped cost,
// split by the attributes a price lookup needs (tool for the reasoning-billing
// rule, model/provider for the rate, service tier for the tier rate). Keys
// repeats the Filter.GroupBy dimension values so a caller can fold the
// display-priced result back into the matching Summarize bucket.
type UnpricedGroup struct {
	Keys        map[string]string
	OrderedKeys []string
	Tool        string
	Model       string
	Provider    string
	ServiceTier string

	Events        int64
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
	Reasoning     int64
}

// Summary is the result of Summarize: grouped buckets plus a grand total.
type Summary struct {
	GroupBy []string
	Buckets []Bucket
	Totals  Bucket
}

// RollupSummary is the result of SummarizeRollup: the same buckets Summarize
// produces, from the derived rollup instead of the ledger, plus the range those
// buckets actually cover.
//
// Schema 8 makes the bucket measures complete, including Sessions and
// ComputedCostEvents. The distinct type remains for source compatibility and
// because this method still reports its outward-snapped range explicitly.
type RollupSummary struct {
	GroupBy []string
	Buckets []Bucket
	Totals  Bucket
	// Since and Until are the requested bounds snapped OUTWARD to whole UTC
	// 15-minute buckets - the rollup's resolution - so the buckets can cover
	// slightly more than was asked for. Zero means that bound was open. A
	// caller labelling a range must label these, not the ones it passed in.
	Since time.Time
	Until time.Time
}

// ListOption tunes what ListEvents projects. It exists so the expensive column
// is opt-in at the call site rather than a default every caller pays for.
type ListOption func(*listOptions)

type listOptions struct {
	includeRaw      bool
	keyset          bool
	eventTimeKeyset bool
	afterEventTime  time.Time
	afterID         int64
	limit           int
}

// WithKeyset turns ListEvents into one page of a keyset walk: at most limit rows
// with a row id ABOVE afterID, ordered by id. Pass afterID 0 for the first page
// and the last id of a page for the next one.
//
// The order changes with the option, and it has to: ids are AUTOINCREMENT, so
// ordering by id is a total order that a cursor can resume from exactly, while
// the default (event_time, id) order interleaves later-ingested rows with
// earlier event times and would make an id cursor skip them. A caller that wants
// event-time order must page some other way, or not page at all.
//
// A limit of 0 or less means unlimited, which is the point of a cap living at
// the caller: the store enforces the walk, not the policy.
func WithKeyset(afterID int64, limit int) ListOption {
	return func(o *listOptions) {
		o.keyset = true
		o.eventTimeKeyset = false
		o.afterID = afterID
		o.limit = limit
	}
}

// WithEventTimeKeyset turns ListEvents into one page of its existing total
// event-time order. Rows after (after, afterID) are returned, ordered by
// (event_time_unix, id); afterID 0 starts the walk and ignores after. Export
// uses this cursor so a late-observed old event keeps the same position the
// unpaged public output has always used. Existing WithKeyset remains id-ordered.
func WithEventTimeKeyset(after time.Time, afterID int64, limit int) ListOption {
	return func(o *listOptions) {
		o.keyset = false
		o.eventTimeKeyset = true
		o.afterEventTime = after
		o.afterID = afterID
		o.limit = limit
	}
}

// WithRaw restores the raw audit payload to a ListEvents projection. ONLY the
// export --include-raw path may pass it: raw can carry full transcript content
// for rows appended before the usage-object allow-list landed, and every other
// consumer has no use for it. Without it, raw never leaves the database.
func WithRaw() ListOption {
	return func(o *listOptions) { o.includeRaw = true }
}

// Applied reports committed new ledger/context rows and changed code snapshots.
// Duplicate ledger/context keys and unchanged code snapshots do not count.
type Applied struct {
	Events   int
	Activity int
	// TurnContexts counts new usage_turn_context rows: (turn, dimension) pairs
	// newly recorded as having run under something. ONE turn can contribute up
	// to five of them, one per dimension, so this counts rows and not turns. A
	// pair already carrying a context counts here no more than a duplicate event
	// counts in Events.
	TurnContexts int
	// CodeChanges counts inserted or updated source snapshots. Identical
	// repeats and older source versions do not count.
	CodeChanges int
}

// ObservationBatch is everything one read of one source commits together. It is
// a struct rather than a parameter list because the set grows: activity joined
// events in v5, skill contexts in v6, all five turn-context dimensions in v7,
// and each addition must land in the SAME transaction as the checkpoint that
// gates its re-read, not in a second call that a crash could separate from the
// first.
type ObservationBatch struct {
	Events   []model.UsageEvent
	Activity []model.ActivityEvent
	// TurnContexts records what each usage event ran UNDER: its subagent, its
	// skill, its MCP tool and server, its plugin. At most one value per (event,
	// dimension), keyed by the event's dedup key — see model.TurnContext.
	TurnContexts []model.TurnContext
	// Checkpoint, when non-nil, is upserted in the same transaction.
	Checkpoint *model.SourceCheckpoint
	// CodeChanges replaces mutable per-change snapshots when their source
	// version is not older than the stored version.
	CodeChanges []model.CodeChange
}

// SourceStat summarises stored usage per tool for the `sources` command.
type SourceStat struct {
	Tool         string
	Models       []string
	Sessions     int64
	Events       int64
	Total        int64
	FirstEvent   time.Time
	LastEvent    time.Time
	LastObserved time.Time
}

// DBStats describes the database as a whole for the `doctor` command.
type DBStats struct {
	Path          string
	Events        int64
	Snapshots     int64
	DistinctTools int64
	DistinctModel int64
	SizeBytes     int64
	EarliestEvent time.Time
	LatestEvent   time.Time
	SchemaVersion int // version recorded in the database, not the binary's
}
