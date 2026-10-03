package collect

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// recorder collects what WithCycleCallback was handed, under a mutex because
// the callback runs on Run's goroutine and the test reads from its own.
type recorder struct {
	mu    sync.Mutex
	stats []CycleStats
	errs  []error
}

func (r *recorder) record(s CycleStats, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats = append(r.stats, s)
	r.errs = append(r.errs, err)
}

func (r *recorder) passes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stats)
}

// TestRunCollectsImmediatelyThenOnTheTicker pins the two properties a caller
// depends on: the FIRST pass does not wait out an interval (a collector that
// did would report nothing for a whole cadence after every restart, which is
// precisely when a gap needs filling), and the loop keeps going afterwards.
func TestRunCollectsImmediatelyThenOnTheTicker(t *testing.T) {
	ev := model.UsageEvent{
		Tool: model.ToolCodex, EventTime: refDay, TotalTokens: 7,
		DedupKey: "codex|run-1", Kind: model.KindUsage,
	}
	var call int
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			call++
			e := ev
			e.DedupKey = "codex|run-" + strconv.Itoa(call)
			return adapter.Observation{Events: []model.UsageEvent{e}}
		},
	}
	st := newFakeStore()
	var rec recorder

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, 10*time.Millisecond, adapter.NewRegistry(ad), st, adapter.DiscoverConfig{},
			WithCycleCallback(rec.record))
	}()

	// The first pass must land well inside one interval of the real floor, so
	// this cannot pass by waiting for a tick.
	waitFor(t, time.Second, func() bool { return rec.passes() >= 1 })
	waitFor(t, 3*time.Second, func() bool { return rec.passes() >= 3 })

	cancel()
	select {
	case err := <-done:
		// Being asked to stop is not a failure.
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if n := len(st.events); n < 3 {
		t.Fatalf("stored %d events over at least 3 passes; each pass appends one", n)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, err := range rec.errs {
		if err != nil {
			t.Errorf("pass %d reported %v; a clean pass reports nil", i, err)
		}
	}
	if rec.stats[0].Sources != 1 {
		t.Errorf("first pass saw %d sources, want 1", rec.stats[0].Sources)
	}
}

// TestRunClampsAPathologicalInterval: a zero or negative interval is a caller's
// mistake, and the honest response is the floor rather than a loop that re-reads
// every transcript on the machine as fast as the disk allows. Measuring the
// clamp directly would mean waiting a second, so this asserts the decision the
// loop makes rather than the wall clock: within a tenth of the floor, a clamped
// Run has made exactly its immediate first pass and no tick.
func TestRunClampsAPathologicalInterval(t *testing.T) {
	ad := &fakeAdapter{
		id:   model.ToolCodex,
		emit: func(int) adapter.Observation { return adapter.Observation{} },
	}
	var rec recorder

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, 0, adapter.NewRegistry(ad), newFakeStore(), adapter.DiscoverConfig{},
			WithCycleCallback(rec.record))
	}()

	waitFor(t, time.Second, func() bool { return rec.passes() >= 1 })
	time.Sleep(minInterval / 10)
	if n := rec.passes(); n != 1 {
		t.Fatalf("%d passes within a tenth of the %s floor; interval 0 was not clamped", n, minInterval)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v on cancel, want nil", err)
	}
}
