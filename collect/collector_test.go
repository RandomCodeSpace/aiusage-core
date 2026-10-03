package collect

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// ---------------------------------------------------------------------------
// In-test fake store.
//
// The real SQLite-backed store (store.Open) is assembled in a sibling package
// during integration. To keep this leaf package self-checkable, the tests use a
// faithful in-memory implementation of collect.Store that reproduces the two
// behaviours the collector depends on:
//   - InsertEvents is INSERT OR IGNORE on DedupKey (append-only, idempotent).
//   - LastState / UpsertState keep exactly one row per (tool, key) accumulator.
//
// Integration swaps this for store.Open(tmpfile); the collector code is store-
// agnostic via the Store interface this package declares for itself (issue #72,
// decision 8), which is why the fake is six methods of real behaviour rather
// than thirty methods of which most return nil. The rest of what it carries is
// read-back the assertions use, not surface the collector can reach.
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu            sync.Mutex
	dedup         map[string]struct{}
	events        []model.UsageEvent
	activityDedup map[string]struct{}
	activity      []model.ActivityEvent
	ctxDedup      map[string]struct{} // key: usage dedup key + dimension, one value per axis
	turnContexts  []model.TurnContext
	state         map[string]model.AggregateSnapshot // key: tool|key
	checkpoints   map[string]model.SourceCheckpoint  // key: tool|sourcePath
	upserts       int                                // standalone UpsertState calls (snapshot path must not use it)
	ensureCalls   int                                // EnsureRollup calls (one per cycle)
}

var _ Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{
		dedup:         map[string]struct{}{},
		activityDedup: map[string]struct{}{},
		ctxDedup:      map[string]struct{}{},
		state:         map[string]model.AggregateSnapshot{},
		checkpoints:   map[string]model.SourceCheckpoint{},
	}
}

func (s *fakeStore) InsertEvents(_ context.Context, events []model.UsageEvent) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertLocked(events)
}

func (s *fakeStore) insertLocked(events []model.UsageEvent) (int, error) {
	inserted := 0
	for _, e := range events {
		if e.DedupKey == "" {
			return inserted, errors.New("empty dedup key")
		}
		if _, ok := s.dedup[e.DedupKey]; ok {
			continue // INSERT OR IGNORE
		}
		s.dedup[e.DedupKey] = struct{}{}
		s.events = append(s.events, e)
		inserted++
	}
	return inserted, nil
}

func (s *fakeStore) LastState(_ context.Context, tool, key string) (*model.AggregateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.state[tool+"|"+key]; ok {
		cp := v
		return &cp, nil
	}
	return nil, nil
}

func (s *fakeStore) UpsertState(_ context.Context, st model.AggregateSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts++
	s.state[st.Tool+"|"+st.Key] = st
	return nil
}

// ApplySnapshot mirrors the SQLite contract: events + state (+ checkpoint)
// land together, and a fully-collided insert leaves baseline and checkpoint
// untouched.
func (s *fakeStore) ApplySnapshot(_ context.Context, events []model.UsageEvent, st model.AggregateSnapshot, cp *model.SourceCheckpoint) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inserted, err := s.insertLocked(events)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 || inserted > 0 {
		s.state[st.Tool+"|"+st.Key] = st
		if cp != nil {
			s.checkpoints[cp.Tool+"|"+cp.SourcePath] = *cp
		}
	}
	return inserted, nil
}

// ApplyEvents mirrors the SQLite contract: events and checkpoint land together.
// Like the real store it is the events-only shorthand for ApplyObservation, so
// the two can never drift.
func (s *fakeStore) ApplyEvents(ctx context.Context, events []model.UsageEvent, cp *model.SourceCheckpoint) (int, error) {
	res, err := s.ApplyObservation(ctx, events, nil, cp)
	return res.Events, err
}

// ApplyObservation mirrors the SQLite contract: both ledgers and the checkpoint
// land together, activity deduped on its own key.
func (s *fakeStore) ApplyObservation(ctx context.Context, events []model.UsageEvent, activity []model.ActivityEvent, cp *model.SourceCheckpoint) (store.Applied, error) {
	return s.ApplyBatch(ctx, store.ObservationBatch{Events: events, Activity: activity, Checkpoint: cp})
}

// ApplyBatch mirrors the SQLite contract: all three streams and the checkpoint
// land together, activity deduped on its own key and a skill context deduped on
// the USAGE key it describes — which is what makes a second sighting of a turn
// a no-op rather than a second helping of its cost.
func (s *fakeStore) ApplyBatch(_ context.Context, b store.ObservationBatch) (store.Applied, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out store.Applied
	inserted, err := s.insertLocked(b.Events)
	out.Events = inserted
	if err != nil {
		return out, err
	}
	for _, a := range b.Activity {
		if a.DedupKey == "" {
			return out, errors.New("empty activity dedup key")
		}
		if _, ok := s.activityDedup[a.DedupKey]; ok {
			continue
		}
		s.activityDedup[a.DedupKey] = struct{}{}
		s.activity = append(s.activity, a)
		out.Activity++
	}
	for _, c := range b.TurnContexts {
		if c.UsageDedupKey == "" {
			return out, errors.New("empty turn context usage dedup key")
		}
		if c.Value == "" {
			return out, errors.New("empty turn context value")
		}
		if !c.Dimension.Valid() {
			return out, fmt.Errorf("unknown turn context dimension %q", c.Dimension)
		}
		// Keyed by (usage row, dimension), exactly as the real table's composite
		// primary key is: a second value on one axis is absorbed, a value on
		// another axis still lands.
		k := c.UsageDedupKey + "|" + string(c.Dimension)
		if _, ok := s.ctxDedup[k]; ok {
			continue
		}
		s.ctxDedup[k] = struct{}{}
		s.turnContexts = append(s.turnContexts, c)
		out.TurnContexts++
	}
	if b.Checkpoint != nil {
		s.checkpoints[b.Checkpoint.Tool+"|"+b.Checkpoint.SourcePath] = *b.Checkpoint
	}
	return out, nil
}

// SummarizeActivity reproduces the real store's attribution: each call takes
// its joined usage row's counts divided by the calls that shared the turn, so a
// multi-call turn is never counted more than once. The collector cannot reach
// it — it is not on the Store interface this package declares — and it is here
// so a collect-level test can assert the non-inflation invariant without
// SQLite.
func (s *fakeStore) SummarizeActivity(_ context.Context, f store.ActivityFilter) (*store.ActivitySummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	byKey := map[string]model.UsageEvent{}
	for _, e := range s.events {
		byKey[e.DedupKey] = e
	}
	var b store.ActivityBucket
	for _, a := range s.activity {
		if !f.Since.IsZero() && a.EventTime.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !a.EventTime.Before(f.Until) {
			continue
		}
		b.Calls++
		u, ok := byKey[a.UsageDedupKey]
		if !ok {
			b.UnattributedCalls++
			continue
		}
		n := int64(a.CallsInTurn)
		if n < 1 {
			n = 1
		}
		b.AttributedInput += u.InputTokens / n
		b.AttributedOutput += u.OutputTokens / n
		b.AttributedTotal += u.TotalTokens / n
		if c, priced := u.Cost(); priced {
			b.AttributedCostMicroUSD += c / n
		} else {
			b.UnpricedCalls++
		}
	}
	return &store.ActivitySummary{GroupBy: f.GroupBy, Totals: b}, nil
}

// SummarizeTurnContext reproduces the real store's turn attribution, which is
// the interesting half: NO division, and ONE dimension per query. Each context
// names one usage row and each usage row has at most one value per dimension
// (the fake's ctxDedup is keyed by (usage key, dimension), exactly as the real
// table's composite primary key is), so a turn's full cost is added once per
// query and the whole thing is bounded by the ledger without a divisor
// anywhere. Pinning the dimension is what keeps that true: without it a turn
// carrying four contexts would be counted four times, which is the mistake the
// required argument exists to prevent.
//
// The collector cannot reach it either; it is here so a collect-level test can
// assert that every attribution stays inside the real total.
func (s *fakeStore) SummarizeTurnContext(_ context.Context, dim model.TurnDimension, f store.ActivityFilter) (*store.TurnContextSummary, error) {
	if !dim.Valid() {
		return nil, fmt.Errorf("unknown turn-context dimension %q", dim)
	}
	if len(f.Kinds) > 0 || len(f.Names) > 0 {
		return nil, errors.New("turn contexts cannot be restricted by activity kinds or names")
	}
	if len(f.Skills) > 0 && dim != model.DimensionSkill {
		return nil, fmt.Errorf("Skills cannot restrict the %q dimension", dim)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	byKey := map[string]model.UsageEvent{}
	for _, e := range s.events {
		byKey[e.DedupKey] = e
	}
	var b store.TurnContextBucket
	for _, c := range s.turnContexts {
		if c.Dimension != dim {
			continue
		}
		if !f.Since.IsZero() && c.EventTime.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !c.EventTime.Before(f.Until) {
			continue
		}
		if len(f.Values) > 0 && !containsStr(f.Values, c.Value) {
			continue
		}
		if len(f.Skills) > 0 && !containsStr(f.Skills, c.Value) {
			continue
		}
		b.Turns++
		u, ok := byKey[c.UsageDedupKey]
		if !ok {
			b.UnjoinedTurns++
			continue
		}
		b.InputTokens += u.InputTokens
		b.OutputTokens += u.OutputTokens
		b.TotalTokens += u.TotalTokens
		if cost, priced := u.Cost(); priced {
			b.CostMicroUSD += cost
		} else {
			b.UnpricedTurns++
		}
	}
	return &store.TurnContextSummary{Dimension: dim, GroupBy: f.GroupBy, Totals: b}, nil
}

func containsStr(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

func (s *fakeStore) Checkpoint(_ context.Context, tool, sourcePath string) (*model.SourceCheckpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.checkpoints[tool+"|"+sourcePath]; ok {
		cp := v
		return &cp, nil
	}
	return nil, nil
}

// Summarize sums total_tokens over events whose EventTime is in [Since, Until).
// Zero bounds are treated as open. Grouping is not needed by these tests, so a
// single grand-total bucket is returned.
func (s *fakeStore) Summarize(_ context.Context, f store.Filter) (*store.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b store.Bucket
	for _, e := range s.events {
		if !f.Since.IsZero() && e.EventTime.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !e.EventTime.Before(f.Until) {
			continue
		}
		b.Events++
		b.Input += e.InputTokens
		b.Output += e.OutputTokens
		b.CacheCreation += e.CacheCreationTokens
		b.CacheRead += e.CacheReadTokens
		b.Reasoning += e.ReasoningTokens
		b.Total += e.TotalTokens
	}
	return &store.Summary{GroupBy: f.GroupBy, Totals: b}, nil
}

func (s *fakeStore) ListEvents(_ context.Context, _ store.Filter, _ ...store.ListOption) ([]model.UsageEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.UsageEvent, len(s.events))
	copy(out, s.events)
	return out, nil
}

// SummarizeRollup is unreachable through the collector; the fake keeps no
// rollup, so it answers from the same events Summarize reads. It is here for the
// tests that read a rollup-served answer back.
func (s *fakeStore) SummarizeRollup(ctx context.Context, f store.Filter) (*store.RollupSummary, error) {
	sum, err := s.Summarize(ctx, f)
	if err != nil {
		return nil, err
	}
	return &store.RollupSummary{
		GroupBy: sum.GroupBy,
		Buckets: sum.Buckets,
		Totals:  sum.Totals,
		Since:   f.Since,
		Until:   f.Until,
	}, nil
}

// EnsureRollup / RebuildRollup: the fake derives nothing, so there is nothing
// to fall out of step. ensureCalls counts the collector's calls so a test can
// pin that a cycle checks the rollup before it appends.
func (s *fakeStore) EnsureRollup(context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCalls++
	return false, nil
}

func (s *fakeStore) Close() error { return nil }

func windowTotal(t *testing.T, st *fakeStore, since, until time.Time) int64 {
	t.Helper()
	sum, err := st.Summarize(context.Background(), store.Filter{Since: since, Until: until})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	return sum.Totals.Total
}

// ---------------------------------------------------------------------------
// Configurable fake adapter.
// ---------------------------------------------------------------------------

type fakeAdapter struct {
	id    string
	class model.SourceClass
	// emit returns the observation for each Collect call; the int is the
	// 0-based call index, letting a test change behaviour across cycles.
	emit        func(call int) adapter.Observation
	discoverErr error
	collectErr  error
	mu          sync.Mutex
	calls       int
}

func (a *fakeAdapter) ID() string          { return a.id }
func (a *fakeAdapter) DisplayName() string { return a.id }

// Capabilities satisfies the interface. The collector never reads it — the
// declaration is a display fact, not a collection one — so the fake states the
// most conservative thing it can and nothing here depends on the values.
func (a *fakeAdapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      a.id,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(a.id),
		Tier:      model.TierFixture,
	}
}

func (a *fakeAdapter) Discover(_ context.Context, _ adapter.DiscoverConfig) ([]adapter.Source, error) {
	return []adapter.Source{{Tool: a.id, Class: a.class, Path: a.id + "/src", Label: a.id}}, a.discoverErr
}

func (a *fakeAdapter) Collect(_ context.Context, _ adapter.Source) (adapter.Observation, error) {
	a.mu.Lock()
	call := a.calls
	a.calls++
	a.mu.Unlock()
	return a.emit(call), a.collectErr
}

// fixed reference date for deterministic event-time windows.
var refDay = time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// (a) delta helpers.
// ---------------------------------------------------------------------------

func TestFieldDelta(t *testing.T) {
	cases := []struct {
		name      string
		last, cur int64
		want      int64
	}{
		{"increasing", 100, 250, 150},
		{"holds steady", 100, 100, 0},
		{"decreasing reset takes current", 1000, 30, 30},
		{"reset to zero", 1000, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fieldDelta(c.last, c.cur); got != c.want {
				t.Fatalf("fieldDelta(%d,%d)=%d want %d", c.last, c.cur, got, c.want)
			}
			if got := fieldDelta(c.last, c.cur); got < 0 {
				t.Fatalf("fieldDelta produced negative %d", got)
			}
		})
	}
}

func TestSnapshotDeltaNilLastTakesCurrent(t *testing.T) {
	cur := model.AggregateSnapshot{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
	d := snapshotDelta(nil, cur)
	if d.input != 10 || d.output != 20 || d.total != 30 {
		t.Fatalf("nil-last delta = %+v want full current", d)
	}
}

func TestSnapshotDeltaResetNeverNegative(t *testing.T) {
	last := &model.AggregateSnapshot{InputTokens: 500, OutputTokens: 500, TotalTokens: 1000}
	cur := model.AggregateSnapshot{InputTokens: 5, OutputTokens: 0, TotalTokens: 5}
	d := snapshotDelta(last, cur)
	if d.input != 5 || d.output != 0 || d.total != 5 {
		t.Fatalf("reset delta = %+v want current values", d)
	}
	if d.input < 0 || d.output < 0 || d.total < 0 {
		t.Fatalf("reset delta went negative: %+v", d)
	}
}

func TestSnapshotDeltaGrowthDiffs(t *testing.T) {
	last := &model.AggregateSnapshot{InputTokens: 100, TotalTokens: 100}
	cur := model.AggregateSnapshot{InputTokens: 300, TotalTokens: 300}
	d := snapshotDelta(last, cur)
	if d.input != 200 || d.total != 200 {
		t.Fatalf("growth delta = %+v want diff of 200", d)
	}
}

// ---------------------------------------------------------------------------
// (b) cycle idempotency.
// ---------------------------------------------------------------------------

func TestRunCycleIdempotentEvents(t *testing.T) {
	ev := model.UsageEvent{
		Tool: model.ToolCodex, Model: "gpt", EventTime: refDay.Add(6 * time.Hour),
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		DedupKey: "codex|req-1", Kind: model.KindUsage,
	}
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{Events: []model.UsageEvent{ev}} },
	}
	reg := adapter.NewRegistry(ad)
	st := newFakeStore()
	ctx := context.Background()
	dc := adapter.DiscoverConfig{}

	s1, err := RunOnce(ctx, reg, st, dc)
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if s1.EventsInserted != 1 {
		t.Fatalf("cycle 1 inserted=%d want 1", s1.EventsInserted)
	}

	s2, err := RunOnce(ctx, reg, st, dc)
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if s2.EventsInserted != 0 {
		t.Fatalf("cycle 2 inserted=%d want 0 (idempotent)", s2.EventsInserted)
	}
	if s2.EventsSeen != 1 {
		t.Fatalf("cycle 2 seen=%d want 1", s2.EventsSeen)
	}
}

func TestRunCycleStampsObservedTime(t *testing.T) {
	ev := model.UsageEvent{
		Tool: model.ToolCodex, EventTime: refDay.Add(6 * time.Hour),
		TotalTokens: 10, DedupKey: "codex|req-stamp", Kind: model.KindUsage,
		// ObservedTime intentionally zero.
	}
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{Events: []model.UsageEvent{ev}} },
	}
	st := newFakeStore()
	if _, err := RunOnce(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	got, _ := st.ListEvents(context.Background(), store.Filter{})
	if len(got) != 1 {
		t.Fatalf("stored %d events want 1", len(got))
	}
	if got[0].ObservedTime.IsZero() {
		t.Fatalf("ObservedTime was not stamped")
	}
}

func TestPerSourceErrorIsNonFatal(t *testing.T) {
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		collectErr: errors.New("boom"),
		emit:       func(int) adapter.Observation { return adapter.Observation{} },
	}
	st := newFakeStore()
	stats, err := RunOnce(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("cycle should not fail on per-source error: %v", err)
	}
	if len(stats.Errors) == 0 {
		t.Fatalf("expected a non-fatal error recorded")
	}
}

// ---------------------------------------------------------------------------
// (c) INVARIANT — event-level: a window total never decreases when the source
// later shrinks (compaction / deletion).
// ---------------------------------------------------------------------------

func TestEventLevelInvariantSurvivesCompaction(t *testing.T) {
	winStart := refDay.Add(6 * time.Hour)              // 06:00
	winEnd := refDay.Add(6*time.Hour + 15*time.Minute) // 06:15

	// 20 events x 100,000 = 2,000,000 tokens inside [06:00,06:15].
	full := make([]model.UsageEvent, 0, 20)
	for i := 0; i < 20; i++ {
		full = append(full, model.UsageEvent{
			Tool:        model.ToolClaudeCode,
			EventTime:   winStart.Add(time.Duration(i) * 30 * time.Second),
			TotalTokens: 100_000,
			DedupKey:    "cc|20260529|" + strconv.Itoa(i),
			Kind:        model.KindUsage,
		})
	}

	ad := &fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(call int) adapter.Observation {
			if call == 0 {
				return adapter.Observation{Events: full}
			}
			// Simulated compaction: the source now exposes nothing.
			return adapter.Observation{}
		},
	}
	reg := adapter.NewRegistry(ad)
	st := newFakeStore()
	ctx := context.Background()

	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if got := windowTotal(t, st, winStart, winEnd); got != 2_000_000 {
		t.Fatalf("after cycle 1 window total=%d want 2,000,000", got)
	}

	// Source compacted to empty; re-poll must not erode the stored history.
	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := windowTotal(t, st, winStart, winEnd); got != 2_000_000 {
		t.Fatalf("after compaction window total=%d want still 2,000,000", got)
	}
}

// ---------------------------------------------------------------------------
// (d) INVARIANT — aggregate: cumulative materialised total grows with the
// snapshot and never decreases when the snapshot later resets/deletes.
// ---------------------------------------------------------------------------

func TestAggregateInvariantMonotonicWithReset(t *testing.T) {
	const key = "session-xyz"
	snap := func(total int64) adapter.Observation {
		return adapter.Observation{Snapshots: []model.AggregateSnapshot{{
			Tool: model.ToolHermes, Key: key, SessionID: key,
			InputTokens: total, TotalTokens: total,
		}}}
	}

	ad := &fakeAdapter{
		id: model.ToolHermes, class: model.Aggregate,
		emit: func(call int) adapter.Observation {
			switch call {
			case 0:
				return snap(900_000)
			case 1:
				return snap(2_000_000)
			default:
				// Reset / deletion: source drops to a tiny value.
				return snap(0)
			}
		},
	}
	reg := adapter.NewRegistry(ad)
	st := newFakeStore()
	ctx := context.Background()

	// Real polls are minutes apart, so each cycle's synthetic-event DedupKey
	// (agg|tool|key|observedUnix) lands on a distinct second. Drive the clock
	// forward between cycles to reproduce that; otherwise two deltas in the same
	// second would collide on dedup.
	clock := refDay.Add(6 * time.Hour)
	restore := setNow(func() time.Time { return clock })
	defer restore()

	// Cycle 1: first observation -> full 900,000 materialised.
	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if got := windowTotal(t, st, time.Time{}, time.Time{}); got != 900_000 {
		t.Fatalf("after cycle 1 materialised=%d want 900,000", got)
	}

	// Cycle 2: grows to 2,000,000 -> +1,100,000 delta -> cumulative 2,000,000.
	clock = clock.Add(time.Minute)
	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := windowTotal(t, st, time.Time{}, time.Time{}); got != 2_000_000 {
		t.Fatalf("after cycle 2 materialised=%d want 2,000,000", got)
	}

	// Cycle 3: snapshot reset to 0 -> no negative delta -> stored stays >= 2,000,000.
	clock = clock.Add(time.Minute)
	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if got := windowTotal(t, st, time.Time{}, time.Time{}); got < 2_000_000 {
		t.Fatalf("after reset materialised=%d want >= 2,000,000", got)
	}
}

// ---------------------------------------------------------------------------
// (e) atomic snapshot apply: crash window must not double count.
// ---------------------------------------------------------------------------

// crashStore wraps fakeStore and fails ApplySnapshot once, persisting nothing —
// the transactional equivalent of the old crash between the committed event
// insert and the separate state upsert.
type crashStore struct {
	*fakeStore
	fail bool
}

func (s *crashStore) ApplySnapshot(ctx context.Context, events []model.UsageEvent, st model.AggregateSnapshot, cp *model.SourceCheckpoint) (int, error) {
	if s.fail {
		s.fail = false
		return 0, errors.New("injected crash")
	}
	return s.fakeStore.ApplySnapshot(ctx, events, st, cp)
}

// TestSnapshotCrashWindowNoDoubleCount drives the collector through a failed
// snapshot apply. The old two-step write committed the delta event, crashed
// before the state upsert, and the next cycle re-derived the same delta under
// a fresh nano dedup key — a permanent double count. With the atomic apply the
// failed cycle persists nothing and the retry lands the delta exactly once.
func TestSnapshotCrashWindowNoDoubleCount(t *testing.T) {
	ad := &fakeAdapter{
		id: model.ToolHermes, class: model.Aggregate,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Snapshots: []model.AggregateSnapshot{{
				Tool: model.ToolHermes, Key: "cell", SessionID: "cell",
				InputTokens: 900_000, TotalTokens: 900_000,
			}}}
		},
	}
	reg := adapter.NewRegistry(ad)
	st := &crashStore{fakeStore: newFakeStore(), fail: true}
	ctx := context.Background()

	clock := refDay.Add(6 * time.Hour)
	restore := setNow(func() time.Time { return clock })
	defer restore()

	stats, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if len(stats.Errors) == 0 {
		t.Fatalf("crashed apply should surface a non-fatal error")
	}
	if got := windowTotal(t, st.fakeStore, time.Time{}, time.Time{}); got != 0 {
		t.Fatalf("crashed cycle persisted %d tokens, want 0 (atomic rollback)", got)
	}
	if v, _ := st.LastState(ctx, model.ToolHermes, "cell"); v != nil {
		t.Fatalf("crashed cycle advanced state: %+v", v)
	}

	clock = clock.Add(time.Minute)
	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := windowTotal(t, st.fakeStore, time.Time{}, time.Time{}); got != 900_000 {
		t.Fatalf("total=%d want exactly 900,000 (no double count)", got)
	}
	if st.upserts != 0 {
		t.Fatalf("snapshot path called UpsertState %d times; state must only advance inside ApplySnapshot", st.upserts)
	}
}

// countingStore wraps fakeStore and counts ApplySnapshot calls, so tests can
// prove the zero-delta path skips the write entirely.
type countingStore struct {
	*fakeStore
	applies int
}

func (s *countingStore) ApplySnapshot(ctx context.Context, events []model.UsageEvent, st model.AggregateSnapshot, cp *model.SourceCheckpoint) (int, error) {
	s.applies++
	return s.fakeStore.ApplySnapshot(ctx, events, st, cp)
}

// TestSnapshotZeroDeltaSkipsStateWrite: an unchanged aggregate cell must not
// rewrite its state row every cycle — the cycle skips ApplySnapshot outright
// and resumes writing when the counters move again.
func TestSnapshotZeroDeltaSkipsStateWrite(t *testing.T) {
	snap := func(total int64) adapter.Observation {
		return adapter.Observation{Snapshots: []model.AggregateSnapshot{{
			Tool: model.ToolHermes, Key: "cell", SessionID: "cell",
			InputTokens: total, TotalTokens: total,
		}}}
	}
	ad := &fakeAdapter{
		id: model.ToolHermes, class: model.Aggregate,
		emit: func(call int) adapter.Observation {
			if call < 2 {
				return snap(900_000) // cycle 2 repeats cycle 1's counters
			}
			return snap(1_000_000)
		},
	}
	reg := adapter.NewRegistry(ad)
	st := &countingStore{fakeStore: newFakeStore()}
	ctx := context.Background()

	clock := refDay.Add(6 * time.Hour)
	restore := setNow(func() time.Time { return clock })
	defer restore()

	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if st.applies != 1 {
		t.Fatalf("cycle 1 ApplySnapshot calls = %d, want 1", st.applies)
	}
	stored := st.state[model.ToolHermes+"|cell"]

	clock = clock.Add(time.Minute)
	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if st.applies != 1 {
		t.Fatalf("zero-delta cycle called ApplySnapshot (calls = %d, want still 1)", st.applies)
	}
	if got := st.state[model.ToolHermes+"|cell"]; got != stored {
		t.Fatalf("zero-delta cycle rewrote state: %+v -> %+v", stored, got)
	}
	if got := windowTotal(t, st.fakeStore, time.Time{}, time.Time{}); got != 900_000 {
		t.Fatalf("after zero-delta cycle total = %d, want 900,000", got)
	}

	// Counters move again: the write path resumes and the delta materialises.
	clock = clock.Add(time.Minute)
	if _, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if st.applies != 2 {
		t.Fatalf("cycle 3 ApplySnapshot calls = %d, want 2", st.applies)
	}
	if got := windowTotal(t, st.fakeStore, time.Time{}, time.Time{}); got != 1_000_000 {
		t.Fatalf("after growth total = %d, want 1,000,000", got)
	}
}

// TestSnapshotZeroDeltaStillLandsCheckpoint: skipping the state rewrite must
// not also drop a pending source checkpoint, or the source would be re-read
// every cycle forever.
func TestSnapshotZeroDeltaStillLandsCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	cell := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "cell", SessionID: "cell",
		InputTokens: 10, TotalTokens: 10,
	}
	if _, err := st.ApplySnapshot(ctx, nil, cell, nil); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	cp := &model.SourceCheckpoint{Tool: model.ToolHermes, SourcePath: "src", State: "gate"}
	n, advanced, err := storeSnapshot(ctx, st, cell, refDay.Add(6*time.Hour), cp, nil)
	if err != nil || n != 0 || !advanced {
		t.Fatalf("storeSnapshot n=%d advanced=%v err=%v, want 0,true,nil", n, advanced, err)
	}
	got, err := st.Checkpoint(ctx, model.ToolHermes, "src")
	if err != nil || got == nil || got.State != "gate" {
		t.Fatalf("checkpoint did not land on zero-delta cycle: %+v (err=%v)", got, err)
	}
	if len(st.events) != 0 {
		t.Fatalf("zero-delta cycle stored %d events, want 0", len(st.events))
	}
	if !st.state[model.ToolHermes+"|cell"].ObservedTime.IsZero() {
		t.Fatalf("zero-delta cycle rewrote the state row")
	}
}

// ---------------------------------------------------------------------------
// (f) AllFailed: the exit-code contract for `once`.
// ---------------------------------------------------------------------------

func TestRunCycleAllFailed(t *testing.T) {
	bad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		collectErr: errors.New("boom"),
		emit:       func(int) adapter.Observation { return adapter.Observation{} },
	}
	stats, err := RunOnce(context.Background(), adapter.NewRegistry(bad), newFakeStore(), adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if stats.SourcesFailed != 1 {
		t.Fatalf("SourcesFailed=%d want 1", stats.SourcesFailed)
	}
	if !stats.AllFailed() {
		t.Fatalf("every source failed but AllFailed=false: %+v", stats)
	}
}

func TestRunCyclePartialFailureNotAllFailed(t *testing.T) {
	bad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		collectErr: errors.New("boom"),
		emit:       func(int) adapter.Observation { return adapter.Observation{} },
	}
	good := &fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Events: []model.UsageEvent{{
				Tool: model.ToolClaudeCode, EventTime: refDay.Add(6 * time.Hour),
				TotalTokens: 10, DedupKey: "cc|partial-1", Kind: model.KindUsage,
			}}}
		},
	}
	stats, err := RunOnce(context.Background(), adapter.NewRegistry(bad, good), newFakeStore(), adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if len(stats.Errors) == 0 {
		t.Fatalf("expected the bad source's error to stay visible")
	}
	if stats.AllFailed() {
		t.Fatalf("partial failure reported as all-failed: %+v", stats)
	}
}

func TestRunCycleNoErrorsNotAllFailed(t *testing.T) {
	var stats CycleStats
	if stats.AllFailed() {
		t.Fatalf("empty cycle (no sources, no errors) must not report all-failed")
	}
}

// ---------------------------------------------------------------------------
// (g) truncation: a cancelled cycle must not read as a completed one.
// ---------------------------------------------------------------------------

// TestRunCycleCanceledMarksStatsTruncated: cancellation lands while the first
// adapter is being read, so the second adapter never runs. The returned counts
// cover one adapter out of two and must be flagged partial — without the flag
// a caller logs "adapters=1 sources=1" in the exact shape of a finished cycle.
func TestRunCycleCanceledMarksStatsTruncated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			cancel() // the daemon's stop signal arrives mid-cycle
			return adapter.Observation{}
		},
	}
	second := &fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{} },
	}

	stats, err := RunOnce(ctx, adapter.NewRegistry(first, second), newFakeStore(), adapter.DiscoverConfig{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cycle error = %v, want context.Canceled", err)
	}
	if !stats.Canceled {
		t.Fatalf("cycle truncated after %d/2 adapters but stats.Canceled=false: %+v", stats.Adapters, stats)
	}
	if stats.Adapters != 1 {
		t.Fatalf("Adapters=%d, want 1 (the second adapter must not have run)", stats.Adapters)
	}
}

// TestRunCycleCompleteNotCanceled pins the other half: a cycle that ran to the
// end reports Canceled=false, so the flag distinguishes the two.
func TestRunCycleCompleteNotCanceled(t *testing.T) {
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{} },
	}
	stats, err := RunOnce(context.Background(), adapter.NewRegistry(ad), newFakeStore(), adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if stats.Canceled {
		t.Fatalf("completed cycle reported as canceled: %+v", stats)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", max)
}

// setNow overrides the collector's observation clock and returns a restore func.
func setNow(fn func() time.Time) func() {
	prev := nowFn
	nowFn = fn
	return func() { nowFn = prev }
}

// TestSyntheticEventKeepsRecordTime: an adapter-populated snapshot timestamp
// becomes the synthetic event's EventTime (a downtime gap's delta must not
// land as a spike at the restart second); the cycle instant is only the
// fallback and always stamps ObservedTime.
func TestSyntheticEventKeepsRecordTime(t *testing.T) {
	recorded := refDay.Add(2 * time.Hour)
	cycle := refDay.Add(9 * time.Hour)
	restore := setNow(func() time.Time { return cycle })
	defer restore()

	ad := &fakeAdapter{
		id: model.ToolHermes, class: model.Aggregate,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Snapshots: []model.AggregateSnapshot{
				{Tool: model.ToolHermes, Key: "with-ts", SessionID: "with-ts",
					ObservedTime: recorded, InputTokens: 10, TotalTokens: 10},
				{Tool: model.ToolHermes, Key: "no-ts", SessionID: "no-ts",
					InputTokens: 5, TotalTokens: 5},
			}}
		},
	}
	st := newFakeStore()
	if _, err := RunOnce(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	evs, _ := st.ListEvents(context.Background(), store.Filter{})
	if len(evs) != 2 {
		t.Fatalf("stored %d events, want 2", len(evs))
	}
	byKey := map[string]model.UsageEvent{}
	for _, e := range evs {
		byKey[e.SessionID] = e
	}
	if got := byKey["with-ts"].EventTime; !got.Equal(recorded) {
		t.Errorf("EventTime = %v, want record time %v", got, recorded)
	}
	if got := byKey["with-ts"].ObservedTime; !got.Equal(cycle) {
		t.Errorf("ObservedTime = %v, want cycle time %v", got, cycle)
	}
	if got := byKey["no-ts"].EventTime; !got.Equal(cycle) {
		t.Errorf("fallback EventTime = %v, want cycle time %v", got, cycle)
	}
}
