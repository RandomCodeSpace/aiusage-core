package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// rawPayload stands in for what the column really holds on an old ledger: a
// transcript line, not a usage object.
const rawPayload = `{"usage":{"input_tokens":11},"content":"a whole transcript line"}`

func seedWithRaw(t *testing.T, st *Ledger) {
	t.Helper()
	at := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	evs := []model.UsageEvent{
		rollupEvent("proj-1", model.ToolCodex, "gpt-5", "/w/alpha", at, 10),
		rollupEvent("proj-2", model.ToolCodex, "gpt-5", "/w/alpha", at.Add(time.Hour), -1),
	}
	for i := range evs {
		evs[i].Raw = rawPayload
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestListEventsExcludesRawByDefault pins the projection at the SQL level: the
// default event listing must not so much as ASK for the raw column. Reading it
// and dropping it afterwards would still drag the ledger's bulk (tens of MB of
// transcript-bearing rows) through the driver on every call.
func TestListEventsExcludesRawByDefault(t *testing.T) {
	st := openCounting(t)
	seedWithRaw(t, st)
	ctx := context.Background()

	var evs []model.UsageEvent
	queries := queriesDuring(func() {
		var err error
		evs, err = st.ListEvents(ctx, Filter{})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
	})
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	for _, e := range evs {
		if e.Raw != "" {
			t.Errorf("event %s carried raw without an opt-in", e.DedupKey)
		}
	}
	for _, q := range queries {
		if strings.Contains(strings.ToLower(q), "raw") {
			t.Errorf("default ListEvents projection names the raw column:\n%s", q)
		}
	}
	if len(queries) == 0 {
		t.Fatalf("no statement was recorded; the projection guard is not watching anything")
	}
}

// TestListEventsWithRawIsTheOnlyWayIn: export --include-raw still works, and it
// is the only caller that gets the payload.
func TestListEventsWithRawIsTheOnlyWayIn(t *testing.T) {
	st := openTemp(t)
	seedWithRaw(t, st)
	ctx := context.Background()

	evs, err := st.ListEvents(ctx, Filter{}, WithRaw())
	if err != nil {
		t.Fatalf("list events with raw: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	for _, e := range evs {
		if e.Raw != rawPayload {
			t.Errorf("event %s raw = %q, want the stored payload", e.DedupKey, e.Raw)
		}
	}
}

// TestListEventsProjectsRowID pins the id that keyset pagination will page on:
// present, matching the ledger's own ids, and ordered with the listing.
func TestListEventsProjectsRowID(t *testing.T) {
	st := openTemp(t)
	seedWithRaw(t, st)
	ctx := context.Background()

	evs, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	byKey := map[string]int64{}
	for i, e := range evs {
		if e.ID <= 0 {
			t.Fatalf("event %s has id %d; the listing must project the row id", e.DedupKey, e.ID)
		}
		if i > 0 && e.ID <= evs[i-1].ID {
			t.Errorf("ids are not ascending with the listing order: %d after %d", e.ID, evs[i-1].ID)
		}
		byKey[e.DedupKey] = e.ID
	}
	for key, id := range byKey {
		var stored int64
		if err := st.db.QueryRowContext(ctx,
			`SELECT id FROM usage_events WHERE dedup_key=?`, key).Scan(&stored); err != nil {
			t.Fatalf("read stored id for %s: %v", key, err)
		}
		if stored != id {
			t.Errorf("event %s projected id %d, ledger holds %d", key, id, stored)
		}
	}
}

// TestListEventsEventTimeKeysetPreservesDefaultOrder pins the cursor used by
// streaming exports. A later-observed row may describe an older event, and
// several rows may share the same second, so only (event_time,id) can page the
// historical output without skipping, duplicating or reordering it.
func TestListEventsEventTimeKeysetPreservesDefaultOrder(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	fixtures := []model.UsageEvent{
		rollupEvent("inserted-first", model.ToolCodex, "gpt-5", "/w/a", base.Add(10*time.Minute), -1),
		rollupEvent("late-old-a", model.ToolCodex, "gpt-5", "/w/a", base, -1),
		rollupEvent("late-old-b", model.ToolCodex, "gpt-5", "/w/a", base, -1),
		rollupEvent("inserted-last", model.ToolCodex, "gpt-5", "/w/a", base.Add(20*time.Minute), -1),
	}
	for i := range fixtures {
		fixtures[i].ObservedTime = base.Add(time.Duration(i+1) * time.Hour)
		fixtures[i].Raw = "raw-" + fixtures[i].DedupKey
	}
	if _, err := st.InsertEvents(ctx, fixtures); err != nil {
		t.Fatalf("insert: %v", err)
	}

	defaultRows, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	wantEventOrder := []string{"late-old-a", "late-old-b", "inserted-first", "inserted-last"}
	if got := eventKeys(defaultRows); strings.Join(got, ",") != strings.Join(wantEventOrder, ",") {
		t.Fatalf("default order = %v, want %v", got, wantEventOrder)
	}

	// afterID zero is the documented first-page token: the timestamp is ignored
	// so callers do not need a special zero-time branch.
	first, err := st.ListEvents(ctx, Filter{},
		WithEventTimeKeyset(base.Add(24*time.Hour), 0, 1), WithRaw())
	if err != nil {
		t.Fatalf("first cursor page: %v", err)
	}
	if len(first) != 1 || first[0].DedupKey != wantEventOrder[0] {
		t.Fatalf("first cursor page = %v, want %s", eventKeys(first), wantEventOrder[0])
	}

	var walked []model.UsageEvent
	var afterTime time.Time
	var afterID int64
	for range len(wantEventOrder) + 1 {
		page, err := st.ListEvents(ctx, Filter{},
			WithEventTimeKeyset(afterTime, afterID, 1), WithRaw())
		if err != nil {
			t.Fatalf("cursor page after (%s,%d): %v", afterTime, afterID, err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, page...)
		last := page[len(page)-1]
		afterTime, afterID = last.EventTime, last.ID
	}
	if got := eventKeys(walked); strings.Join(got, ",") != strings.Join(wantEventOrder, ",") {
		t.Fatalf("event-time cursor order = %v, want %v", got, wantEventOrder)
	}
	for _, e := range walked {
		if e.Raw != "raw-"+e.DedupKey {
			t.Errorf("event-time cursor lost WithRaw for %s: %q", e.DedupKey, e.Raw)
		}
	}

	// The older cursor keeps its independent id-order contract.
	var idWalk []model.UsageEvent
	afterID = 0
	for range len(fixtures) + 1 {
		page, err := st.ListEvents(ctx, Filter{}, WithKeyset(afterID, 2))
		if err != nil {
			t.Fatalf("id cursor page after %d: %v", afterID, err)
		}
		if len(page) == 0 {
			break
		}
		idWalk = append(idWalk, page...)
		afterID = page[len(page)-1].ID
	}
	wantIDOrder := []string{"inserted-first", "late-old-a", "late-old-b", "inserted-last"}
	if got := eventKeys(idWalk); strings.Join(got, ",") != strings.Join(wantIDOrder, ",") {
		t.Fatalf("id cursor order = %v, want %v", got, wantIDOrder)
	}
}

// TestEventTimeKeysetUsesCompositeSeek pins the query shape behind bounded
// export memory. The logically equivalent OR predicate makes SQLite rescan
// event-time ranges on every page and regressed 100k CSV export by 13-15%.
func TestEventTimeKeysetUsesCompositeSeek(t *testing.T) {
	st := openCounting(t)
	ctx := context.Background()
	queries := queriesDuring(func() {
		if _, err := st.ListEvents(ctx, Filter{},
			WithEventTimeKeyset(time.Unix(1, 0).UTC(), 1, 10)); err != nil {
			t.Fatalf("list keyset page: %v", err)
		}
	})
	if len(queries) != 1 {
		t.Fatalf("prepared %d statements, want 1: %v", len(queries), queries)
	}
	if !strings.Contains(queries[0], "(event_time_unix, id) > (?, ?)") {
		t.Fatalf("event-time keyset is not a composite seek:\n%s", queries[0])
	}
	if strings.Contains(queries[0], " OR ") {
		t.Fatalf("event-time keyset restored the range-rescanning OR predicate:\n%s", queries[0])
	}
}

func eventKeys(events []model.UsageEvent) []string {
	out := make([]string, len(events))
	for i := range events {
		out[i] = events[i].DedupKey
	}
	return out
}
