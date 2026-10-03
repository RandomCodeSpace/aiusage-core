package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

const (
	priceSyncRevisionKey = "price_sync_revision"
	priceSyncCursorKey   = "price_sync_cursor"
	priceSyncBatchSize   = 512
	usageNoUpdateSQL     = `CREATE TRIGGER trg_events_no_update
BEFORE UPDATE ON usage_events
BEGIN SELECT RAISE(ABORT, 'usage_events is append-only: UPDATE forbidden'); END`
)

// SyncUnpriced fills at most 512 NULL costs from one revision of the pricing
// tables. Priced rows are never selected or overwritten. The persisted cursor
// avoids rescanning old unknown models until the revision changes; later IDs
// remain eligible. The bound covers candidates and writes, not SQLite's scan
// through intervening priced rows. Raw and transient cache-TTL data are absent.
//
// This is the sole historical update exception. The strict update trigger is
// removed and restored inside the same write transaction as the guarded price
// updates, derived rollup changes and cursor. Failure rolls all of them back.
func (l *Ledger) SyncUnpriced(ctx context.Context, revision string,
	price func(model.UsageEvent) (int64, string, bool)) (int, error) {
	if revision == "" || price == nil {
		return 0, nil
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin price sync: %w", err)
	}
	defer tx.Rollback()

	var oldRevision, cursorValue string
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT value FROM schema_meta WHERE key=?),''),
		COALESCE((SELECT value FROM schema_meta WHERE key=?),'0')`,
		priceSyncRevisionKey, priceSyncCursorKey).Scan(&oldRevision, &cursorValue); err != nil {
		return 0, fmt.Errorf("store: read price sync state: %w", err)
	}
	var cursor int64
	if oldRevision == revision {
		cursor, err = strconv.ParseInt(cursorValue, 10, 64)
		if err != nil || cursor < 0 {
			return 0, fmt.Errorf("store: invalid price sync cursor %q", cursorValue)
		}
	}
	var maxID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM usage_events`).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("store: read price sync high-water: %w", err)
	}
	if oldRevision == revision && cursor == maxID {
		return 0, nil
	}
	if cursor > maxID {
		cursor = 0
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+eventColumnsNoRaw+`
		FROM usage_events WHERE cost_micro_usd IS NULL AND id>? AND id<=?
		ORDER BY id LIMIT ?`, cursor, maxID, priceSyncBatchSize)
	if err != nil {
		return 0, fmt.Errorf("store: query unpriced batch: %w", err)
	}
	var events []model.UsageEvent
	for rows.Next() {
		e, scanErr := scanUsageEvent(rows, false)
		if scanErr != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan unpriced event: %w", scanErr)
		}
		events = append(events, e)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return 0, fmt.Errorf("store: read unpriced batch: %w", readErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("store: close unpriced batch: %w", closeErr)
	}
	if len(events) == priceSyncBatchSize {
		cursor = events[len(events)-1].ID
	} else {
		cursor = maxID
	}

	var roll rollupDelta
	var priced []model.UsageEvent
	for _, e := range events {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		cost, source, ok := price(e)
		if !ok || cost < 0 || (cost == 0 && !strings.HasSuffix(source, "+free")) || model.PriceProvenance(source) != model.CostComputed {
			continue
		}
		roll.adjust(e, e.ID, -1)
		e.SetCost(cost, source)
		roll.add(e, e.ID)
		priced = append(priced, e)
	}
	if len(priced) > 0 {
		var triggerSQL string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
			WHERE type='trigger' AND name='trg_events_no_update' AND tbl_name='usage_events'`).Scan(&triggerSQL); err != nil {
			return 0, fmt.Errorf("store: read price sync update guard: %w", err)
		}
		normalize := func(s string) string {
			s = strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(s), ";")), " ")
			return strings.Replace(s, "CREATE TRIGGER IF NOT EXISTS", "CREATE TRIGGER", 1)
		}
		if normalize(triggerSQL) != normalize(usageNoUpdateSQL) {
			return 0, fmt.Errorf("store: price sync requires the canonical usage update guard")
		}
		var mark string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key=?`, rollupWatermarkKey).Scan(&mark); err != nil {
			return 0, fmt.Errorf("store: read price sync rollup watermark: %w", err)
		}
		if mark != strconv.FormatInt(maxID, 10) {
			return 0, fmt.Errorf("store: price sync requires a current usage rollup")
		}
		if err := checkPriceSyncRollup(ctx, tx, &roll); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER trg_events_no_update`); err != nil {
			return 0, fmt.Errorf("store: open price sync update guard: %w", err)
		}
		for _, e := range priced {
			result, err := tx.ExecContext(ctx, `UPDATE usage_events SET cost_micro_usd=?, price_source=?
				WHERE id=? AND cost_micro_usd IS NULL`, *e.CostMicroUSD, e.PriceSource, e.ID)
			if err != nil {
				return 0, fmt.Errorf("store: fill unpriced event: %w", err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("store: count price sync update: %w", err)
			}
			if n != 1 {
				return 0, fmt.Errorf("store: unpriced event changed during price sync")
			}
		}
		if err := roll.apply(ctx, tx); err != nil {
			return 0, err
		}
		for k, c := range roll.cells {
			if c.events < 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM usage_rollup WHERE `+priceSyncRollupKeySQL+` AND events=0`, priceSyncRollupArgs(k)...); err != nil {
					return 0, fmt.Errorf("store: remove emptied unpriced rollup cell: %w", err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, triggerSQL); err != nil {
			return 0, fmt.Errorf("store: restore price sync update guard: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_meta(key,value) VALUES(?,?),(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		priceSyncRevisionKey, revision, priceSyncCursorKey, strconv.FormatInt(cursor, 10)); err != nil {
		return 0, fmt.Errorf("store: save price sync state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit price sync: %w", err)
	}
	return len(priced), nil
}

const priceSyncRollupKeySQL = `bucket_start_unix=? AND tool=? AND model=? AND project=?
	AND session_id=? AND provider=? AND service_tier=? AND price_class=?`

func priceSyncRollupArgs(k rollupKey) []any {
	return []any{k.bucket, k.tool, k.model, k.project, k.session, k.provider, k.serviceTier, k.priceClass}
}

// Refuse to subtract from a missing/stale cell instead of creating negative
// derived measures. A fully consumed cell must become exactly empty.
func checkPriceSyncRollup(ctx context.Context, tx *sql.Tx, roll *rollupDelta) error {
	for k, c := range roll.cells {
		if c.events >= 0 {
			continue
		}
		var old rollupCell
		err := tx.QueryRowContext(ctx, `SELECT input_tokens, output_tokens, cache_creation_tokens,
			cache_read_tokens, reasoning_tokens, total_tokens, events, cost_micro_usd, unpriced_events
			FROM usage_rollup WHERE `+priceSyncRollupKeySQL, priceSyncRollupArgs(k)...).Scan(
			&old.input, &old.output, &old.cacheCreation, &old.cacheRead, &old.reasoning,
			&old.total, &old.events, &old.costMicroUSD, &old.unpriced)
		if err != nil {
			return fmt.Errorf("store: read unpriced rollup cell: %w", err)
		}
		if old.costMicroUSD != 0 || old.unpriced != old.events {
			return fmt.Errorf("store: inconsistent unpriced rollup cell")
		}
		for _, pair := range [][2]int64{
			{old.input, c.input}, {old.output, c.output}, {old.cacheCreation, c.cacheCreation},
			{old.cacheRead, c.cacheRead}, {old.reasoning, c.reasoning}, {old.total, c.total},
			{old.events, c.events}, {old.unpriced, c.unpriced},
		} {
			if pair[0]+pair[1] < 0 || (old.events+c.events == 0 && pair[0]+pair[1] != 0) {
				return fmt.Errorf("store: insufficient unpriced rollup cell")
			}
		}
	}
	return nil
}
