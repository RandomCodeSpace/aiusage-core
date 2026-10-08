package collect

import (
	"context"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
)

// minInterval is the last-resort floor under Run's ticker and pass spacing:
// zero or negative intervals get one pass per second, not a tight loop that
// re-reads every transcript on this machine as fast as the disk allows. It is a
// guard, not a policy - a caller with an opinion about cadence states it, and
// this CLI's own config clamps to [60,1800]s long before the value reaches here.
const minInterval = time.Second

// Run collects until ctx is cancelled: one pass immediately, then on its
// background ticker or a WithTrigger request. Passes run serially and their
// starts are separated by WithMinInterval, which defaults to one second.
// Pending requests coalesce into one pass. It is the whole of the long-running
// half of this package (issue #72, decision 3), a loop over RunOnce.
//
// WHAT IT DELIBERATELY DOES NOT DO: take a pidfile lock, write a pid, record a
// build identity, watch its own executable, or install a signal handler. Those
// are how one machine supervises one process, and a consumer embedding this
// package has its own answers to all of them - a supervisor, a container, a
// goroutine inside a larger service. Baking this project's answers in would make
// every one of those callers fight them. aiusage's own daemon keeps them, one
// level down in internal/daemon, composing this package the way any other
// consumer would.
//
// The consequence a caller owns: NOTHING here prevents two Runs against one
// database. They would not corrupt it - the ledger is append-only and dedup
// keys collide harmlessly - but two passes reading one aggregate accumulator at
// the same time can each derive the same delta under a different dedup key and
// append it twice. One Run per database.
//
// A per-pass failure is not fatal, because RunOnce's own errors are per-source
// and it is expected to be run on a ticker: the loop reports each pass through
// WithCycleCallback (if one was given) and keeps going. Run returns nil on
// cancellation - being asked to stop is not a failure - so an error from it
// means the loop could not continue at all.
func Run(ctx context.Context, interval time.Duration, reg *adapter.Registry, st Store, dc adapter.DiscoverConfig, opts ...Option) error {
	if interval < minInterval {
		interval = minInterval
	}
	var o cycleOptions
	for _, fn := range opts {
		fn(&o)
	}
	gap := o.minInterval
	if gap < minInterval {
		gap = minInterval
	}
	trigger := o.trigger
	// Fold a buffered burst into the next pass. Bound the drain so a
	// continuous producer cannot postpone collection or cancellation.
	drainTrigger := func() {
		for queued := len(trigger); queued > 0; queued-- {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-trigger:
				if !ok {
					trigger = nil
				}
			default:
				return
			}
		}
	}

	var lastStart time.Time
	pass := func() {
		lastStart = time.Now()
		stats, err := RunOnce(ctx, reg, st, dc, opts...)
		if o.onCycle != nil {
			o.onCycle(stats, err)
		}
	}

	// Immediate first pass, then on the ticker. A collector that waited out a
	// full interval before its first read would report nothing for that long
	// after a restart, and a restart is exactly when a gap needs filling.
	drainTrigger()
	pass()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-trigger:
			if !ok {
				trigger = nil
				continue
			}
		case <-ticker.C:
		}

		if delay := time.Until(lastStart.Add(gap)); delay > 0 {
			timer := time.NewTimer(delay)
		waiting:
			for {
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case _, ok := <-trigger:
					if !ok {
						trigger = nil
					}
				case <-ticker.C:
				case <-timer.C:
					break waiting
				}
			}
		}

		drainTrigger()
		select {
		case <-ticker.C:
		default:
		}
		if ctx.Err() != nil {
			return nil
		}
		pass()
	}
}
