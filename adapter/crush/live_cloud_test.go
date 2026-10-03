package crush

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// These are two snapshots of one real session. The assigned token counters
// decrease on the second turn while the harness's accumulated cost increases.
func TestLiveOllamaCloudCostReplay(t *testing.T) {
	const session = "142e73d2-10b5-4218-baa5-5d5015dc2bc7"
	const fixtures = "live-0.85.0-ollama-cloud"
	path := buildDB(t, t.TempDir(), filepath.Join(fixtures, "first.sql"))
	src := adapter.Source{Tool: model.ToolCrush, Class: model.EventLevel, Path: path,
		Meta: map[string]string{"project": "/capture"}}
	stamp := func(sec int64) {
		t.Helper()
		if err := os.Chtimes(path, time.Unix(sec, 0), time.Unix(sec, 0)); err != nil {
			t.Fatal(err)
		}
	}
	check := func(obs adapter.Observation, delta, total, prompt, completion, when int64) {
		t.Helper()
		if len(obs.Events) != 1 || len(obs.Snapshots)+len(obs.Activity)+len(obs.TurnContexts) != 0 {
			t.Fatalf("want one cost-only event: %+v", obs)
		}
		e := obs.Events[0]
		cost, known := e.Cost()
		if !known || cost != delta || e.PriceSource != PriceSourceReported ||
			e.Tool != model.ToolCrush || e.Kind != model.KindUsage || e.SessionID != session ||
			e.Provider != "ollama-cloud" || e.Model != "" || !e.EventTime.Equal(time.Unix(when, 0)) {
			t.Fatalf("wrong live cost event: %+v", e)
		}
		if e.InputTokens|e.OutputTokens|e.CacheCreationTokens|e.CacheReadTokens|e.ReasoningTokens|e.TotalTokens != 0 {
			t.Fatalf("assigned counters became usage: %+v", e)
		}
		var raw rawPayload
		if err := json.Unmarshal([]byte(e.Raw), &raw); err != nil {
			t.Fatal(err)
		}
		if raw.Model != "gpt-oss:20b" || raw.Provider != "ollama-cloud" || raw.ModelsSeen != 1 ||
			raw.CostMicroUSD != total || raw.CostDeltaMicroUSD != delta ||
			raw.PromptTokensAssigned != prompt || raw.CompletionTokensAssigned != completion {
			t.Fatalf("wrong live audit evidence: %+v", raw)
		}
		if got := state(t, obs).Cost[session]; got != total {
			t.Fatalf("watermark %d, want %d", got, total)
		}
	}
	read := func(cp *model.SourceCheckpoint) adapter.Observation {
		t.Helper()
		obs, err := Adapter{}.CollectIncremental(context.Background(), src, cp)
		if err != nil {
			t.Fatal(err)
		}
		return obs
	}

	stamp(1)
	first := read(nil)
	check(first, 801, 801, 10733, 164, 1789287046)
	if repeat := read(first.Checkpoint); len(repeat.Events) != 0 {
		t.Fatalf("unchanged first snapshot charged again: %+v", repeat.Events)
	}

	// Replace only test data with the second actual source snapshot. Do not
	// synthesize cost growth or reconstruct cost from assigned token columns.
	secondSQL, err := os.ReadFile(filepath.Join("testdata", fixtures, "second.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	_, applyErr := db.Exec("DELETE FROM messages; DELETE FROM sessions;" + string(secondSQL))
	closeErr := db.Close()
	if applyErr != nil || closeErr != nil {
		t.Fatalf("apply second capture: %v; close: %v", applyErr, closeErr)
	}
	stamp(2)
	second := read(first.Checkpoint)
	check(second, 746, 1547, 10540, 29, 1789287087)
	if repeat := read(second.Checkpoint); len(repeat.Events) != 0 {
		t.Fatalf("unchanged second snapshot charged again: %+v", repeat.Events)
	}
	// A fresh import must agree with the two incremental charges together.
	check(read(nil), 1547, 1547, 10540, 29, 1789287087)
}
