package opencode

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// The live main-database subset records session.version=1.18.4. This identifies
// session metadata, not necessarily each message's writer after an upgrade.
// The fixture excludes WAL and does not claim current-tail or JSON-tree proof.
func TestLiveSession1184MainDBReplay(t *testing.T) {
	fixture, err := os.ReadFile("testdata/live-1.18.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	prefix, deltaSQL, ok := strings.Cut(string(fixture), "-- second observed message\n")
	if !ok {
		t.Fatal("fixture has no observed second message")
	}
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(prefix); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := db.QueryRow("SELECT version FROM session WHERE id='ses_06aa3d3c2ffe6RYymjXr7w8SZQ'").Scan(&version); err != nil || version != "1.18.4" {
		t.Fatalf("embedded session version = %q, %v", version, err)
	}
	src := adapter.Source{Tool: model.ToolOpenCode, Class: model.EventLevel, Path: path, Meta: map[string]string{"kind": kindDB}}
	a := Adapter{}
	ctx := context.Background()
	collect := func(cp *model.SourceCheckpoint) adapter.Observation {
		t.Helper()
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		obs, err := a.CollectIncremental(ctx, src, cp)
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("collection changed source database bytes")
		}
		return obs
	}
	ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	apply := func(obs adapter.Observation, events, calls int) {
		t.Helper()
		got, err := ledger.ApplyBatch(ctx, store.ObservationBatch{Events: obs.Events, Activity: obs.Activity, Checkpoint: obs.Checkpoint})
		if err != nil || got.Events != events || got.Activity != calls {
			t.Fatalf("apply = %+v, %v; want %d usage and %d calls", got, err, events, calls)
		}
	}
	first := collect(nil)
	if len(first.Events) != 1 || len(first.Activity) != 1 || first.Checkpoint == nil {
		t.Fatalf("prefix = %+v", first)
	}
	apply(first, 1, 1)
	apply(first, 0, 0)
	if _, err := db.Exec(deltaSQL); err != nil {
		t.Fatal(err)
	}
	delta := collect(first.Checkpoint)
	if len(delta.Events) != 1 || len(delta.Activity) != 1 || delta.Checkpoint == nil {
		t.Fatalf("second observed message = %+v", delta)
	}
	apply(delta, 1, 1)
	full := collect(nil)
	if !reflect.DeepEqual(full.Events, append(first.Events, delta.Events...)) || !reflect.DeepEqual(full.Activity, append(first.Activity, delta.Activity...)) {
		t.Fatal("full and incremental replay differ")
	}
	apply(full, 0, 0)
	want := [][5]int64{{2959, 56, 467, 114176, 117658}, {1151, 120, 80, 118784, 120135}}
	times := []int64{1784918467581, 1784918498784}
	for i, event := range full.Events {
		got := [5]int64{event.InputTokens, event.OutputTokens, event.ReasoningTokens, event.CacheReadTokens, event.TotalTokens}
		id := []string{"msg_f956e63fd001h5S0I3jNKNPWZK", "msg_f956edde0001iABPXwtVvo21W3"}[i]
		if got != want[i] || event.MessageID != id || event.DedupKey != "opencode|"+id ||
			event.SessionID != "ses_06aa3d3c2ffe6RYymjXr7w8SZQ" || event.Project != "/sample/project" ||
			event.Model != "gpt-5.6-terra" || event.Provider != model.ProviderOpenAI ||
			event.CacheCreationTokens != 0 || event.CostMicroUSD != nil || !event.EventTime.Equal(time.UnixMilli(times[i]).UTC()) {
			t.Fatalf("usage %d = %+v", i, event)
		}
		call := full.Activity[i]
		if call.UsageDedupKey != event.DedupKey || call.MessageID != id || call.SessionID != event.SessionID || call.Project != event.Project || call.Model != event.Model ||
			call.CallsInTurn != 1 || call.TurnSeq != 0 || call.Kind != model.ActivityTool || !call.EventTime.Equal(event.EventTime) ||
			call.DedupKey != []string{"opencode|part|prt_f956e8f13001sqfOoc7J3eAjeh", "opencode|part|prt_f956eec980014YS2vTs3X79XE4"}[i] || call.Name != []string{"live-tool-1", "live-tool-2"}[i] {
			t.Fatalf("activity %d = %+v", i, call)
		}
	}
	idle := collect(delta.Checkpoint)
	if len(idle.Events) != 0 || len(idle.Activity) != 0 {
		t.Fatalf("idle replay emitted data: %+v", idle)
	}
}
