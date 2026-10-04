package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// ReconcileClaudeBatch explicitly opts out of immutable accounting for growing
// Claude usage records. New observations have ApplyBatch's insertion semantics.
// For an existing Claude usage identity, a strictly larger total may replace
// token counters and cost, provided every counter is nondecreasing and the
// model, provider, tier, session, project and request/message identities agree.
// Older/equal totals are ignored. Conflicting growth rolls back the whole batch.
// IDs, timestamps, raw payloads and other recorded metadata are preserved.
//
// The supplied cost must price the complete replacement observation. A nil cost
// makes the revised row unpriced; an earlier partial cost is never carried over.
// Existing vendor costs cannot be replaced with computed costs. Usage, rollup,
// activity, turn contexts and checkpoint commit together. Returned counts only
// count inserts, not revised usage. Replays do not insert extra turns or calls.
//
// The rollup must be current; call EnsureRollup before reconciliation. Generic
// ApplyBatch/ApplyEvents/InsertEvents remain append-only. Single-writer ownership
// is required as for collection. This does not repair deleted source records,
// non-monotonic revisions or previously stamped unrelated pricing errors.
func (l *Ledger) ReconcileClaudeBatch(ctx context.Context, b ObservationBatch) (Applied, error) {
	for _, e := range b.Events {
		if e.Tool != model.ToolClaudeCode || (e.Kind != "" && e.Kind != model.KindUsage) {
			return Applied{}, fmt.Errorf("store: Claude reconciliation only accepts Claude usage events")
		}
	}
	return l.applyBatch(ctx, b, true)
}

func reconcileClaudeTx(ctx context.Context, tx *sql.Tx, events []model.UsageEvent) error {
	var roll rollupDelta
	guardOpen := false
	for _, incoming := range events {
		rows, err := tx.QueryContext(ctx, `SELECT `+eventColumnsNoRaw+` FROM usage_events WHERE dedup_key=?`, incoming.DedupKey)
		if err != nil {
			return err
		}
		if !rows.Next() {
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			continue // An invalid new row may have been skipped by insertion.
		}
		old, err := scanUsageEvent(rows, false)
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if incoming.TotalTokens <= old.TotalTokens {
			continue
		}
		if old.Tool != model.ToolClaudeCode || old.Kind != model.KindUsage ||
			old.Model != incoming.Model || old.Provider != incoming.Provider ||
			old.ServiceTier != incoming.ServiceTier || old.SessionID != incoming.SessionID ||
			old.Project != incoming.Project || old.RequestID != incoming.RequestID || old.MessageID != incoming.MessageID {
			return fmt.Errorf("store: conflicting Claude identity %q", incoming.DedupKey)
		}
		for _, pair := range [][2]int64{
			{old.InputTokens, incoming.InputTokens}, {old.OutputTokens, incoming.OutputTokens},
			{old.CacheCreationTokens, incoming.CacheCreationTokens}, {old.CacheReadTokens, incoming.CacheReadTokens},
			{old.ReasoningTokens, incoming.ReasoningTokens},
		} {
			if pair[1] < pair[0] {
				return fmt.Errorf("store: non-monotonic Claude counters for %q", incoming.DedupKey)
			}
		}
		if model.PriceProvenance(old.PriceSource) == model.CostVendor &&
			(incoming.CostMicroUSD == nil || model.PriceProvenance(incoming.PriceSource) != model.CostVendor) {
			return fmt.Errorf("store: cannot replace vendor cost for %q with an estimate", incoming.DedupKey)
		}
		if !guardOpen {
			if err := openUsageUpdateGuard(ctx, tx); err != nil {
				return err
			}
			guardOpen = true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE usage_events SET input_tokens=?, output_tokens=?,
			cache_creation_tokens=?, cache_read_tokens=?, reasoning_tokens=?, total_tokens=?,
			cost_micro_usd=?, price_source=? WHERE id=?`, incoming.InputTokens, incoming.OutputTokens,
			incoming.CacheCreationTokens, incoming.CacheReadTokens, incoming.ReasoningTokens, incoming.TotalTokens,
			nullCost(incoming), incoming.PriceSource, old.ID); err != nil {
			return fmt.Errorf("store: reconcile Claude usage: %w", err)
		}
		roll.adjust(old, old.ID, -1)
		// Preserve the recorded dimensions and timestamps in the rollup as well.
		updated := old
		updated.InputTokens, updated.OutputTokens = incoming.InputTokens, incoming.OutputTokens
		updated.CacheCreationTokens, updated.CacheReadTokens = incoming.CacheCreationTokens, incoming.CacheReadTokens
		updated.ReasoningTokens, updated.TotalTokens = incoming.ReasoningTokens, incoming.TotalTokens
		updated.CostMicroUSD, updated.PriceSource = incoming.CostMicroUSD, incoming.PriceSource
		roll.add(updated, old.ID)
	}
	if !guardOpen {
		return nil
	}
	if err := roll.apply(ctx, tx); err != nil {
		return err
	}
	for k := range roll.cells {
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_rollup WHERE `+priceSyncRollupKeySQL+` AND events=0`, priceSyncRollupArgs(k)...); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, usageNoUpdateSQL)
	return err
}
