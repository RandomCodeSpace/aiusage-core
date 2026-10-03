package model

import "time"

// CodeChange is the latest line-count snapshot a harness reports for one
// change identity, such as a user turn. It is not a token usage event or the
// final repository diff. A later snapshot may decrease after an undo.
// Counts and identity only: no patch text, file contents or tool arguments.
type CodeChange struct {
	Tool      string
	ChangeID  string
	SessionID string
	Project   string
	// Known distinguishes observed counts, including zero, from unavailable
	// data. Unknown snapshots have zero counts and replace older known values.
	Known        bool
	LinesAdded   int64
	LinesRemoved int64
	UpdatedAt    time.Time
	ObservedTime time.Time
}
