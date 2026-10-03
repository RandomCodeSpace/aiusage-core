package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// TestErrSchemaNewerIsMatchable pins the sentinel through BOTH handles. A
// consumer that meets a database from a newer build has exactly one useful
// response - upgrade - and it must not have to match on message text to know
// that is the case it is in.
func TestErrSchemaNewerIsMatchable(t *testing.T) {
	stampNewer := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "usage.db")
		st, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		raw := rawDB(t, path)
		if _, err := raw.Exec(`UPDATE schema_meta SET value=? WHERE key='schema_version'`,
			strconv.Itoa(SchemaVersion+1)); err != nil {
			t.Fatalf("stamp newer: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatalf("close raw: %v", err)
		}
		return path
	}

	t.Run("Open", func(t *testing.T) {
		path := stampNewer(t)
		st, err := Open(path)
		if err == nil {
			st.Close()
			t.Fatalf("expected a refusal opening a newer database")
		}
		if !errors.Is(err, ErrSchemaNewer) {
			t.Fatalf("Open error %v does not match ErrSchemaNewer", err)
		}
		// The message still names both versions: errors.Is is for the program,
		// the text is for the person reading the failure.
		for _, want := range []string{"v" + strconv.Itoa(SchemaVersion+1), "v" + strconv.Itoa(SchemaVersion)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not name %s", err, want)
			}
		}
	})

	t.Run("OpenReadOnly", func(t *testing.T) {
		path := stampNewer(t)
		st, err := OpenReadOnly(path)
		if err == nil {
			st.Close()
			t.Fatalf("expected a refusal opening a newer database read-only")
		}
		if !errors.Is(err, ErrSchemaNewer) {
			t.Fatalf("OpenReadOnly error %v does not match ErrSchemaNewer", err)
		}
	})

	// The OTHER direction must NOT carry it. An older database is migrated by
	// Open and, through the read handle, asks for the opposite action - a
	// caller branching on the sentinel would take the wrong one.
	t.Run("older is not this error", func(t *testing.T) {
		st, err := OpenReadOnly(legacyDB(t, 3))
		if err == nil {
			st.Close()
			t.Fatalf("expected a refusal opening a v3 database read-only")
		}
		if errors.Is(err, ErrSchemaNewer) {
			t.Fatalf("an OLDER database reported ErrSchemaNewer: %v", err)
		}
	})
}

// TestSkippedRowsErrorCarriesTheDetail is the partial-success contract: the
// error is non-nil, the counts beside it are real, and the rows that failed are
// readable through errors.As rather than by parsing the message.
func TestSkippedRowsErrorCarriesTheDetail(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	good := model.UsageEvent{
		Tool: model.ToolCodex, Model: "gpt-5", EventTime: at, ObservedTime: at,
		InputTokens: 10, TotalTokens: 10, DedupKey: "err-good", Kind: model.KindUsage,
	}
	// Two poison rows of the two kinds that exist: no dedup key at all, and a
	// CHECK violation (a negative token count).
	keyless := good
	keyless.DedupKey = ""
	negative := good
	negative.DedupKey = "err-negative"
	negative.InputTokens = -1

	n, err := st.InsertEvents(ctx, []model.UsageEvent{keyless, good, negative})
	if err == nil {
		t.Fatalf("expected a partial-success error, inserted %d", n)
	}
	// The count beside the error is the whole point: the good row landed.
	if n != 1 {
		t.Fatalf("inserted %d, want 1 - the counts must stay true when the error is non-nil", n)
	}

	var skipped *SkippedRowsError
	if !errors.As(err, &skipped) {
		t.Fatalf("error %v is not a *SkippedRowsError", err)
	}
	if skipped.Table != tableUsageEvents {
		t.Errorf("Table = %q, want %q", skipped.Table, tableUsageEvents)
	}
	if skipped.Total != 3 || skipped.Skipped() != 2 {
		t.Fatalf("Total/Skipped = %d/%d, want 3/2", skipped.Total, skipped.Skipped())
	}
	// Rows are in the order they were offered, and each names its own row: the
	// empty key is empty exactly because that is what was wrong with it.
	if skipped.Rows[0].DedupKey != "" {
		t.Errorf("first skipped row key = %q, want empty (that was its fault)", skipped.Rows[0].DedupKey)
	}
	if skipped.Rows[1].DedupKey != "err-negative" {
		t.Errorf("second skipped row key = %q, want err-negative", skipped.Rows[1].DedupKey)
	}
	for i, r := range skipped.Rows {
		if r.Err == nil {
			t.Errorf("skipped row %d carries no cause", i)
		}
	}
	// Unwrap reaches the first row's cause, so errors.Is still works through it.
	if !errors.Is(err, skipped.Rows[0].Err) {
		t.Errorf("errors.Is does not reach the first skipped row's cause")
	}
	// And the message is unchanged, because operators read it.
	if !strings.Contains(err.Error(), "skipped 2 of 3 event(s)") {
		t.Errorf("message = %q, want it to name 2 of 3 event(s)", err)
	}

	// The ledger holds exactly the good row.
	evs, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 1 || evs[0].DedupKey != "err-good" {
		t.Fatalf("stored %d rows (%v), want just the good one", len(evs), evs)
	}
}

// TestSkippedRowsErrorNamesEachTable: all three append-only tables report the
// same shape, and each says which one it was. A caller logging "3 of 4 skipped"
// with no table has told nobody anything.
func TestSkippedRowsErrorNamesEachTable(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

	batch := ObservationBatch{
		Events: []model.UsageEvent{{
			Tool: model.ToolCodex, EventTime: at, ObservedTime: at,
			TotalTokens: 5, DedupKey: "", Kind: model.KindUsage,
		}},
		Activity: []model.ActivityEvent{{
			Tool: model.ToolCodex, Kind: model.ActivityTool, Name: "read",
			EventTime: at, ObservedTime: at, DedupKey: "",
		}},
		TurnContexts: []model.TurnContext{{
			UsageDedupKey: "", Dimension: model.DimensionSkill, Value: "x",
			Tool: model.ToolCodex, EventTime: at, ObservedTime: at,
		}},
	}
	applied, err := st.ApplyBatch(ctx, batch)
	if err == nil {
		t.Fatalf("expected a partial-success error")
	}
	if applied != (Applied{}) {
		t.Fatalf("applied = %+v, want zero - every row was poison", applied)
	}
	// ApplyBatch reports the ledger's skip first: it is the authoritative half,
	// and a caller that logs one line should see that one.
	var skipped *SkippedRowsError
	if !errors.As(err, &skipped) {
		t.Fatalf("error %v is not a *SkippedRowsError", err)
	}
	if skipped.Table != tableUsageEvents {
		t.Fatalf("Table = %q, want the usage ledger reported first", skipped.Table)
	}

	// Each table on its own, so the nouns and the table names are all covered.
	for _, tc := range []struct {
		name  string
		batch ObservationBatch
		table string
		noun  string
	}{
		{"activity", ObservationBatch{Activity: batch.Activity}, tableActivityEvents, "activity row(s)"},
		{"turn context", ObservationBatch{TurnContexts: batch.TurnContexts}, tableTurnContext, "turn context row(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.ApplyBatch(ctx, tc.batch)
			var e *SkippedRowsError
			if !errors.As(err, &e) {
				t.Fatalf("error %v is not a *SkippedRowsError", err)
			}
			if e.Table != tc.table {
				t.Errorf("Table = %q, want %q", e.Table, tc.table)
			}
			if !strings.Contains(e.Error(), tc.noun) {
				t.Errorf("message %q does not call the rows %q", e, tc.noun)
			}
		})
	}
}
