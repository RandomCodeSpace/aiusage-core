package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// These are native JSON files from the official, hash-verified 1.1.65 writer.
// Both completed messages and the successful read part retain their source IDs.
// JSON collection reads the entire message tree and currently emits usage only.
func TestLiveWriter1165NativeJSONReplay(t *testing.T) {
	const session = "ses_f6971461fffeDLctuzpLpyhs6L"
	ids := []string{"msg_0968eba98001rMDB7c7S5bO7or", "msg_0968ec09f001UrEgRvui4lYkns"}
	fixture := "testdata/live-json-1.1.65"
	dataDir := t.TempDir()
	writeFixture := func(name string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dataDir, "storage", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(filepath.Join("session", "global", session+".json"))
	writeFixture(filepath.Join("part", ids[0], "prt_0968ec04a0018TMQouqpOhKLzf.json"))
	writeFixture(filepath.Join("message", session, ids[0]+".json"))
	rawSession, err := os.ReadFile(filepath.Join(dataDir, "storage", "session", "global", session+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var info struct{ ID, Version string }
	if err := json.Unmarshal(rawSession, &info); err != nil || info.ID != session || info.Version != "1.1.65" {
		t.Fatalf("native session provenance = %+v, %v", info, err)
	}
	sources := discover(t, dataDir)
	if len(sources) != 1 || sources[0].Meta["kind"] != kindJSON {
		t.Fatalf("native JSON discovery = %+v", sources)
	}
	src := sources[0]
	ctx := context.Background()
	collect := func() adapter.Observation {
		t.Helper()
		before := map[string][]byte{}
		if err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			raw, err := os.ReadFile(path)
			before[path] = raw
			return err
		}); err != nil {
			t.Fatal(err)
		}
		obs, err := (Adapter{}).CollectIncremental(ctx, src, nil)
		if err != nil {
			t.Fatal(err)
		}
		for path, raw := range before {
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("collection changed native source bytes")
			}
		}
		if len(obs.Activity) != 0 || obs.Checkpoint != nil {
			t.Fatalf("JSON usage-only/full-read contract changed: %+v", obs)
		}
		return obs
	}
	ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	apply := func(obs adapter.Observation, want int) {
		t.Helper()
		got, err := ledger.ApplyBatch(ctx, store.ObservationBatch{Events: obs.Events, Activity: obs.Activity})
		if err != nil || got.Events != want || got.Activity != 0 {
			t.Fatalf("ledger replay = %+v, %v; want %d new usage rows", got, err, want)
		}
	}
	first := collect()
	if len(first.Events) != 1 {
		t.Fatalf("observed prefix = %+v", first)
	}
	apply(first, 1)
	apply(collect(), 0)
	writeFixture(filepath.Join("message", session, ids[1]+".json"))
	full := collect()
	if len(full.Events) != 2 || !reflect.DeepEqual(first.Events, full.Events[:1]) {
		t.Fatalf("observed second message = %+v", full)
	}
	apply(full, 1)
	apply(collect(), 0)
	want := [][6]int64{{589, 46, 0, 2112, 0, 2747}, {143, 6, 0, 2688, 0, 2837}}
	times := []int64{1789232331416, 1789232332959}
	for i, event := range full.Events {
		got := [6]int64{event.InputTokens, event.OutputTokens, event.ReasoningTokens, event.CacheReadTokens, event.CacheCreationTokens, event.TotalTokens}
		if got != want[i] || event.MessageID != ids[i] || event.DedupKey != "opencode|"+ids[i] ||
			event.SessionID != session || event.Project != "/sample/project" || event.Tool != model.ToolOpenCode ||
			event.Model != "gemma4:31b-cloud" || event.Provider != "capture" || event.CostMicroUSD != nil ||
			!event.EventTime.Equal(time.UnixMilli(times[i]).UTC()) {
			t.Fatalf("native usage %d = %+v", i, event)
		}
	}
}

// Constructed from Session.getUsage and the bundled OpenAI-compatible SDK in
// the 1.1.65 executable, SHA256
// f8c0ff3e406b0448a4ed5df07c316e89f6fb6848da2ffb9879fdf6671e8acd4e.
// SDK input=120, output=30, reasoning=30, cached=30, total=150 becomes legacy
// input=90, output=30, reasoning=30, cache read=30, total=150. Current 1.18.29
// subtracts reasoning from output first. Both must normalize to the same event.
// These nonzero reasoning cases are constructed writer evidence, not live usage.
func TestWriterReasoningOverlapIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tokens     [6]int64 // input, output, reasoning, cache read, cache write, total
		wantOutput int64
		wantTotal  int64 // negative skips total checks for malformed overflow data
	}{
		{"legacy reasoning only", [6]int64{90, 30, 30, 30, 0, 150}, 0, 150},
		{"legacy mixed output", [6]int64{90, 30, 20, 30, 0, 150}, 10, 150},
		{"current reasoning only", [6]int64{90, 0, 30, 30, 0, 150}, 0, 150},
		{"current mixed output", [6]int64{90, 10, 20, 30, 0, 150}, 10, 150},
		{"missing total is ambiguous", [6]int64{90, 30, 30, 30, 0, 0}, 30, 180},
		{"inexact total is ambiguous", [6]int64{90, 30, 30, 30, 0, 149}, 30, 180},
		{"reasoning exceeds output", [6]int64{90, 10, 20, 30, 0, 130}, 10, 150},
		{"component sum overflows", [6]int64{1<<63 - 1, 10, 2, 1<<63 - 1, 7, 15}, 10, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.tokens
			raw := []byte(fmt.Sprintf(`{"id":"constructed-overlap","sessionID":"constructed-session","modelID":"constructed-model","time":{"created":1789232331416},"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":%d,"write":%d},"total":%d}}`, v[0], v[1], v[2], v[3], v[4], v[5]))
			event, ok, err := buildEvent(raw, "", "", "constructed-writer-reasoning")
			if err != nil || !ok || event.OutputTokens != tc.wantOutput || event.ReasoningTokens != v[2] ||
				event.InputTokens != v[0] || event.CacheReadTokens != v[3] || event.CacheCreationTokens != v[4] ||
				(tc.wantTotal >= 0 && event.TotalTokens != tc.wantTotal) {
				t.Fatalf("constructed normalization = %+v, ok=%v, err=%v", event, ok, err)
			}
		})
	}
}
