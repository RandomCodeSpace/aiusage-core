package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// collectCodeChanges reads only count scalars from user-message snapshots.
// OpenCode v1.18.29 session/summary.ts rewrites summary.diffs asynchronously
// after each assistant step. Session-level summary counters are placeholders.
// These are recorded changes between producer snapshots, not a final session
// diff or proof that every changed line was authored by the agent.
func collectCodeChanges(ctx context.Context, tx *sql.Tx) ([]model.CodeChange, error) {
	rows, err := tx.QueryContext(ctx, `SELECT
		m.id, m.session_id, s.directory, m.time_updated,
		json_type(m.data, '$.summary.diffs'), d.type,
		CASE WHEN d.type = 'object' THEN json_type(d.value, '$.additions') END,
		CASE WHEN d.type = 'object' THEN json_extract(d.value, '$.additions') END,
		CASE WHEN d.type = 'object' THEN json_type(d.value, '$.deletions') END,
		CASE WHEN d.type = 'object' THEN json_extract(d.value, '$.deletions') END
		FROM message m JOIN session s ON s.id = m.session_id
		LEFT JOIN json_each(CASE WHEN json_type(m.data, '$.summary.diffs') = 'array'
			THEN json_extract(m.data, '$.summary.diffs') ELSE '[]' END) d
		WHERE CASE WHEN json_valid(m.data) THEN json_extract(m.data, '$.role') END = 'user'
		ORDER BY m.rowid, d.key`)
	if err != nil {
		return nil, fmt.Errorf("opencode: query line counts: %w", err)
	}
	defer rows.Close()

	var changes []model.CodeChange
	var current model.CodeChange
	var invalid bool
	var invalidMessages int
	finish := func() {
		if current.ChangeID == "" {
			return
		}
		if invalid {
			current.Known = false
			current.LinesAdded, current.LinesRemoved = 0, 0
			invalidMessages++
		}
		changes = append(changes, current)
	}
	for rows.Next() {
		var id, session, project string
		var updated int64
		var diffsType, entryType, addedType, removedType sql.NullString
		var added, removed any
		if err := rows.Scan(&id, &session, &project, &updated, &diffsType, &entryType,
			&addedType, &added, &removedType, &removed); err != nil {
			return nil, fmt.Errorf("opencode: read line counts: %w", err)
		}
		if id != current.ChangeID {
			finish()
			current = model.CodeChange{
				Tool: model.ToolOpenCode, ChangeID: id, SessionID: session,
				Project: project, UpdatedAt: time.UnixMilli(updated).UTC(),
			}
			invalid = diffsType.Valid && diffsType.String != "array" && diffsType.String != "null"
		}
		// Absent, null and empty arrays cannot distinguish disabled/missing
		// snapshots from a revert. Emit unknown to replace any prior counts.
		if !entryType.Valid || invalid {
			continue
		}
		a, aOK := added.(int64)
		r, rOK := removed.(int64)
		if entryType.String != "object" || addedType.String != "integer" || removedType.String != "integer" ||
			!aOK || !rOK || a < 0 || r < 0 || math.MaxInt64-current.LinesAdded < a || math.MaxInt64-current.LinesRemoved < r {
			invalid = true
			continue
		}
		current.Known = true
		current.LinesAdded += a
		current.LinesRemoved += r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("opencode: read line counts: %w", err)
	}
	finish()
	if invalidMessages > 0 {
		return changes, fmt.Errorf("opencode: invalid line-count snapshots in %d user messages", invalidMessages)
	}
	return changes, nil
}
