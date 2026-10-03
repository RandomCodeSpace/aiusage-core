package claudecode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func TestLive21269Replay(t *testing.T) {
	fixture, err := os.ReadFile("testdata/live-2.1.269.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "projects", "sample", "4b63eda6-2b8e-48aa-9cff-e922fcfd2b2f.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(fixture, []byte("\n"))
	if err := os.WriteFile(path, bytes.Join(lines[:4], nil), 0600); err != nil {
		t.Fatal(err)
	}
	src := adapter.Source{Tool: model.ToolClaudeCode, Class: model.EventLevel, Path: root}
	a := Adapter{}
	ctx := context.Background()
	first, err := a.Collect(ctx, src)
	if err != nil || len(first.Events) != 1 || len(first.Activity) != 2 || first.Checkpoint == nil {
		t.Fatalf("first = %+v, %v", first, err)
	}
	ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	apply := func(obs adapter.Observation, events, calls, contexts int) {
		t.Helper()
		got, err := ledger.ApplyBatch(ctx, store.ObservationBatch{Events: obs.Events, Activity: obs.Activity, TurnContexts: obs.TurnContexts, Checkpoint: obs.Checkpoint})
		if err != nil || got.Events != events || got.Activity != calls || got.TurnContexts != contexts {
			t.Fatalf("apply = %+v, %v", got, err)
		}
	}
	apply(first, 1, 2, 0)
	apply(first, 0, 0, 0)
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	delta, err := a.CollectIncremental(ctx, src, first.Checkpoint)
	if err != nil || len(delta.Events) != 2 || len(delta.Activity) != 3 || len(delta.TurnContexts) != 1 || delta.Checkpoint == nil {
		t.Fatalf("changed root = %+v, %v", delta, err)
	}
	apply(delta, 1, 1, 1)
	full, err := a.Collect(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Events, delta.Events) || !reflect.DeepEqual(full.Activity, delta.Activity) || !reflect.DeepEqual(full.TurnContexts, delta.TurnContexts) {
		t.Fatal("full and changed-root replay differ")
	}
	apply(full, 0, 0, 0)
	want := map[string][6]int64{
		"claude-code|msg_011Ceyht1FpNTEpEEXn46REc": {2, 18658, 29969, 522, 104, 49151},
		"claude-code|msg_011Ceyhu1vYFxVMfZgBsFqvn": {32, 7941, 48627, 1264, 1044, 57864},
	}
	for _, event := range full.Events {
		got := [6]int64{event.InputTokens, event.CacheCreationTokens, event.CacheReadTokens, event.OutputTokens, event.ReasoningTokens, event.TotalTokens}
		if expected, ok := want[event.DedupKey]; !ok || got != expected || event.Model != "claude-fable-5-1" || event.Provider != model.ProviderAnthropic || event.CostMicroUSD != nil {
			t.Fatalf("usage = %+v", event)
		}
	}
	wantCalls := map[string]string{
		"claude-code|call|toolu_014XXo3PMyCWcD2WjS5E2yYf": "claude-code|msg_011Ceyht1FpNTEpEEXn46REc",
		"claude-code|call|toolu_01KuYmYefKDrkt7BV9M9cvzK": "claude-code|msg_011Ceyht1FpNTEpEEXn46REc",
		"claude-code|call|toolu_01EBtnYjxM52YHXkfkKpW1Dk": "claude-code|msg_011Ceyhu1vYFxVMfZgBsFqvn",
	}
	for _, call := range full.Activity {
		if expected, ok := wantCalls[call.DedupKey]; !ok || call.UsageDedupKey != expected {
			t.Fatalf("call identity differs: %+v", call)
		}
		if _, ok := want[call.UsageDedupKey]; !ok {
			t.Fatalf("call has no exact usage join: %+v", call)
		}
		n := 2
		if call.UsageDedupKey == "claude-code|msg_011Ceyhu1vYFxVMfZgBsFqvn" {
			n = 1
		}
		if call.CallsInTurn != n {
			t.Fatalf("call divisor = %+v", call)
		}
	}
	contextRow := full.TurnContexts[0]
	if contextRow.UsageDedupKey != "claude-code|msg_011Ceyhu1vYFxVMfZgBsFqvn" || contextRow.Dimension != model.DimensionSkill || contextRow.Value != "live-skill" {
		t.Fatalf("context = %+v", contextRow)
	}
	idle, err := a.CollectIncremental(ctx, src, delta.Checkpoint)
	if err != nil || len(idle.Events) != 0 || len(idle.Activity) != 0 || len(idle.TurnContexts) != 0 {
		t.Fatalf("unchanged = %+v, %v", idle, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, fixture) {
		t.Fatal("collection changed source bytes")
	}
}
