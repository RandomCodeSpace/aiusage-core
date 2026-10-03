package store

import (
	"errors"
	"fmt"
)

// errors.go holds the two error contracts a consumer of this package may have
// to BRANCH on, and deliberately nothing else (issue #72, decision 6). A
// sentinel is a promise: once it exists, code outside this module tests for it
// and it can never be retired quietly. So one exists only where a caller can do
// something different because of it, and every other failure stays a plain
// wrapped error carrying a message a human reads.
//
// Neither is new behaviour. Both name a contract this package already had and
// that a caller could previously only detect by matching on message text.

// ErrSchemaNewer reports a database written by a NEWER build of aiusage than
// the one trying to open it. It is the one refusal at open that a caller can
// act on - upgrade the binary, or point at a different file - as against a
// corrupt file, a missing directory or a permission problem, which are all
// "the open failed" and are handled the same way.
//
// The refusal is absolute and is the same one in both directions of the split:
// Open will not migrate a schema it does not understand (an older binary
// stamping a version backwards would silently strip whatever the newer one
// added), and OpenReadOnly will not serve one. The wrapped message names both
// versions, which is what a person reading the error needs; errors.Is is what a
// program needs.
//
// It does NOT cover the opposite case. A database OLDER than the binary is not
// an error at all through Open - it migrates - and through OpenReadOnly it is a
// plain error, because "open it read-write once" is the same instruction
// whatever produced the mismatch.
var ErrSchemaNewer = errors.New("store: database schema is newer than this binary")

// SkippedRow is one row a batch insert refused, and the reason. DedupKey is the
// row's own key, which is what makes the report actionable: it names the row in
// the source the adapter derived it from, and it is empty exactly when the
// missing key IS the reason.
type SkippedRow struct {
	DedupKey string
	Err      error
}

// SkippedRowsError is the PARTIAL SUCCESS this package's batch writes can
// return: a non-nil error accompanied by counts that are still true (issue #72,
// decision 6). It is the one genuinely unusual contract here and so it is the
// one shape with a name.
//
// Why a batch does not fail whole: a row that cannot be inserted (a CHECK
// violation, an empty dedup key) is a PERMANENT property of that row, and the
// source it came from is re-read every cycle. Aborting the transaction would
// discard the good rows beside it and then re-derive exactly the same poison
// row on the next pass, forever - so the bad rows are skipped, the rest commits,
// and the checkpoint advances past all of it.
// Only these known row violations may skip. Every other insert or commit
// error rolls the batch back and returns zero counts. ApplySnapshot also
// rolls back rejected delta rows and never returns SkippedRowsError.
//
// The consequence for a caller is the part worth stating: WHEN THIS ERROR IS
// RETURNED, THE COUNTS RETURNED WITH IT ARE REAL. InsertEvents' int and
// ApplyBatch's Applied describe rows that actually landed, and a caller that
// treats a non-nil error as "nothing happened" will under-report a pass it
// should be reporting in full. Read it with errors.As:
//
//	applied, err := st.ApplyBatch(ctx, batch)
//	var skipped *store.SkippedRowsError
//	if errors.As(err, &skipped) {
//		log.Printf("%d of %d %s rejected; %d events landed",
//			skipped.Skipped(), skipped.Total, skipped.Table, applied.Events)
//	}
//
// Unwrap returns the FIRST row's error, so errors.Is against whatever the
// driver produced still works, on the same row the message names. The rest are
// in Rows.
type SkippedRowsError struct {
	// Table is the SQL table the rows were bound for: usage_events,
	// activity_events or usage_turn_context. One batch write can produce one of
	// these per table, and ApplyBatch returns them in ledger order - the usage
	// skip first, since it is the authoritative half - so a caller that reports
	// one line sees the one that matters.
	Table string
	// Total is how many rows were OFFERED, not how many failed; len(Rows) is
	// the failures. The ratio is the useful thing: 1 of 3000 is a poison row, and
	// 3000 of 3000 is an adapter that has started emitting nonsense.
	Total int
	// Rows lists the skipped rows in the order they were offered.
	Rows []SkippedRow
}

// Skipped is len(Rows), named because "how many were rejected" reads better at
// a call site than a length.
func (e *SkippedRowsError) Skipped() int { return len(e.Rows) }

func (e *SkippedRowsError) Error() string {
	if len(e.Rows) == 0 {
		// Unreachable through this package (the error is only built for a
		// non-empty skip list) and still worth answering honestly rather than
		// panicking in a deferred log line.
		return fmt.Sprintf("store: skipped 0 of %d %s", e.Total, rowNoun(e.Table))
	}
	return fmt.Sprintf("store: skipped %d of %d %s; first: %v",
		len(e.Rows), e.Total, rowNoun(e.Table), e.Rows[0].Err)
}

// Unwrap exposes the first skipped row's cause, so errors.Is reaches the driver
// error or the CHECK violation behind it.
func (e *SkippedRowsError) Unwrap() error {
	if len(e.Rows) == 0 {
		return nil
	}
	return e.Rows[0].Err
}

// rowNoun is what a table's rows are called in a message. It exists so the
// three tables read the way they always did rather than as "3 usage_events
// row(s)".
func rowNoun(table string) string {
	switch table {
	case tableUsageEvents:
		return "event(s)"
	case tableActivityEvents:
		return "activity row(s)"
	case tableTurnContext:
		return "turn context row(s)"
	}
	return "row(s)"
}

// The three tables a batch write can skip rows in. Named constants because they
// reach a caller through SkippedRowsError.Table, which makes them API.
const (
	tableUsageEvents    = "usage_events"
	tableActivityEvents = "activity_events"
	tableTurnContext    = "usage_turn_context"
)

// rowSkips accumulates the skipped rows of one table during an insert loop. The
// insert paths share it so all three report the same shape.
type rowSkips struct {
	table string
	rows  []SkippedRow
}

// add records one skipped row.
func (s *rowSkips) add(dedupKey string, err error) {
	s.rows = append(s.rows, SkippedRow{DedupKey: dedupKey, Err: err})
}

// err returns the batch's partial-success error, or nil when every row was
// accepted. total is how many rows were offered.
func (s *rowSkips) err(total int) error {
	if len(s.rows) == 0 {
		return nil
	}
	return &SkippedRowsError{Table: s.table, Total: total, Rows: s.rows}
}

// isSkippableRowError admits only the extended CHECK result. Expected dedup
// collisions already use ON CONFLICT; all other driver errors fail the batch.
func isSkippableRowError(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code() == 275 // SQLITE_CONSTRAINT_CHECK
}
