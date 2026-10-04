// Package collect drives one or more collection cycles: it iterates the
// adapter registry, reads each discovered source read-only, and persists the
// observations into the append-only store. EventLevel observations are stored
// directly (deduplicated on DedupKey). Aggregate observations are turned into
// synthetic immutable events via a monotonic-with-reset delta against the last
// stored accumulator state, so a source that later shrinks (compaction,
// deletion, reset) can never reduce a previously-reported total.
//
// TWO ENTRY POINTS, AND NOTHING AROUND THEM: RunOnce performs one pass and Run
// performs one every interval until its context is cancelled (issue #72,
// decision 3). Everything a MACHINE needs around a long-running collector -
// a pidfile and its lock, a recorded build identity, a stop-and-wait, a watch on
// the executable for an upgrade - is deliberately absent, because those are this
// project's answers to questions a consumer embedding the package has already
// answered its own way. aiusage's own daemon keeps all of it in internal/daemon,
// which composes this package exactly the way any other consumer would.
//
// The store is taken as Store, an interface this package declares over the six
// methods a pass actually uses (decision 8), so *store.Ledger satisfies it
// without either package naming the other's shape.
//
// INJECTION SEAMS: RunOnce accepts a Pricer interface to price each new event
// at ingest time, and an optional Refresher to update the price table before
// stamping. A versioned pricer also fills missing historical costs when the
// store supports price sync. A Pricer returns microUSD, source, and ok;
// ok=false leaves the event unpriced (CostMicroUSD = nil). Cost stamped by an adapter
// (attached to the event before collection) is never overwritten by the ladder:
// the Pricer is consulted only for events that arrive UNPRICED. WithPricer() /
// WithoutRaw() are Options that configure a cycle; Options are composable and
// independent.
//
// INCREMENTAL COLLECTION: adapters that implement adapter.Incremental can
// supply a checkpoint (SourceCheckpoint) to skip unchanged sources — a seam for
// efficiency, not correctness. The checkpoint rides the same transaction as its
// events, so a crash cannot advance a checkpoint past the rows it accounts for.
// A checkpoint load failure stops that source: some adapters keep monetary
// baselines in the checkpoint, so a full read could double charge history.
//
// SINGLE-TRANSACTION OBSERVATION: events, activity records, and turn contexts
// from one source observation land in ONE transaction per source, alongside the
// source checkpoint (for append-only sources; for aggregate snapshots the
// checkpoint lands on the final snapshot). A crashed or collided cell re-reads
// the whole source. This transaction boundary is the unit of consistency: an
// observer cannot see one part of an observation without the others.
//
// ACTIVITY and TURN CONTEXT: The collector records which tool was called
// (activity_events) and which subagent/skill/MCP tool/server/plugin each turn
// ran under (usage_turn_context). Activity rows carry no cost column — cost is
// derived on read by joining the usage event the activity names, so one turn's
// tokens are not multiplied by the number of calls it made. Turn contexts carry
// no token information — they are a property of the turn (dimension attribute),
// and a turn commonly carries several (subagent AND skill AND MCP tool, etc.),
// so a dimension-blind query would overstate cost by summing the same turn
// multiple times.
package collect

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// nowFn yields the cycle's observation timestamp. It is a package-level seam so
// tests can simulate polls at distinct instants (in production, real polls are
// minutes apart). Always returns UTC.
var nowFn = func() time.Time { return time.Now().UTC() }

// Pricer stamps a cost on a usage event just before it is appended. It returns
// the cost in micro-USD and the price_source that produced it. ok=false leaves
// the event unpriced (a NULL cost column) — the honest state, since a stored 0
// would claim the request was free. Reporting prices NULL rows from the current
// table at display time instead. A versioned pricer also lets collection fill
// missing stored costs when rates become available later.
type Pricer interface {
	PriceEvent(e model.UsageEvent) (microUSD int64, source string, ok bool)
}

// Refresher is the optional half of Pricer: a price ladder that can update
// itself from upstream. RunOnce offers it one chance per cycle; the
// implementation is expected to throttle itself and to fail silently.
type Refresher interface {
	Refresh(ctx context.Context) error
}

// Option configures a collection cycle.
type Option func(*cycleOptions)

type cycleOptions struct {
	pricer          Pricer
	noRaw           bool
	onCycle         func(CycleStats, error)
	reconcileClaude bool
}

// WithPricer stamps a cost on every new event from the price table in effect at
// ingest. A pricer with Revision() string also enables historical price sync
// on stores that support it. Without a pricer, events are stored unpriced.
// If the pricer also has PriceStoredEvent, historical sync uses that method to
// account for source enrichment that was not persisted; fresh events continue
// to use PriceEvent.
func WithPricer(p Pricer) Option {
	return func(o *cycleOptions) { o.pricer = p }
}

// WithClaudeReconciliation opts into updating growing Claude usage records.
// It requires a store with ReconcileClaudeBatch, such as *store.Ledger.
// Claude sources are reread each pass so even pre-existing checkpoints cannot
// hide growth. Other adapters and collection without this option are unchanged.
// See store.Ledger.ReconcileClaudeBatch for the bounded mutation contract.
func WithClaudeReconciliation() Option {
	return func(o *cycleOptions) { o.reconcileClaude = true }
}

type claudeReconciler interface {
	ReconcileClaudeBatch(context.Context, store.ObservationBatch) (store.Applied, error)
}

// WithCycleCallback reports the outcome of each pass Run makes: the stats and
// the error RunOnce returned, in that order, on Run's own goroutine.
//
// It exists because Run is a loop with no return value until it stops, so
// without it a long-running collector is a black box - a consumer could not log
// a pass, export a counter, or notice that every source has been failing for an
// hour. RunOnce ignores it: a caller holding the stats already has them.
//
// The callback runs BETWEEN passes and blocks the next one, which is the honest
// ordering (a pass is not reported until it is over) and makes a slow callback a
// slow collector. Keep it to logging or a metric.
func WithCycleCallback(fn func(CycleStats, error)) Option {
	return func(o *cycleOptions) { o.onCycle = fn }
}

// WithoutRaw drops every adapter's raw audit payload before it reaches the
// store (config privacy.no_raw). The collector is the choke point every
// adapter's output passes through, so the switch holds for adapters that never
// learn about it — including ones added later — and covers both raw columns:
// usage_events.raw via the events, aggregate_state.raw and the synthetic delta
// event via the snapshots.
func WithoutRaw() Option {
	return func(o *cycleOptions) { o.noRaw = true }
}

// CycleStats reports the outcome of a single RunOnce.
type CycleStats struct {
	Adapters       int      // adapters iterated
	Sources        int      // sources discovered + collected
	SourcesFailed  int      // sources that recorded errors and stored nothing
	EventsInserted int      // new dedup keys actually written (event + synthetic)
	EventsSeen     int      // observed event-level records (pre-dedup)
	Snapshots      int      // aggregate snapshots observed
	Errors         []string // non-fatal per-adapter / per-source errors
	PricesSynced   int      // existing unpriced rows given a computed cost

	// ActivitySeen counts observed tool/skill/hook invocations (pre-dedup) and
	// ActivityInserted the new activity dedup keys actually written. They are
	// kept apart from the event counts because they measure a different ledger:
	// a cycle that inserts no events can still insert activity, and reading one
	// as the other would misreport both.
	ActivitySeen     int
	ActivityInserted int

	// TurnContextsSeen counts observed (turn, dimension) attributions — which
	// subagent, skill, MCP tool, MCP server or plugin each turn ran under — and
	// TurnContextsInserted the new ones actually written. A context is one per
	// (USAGE EVENT, DIMENSION), so ONE turn can contribute up to five: these
	// count rows, not turns, and they track the event counts rather than the
	// activity ones, being the turns a context spent and not the calls it made.
	TurnContextsSeen     int
	TurnContextsInserted int

	// RollupRebuilt reports that the pass found the derived rollup out
	// of step with the ledger and rebuilt it before collecting. Expected once,
	// on the first pass after the v4 migration; anywhere else it means a write
	// reached the ledger without its rollup delta and is worth noticing.
	RollupRebuilt bool

	// Canceled reports that the context was cancelled mid-pass and the cycle
	// stopped early. Every count above is then partial — in particular the
	// adapter and source being processed are already counted — so a truncated
	// cycle must never be read (or logged) as a completed one.
	Canceled bool

	// CodeChangesUpdated counts per-turn snapshots inserted or changed.
	CodeChangesUpdated int
}

// AllFailed reports whether the cycle produced only errors: every discovered
// source failed outright, or discovery itself yielded nothing but errors.
// `once` maps this to its nonzero exit for cron use; partial failure stays 0.
func (s CycleStats) AllFailed() bool {
	return len(s.Errors) > 0 && s.SourcesFailed == s.Sources
}

// RunOnce performs one full collection pass. Per-source and per-adapter errors
// are non-fatal: they are appended to CycleStats.Errors and collection
// continues. RunOnce returns a non-nil error for cancellation or an explicitly
// requested capability that the store does not support.
//
// A cancelled context truncates the pass: RunOnce returns ctx.Err() together
// with CycleStats.Canceled set, and the counts in those stats cover only the
// work done so far. Nothing is lost by the truncation — checkpoints ride the
// event transactions — but the stats must be reported as partial.
func RunOnce(ctx context.Context, reg *adapter.Registry, st Store, dc adapter.DiscoverConfig, opts ...Option) (CycleStats, error) {
	var o cycleOptions
	for _, fn := range opts {
		fn(&o)
	}

	var stats CycleStats
	if err := ctx.Err(); err != nil {
		stats.Canceled = true
		return stats, err
	}
	observedAt := nowFn()
	if o.reconcileClaude {
		if _, ok := st.(claudeReconciler); !ok {
			return stats, fmt.Errorf("collect: Claude reconciliation requires a store with ReconcileClaudeBatch")
		}
	}

	// Before anything is appended. The rollup's deltas ride the event
	// transactions, so the only way it can fall behind is a ledger that grew
	// without this code running: the v4 migration creates it empty, and that is
	// the case this call exists for. Doing it first means every reader sees a
	// consistent summary for the whole pass rather than after it, and the pass's
	// own deltas land on a rollup that is already in step.
	//
	// Non-fatal: collection is the daemon's job and must not be held hostage to
	// a derived summary. A failure is reported and the pass continues.
	if rebuilt, err := st.EnsureRollup(ctx); err != nil {
		stats.Errors = append(stats.Errors, fmt.Sprintf("rollup: %v", err))
	} else {
		stats.RollupRebuilt = rebuilt
	}

	// One refresh attempt per cycle, before anything is stamped, so a stamped
	// cost uses the newest table this machine can reach. The ladder throttles
	// and swallows its own failures; a stale table is never a cycle error.
	if r, ok := o.pricer.(Refresher); ok {
		_ = r.Refresh(ctx)
	}

	// Optional capabilities preserve the existing Store and Pricer contracts.
	// The ledger persists progress; unchanged source checkpoints must not stop
	// older usage from receiving a price that was unavailable at ingestion.
	if p, ok := o.pricer.(interface{ Revision() string }); ok {
		if revision := p.Revision(); revision != "" {
			if s, ok := st.(interface {
				SyncUnpriced(context.Context, string, func(model.UsageEvent) (int64, string, bool)) (int, error)
			}); ok {
				var err error
				price := o.pricer.PriceEvent
				if stored, ok := o.pricer.(interface {
					PriceStoredEvent(model.UsageEvent) (int64, string, bool)
				}); ok {
					price = stored.PriceStoredEvent
				}
				stats.PricesSynced, err = s.SyncUnpriced(ctx, revision, price)
				if err != nil {
					stats.Errors = append(stats.Errors, fmt.Sprintf("price sync: %v", err))
					if ctx.Err() != nil {
						stats.Canceled = true
						return stats, ctx.Err()
					}
				}
			}
		}
	}

	for _, ad := range reg.All() {
		if err := ctx.Err(); err != nil {
			stats.Canceled = true
			return stats, err
		}
		stats.Adapters++

		sources, err := ad.Discover(ctx, dc)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("discover %s: %v", ad.ID(), err))
			// Discover may still return best-effort sources alongside an error.
		}

		for _, src := range sources {
			if err := ctx.Err(); err != nil {
				stats.Canceled = true
				return stats, err
			}
			stats.Sources++
			errsBefore := len(stats.Errors)
			progressed := false

			var obs adapter.Observation
			var err error
			apply := st.ApplyBatch
			if o.reconcileClaude && ad.ID() == model.ToolClaudeCode {
				obs, err = ad.Collect(ctx, src)
				apply = st.(claudeReconciler).ReconcileClaudeBatch
			} else {
				obs, err = collectSource(ctx, ad, st, src)
			}
			if err != nil {
				stats.Errors = append(stats.Errors, fmt.Sprintf("collect %s %s: %v", ad.ID(), src.Path, err))
				// Best-effort: a bad source must not abort the cycle. Still
				// process whatever observations were returned.
			}
			// Before anything is stamped or stored, and regardless of the error
			// above — a partially-collected observation is persisted too.
			if o.noRaw {
				stripRaw(&obs)
			}

			// The checkpoint rides the events transaction unless snapshots
			// exist — then it rides the last snapshot's transaction instead,
			// so it can only land after every cell of the source applied.
			evCp := obs.Checkpoint
			if len(obs.Snapshots) > 0 {
				evCp = nil
			}

			// ApplyBatch commits per-row: a non-nil error may accompany a
			// partial insert (skipped poison rows), so the counts hold
			// regardless. Events, activity and skill contexts ride ONE
			// transaction with the checkpoint, so the checkpoint can never
			// advance past rows that did not land.
			applied, sErr := storeObservation(ctx, apply, obs, observedAt, evCp, o.pricer)
			stats.EventsSeen += len(obs.Events)
			stats.EventsInserted += applied.Events
			stats.ActivitySeen += len(obs.Activity)
			stats.ActivityInserted += applied.Activity
			stats.TurnContextsSeen += len(obs.TurnContexts)
			stats.TurnContextsInserted += applied.TurnContexts
			stats.CodeChangesUpdated += applied.CodeChanges
			if sErr != nil {
				stats.Errors = append(stats.Errors, fmt.Sprintf("insert events %s %s: %v", ad.ID(), src.Path, sErr))
			}
			if applied.Events > 0 || applied.Activity > 0 || applied.CodeChanges > 0 ||
				(sErr == nil && (len(obs.Events) > 0 || len(obs.Activity) > 0 || len(obs.CodeChanges) > 0)) {
				progressed = true
			}

			// clean tracks whether every write of this source so far applied
			// fully. The checkpoint may only land on the final snapshot when
			// clean: a failed or collided cell must stay re-derivable, and an
			// advanced checkpoint would gate its re-read off.
			clean := sErr == nil
			for i, s := range obs.Snapshots {
				stats.Snapshots++
				var cp *model.SourceCheckpoint
				if clean && i == len(obs.Snapshots)-1 {
					cp = obs.Checkpoint
				}
				n, advanced, sErr := storeSnapshot(ctx, st, s, observedAt, cp, o.pricer)
				if sErr != nil {
					stats.Errors = append(stats.Errors, fmt.Sprintf("snapshot %s %s: %v", s.Tool, s.Key, sErr))
					clean = false
					continue
				}
				if !advanced {
					clean = false
				}
				stats.EventsInserted += n
				progressed = true
			}

			if len(stats.Errors) > errsBefore && !progressed {
				stats.SourcesFailed++
			}
		}
	}

	if err := ctx.Err(); err != nil {
		stats.Canceled = true
		return stats, err
	}
	return stats, nil
}

// stripRaw clears every audit payload an observation carries.
//
// Activity is absent from it on purpose and not by oversight:
// model.ActivityEvent has no raw field and activity_events has no raw column,
// so a tool's input has nowhere to be dropped FROM. privacy.no_raw is satisfied
// there by construction rather than by a switch someone has to remember.
//
// Snapshots are cleared before the baseline comparison in storeSnapshot, so a
// cell whose stored state still holds a payload from before the switch counts as changed
// once and is rewritten without it — aggregate_state is mutable, so the switch
// is retroactive there. usage_events is not: rows already appended keep what
// they were written with.
func stripRaw(obs *adapter.Observation) {
	for i := range obs.Events {
		obs.Events[i].Raw = ""
	}
	for i := range obs.Snapshots {
		obs.Snapshots[i].Raw = ""
	}
}

// collectSource reads one source, going through the incremental path when the
// adapter supports it. A checkpoint load failure holds this source until its
// saved state can be read; a successful absent checkpoint is a first read.
func collectSource(ctx context.Context, ad adapter.Adapter, st Store, src adapter.Source) (adapter.Observation, error) {
	inc, ok := ad.(adapter.Incremental)
	if !ok {
		return ad.Collect(ctx, src)
	}
	cp, err := st.Checkpoint(ctx, src.Tool, src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("load checkpoint: %w", err)
	}
	return inc.CollectIncremental(ctx, src, cp)
}

// storeObservation stamps ObservedTime and cost on event-level records that
// lack one, stamps ObservedTime on activity rows, then appends both ledgers
// idempotently together with the source checkpoint in ONE transaction.
//
// Activity is deliberately NOT priced here. An activity row carries no cost
// column at all: its cost is derived on read by joining the usage row it names,
// so there is exactly one stamped cost per turn and no second copy to keep in
// step with the ladder.
func storeObservation(ctx context.Context, apply func(context.Context, store.ObservationBatch) (store.Applied, error), obs adapter.Observation, observedAt time.Time, cp *model.SourceCheckpoint, p Pricer) (store.Applied, error) {
	events, activity, contexts := obs.Events, obs.Activity, obs.TurnContexts
	if len(events) == 0 && len(activity) == 0 && len(contexts) == 0 && len(obs.CodeChanges) == 0 && cp == nil {
		return store.Applied{}, nil
	}
	stamped := make([]model.UsageEvent, len(events))
	for i, e := range events {
		if e.ObservedTime.IsZero() {
			e.ObservedTime = observedAt
		}
		stampCost(p, &e)
		stamped[i] = e
	}
	acts := make([]model.ActivityEvent, len(activity))
	for i, a := range activity {
		if a.ObservedTime.IsZero() {
			a.ObservedTime = observedAt
		}
		acts[i] = a
	}
	// Turn contexts are not priced here either, and for a stronger reason than
	// activity: they carry no token columns at all. The cost of a skill or an
	// agent is the cost of the usage rows it names, read through the join, so
	// there is one stamped figure per turn and never a second copy to keep in
	// step — nor five copies, one per dimension, which is the shape this would
	// have taken had the contexts carried their own tokens.
	ctxs := make([]model.TurnContext, len(contexts))
	for i, c := range contexts {
		if c.ObservedTime.IsZero() {
			c.ObservedTime = observedAt
		}
		ctxs[i] = c
	}
	changes := make([]model.CodeChange, len(obs.CodeChanges))
	for i, change := range obs.CodeChanges {
		if change.ObservedTime.IsZero() {
			change.ObservedTime = observedAt
		}
		changes[i] = change
	}
	return apply(ctx, store.ObservationBatch{
		Events: stamped, Activity: acts, TurnContexts: ctxs, CodeChanges: changes, Checkpoint: cp,
	})
}

// stampCost prices an event at the table in effect right now. A model no rung
// of the ladder knows is left unpriced: CostMicroUSD stays nil, which stores as
// SQL NULL. Stamping 0 instead would assert the request was free, and the
// ledger is append-only — that lie could never be corrected in place.
//
// AN EVENT THAT ALREADY CARRIES A COST KEEPS IT, which is what "stamps cost on
// records that lack one" has always meant and is now enforced rather than
// assumed. A cost set by the adapter came from the HARNESS's own accounting —
// copilot's vendor-priced nano-AIU valuation, crush's session cost, goose's
// provider-reported figure — and the ladder is an ESTIMATE of the same charge
// from a public rate card. Letting the estimate overwrite the vendor's own
// number is a strict loss of fidelity, and it silently happened whenever the
// table happened to know the model id: copilot proxies `gpt-5-mini`, which the
// embedded LiteLLM snapshot prices, so every Copilot call on that model would
// have had its exact vendor cost replaced by an approximation of it.
func stampCost(p Pricer, e *model.UsageEvent) {
	if p == nil || e.CostMicroUSD != nil {
		return
	}
	if micro, source, ok := p.PriceEvent(*e); ok {
		e.SetCost(micro, source)
	}
}

// storeSnapshot materialises the positive monotonic-with-reset delta for one
// aggregate cell as a synthetic immutable event and records the new state in
// one atomic store operation — a crash between the two would re-derive the
// same delta next cycle under a fresh dedup key, double counting it forever.
// A dedup collision (two cycles at the same observed instant) is handled by
// ApplySnapshot, which keeps the old baseline so the delta is re-derivable.
// Returns the number of new dedup keys inserted (0 or 1) and whether the
// state actually advanced (false on a collision — the caller must then hold
// the source checkpoint back so the delta stays re-derivable).
func storeSnapshot(ctx context.Context, st Store, s model.AggregateSnapshot, observedAt time.Time, cp *model.SourceCheckpoint, p Pricer) (int, bool, error) {
	last, err := st.LastState(ctx, s.Tool, s.Key)
	if err != nil {
		return 0, false, fmt.Errorf("last state: %w", err)
	}

	// Zero-delta fast path: the stored baseline already matches the observed
	// counters, so there is nothing to append and no state row worth
	// rewriting — only a pending source checkpoint still needs to land.
	if last != nil && snapshotUnchanged(*last, s) {
		if cp != nil {
			if _, err := st.ApplyEvents(ctx, nil, cp); err != nil {
				return 0, false, fmt.Errorf("checkpoint: %w", err)
			}
		}
		return 0, true, nil
	}

	d := snapshotDelta(last, s)

	var events []model.UsageEvent
	if d.hasPositive() {
		ev := syntheticEvent(s, d, observedAt)
		stampCost(p, &ev)
		events = []model.UsageEvent{ev}
	}

	// State advances even without a positive delta, so subsequent polls diff
	// against the latest counters.
	s.ObservedTime = observedAt
	n, err := st.ApplySnapshot(ctx, events, s, cp)
	if err != nil {
		return 0, false, fmt.Errorf("apply snapshot: %w", err)
	}
	advanced := len(events) == 0 || n > 0
	return n, advanced, nil
}

// snapshotUnchanged reports whether cur carries exactly the counters and
// attributes already stored for the cell. ObservedTime is deliberately
// excluded: nothing diffs against it, so its advance alone does not warrant a
// row rewrite every cycle. Provider is excluded for a harder reason: it has no
// aggregate_state column, so the stored side is always empty and comparing it
// would report "changed" on every poll of every provider-bearing source,
// defeating the fast path entirely. It is carried onto the delta event from
// the fresh snapshot, never from the baseline, so nothing is lost.
func snapshotUnchanged(last, cur model.AggregateSnapshot) bool {
	return last.InputTokens == cur.InputTokens &&
		last.OutputTokens == cur.OutputTokens &&
		last.CacheCreationTokens == cur.CacheCreationTokens &&
		last.CacheReadTokens == cur.CacheReadTokens &&
		last.ReasoningTokens == cur.ReasoningTokens &&
		last.TotalTokens == cur.TotalTokens &&
		last.Model == cur.Model &&
		last.SessionID == cur.SessionID &&
		last.Project == cur.Project &&
		last.SourcePath == cur.SourcePath &&
		last.Raw == cur.Raw
}

// delta carries the per-field positive change for one aggregate cell.
type delta struct {
	input         int64
	output        int64
	cacheCreation int64
	cacheRead     int64
	reasoning     int64
	total         int64
}

func (d delta) hasPositive() bool {
	return d.input > 0 || d.output > 0 || d.cacheCreation > 0 ||
		d.cacheRead > 0 || d.reasoning > 0 || d.total > 0
}

// fieldDelta computes the monotonic-with-reset change for a single field:
// if the counter grew or held, take the increment; if it shrank (a reset,
// truncation or deletion) take the current value. Never returns negative.
func fieldDelta(last, cur int64) int64 {
	if cur >= last {
		return cur - last
	}
	return cur
}

// snapshotDelta derives the positive per-field delta of cur relative to last.
// A nil last (first observation of the cell) yields the full current counters.
func snapshotDelta(last *model.AggregateSnapshot, cur model.AggregateSnapshot) delta {
	if last == nil {
		return delta{
			input:         maxZero(cur.InputTokens),
			output:        maxZero(cur.OutputTokens),
			cacheCreation: maxZero(cur.CacheCreationTokens),
			cacheRead:     maxZero(cur.CacheReadTokens),
			reasoning:     maxZero(cur.ReasoningTokens),
			total:         maxZero(cur.TotalTokens),
		}
	}
	return delta{
		input:         fieldDelta(last.InputTokens, cur.InputTokens),
		output:        fieldDelta(last.OutputTokens, cur.OutputTokens),
		cacheCreation: fieldDelta(last.CacheCreationTokens, cur.CacheCreationTokens),
		cacheRead:     fieldDelta(last.CacheReadTokens, cur.CacheReadTokens),
		reasoning:     fieldDelta(last.ReasoningTokens, cur.ReasoningTokens),
		total:         fieldDelta(last.TotalTokens, cur.TotalTokens),
	}
}

func maxZero(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// syntheticEvent builds the immutable usage event representing one aggregate
// delta. EventTime is the adapter-populated per-record time when present
// (gemini/agy record real turn timestamps) so a downtime gap's delta does not
// land as a spike at the restart second; it falls back to the cycle instant.
// The DedupKey stays on the cycle instant at nanosecond resolution so distinct
// polls never collide even when the record timestamp is unchanged; an
// exact-instant collision (frozen clock) is handled by the store's
// ApplySnapshot, which refuses to advance state when the insert was fully
// ignored.
func syntheticEvent(s model.AggregateSnapshot, d delta, observedAt time.Time) model.UsageEvent {
	eventTime := s.ObservedTime
	if eventTime.IsZero() {
		eventTime = observedAt
	}
	return model.UsageEvent{
		Tool:                s.Tool,
		Model:               s.Model,
		Provider:            s.Provider,
		SessionID:           s.SessionID,
		Project:             s.Project,
		EventTime:           eventTime,
		ObservedTime:        observedAt,
		InputTokens:         d.input,
		OutputTokens:        d.output,
		CacheCreationTokens: d.cacheCreation,
		CacheReadTokens:     d.cacheRead,
		ReasoningTokens:     d.reasoning,
		TotalTokens:         d.total,
		SourcePath:          s.SourcePath,
		DedupKey:            "agg|" + s.Tool + "|" + s.Key + "|" + strconv.FormatInt(observedAt.UnixNano(), 10),
		Kind:                model.KindUsage,
		Raw:                 s.Raw,
	}
}
