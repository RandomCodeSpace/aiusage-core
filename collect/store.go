package collect

import (
	"context"

	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// Store is what a collection pass needs from a ledger, and nothing else: six
// methods, all of which a *store.Ledger already satisfies.
//
// It is declared HERE, at the point of use, rather than exported by package
// store (issue #72, decision 8). A fat interface beside the implementation has
// to name every method the implementation has, so it grows whenever the store
// grows and every fake in the module breaks on a method the fake's own test
// never calls. Declared here it names what THIS package consumes: adding a
// query to the store cannot break the collector's tests, and a caller wiring a
// different ledger in has six methods to write instead of thirty.
//
// The contracts these carry are the store's, not this package's, and they are
// not restated below - see store.Ledger for the single-transaction and
// idempotence rules each one promises. What matters here is which of them the
// collector depends on:
//
//   - EnsureRollup, once per pass, before anything is appended.
//   - Checkpoint, per source, for the incremental gate.
//   - ApplyBatch, per source observation: the one transaction that carries
//     events, activity, turn contexts and the checkpoint together.
//   - LastState / ApplySnapshot, per aggregate cell, for the
//     monotonic-with-reset delta and its baseline.
//   - ApplyEvents, for the checkpoint-only write of an unchanged aggregate
//     cell.
type Store interface {
	EnsureRollup(ctx context.Context) (bool, error)
	Checkpoint(ctx context.Context, tool, sourcePath string) (*model.SourceCheckpoint, error)
	ApplyBatch(ctx context.Context, b store.ObservationBatch) (store.Applied, error)
	ApplyEvents(ctx context.Context, events []model.UsageEvent, cp *model.SourceCheckpoint) (int, error)
	LastState(ctx context.Context, tool, key string) (*model.AggregateSnapshot, error)
	ApplySnapshot(ctx context.Context, events []model.UsageEvent, state model.AggregateSnapshot, cp *model.SourceCheckpoint) (int, error)
}

// The full handle satisfies it. A read handle deliberately does not, and cannot
// be made to: the writes are absent from its type.
var _ Store = (*store.Ledger)(nil)
