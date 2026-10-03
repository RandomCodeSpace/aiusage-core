package dsh

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// Captured from DSH 0.1.0-rc.6 on Linux amd64 on 2026-09-12. The actual
// read-tool turn retains its accounting, timestamps, sequence references and
// join. Content was removed and cwd was replaced; original identities were retained.
func TestLiveRC6Replay(t *testing.T) {
	fixture, err := os.ReadFile("testdata/live-0.1.0-rc.6.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	for _, compressed := range []bool{false, true} {
		name := "plain"
		if compressed {
			name = "zstd-frames"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			var path string
			var initial, complete []byte
			if compressed {
				path = plantZstd(t, home, "sample", "live", [][]string{lines[:1], lines[1:5]})
				initial, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				complete = append(append([]byte(nil), initial...), zstdFrames(t, [][]string{lines[5:]})...)
			} else {
				path = plantSession(t, home, "sample", "live", lines[:5])
				initial, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				complete = fixture
			}
			src := adapter.Source{Tool: model.ToolDSH, Class: model.EventLevel, Path: path}
			a := Adapter{}
			ctx := context.Background()
			first, err := a.Collect(ctx, src)
			if err != nil || len(first.Events) != 1 || len(first.Activity) != 1 || first.Checkpoint == nil {
				t.Fatalf("first = %+v, %v", first, err)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, initial) {
				t.Fatal("collection changed initial source bytes")
			}
			ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			apply := func(obs adapter.Observation, events, calls int) {
				t.Helper()
				got, err := ledger.ApplyBatch(ctx, store.ObservationBatch{Events: obs.Events, Activity: obs.Activity, Checkpoint: obs.Checkpoint})
				if err != nil || got.Events != events || got.Activity != calls {
					t.Fatalf("apply = %+v, %v", got, err)
				}
			}
			apply(first, 1, 1)
			apply(first, 0, 0)
			if err := os.WriteFile(path, complete, 0600); err != nil {
				t.Fatal(err)
			}
			delta, err := a.CollectIncremental(ctx, src, first.Checkpoint)
			if err != nil || len(delta.Events) != 2 || len(delta.Activity) != 1 || delta.Checkpoint == nil {
				t.Fatalf("changed source = %+v, %v", delta, err)
			}
			apply(delta, 1, 0)
			full, err := a.Collect(ctx, src)
			if err != nil || !reflect.DeepEqual(full.Events, delta.Events) || !reflect.DeepEqual(full.Activity, delta.Activity) {
				t.Fatalf("full and changed-source replay differ: %v", err)
			}
			apply(full, 0, 0)
			want := [][4]int64{{1155, 17, 0, 1172}, {124, 7, 1120, 1251}}
			for i, event := range full.Events {
				got := [4]int64{event.InputTokens, event.OutputTokens, event.CacheReadTokens, event.TotalTokens}
				id := []string{"858ae9ba-fa62-42f1-8eb8-e4415f0ecbc9", "10f3eecd-50d3-4fa9-bd3c-cd6fb13f6e00"}[i]
				when := []int64{1789230047413, 1789230048002}[i]
				requestID := []string{"chatcmpl-297", "chatcmpl-510"}[i]
				if got != want[i] || event.DedupKey != "dsh|msg|"+id || event.MessageID != id || event.RequestID != requestID || event.SessionID != "session-84c6a0b8-df1d-4a42-8fac-4ee54bcf4a2e" || event.Model != "gemma4:31b-cloud" || event.Provider != "ollama-local" || event.EventTime != ms(when) || event.ReasoningTokens != 0 || event.CostMicroUSD != nil {
					t.Fatalf("event %d = %+v", i, event)
				}
			}
			call := full.Activity[0]
			if call.Name != "read" || call.UsageDedupKey != "dsh|msg|858ae9ba-fa62-42f1-8eb8-e4415f0ecbc9" || call.DedupKey != "dsh|call|858ae9ba-fa62-42f1-8eb8-e4415f0ecbc9|call_x89kcb1t" || call.CallsInTurn != 1 || call.TurnSeq != 0 || call.EventTime != ms(1789230047415) {
				t.Fatalf("exact call join = %+v", call)
			}
			if len(full.TurnContexts) != 0 {
				t.Fatal("capture gained an invented turn context")
			}
			idle, err := a.CollectIncremental(ctx, src, delta.Checkpoint)
			if err != nil || len(idle.Events) != 0 || len(idle.Activity) != 0 || idle.Checkpoint != nil {
				t.Fatalf("idle = %+v, %v", idle, err)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, complete) {
				t.Fatal("collection changed completed source bytes")
			}
		})
	}
}
