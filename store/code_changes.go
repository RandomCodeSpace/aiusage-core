package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// Code changes are mutable source snapshots, independent of append-only usage.
// Keep this definition in sync with the fresh schema in schema.sql.
const codeChangesTableDDL = `CREATE TABLE IF NOT EXISTS code_changes (
  tool               TEXT    NOT NULL CHECK (tool <> ''),
  change_id          TEXT    NOT NULL CHECK (change_id <> ''),
  session_id         TEXT    NOT NULL CHECK (session_id <> ''),
  project            TEXT    NOT NULL DEFAULT '',
  known              INTEGER NOT NULL CHECK (known IN (0, 1)),
  lines_added        INTEGER NOT NULL CHECK (lines_added >= 0),
  lines_removed      INTEGER NOT NULL CHECK (lines_removed >= 0),
  updated_at_unix_ms INTEGER NOT NULL,
  observed_time_unix INTEGER NOT NULL,
  PRIMARY KEY (tool, change_id),
  CHECK (known = 1 OR (lines_added = 0 AND lines_removed = 0))
) WITHOUT ROWID`

const codeChangesSessionIndexDDL = `CREATE INDEX IF NOT EXISTS idx_code_changes_session ON code_changes(tool, session_id, project)`

// CodeChangeSummary totals the latest reported snapshots for one session.
// UnknownChanges counts snapshots whose line counts are unavailable.
type CodeChangeSummary struct {
	LinesAdded     int64
	LinesRemoved   int64
	KnownChanges   int64
	UnknownChanges int64
}

// SessionCodeChanges requires an exact tool and session. An empty project
// includes every project in that session; a nonempty project is matched exactly.
func (s *Reader) SessionCodeChanges(ctx context.Context, tool, sessionID, project string) (CodeChangeSummary, error) {
	if tool == "" || sessionID == "" {
		return CodeChangeSummary{}, fmt.Errorf("store: code changes require tool and session ID")
	}
	query := `SELECT
		COALESCE(SUM(CASE WHEN known=1 THEN lines_added ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN known=1 THEN lines_removed ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN known=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN known=0 THEN 1 ELSE 0 END),0)
		FROM code_changes WHERE tool=? AND session_id=?`
	args := []any{tool, sessionID}
	if project != "" {
		query += " AND project=?"
		args = append(args, project)
	}
	var out CodeChangeSummary
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&out.LinesAdded, &out.LinesRemoved, &out.KnownChanges, &out.UnknownChanges); err != nil {
		return CodeChangeSummary{}, fmt.Errorf("store: session code changes: %w", err)
	}
	return out, nil
}

func upsertCodeChangesTx(ctx context.Context, tx *sql.Tx, changes []model.CodeChange) (int, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO code_changes (
		tool, change_id, session_id, project, known, lines_added, lines_removed,
		updated_at_unix_ms, observed_time_unix
	) VALUES (?,?,?,?,?,?,?,?,?)
	ON CONFLICT(tool, change_id) DO UPDATE SET
		session_id=excluded.session_id, project=excluded.project, known=excluded.known,
		lines_added=excluded.lines_added, lines_removed=excluded.lines_removed,
		updated_at_unix_ms=excluded.updated_at_unix_ms, observed_time_unix=excluded.observed_time_unix
	WHERE excluded.updated_at_unix_ms >= code_changes.updated_at_unix_ms
		AND (excluded.updated_at_unix_ms <> code_changes.updated_at_unix_ms
			OR excluded.session_id <> code_changes.session_id OR excluded.project <> code_changes.project
			OR excluded.known <> code_changes.known OR excluded.lines_added <> code_changes.lines_added
			OR excluded.lines_removed <> code_changes.lines_removed)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare code changes: %w", err)
	}
	defer stmt.Close()
	var changed int
	for _, c := range changes {
		if c.Tool == "" || c.ChangeID == "" || c.SessionID == "" || c.UpdatedAt.IsZero() {
			return 0, fmt.Errorf("store: code change %q/%q requires tool, change ID, session ID and source update time", c.Tool, c.ChangeID)
		}
		if c.LinesAdded < 0 || c.LinesRemoved < 0 || (!c.Known && (c.LinesAdded != 0 || c.LinesRemoved != 0)) {
			return 0, fmt.Errorf("store: code change %q/%q has invalid line counts", c.Tool, c.ChangeID)
		}
		observed := c.ObservedTime
		if observed.IsZero() {
			observed = c.UpdatedAt
		}
		result, err := stmt.ExecContext(ctx, c.Tool, c.ChangeID, c.SessionID, c.Project, c.Known,
			c.LinesAdded, c.LinesRemoved, c.UpdatedAt.UnixMilli(), observed.Unix())
		if err != nil {
			return 0, fmt.Errorf("store: upsert code change %q/%q: %w", c.Tool, c.ChangeID, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: count code change %q/%q: %w", c.Tool, c.ChangeID, err)
		}
		changed += int(n)
	}
	return changed, nil
}
