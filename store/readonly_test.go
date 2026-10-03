package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// seedForReadOnly creates a current-version database holding two events and
// returns its path, with the read-write handle already closed.
func seedForReadOnly(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	at := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	if _, err := st.InsertEvents(context.Background(), []model.UsageEvent{
		rollupEvent("ro-1", model.ToolCodex, "gpt-5", "/w/alpha", at, 100),
		rollupEvent("ro-2", model.ToolClaudeCode, "claude-sonnet-4-6", "/w/beta", at.Add(time.Hour), -1),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// TestOpenReadOnlyServesReads is the point of the handle: everything the
// reporting surfaces need works through it.
func TestOpenReadOnlyServesReads(t *testing.T) {
	path := seedForReadOnly(t)
	ctx := context.Background()

	st, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer st.Close()

	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(sum.Buckets) != 2 || sum.Totals.Events != 2 {
		t.Fatalf("summarize buckets=%d events=%d want 2,2", len(sum.Buckets), sum.Totals.Events)
	}
	roll, err := st.SummarizeRollup(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("summarize rollup: %v", err)
	}
	if roll.Totals.Events != 2 || roll.Totals.UnpricedEvents != 1 {
		t.Fatalf("rollup events=%d unpriced=%d want 2,1", roll.Totals.Events, roll.Totals.UnpricedEvents)
	}
	evs, err := st.ListEvents(ctx, Filter{})
	if err != nil || len(evs) != 2 {
		t.Fatalf("list events n=%d err=%v want 2,nil", len(evs), err)
	}
	if _, err := st.SourceStats(ctx); err != nil {
		t.Fatalf("source stats: %v", err)
	}
	if _, err := st.Stats(ctx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if _, err := st.Checkpoint(ctx, model.ToolCodex, "/src/a"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// ledgerWrites is every write this package offers, spelled as one interface so
// a test can ask whether a handle has them. It is the shape a consumer would
// have to declare to call a write through an interface, which is exactly what
// the read handle must never satisfy.
type ledgerWrites interface {
	InsertEvents(ctx context.Context, events []model.UsageEvent) (int, error)
	ApplyEvents(ctx context.Context, events []model.UsageEvent, cp *model.SourceCheckpoint) (int, error)
	ApplyObservation(ctx context.Context, events []model.UsageEvent, activity []model.ActivityEvent, cp *model.SourceCheckpoint) (Applied, error)
	ApplyBatch(ctx context.Context, b ObservationBatch) (Applied, error)
	ApplySnapshot(ctx context.Context, events []model.UsageEvent, state model.AggregateSnapshot, cp *model.SourceCheckpoint) (int, error)
	UpsertState(ctx context.Context, s model.AggregateSnapshot) error
	RebuildRollup(ctx context.Context) error
	EnsureRollup(ctx context.Context) (bool, error)
}

// The full handle has all of them. Stated here rather than in the source
// because it is a claim about the SPLIT, not about either type alone: a write
// that quietly moved onto Reader would still satisfy this, which is what the
// next test is for.
var _ ledgerWrites = (*Ledger)(nil)

// TestReadHandleHasNoWrites is the guarantee a serving process is given, and it
// is now a property of the TYPE rather than a runtime refusal: a *Reader carries
// no write method, so a program that calls one does not compile and there is no
// error path to test.
//
// What is left to check is that the split has not silently leaked - a write
// added to Reader instead of Ledger, or Reader growing a method that satisfies
// the writer interface some other way. The assertion is therefore inverted: the
// read handle must NOT satisfy ledgerWrites, method by method, so a failure
// names the method that leaked instead of reporting that "something" changed.
func TestReadHandleHasNoWrites(t *testing.T) {
	var handle any = (*Reader)(nil)
	if _, ok := handle.(ledgerWrites); ok {
		t.Fatalf("*Reader satisfies ledgerWrites; a read-only handle must carry no write method")
	}
	for _, w := range []struct {
		name  string
		probe any
	}{
		{"InsertEvents", (*interface {
			InsertEvents(context.Context, []model.UsageEvent) (int, error)
		})(nil)},
		{"ApplyEvents", (*interface {
			ApplyEvents(context.Context, []model.UsageEvent, *model.SourceCheckpoint) (int, error)
		})(nil)},
		{"ApplyObservation", (*interface {
			ApplyObservation(context.Context, []model.UsageEvent, []model.ActivityEvent, *model.SourceCheckpoint) (Applied, error)
		})(nil)},
		{"ApplyBatch", (*interface {
			ApplyBatch(context.Context, ObservationBatch) (Applied, error)
		})(nil)},
		{"ApplySnapshot", (*interface {
			ApplySnapshot(context.Context, []model.UsageEvent, model.AggregateSnapshot, *model.SourceCheckpoint) (int, error)
		})(nil)},
		{"UpsertState", (*interface {
			UpsertState(context.Context, model.AggregateSnapshot) error
		})(nil)},
		{"RebuildRollup", (*interface{ RebuildRollup(context.Context) error })(nil)},
		{"EnsureRollup", (*interface {
			EnsureRollup(context.Context) (bool, error)
		})(nil)},
	} {
		if reflect.TypeOf((*Reader)(nil)).Implements(reflect.TypeOf(w.probe).Elem()) {
			t.Errorf("*Reader has %s; it belongs on Ledger", w.name)
		}
	}
}

// TestReadOnlyDSNRefusesAWrite is the defence in depth behind the type. The
// compile-time absence covers callers of this package; the DSN covers a
// statement issued from INSIDE it, which is the only place a write could now
// come from. mode=ro cannot create, write or lock the file and query_only(1)
// makes the connection refuse a write statement outright, so the guarantee does
// not rest on this package never making a mistake.
func TestReadOnlyDSNRefusesAWrite(t *testing.T) {
	path := seedForReadOnly(t)
	st, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer st.Close()

	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO schema_meta(key, value) VALUES('probe','1')`); err == nil {
		t.Fatalf("a read-only connection accepted a write statement")
	}

	// Nothing reached the file.
	rw, err := Open(path)
	if err != nil {
		t.Fatalf("reopen read-write: %v", err)
	}
	defer rw.Close()
	evs, err := rw.ListEvents(context.Background(), Filter{})
	if err != nil || len(evs) != 2 {
		t.Fatalf("post-refusal events=%d err=%v want 2,nil", len(evs), err)
	}
}

// TestOpenReadOnlyRefusesOlderSchema: an older database is refused, NOT
// migrated. Migrating would be a write, and this handle exists precisely
// because a serving process must not be able to make one.
func TestOpenReadOnlyRefusesOlderSchema(t *testing.T) {
	ctx := context.Background()
	path := legacyDB(t, 3)

	st, err := OpenReadOnly(path)
	if err == nil {
		st.Close()
		t.Fatalf("expected refusal opening a v3 database read-only")
	}
	for _, want := range []string{"v3", fmt.Sprintf("v%d", SchemaVersion), path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}

	// The file must be untouched: still v3, still without the v4 table.
	raw := rawDB(t, path)
	if v, err := readSchemaVersion(ctx, raw); err != nil || v != 3 {
		t.Fatalf("post-refusal version=%d err=%v want 3,nil", v, err)
	}
	ok, err := tableExists(ctx, raw, "usage_rollup")
	if err != nil {
		t.Fatalf("check rollup table: %v", err)
	}
	if ok {
		t.Fatalf("a read-only open created the v4 table; it must never migrate")
	}
}

// TestOpenReadOnlyRefusesNewerSchema is the other direction: a database written
// by a newer binary is refused rather than served through a schema this build
// does not understand.
func TestOpenReadOnlyRefusesNewerSchema(t *testing.T) {
	path := seedForReadOnly(t)
	newer := SchemaVersion + 1
	raw := rawDB(t, path)
	if _, err := raw.Exec(`UPDATE schema_meta SET value=? WHERE key='schema_version'`,
		strconv.Itoa(newer)); err != nil {
		t.Fatalf("stamp newer: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	st, err := OpenReadOnly(path)
	if err == nil {
		st.Close()
		t.Fatalf("expected refusal opening a v%d database read-only", newer)
	}
	for _, want := range []string{fmt.Sprintf("v%d", newer), fmt.Sprintf("v%d", SchemaVersion)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
}

// TestOpenReadOnlyLeavesFileModeAlone: Open tightens the database to 0600
// because raw can hold transcript content. A read-only handle must not, since
// chmod is a change to a file a reader was given no mandate over.
func TestOpenReadOnlyLeavesFileModeAlone(t *testing.T) {
	path := seedForReadOnly(t)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	st, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer st.Close()
	// Touch the file through the handle so a lazy connection is really opened.
	if _, err := st.Stats(context.Background()); err != nil {
		t.Fatalf("stats: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Fatalf("file mode = %04o, want 0640 (a read-only open must not chmod)", got)
	}
}

// TestOpenReadOnlyRejectsMissingFile: mode=ro never creates, so a wrong path is
// an error at open rather than an empty database that reports zero usage.
func TestOpenReadOnlyRejectsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	st, err := OpenReadOnly(path)
	if err == nil {
		st.Close()
		t.Fatalf("expected an error opening a nonexistent database read-only")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatalf("a read-only open created %s", path)
	}
	if _, err := OpenReadOnly(""); err == nil {
		t.Fatalf("expected an error for an empty path")
	}
}
