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

// This capture was produced by the version- and hash-bound 1.18.29 writer.
// Its config/data roots were temporary; inherited PWD selected the original
// workspace and opened one project metadata file write-capable. See fixture
// comments for that isolation limit. The recorded read attempt failed.
func TestLiveWriter11829DBReplay(t *testing.T) {
	fixture, err := os.ReadFile("testdata/live-1.18.29.sql")
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
	if err := db.QueryRow("SELECT version FROM session WHERE id='ses_f69974741ffetkJiV5sUeyaFHD'").Scan(&version); err != nil || version != "1.18.29" {
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
	if len(delta.Events) != 1 || len(delta.Activity) != 0 || delta.Checkpoint == nil {
		t.Fatalf("second observed message = %+v", delta)
	}
	apply(delta, 1, 0)
	full := collect(nil)
	if !reflect.DeepEqual(full.Events, append(first.Events, delta.Events...)) || !reflect.DeepEqual(full.Activity, append(first.Activity, delta.Activity...)) {
		t.Fatal("full and incremental replay differ")
	}
	apply(full, 0, 0)
	want := [][5]int64{{540, 31, 0, 1856, 2427}, {204, 17, 0, 2368, 2589}}
	times := []int64{1789229841685, 1789229847291}
	for i, event := range full.Events {
		got := [5]int64{event.InputTokens, event.OutputTokens, event.ReasoningTokens, event.CacheReadTokens, event.TotalTokens}
		id := []string{"msg_09668bd14001q1BuXF6wDvraAx", "msg_09668d2fa001g0ysR4uWzbpKDF"}[i]
		if got != want[i] || event.MessageID != id || event.DedupKey != "opencode|"+id ||
			event.SessionID != "ses_f69974741ffetkJiV5sUeyaFHD" || event.Project != "/sample/project" ||
			event.Model != "gemma4:31b-cloud" || event.Provider != "capture" ||
			event.CacheCreationTokens != 0 || event.CostMicroUSD != nil || !event.EventTime.Equal(time.UnixMilli(times[i]).UTC()) {
			t.Fatalf("usage %d = %+v", i, event)
		}
		if i != 0 {
			continue
		}
		call := full.Activity[0]
		if call.UsageDedupKey != event.DedupKey || call.MessageID != id || call.SessionID != event.SessionID || call.Project != event.Project || call.Model != event.Model ||
			call.CallsInTurn != 1 || call.TurnSeq != 0 || call.Kind != model.ActivityTool || !call.EventTime.Equal(event.EventTime) ||
			call.DedupKey != "opencode|part|prt_09668d09c001BlpnFa4AaBIObH" || call.Name != "read" {
			t.Fatalf("activity %d = %+v", i, call)
		}
	}
	idle := collect(delta.Checkpoint)
	if len(idle.Events) != 0 || len(idle.Activity) != 0 {
		t.Fatalf("idle replay emitted data: %+v", idle)
	}
}

// Constructed edge from Session.getUsage in the captured 1.18.29 executable,
// SHA256 ca6c0e1f42be3120595bf6848937e7586ec862c87fa7aa111e89c7cc6e9a4650.
// SDK input=120, output=30, reasoning=30, cache read=30/write=10, total=150
// becomes the token payload below. Reasoning is additive to the remaining
// output; total fallback must not copy it back into output. This is writer-code
// evidence with constructed usage, not another live or JSON-fallback capture.
func TestWriter11829ConstructedReasoningOnly(t *testing.T) {
	raw := []byte(`{"id":"constructed-reasoning-only","sessionID":"constructed-session","modelID":"constructed-model",
		"time":{"created":1789229841685},"tokens":{"input":80,"output":0,"reasoning":30,"cache":{"read":30,"write":10},"total":150}}`)
	event, ok, err := buildEvent(raw, "", "", "constructed-writer-1.18.29")
	if err != nil || !ok {
		t.Fatalf("constructed writer payload: ok=%v err=%v", ok, err)
	}
	got := [6]int64{event.InputTokens, event.OutputTokens, event.ReasoningTokens, event.CacheReadTokens, event.CacheCreationTokens, event.TotalTokens}
	if want := [6]int64{80, 0, 30, 30, 10, 150}; got != want {
		t.Fatalf("constructed writer accounting = %v, want %v", got, want)
	}
}
