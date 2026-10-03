package pi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

const liveCaptureDir = "testdata/live-2026-09-12"

type liveRow struct {
	key, timestamp, response        string
	input, output, cacheRead, total int64
	call                            string
}

// Live usage and call counts are read from the stopped writer's authoritative
// primary stream. provenance.json binds each capture to the executed build.
func TestLiveCaptureReplayAndExactAccounting(t *testing.T) {
	cases := []struct {
		a                               Adapter
		after, provider, model, session string
		rows                            []liveRow
	}{
		{a: NewPi().(Adapter), after: "pi-after-fork.jsonl", provider: "ollama", model: "gemma4:31b-cloud", session: "01a0965e-c6f8-7c70-a9d6-1e7405b291a6", rows: []liveRow{
			{"pi|8a30e0ab|c41c74c2299c4983078b0b1b", "2026-09-12T16:05:04.262Z", "chatcmpl-14", 249, 15, 0, 264, "call_33r376fc"},
			{"pi|2c1af4f2|f96fc78b010c640d12ffd135", "2026-09-12T16:05:05.722Z", "chatcmpl-248", 50, 7, 224, 281, ""},
			{"pi|608d22e4|5a38dd81a1c2fe73d797d68e", "2026-09-12T16:06:29.502Z", "chatcmpl-491", 303, 15, 0, 318, "call_1sh9lv5y"},
			{"pi|211703dd|6bd903f229dc979ea6d0e609", "2026-09-12T16:06:30.037Z", "chatcmpl-49", 40, 7, 288, 335, ""},
		}},
		{a: NewOpenClaw().(Adapter), after: "openclaw-after.jsonl", provider: "ollama-cloud", model: "gemma4:31b", session: "27b9060a-9a5c-4afc-a3b3-3537edb222fc", rows: []liveRow{
			{"openclaw|d2c3d8de|821c25853de9ec275faeed27", "2026-09-12T16:07:23.416Z", "", 2406, 15, 0, 2421, "call_e6lc6hxq"},
			{"openclaw|ce8567ab|23051bb148abf3f5f90d7ca4", "2026-09-12T16:07:24.643Z", "", 2431, 7, 0, 2438, ""},
			{"openclaw|0d4b14eb|3af5aae8b7553d5f4c67cd6e", "2026-09-12T16:08:24.776Z", "", 2485, 15, 0, 2500, "call_ddd01kn9"},
			{"openclaw|1c4b13b5|627f0ad1cfa88f080a6129c3", "2026-09-12T16:08:25.503Z", "", 2510, 3, 0, 2513, ""},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.a.ID(), func(t *testing.T) {
			ctx := context.Background()
			for _, key := range []string{AgentDirEnv, SessionDirEnv, OpenClawHomeEnv, OpenClawStateDirEnv, OpenClawAgentDirEnv, OpenClawConfigPathEnv, OpenClawProfileEnv} {
				t.Setenv(key, "")
			}
			root := t.TempDir()
			sessions := filepath.Join(root, "sessions")
			if err := os.MkdirAll(sessions, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := adapter.DiscoverConfig{Home: t.TempDir()}
			if tc.a.ID() == model.ToolPi {
				t.Setenv(SessionDirEnv, sessions)
			} else {
				t.Setenv(OpenClawStateDirEnv, root)
			}
			before := liveRead(t, tc.a.ID()+"-before.jsonl")
			path := filepath.Join(sessions, "original.jsonl")
			liveWrite(t, path, before)
			if tc.a.ID() == model.ToolOpenClaw {
				liveWrite(t, filepath.Join(sessions, "original.trajectory.jsonl"), liveRead(t, "openclaw-sidecar-before.trajectory.jsonl"))
			}
			sources, err := tc.a.Discover(ctx, cfg)
			if err != nil || len(sources) != 1 || sources[0].Path != path {
				t.Fatalf("bounded discovery: count=%d, err=%v", len(sources), err)
			}
			first, err := tc.a.CollectIncremental(ctx, sources[0], nil)
			if err != nil || len(first.Events) != 2 || len(first.Activity) != 1 || first.Checkpoint == nil {
				t.Fatalf("initial capture: events=%d, calls=%d, err=%v", len(first.Events), len(first.Activity), err)
			}
			ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			apply := func(obs adapter.Observation, events, calls int) {
				t.Helper()
				got, err := ledger.ApplyBatch(ctx, store.ObservationBatch{Events: obs.Events, Activity: obs.Activity, TurnContexts: obs.TurnContexts, Checkpoint: obs.Checkpoint})
				if err != nil || got.Events != events || got.Activity != calls || got.TurnContexts != 0 {
					t.Fatalf("insert: %+v, %v", got, err)
				}
			}
			apply(first, 2, 1)
			repeated, err := tc.a.Collect(ctx, sources[0])
			if err != nil {
				t.Fatal(err)
			}
			apply(repeated, 0, 0)
			fullBody := liveRead(t, tc.after)
			var cp *model.SourceCheckpoint
			if tc.a.ID() == model.ToolPi {
				path = filepath.Join(sessions, "fork.jsonl")
			} else {
				cp = first.Checkpoint
			}
			liveWrite(t, path, fullBody)
			if tc.a.ID() == model.ToolOpenClaw {
				liveWrite(t, filepath.Join(sessions, "original.trajectory.jsonl"), liveRead(t, "openclaw-sidecar-after.trajectory.jsonl"))
			}
			frozen := liveTreeHashes(t, sessions)
			src := adapter.Source{Tool: tc.a.ID(), Class: model.EventLevel, Path: path}
			delta, err := tc.a.CollectIncremental(ctx, src, cp)
			wantDelta := 2
			if tc.a.ID() == model.ToolPi {
				wantDelta = 4
			}
			if err != nil || len(delta.Events) != wantDelta || delta.Checkpoint == nil || delta.Checkpoint.Offset != int64(len(fullBody)) {
				t.Fatalf("real delta: events=%d, err=%v", len(delta.Events), err)
			}
			apply(delta, 2, 1)
			full, err := tc.a.Collect(ctx, src)
			if err != nil {
				t.Fatal(err)
			}
			nilCP, err := tc.a.CollectIncremental(ctx, src, nil)
			if err != nil || !reflect.DeepEqual(full, nilCP) {
				t.Fatalf("full/nil checkpoint parity: %v", err)
			}
			if len(full.Events) != 4 || len(full.Activity) != 2 || len(full.TurnContexts) != 0 || len(full.Snapshots) != 0 {
				t.Fatalf("full counts: usage=%d activity=%d context=%d", len(full.Events), len(full.Activity), len(full.TurnContexts))
			}
			for i, w := range tc.rows {
				e := full.Events[i]
				if e.DedupKey != w.key || e.Tool != tc.a.ID() || e.Model != tc.model || e.Provider != tc.provider || e.SessionID != tc.session || e.Project != "/fixture/"+tc.a.ID()+"-live" || e.SourcePath != path || e.MessageID != w.response || e.Kind != model.KindUsage || !e.EventTime.Equal(mustTime(t, w.timestamp)) || e.InputTokens != w.input || e.OutputTokens != w.output || e.CacheReadTokens != w.cacheRead || e.CacheCreationTokens != 0 || e.ReasoningTokens != 0 || e.TotalTokens != w.total || e.CostMicroUSD != nil || e.PriceSource != "" {
					t.Fatalf("exact usage row %d differs", i)
				}
				if w.call != "" {
					c := full.Activity[i/2]
					if c.DedupKey != tc.a.ID()+"|call|"+w.call || c.UsageDedupKey != w.key || c.Name != "read" || c.Tool != tc.a.ID() || c.Kind != model.ActivityTool || c.CallsInTurn != 1 || c.TurnSeq != 0 || c.SessionID != e.SessionID || c.Project != e.Project || c.Model != e.Model || c.SourcePath != path || c.MessageID != e.MessageID || !c.EventTime.Equal(e.EventTime) {
						t.Fatalf("exact activity row %d differs", i/2)
					}
				}
			}
			for _, blob := range emitted(t, full) {
				if strings.Contains(blob, secret) {
					t.Fatal("capture content reached normalized output")
				}
			}
			apply(full, 0, 0)
			idle, err := tc.a.CollectIncremental(ctx, src, delta.Checkpoint)
			if err != nil || len(idle.Events) != 0 || len(idle.Activity) != 0 || len(idle.TurnContexts) != 0 || idle.Checkpoint != nil {
				t.Fatalf("idle after delta: %v", err)
			}
			discovered, err := tc.a.Discover(ctx, cfg)
			wantSources := 1
			if tc.a.ID() == model.ToolPi {
				wantSources = 2
			}
			if err != nil || len(discovered) != wantSources {
				t.Fatalf("fork/sidecar discovery: count=%d, err=%v", len(discovered), err)
			}
			if !reflect.DeepEqual(frozen, liveTreeHashes(t, sessions)) {
				t.Fatal("collection changed frozen sources")
			}
			if tc.a.ID() == model.ToolPi && !reflect.DeepEqual(before, mustReadLivePath(t, filepath.Join(sessions, "original.jsonl"))) {
				t.Fatal("fork collection changed original")
			}
		})
	}
}

func TestLiveCaptureUnknownFieldsAndConstructedReasoning(t *testing.T) {
	for _, a := range []Adapter{NewPi().(Adapter), NewOpenClaw().(Adapter)} {
		t.Run(a.ID(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.jsonl")
			body := liveRead(t, a.ID()+"-before.jsonl")
			liveWrite(t, path, body)
			src := adapter.Source{Tool: a.ID(), Path: path}
			base, err := a.Collect(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			mutate := func(reasoning bool) []byte {
				var out []string
				for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
					var row map[string]any
					if err := json.Unmarshal([]byte(line), &row); err != nil {
						t.Fatal(err)
					}
					row["future_field"] = map[string]any{"content": secret}
					if m, ok := row["message"].(map[string]any); ok {
						m["future_field"] = secret
						if u, ok := m["usage"].(map[string]any); ok {
							u["future_counter"] = 999
							if reasoning {
								if a.ID() == model.ToolOpenClaw {
									u["reasoningTokens"] = 4
								} else {
									u["reasoning"] = 4
								}
							}
						}
					}
					b, err := json.Marshal(row)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, string(b))
				}
				out = append(out, `{"type":"future_bookkeeping","payload":"`+secret+`"}`)
				return []byte(strings.Join(out, "\n") + "\n")
			}
			liveWrite(t, path, mutate(false))
			added, err := a.Collect(context.Background(), src)
			if err != nil || !reflect.DeepEqual(base.Events, added.Events) || !reflect.DeepEqual(base.Activity, added.Activity) || !reflect.DeepEqual(base.TurnContexts, added.TurnContexts) {
				t.Fatalf("unknown additions changed accounting: %v", err)
			}
			// This is constructed evidence, not a nonzero live reasoning claim. The
			// exact writer type/mappings in provenance.json establish subset semantics.
			liveWrite(t, path, mutate(true))
			reasoned, err := a.Collect(context.Background(), src)
			if err != nil || len(reasoned.Events) != len(base.Events) {
				t.Fatalf("constructed reasoning: %v", err)
			}
			for i, e := range reasoned.Events {
				if e.ReasoningTokens != 4 || e.OutputTokens != base.Events[i].OutputTokens || e.TotalTokens != base.Events[i].TotalTokens {
					t.Fatalf("reasoning changed output/total row %d", i)
				}
			}
		})
	}
}

func liveRead(t *testing.T, name string) []byte {
	t.Helper()
	return mustReadLivePath(t, filepath.Join(liveCaptureDir, name))
}
func mustReadLivePath(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func liveWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func liveTreeHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out[path] = fmt.Sprintf("%x", sha256.Sum256(mustReadLivePath(t, path)))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
