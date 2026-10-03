package kimicode_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/kimicode"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// The paired samples were captured on 2026-09-12 from the exact writer in
// testdata/live-2026-09-12/capture.json. The second sample resumes the same
// session; its counters and timestamps are preserved, not generated.
func TestFreshLiveCaptureReplayAndDelta(t *testing.T) {
	for _, key := range []string{kimicode.HomeEnv, kimicode.DataDirEnv} {
		t.Setenv(key, "")
	}
	ctx := context.Background()
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	fixtureRoot := filepath.Join("testdata", "live-2026-09-12")
	before := read(filepath.Join(fixtureRoot, "before.jsonl"))
	after := read(filepath.Join(fixtureRoot, "after.jsonl"))
	if !bytes.HasPrefix(after, before) || len(after) <= len(before) {
		t.Fatal("paired live capture is not an appended delta")
	}
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "wd_work_d93cc5eee78f", "session_d3e785cb-20bb-49ab-8615-85fec4cc52c8", "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	a := kimicode.Adapter{}
	sources, err := a.Discover(ctx, adapter.DiscoverConfig{
		Home: t.TempDir(), Overrides: map[string]string{model.ToolKimiCode: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	src := sources[0]
	ledger, err := store.Open(filepath.Join(t.TempDir(), "evidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ledger.Close(); err != nil {
			t.Error(err)
		}
	})
	apply := func(obs adapter.Observation, want int) {
		t.Helper()
		got, err := ledger.ApplyBatch(ctx, store.ObservationBatch{
			Events: obs.Events, Activity: obs.Activity, TurnContexts: obs.TurnContexts,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != (store.Applied{Events: want}) {
			t.Fatalf("inserted %+v, want %d usage and zero activity/context", got, want)
		}
	}
	oracle := func(obs adapter.Observation, index int) {
		t.Helper()
		if len(obs.Events) != 1 || len(obs.Activity) != 0 || len(obs.TurnContexts) != 0 {
			t.Fatalf("streams = %d/%d/%d, want 1/0/0", len(obs.Events), len(obs.Activity), len(obs.TurnContexts))
		}
		e := obs.Events[0]
		want := [][6]int64{{157, 2, 0, 0, 0, 159}, {299, 2, 0, 0, 0, 301}}[index]
		got := [6]int64{e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheCreationTokens, e.ReasoningTokens, e.TotalTokens}
		if got != want {
			t.Fatalf("sample %d counters = %v, want %v", index, got, want)
		}
		wantTime := []time.Time{time.UnixMilli(1789229534683).UTC(), time.UnixMilli(1789229648997).UTC()}[index]
		if !e.EventTime.Equal(wantTime) {
			t.Fatalf("sample %d timestamp differs from writer", index)
		}
		if e.Tool != model.ToolKimiCode || e.Model != "gemma4:31b-cloud" || e.Kind != model.KindUsage || e.DedupKey == "" {
			t.Fatal("usage identity/model does not match the captured writer")
		}
		wantKey := []string{"kimi-code|98064249223f8169e43539beeb851fdfe8bbea3d80015330096c6a60f7e21532", "kimi-code|555db90338b69d713e1975ffce1394cb6867532103c523ea9dbda646be18e8c0"}[index]
		if e.SessionID != "session_d3e785cb-20bb-49ab-8615-85fec4cc52c8" || e.MessageID != "" || e.DedupKey != wantKey {
			t.Fatal("capture identity differs from the original writer record")
		}

		if e.Provider != "" || e.ServiceTier != "" || e.Project != "" || e.CostMicroUSD != nil || e.PriceSource != "" {
			t.Fatal("capture acquired unreported provider, tier, project, or cost")
		}
	}
	first, err := a.CollectIncremental(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	oracle(first, 0)
	if first.Checkpoint == nil {
		t.Fatal("first read has no checkpoint")
	}
	apply(first, 1)
	replay, err := a.Collect(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	oracle(replay, 0)
	apply(replay, 0)
	if sha256.Sum256(read(path)) != sha256.Sum256(before) {
		t.Fatal("collection mutated source")
	}
	unchanged, err := a.CollectIncremental(ctx, src, first.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Events)+len(unchanged.Activity)+len(unchanged.TurnContexts) != 0 || unchanged.Checkpoint != nil {
		t.Fatal("unchanged source emitted records or a checkpoint")
	}
	if err := os.WriteFile(path, after, 0o600); err != nil {
		t.Fatal(err)
	}
	delta, err := a.CollectIncremental(ctx, src, first.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	oracle(delta, 1)
	if delta.Checkpoint == nil {
		t.Fatal("delta has no checkpoint")
	}
	apply(delta, 1)
	unchanged, err = a.CollectIncremental(ctx, src, delta.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Events)+len(unchanged.Activity)+len(unchanged.TurnContexts) != 0 || unchanged.Checkpoint != nil {
		t.Fatal("unchanged source after delta emitted records or a checkpoint")
	}
	replay, err = a.Collect(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Events) != 2 {
		t.Fatalf("full replay events = %d, want 2", len(replay.Events))
	}
	apply(replay, 0)
	if sha256.Sum256(read(path)) != sha256.Sum256(after) {
		t.Fatal("delta collection mutated source")
	}
	if !bytes.Equal(read(filepath.Join(fixtureRoot, "before.jsonl")), before) || !bytes.Equal(read(filepath.Join(fixtureRoot, "after.jsonl")), after) {
		t.Fatal("collection mutated a frozen fixture")
	}

	const marker = "FRESH-CAPTURE-SECRET-MUST-NOT-ESCAPE"
	var planted bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimSpace(after), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		record["prompt"] = marker
		record["apiKey"] = marker
		record["messages"] = []any{map[string]any{"content": marker, "tool_arguments": marker}}
		if usage, ok := record["usage"].(map[string]any); ok {
			usage["response"] = marker
		}
		b, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		planted.Write(b)
		planted.WriteByte('\n')
	}
	plantedPath := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(plantedPath, planted.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	plantedSource := src
	plantedSource.Path = plantedPath
	clean, err := a.Collect(ctx, plantedSource)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Events) != 2 {
		t.Fatalf("planted capture events = %d, want 2", len(clean.Events))
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(marker)) {
		t.Fatal("planted content escaped into an observation")
	}
}
