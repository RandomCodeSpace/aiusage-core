package collect

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
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

// startScheduledRun is used inside a synctest bubble. Cleanup also verifies
// Run's nil-on-cancellation contract, including tests that cancel it early.
func startScheduledRun(t *testing.T, interval time.Duration, ad *fakeAdapter, opts ...Option) (*recorder, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	var rec recorder
	opts = append(opts, WithCycleCallback(rec.record))
	go func() {
		done <- Run(ctx, interval, adapter.NewRegistry(ad), newFakeStore(), adapter.DiscoverConfig{}, opts...)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run returned %v on cancel, want nil", err)
			}
		})
	}
	t.Cleanup(stop)
	synctest.Wait()
	return &rec, stop
}

func emptyRunAdapter() *fakeAdapter {
	return &fakeAdapter{
		id:   model.ToolCodex,
		emit: func(int) adapter.Observation { return adapter.Observation{} },
	}
}

func TestRunPrequeuedTriggersCoalesceWithStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{}, 8)
		for range cap(trigger) {
			trigger <- struct{}{}
		}
		rec, _ := startScheduledRun(t, time.Hour, emptyRunAdapter(),
			WithTrigger(trigger), WithMinInterval(time.Minute))
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got := rec.passes(); got != 1 || len(trigger) != 0 {
			t.Fatalf("passes = %d, pending requests = %d; want one startup pass and no requests", got, len(trigger))
		}
	})
}

func TestRunTriggerCoalescesAndRespectsMinInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{}, 8)
		rec, _ := startScheduledRun(t, 15*time.Minute, emptyRunAdapter(),
			WithTrigger(trigger), WithMinInterval(time.Minute))
		if got := rec.passes(); got != 1 {
			t.Fatalf("startup passes = %d, want 1", got)
		}
		for range cap(trigger) {
			trigger <- struct{}{}
		}
		synctest.Wait()
		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		if got := rec.passes(); got != 1 {
			t.Fatalf("passes before minimum gap = %d, want 1", got)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("passes at minimum gap = %d, want 2", got)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("passes after burst drained = %d, want 2", got)
		}
		trigger <- struct{}{}
		synctest.Wait()
		if got := rec.passes(); got != 3 {
			t.Fatalf("passes after eligible trigger = %d, want 3", got)
		}
	})
}

func TestRunCoalescesTriggersDuringPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{}, 8)
		release := make(chan struct{})
		var starts []time.Time
		ad := &fakeAdapter{
			id: model.ToolCodex,
			emit: func(call int) adapter.Observation {
				starts = append(starts, time.Now())
				if call == 0 {
					<-release
				}
				return adapter.Observation{}
			},
		}
		rec, _ := startScheduledRun(t, time.Hour, ad,
			WithTrigger(trigger), WithMinInterval(time.Minute))
		for range cap(trigger) {
			trigger <- struct{}{}
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if len(starts) != 1 || rec.passes() != 0 {
			t.Fatal("a second pass started while the startup pass was blocked")
		}
		close(release)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("passes after releasing startup = %d, want 2", got)
		}
		if gap := starts[1].Sub(starts[0]); gap != 2*time.Minute {
			t.Fatalf("pass start gap = %s, want 2m", gap)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("passes after buffered burst = %d, want 2", got)
		}
	})
}

func TestRunMinimumSpacingIsBetweenPassStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{}, 1)
		ad := &fakeAdapter{
			id: model.ToolCodex,
			emit: func(call int) adapter.Observation {
				if call == 0 {
					time.Sleep(40 * time.Second)
				}
				return adapter.Observation{}
			},
		}
		rec, _ := startScheduledRun(t, time.Hour, ad,
			WithTrigger(trigger), WithMinInterval(time.Minute))
		time.Sleep(40 * time.Second)
		synctest.Wait()
		trigger <- struct{}{}
		time.Sleep(20*time.Second - time.Nanosecond)
		synctest.Wait()
		if got := rec.passes(); got != 1 {
			t.Fatalf("passes before startup start + 1m = %d, want 1", got)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("passes at startup start + 1m = %d, want 2", got)
		}
	})
}

func TestRunBackgroundContinuesAfterTriggers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{}, 1)
		rec, _ := startScheduledRun(t, 5*time.Minute, emptyRunAdapter(),
			WithTrigger(trigger), WithMinInterval(time.Minute))
		trigger <- struct{}{}
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("passes after trigger = %d, want 2", got)
		}
		time.Sleep(4 * time.Minute)
		synctest.Wait()
		if got := rec.passes(); got != 3 {
			t.Fatalf("passes at original background tick = %d, want 3", got)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if got := rec.passes(); got != 4 {
			t.Fatalf("passes at next background tick = %d, want 4", got)
		}
	})
}

func TestRunMinimumIntervalAlsoSpacesTickerPasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec, _ := startScheduledRun(t, time.Second, emptyRunAdapter(),
			WithTrigger(nil), WithMinInterval(time.Minute))
		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		if got := rec.passes(); got != 1 {
			t.Fatalf("ticker passes before minimum gap = %d, want 1", got)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("ticker passes at minimum gap = %d, want 2", got)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := rec.passes(); got != 3 {
			t.Fatalf("ticker passes at next minimum gap = %d, want 3", got)
		}
	})
}

func TestRunClosedTriggerDoesNotCollect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{})
		close(trigger)
		rec, _ := startScheduledRun(t, 10*time.Second, emptyRunAdapter(), WithTrigger(trigger))
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if got := rec.passes(); got != 1 {
			t.Fatalf("passes from closed trigger = %d, want 1", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("background passes after trigger closed = %d, want 2", got)
		}
	})
}

func TestRunTriggerClosurePreservesPendingPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{}, 1)
		rec, _ := startScheduledRun(t, time.Hour, emptyRunAdapter(),
			WithTrigger(trigger), WithMinInterval(time.Minute))
		trigger <- struct{}{}
		synctest.Wait()
		close(trigger)
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := rec.passes(); got != 2 {
			t.Fatalf("passes after pending trigger closed = %d, want 2", got)
		}
	})
}

func TestRunCancelWhileWaitingForMinimumGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{}, 1)
		rec, stop := startScheduledRun(t, time.Hour, emptyRunAdapter(),
			WithTrigger(trigger), WithMinInterval(time.Minute))
		trigger <- struct{}{}
		synctest.Wait()
		time.Sleep(30 * time.Second)
		stop()
		if got := rec.passes(); got != 1 {
			t.Fatalf("passes after cancelling pending trigger = %d, want 1", got)
		}
	})
}

func TestRunCancelDuringPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		trigger := make(chan struct{}, 1)
		ad := &fakeAdapter{
			id: model.ToolCodex,
			emit: func(int) adapter.Observation {
				<-ctx.Done()
				return adapter.Observation{}
			},
		}
		var rec recorder
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, time.Hour, adapter.NewRegistry(ad), newFakeStore(), adapter.DiscoverConfig{},
				WithTrigger(trigger), WithCycleCallback(rec.record))
		}()
		synctest.Wait()
		trigger <- struct{}{}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run returned %v on cancellation during a pass, want nil", err)
		}
		if rec.passes() != 1 || !rec.stats[0].Canceled || rec.errs[0] != context.Canceled {
			t.Fatalf("cancelled callback = %+v, %v; want one cancelled pass", rec.stats, rec.errs)
		}
	})
}

func TestRunContinuousTriggersAllowCollectionAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := make(chan struct{}, 64)
		release := make(chan struct{})
		collected := make(chan struct{})
		ad := &fakeAdapter{
			id: model.ToolCodex,
			emit: func(call int) adapter.Observation {
				if call == 0 {
					<-release
				} else if call == 1 {
					close(collected)
				}
				return adapter.Observation{}
			},
		}
		_, stop := startScheduledRun(t, time.Hour, ad, WithTrigger(trigger))
		time.Sleep(2 * time.Second)
		stopProducer := make(chan struct{})
		defer close(stopProducer)
		started := make(chan struct{})
		go func() {
			trigger <- struct{}{}
			close(started)
			for {
				select {
				case trigger <- struct{}{}:
				case <-stopProducer:
					return
				}
			}
		}()
		<-started
		close(release)
		<-collected
		stop()
	})
}

func TestRunTriggerMinimumIntervalFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"default", nil},
		{"zero", []Option{WithMinInterval(0)}},
		{"negative", []Option{WithMinInterval(-time.Second)}},
		{"subsecond", []Option{WithMinInterval(time.Millisecond)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				trigger := make(chan struct{}, 1)
				opts := append(tc.opts, WithTrigger(trigger))
				rec, _ := startScheduledRun(t, time.Hour, emptyRunAdapter(), opts...)
				trigger <- struct{}{}
				time.Sleep(time.Second - time.Nanosecond)
				synctest.Wait()
				if got := rec.passes(); got != 1 {
					t.Fatalf("passes before one-second floor = %d, want 1", got)
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if got := rec.passes(); got != 2 {
					t.Fatalf("passes at one-second floor = %d, want 2", got)
				}
			})
		})
	}
}

func TestRunOnceIgnoresSchedulingOptions(t *testing.T) {
	trigger := make(chan struct{}, 1)
	trigger <- struct{}{}
	called := false
	stats, err := RunOnce(t.Context(), adapter.NewRegistry(emptyRunAdapter()), newFakeStore(), adapter.DiscoverConfig{},
		WithTrigger(trigger), WithMinInterval(time.Hour),
		WithCycleCallback(func(CycleStats, error) { called = true }))
	if err != nil || stats.Sources != 1 {
		t.Fatalf("RunOnce = %+v, %v; want one clean source", stats, err)
	}
	if called || len(trigger) != 1 {
		t.Fatal("RunOnce consumed a trigger or called the cycle callback")
	}
}
