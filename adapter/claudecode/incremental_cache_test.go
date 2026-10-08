package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestWorkflowJournalsDoNotBlockCheckpoint(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "session", []string{regressionLine("usage", `"timestamp":"2026-09-01T00:00:00Z",`, "")})
	journal := filepath.Join(root, "projects", "proj", "session", "subagents", "workflows", "wf_x", "journal.jsonl")
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(`{"type":"launched","id":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New().(adapter.Incremental)
	src := adapter.Source{Tool: model.ToolClaudeCode, Path: root}
	first, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || first.Checkpoint == nil || len(first.Events) != 1 || strings.Contains(first.Checkpoint.State, "journal.jsonl") {
		t.Fatalf("workflow journal blocked collection: %+v, %v", first, err)
	}
	// Journal-only writes must not invalidate the transcript manifest.
	if err := os.WriteFile(journal, []byte(`{"type":"result","id":"x","result":"done"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	idle, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil || len(idle.Events) != 0 || idle.Checkpoint != nil {
		t.Fatalf("journal write invalidated checkpoint: %+v, %v", idle, err)
	}
	// A transcript with the same basename outside workflows remains usage.
	writeFixture(t, root, "proj", "journal", []string{regressionLine("other", `"timestamp":"2026-09-01T00:00:01Z",`, "")})
	changed, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil || len(changed.Events) != 2 || changed.Checkpoint == nil {
		t.Fatalf("ordinary journal transcript was skipped: %+v, %v", changed, err)
	}
}

// overwriteKeepingStamp makes a reread observable without timing assertions.
func overwriteKeepingStamp(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", int(info.Size()))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestChangedTranscriptReusesCachedCandidates(t *testing.T) {
	root := t.TempDir()
	ts := "2026-05-29T17:14:22.354Z"
	primary := strings.Replace(assistantWithTools("shared", ts, toolBlock("read", "Read")), `"type":"assistant",`, `"type":"assistant","attributionSkill":"review",`, 1)
	replay := strings.Replace(assistantWithTools("shared", ts, toolBlock("edit", "Edit")), `"requestId":"req-shared"`, `"isSidechain":true,"requestId":"replay"`, 1)
	writeFixture(t, root, "proj", "a", []string{primary})
	writeFixture(t, root, "proj", "b", []string{replay})
	a := New().(adapter.Incremental)
	src := adapter.Source{Tool: model.ToolClaudeCode, Path: root}
	first, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(first.Events) != 1 || len(first.Activity) != 2 || len(first.TurnContexts) != 1 || first.Checkpoint == nil {
		t.Fatalf("initial cross-file dedup: %+v, %v", first, err)
	}
	// Only b changes. Opening a again would fail to parse it.
	overwriteKeepingStamp(t, filepath.Join(root, "projects", "proj", "a.jsonl"))
	writeFixture(t, root, "proj", "b", []string{replay, assistantWithTools("new", ts, toolBlock("bash", "Bash"))})
	changed, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil || len(changed.Events) != 2 || len(changed.Activity) != 3 || len(changed.TurnContexts) != 1 || changed.Checkpoint == nil {
		t.Fatalf("cached cross-file dedup: %+v, %v", changed, err)
	}
	if !reflect.DeepEqual(first.Events[0], changed.Events[0]) || !reflect.DeepEqual(first.Activity, changed.Activity[:2]) || !reflect.DeepEqual(first.TurnContexts, changed.TurnContexts) {
		t.Fatal("cached candidates changed the winning usage, activity union or attribution")
	}
	// Removing a must also remove its candidates from deduplication.
	if err := os.Remove(filepath.Join(root, "projects", "proj", "a.jsonl")); err != nil {
		t.Fatal(err)
	}
	removed, err := a.CollectIncremental(context.Background(), src, changed.Checkpoint)
	if err != nil || len(removed.Events) != 2 || len(removed.Activity) != 2 || len(removed.TurnContexts) != 0 || removed.Checkpoint == nil {
		t.Fatalf("deleted transcript stayed cached: %+v, %v", removed, err)
	}
	// A fresh adapter rebuilds its cache on the next changed pass.
	writeFixture(t, root, "proj", "c", []string{regressionLine("third", `"timestamp":"2026-09-01T00:00:00Z",`, "")})
	cold, err := New().(adapter.Incremental).CollectIncremental(context.Background(), src, removed.Checkpoint)
	warm, warmErr := a.CollectIncremental(context.Background(), src, removed.Checkpoint)
	if err != nil || warmErr != nil || !reflect.DeepEqual(cold, warm) {
		t.Fatalf("cold and cached reads differ: cold=%+v,%v warm=%+v,%v", cold, err, warm, warmErr)
	}
}

func TestIncompleteParseIsNotCached(t *testing.T) {
	root := t.TempDir()
	line := strings.Replace(regressionLine("usage", `"timestamp":"2026-09-01T00:00:00Z",`, ""), `"type":"assistant",`, `"type":"assistant","version":"2.1.269",`, 1)
	bad := strings.Replace(line, `"type":"assistant"`, `"type":"renamed-x"`, 1)
	writeFixture(t, root, "proj", "session", []string{bad})
	path := filepath.Join(root, "projects", "proj", "session.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	a := New().(adapter.Incremental)
	src := adapter.Source{Tool: model.ToolClaudeCode, Path: root}
	first, err := a.CollectIncremental(context.Background(), src, nil)
	if !errors.Is(err, adapter.ErrSourceFormat) || first.Checkpoint != nil {
		t.Fatalf("format error was checkpointed: %+v, %v", first, err)
	}
	// Repair the file while preserving its stamp. Incomplete reads must retry.
	writeFixture(t, root, "proj", "session", []string{line})
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	retry, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(retry.Events) != 1 || retry.Checkpoint == nil {
		t.Fatalf("incomplete parse stayed cached: %+v, %v", retry, err)
	}
}

func TestCachedHooksReplayWithoutCheckpoint(t *testing.T) {
	root := t.TempDir()
	hook := `{"type":"system","subtype":"stop_hook_summary","uuid":"hook-1","timestamp":"2026-09-01T00:00:00Z","sessionId":"s","hookCount":1,"hookInfos":[{"command":"private command"}]}`
	writeFixture(t, root, "proj", "session", []string{hook, regressionLine("usage", `"timestamp":"2026-09-01T00:00:00Z",`, "")})
	a := New()
	src := adapter.Source{Tool: model.ToolClaudeCode, Path: root}
	first, err := a.Collect(context.Background(), src)
	if err != nil || len(first.Events) != 1 || len(first.Activity) != 1 || first.Activity[0].Kind != model.ActivityHook {
		t.Fatalf("initial hook observation: %+v, %v", first, err)
	}
	overwriteKeepingStamp(t, filepath.Join(root, "projects", "proj", "session.jsonl"))
	// Collect supplies no checkpoint, as the reconciliation path does. Cache
	// reuse must still emit every observation instead of skipping the root.
	replay, err := a.Collect(context.Background(), src)
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("full cached replay lost observations: %+v, %v", replay, err)
	}
}

func TestConcurrentCachedSources(t *testing.T) {
	a := New()
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		root := t.TempDir()
		writeFixture(t, root, "proj", "session", []string{regressionLine(id, `"timestamp":"2026-09-01T00:00:00Z",`, "")})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				obs, err := a.Collect(context.Background(), adapter.Source{Tool: model.ToolClaudeCode, Path: root})
				if err != nil || len(obs.Events) != 1 || obs.Events[0].MessageID != id {
					t.Errorf("cached source leaked another root: %+v, %v", obs, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
