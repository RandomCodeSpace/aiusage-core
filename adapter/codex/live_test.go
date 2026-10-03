package codex

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// The accounting fields and timestamps are from a 0.153.4 Linux amd64
// transcript captured on 2026-09-12. Content and operational metadata were
// removed; original session, turn and call identifiers are retained.
func TestLive1534Replay(t *testing.T) {
	fixture, err := os.ReadFile("testdata/live-0.153.4.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(fixture, []byte("\n"))
	prefix := bytes.Join(lines[:4], nil)
	path := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(path, prefix, 0600); err != nil {
		t.Fatal(err)
	}
	src := adapter.Source{Tool: model.ToolCodex, Class: model.EventLevel, Path: path}
	a := Adapter{}
	ctx := context.Background()
	first, err := a.Collect(ctx, src)
	if err != nil || len(first.Events) != 1 || len(first.Activity) != 1 || first.Checkpoint == nil {
		t.Fatalf("first = %+v, %v", first, err)
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
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	delta, err := a.CollectIncremental(ctx, src, first.Checkpoint)
	if err != nil || len(delta.Events) != 1 || len(delta.Activity) != 0 || delta.Checkpoint == nil {
		t.Fatalf("delta = %+v, %v", delta, err)
	}
	apply(delta, 1, 0)
	full, err := a.Collect(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Events, append(first.Events, delta.Events...)) || !reflect.DeepEqual(full.Activity, first.Activity) {
		t.Fatal("full and incremental observations differ")
	}
	apply(full, 0, 0)
	want := [][5]int64{{6917, 32512, 292, 101, 39721}, {10702, 39296, 239, 21, 50237}}
	for i, event := range full.Events {
		got := [5]int64{event.InputTokens, event.CacheReadTokens, event.OutputTokens, event.ReasoningTokens, event.TotalTokens}
		if got != want[i] || event.Model != "gpt-6-astra" || event.Provider != model.ProviderOpenAI || event.CostMicroUSD != nil || event.EventTime.IsZero() {
			t.Fatalf("event %d = %+v", i, event)
		}
	}
	call := full.Activity[0]
	if call.Name != "exec" || call.DedupKey != "codex|call|call_ThSAn1cqdEyAengDnpYofMp0" || call.UsageDedupKey != "" {
		t.Fatalf("unattributed call = %+v", call)
	}
	idle, err := a.CollectIncremental(ctx, src, delta.Checkpoint)
	if err != nil || len(idle.Events) != 0 || len(idle.Activity) != 0 {
		t.Fatalf("unchanged = %+v, %v", idle, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, fixture) {
		t.Fatal("collection changed source bytes")
	}
}

func TestCodexFixtureIgnoresAmbientHome(t *testing.T) {
	if os.Getenv("AIUSAGE_CODEX_ISOLATION_CHILD") == "1" {
		if os.Getenv(HomeEnv) != "" {
			t.Fatal("TestMain retained ambient CODEX_HOME")
		}
		home := t.TempDir()
		path := filepath.Join(codexHome(home), "sessions", "fixture.jsonl")
		writeSession(t, path, []string{`{"type":"turn_context","payload":{"model":"fixture-model"}}`})
		sources, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: home})
		if err != nil || len(sources) != 1 || sources[0].Path != path {
			t.Fatalf("fixture discovery = %+v, %v", sources, err)
		}
		return
	}
	sentinel := t.TempDir()
	writeSession(t, filepath.Join(sentinel, "sessions", "ambient.jsonl"), []string{`{"type":"turn_context","payload":{"model":"ambient-model"}}`})
	t.Setenv(HomeEnv, sentinel)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCodexFixtureIgnoresAmbientHome$")
	cmd.Env = append(os.Environ(), "AIUSAGE_CODEX_ISOLATION_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated child: %v\n%s", err, out)
	}
}
