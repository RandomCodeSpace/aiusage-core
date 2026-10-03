package claudecode

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// contentfulLine is a realistic Claude Code assistant transcript line carrying
// everything the raw policy must refuse to store: thinking blocks, assistant
// text, tool-call inputs, a tool result, the working directory, the git branch
// and the user type. The identifiers and counters it also carries are exactly
// what raw IS allowed to keep.
const contentfulLine = `{"parentUuid":"p-1","isSidechain":false,"userType":"external",` +
	`"cwd":"/home/dev/projects/LEAK-cwd-segment","sessionId":"sess-9","version":"2.0.31",` +
	`"gitBranch":"feature/LEAK-branch","type":"assistant","uuid":"u-1",` +
	`"timestamp":"2026-08-09T10:11:12.345Z","requestId":"req-7",` +
	`"message":{"id":"msg-7","type":"message","role":"assistant","model":"claude-opus-4-7",` +
	`"content":[{"type":"thinking","thinking":"LEAK-thinking the passphrase is hunter2"},` +
	`{"type":"text","text":"LEAK-text here is the acquisition memo"},` +
	`{"type":"tool_use","id":"tu-1","name":"Write","input":{"file_path":"/tmp/LEAK-toolpath","content":"LEAK-toolinput"}}],` +
	`"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,` +
	`"cache_read_input_tokens":5,"service_tier":"standard","speed":"fast",` +
	`"cache_creation":{"ephemeral_5m_input_tokens":7,"ephemeral_1h_input_tokens":3},` +
	`"server_tool_use":{"web_search_requests":2}}},` +
	`"toolUseResult":{"stdout":"LEAK-toolresult"}}`

// leakMarkers appear ONLY inside the parts of contentfulLine that raw must
// never keep. Any of them surfacing in UsageEvent.Raw is a privacy leak.
var leakMarkers = []string{
	"LEAK-cwd-segment", "LEAK-branch", "LEAK-thinking", "hunter2",
	"LEAK-text", "LEAK-toolpath", "LEAK-toolinput", "LEAK-toolresult",
	"external", "assistant", "end_turn", "server_tool_use",
}

// TestRawIsUsageObjectOnly is the allow-list regression for issue #17: raw must
// hold the usage/model/identity sub-object and nothing else. It asserts the
// exact key set at every level, so a future field added to the audit payload
// fails here rather than quietly widening what the ledger stores.
func TestRawIsUsageObjectOnly(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "home-dev-projects-leak", "sess-9", []string{contentfulLine})

	evs := collect(t, root)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(evs), evs)
	}
	raw := evs[0].Raw
	if raw == "" {
		t.Fatal("Raw is empty; the audit payload should still be stored by default")
	}

	for _, marker := range leakMarkers {
		if strings.Contains(raw, marker) {
			t.Errorf("Raw leaked %q:\n%s", marker, raw)
		}
	}
	if len(raw) >= len(contentfulLine) {
		t.Errorf("Raw (%d bytes) is not smaller than the transcript line (%d bytes)", len(raw), len(contentfulLine))
	}

	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("Raw is not valid JSON: %v\n%s", err, raw)
	}
	assertKeys(t, "raw", top, "timestamp", "requestId", "message")
	if got := top["timestamp"]; got != "2026-08-09T10:11:12.345Z" {
		t.Errorf("timestamp = %v", got)
	}
	if got := top["requestId"]; got != "req-7" {
		t.Errorf("requestId = %v", got)
	}

	msg := child(t, top, "message")
	assertKeys(t, "raw.message", msg, "id", "model", "usage")
	if got := msg["id"]; got != "msg-7" {
		t.Errorf("message.id = %v", got)
	}
	// The source model, not the "-fast" variant the event column carries: raw
	// records what the provider said so a mismatch stays auditable.
	if got := msg["model"]; got != "claude-opus-4-7" {
		t.Errorf("message.model = %v, want the untouched source value", got)
	}

	usage := child(t, msg, "usage")
	assertKeys(t, "raw.message.usage",
		usage,
		"input_tokens", "output_tokens", "cache_creation_input_tokens",
		"cache_read_input_tokens", "service_tier", "speed", "cache_creation")
	for key, want := range map[string]float64{
		"input_tokens": 100, "output_tokens": 50,
		"cache_creation_input_tokens": 10, "cache_read_input_tokens": 5,
	} {
		if got, ok := usage[key].(float64); !ok || got != want {
			t.Errorf("usage.%s = %v, want %v", key, usage[key], want)
		}
	}
	if usage["service_tier"] != "standard" || usage["speed"] != "fast" {
		t.Errorf("usage tier/speed = %v/%v", usage["service_tier"], usage["speed"])
	}

	split := child(t, usage, "cache_creation")
	assertKeys(t, "raw.message.usage.cache_creation", split,
		"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens")
	if split["ephemeral_5m_input_tokens"] != float64(7) || split["ephemeral_1h_input_tokens"] != float64(3) {
		t.Errorf("cache_creation split = %v", split)
	}
}

// TestRawOmitsAbsentEnrichment checks the payload does not invent fields the
// transcript never carried: a plain line yields the counters and identifiers
// only, with no empty service_tier/speed/cache_creation keys.
func TestRawOmitsAbsentEnrichment(t *testing.T) {
	root := t.TempDir()
	line := `{"type":"assistant","timestamp":"2026-08-09T10:11:12Z","cwd":"/home/dev/projects/plain",` +
		`"sessionId":"sess-p","message":{"id":"msg-p","model":"claude-opus-4-7",` +
		`"usage":{"input_tokens":3,"output_tokens":4}}}`
	writeFixture(t, root, "plain", "sess-p", []string{line})

	evs := collect(t, root)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(evs[0].Raw), &top); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	assertKeys(t, "raw", top, "timestamp", "message")
	usage := child(t, child(t, top, "message"), "usage")
	assertKeys(t, "raw.message.usage", usage, "input_tokens", "output_tokens")
}

// TestDedupKeyBytesIndependentOfRaw pins both dedup-key branches to the bytes
// they have always been derived from — the message id, or the source path plus
// the sha1 of the whole transcript line. Neither reads the audit payload, so
// changing what raw stores can never re-ingest a user's history as duplicates.
func TestDedupKeyBytesIndependentOfRaw(t *testing.T) {
	root := t.TempDir()
	noID := `{"type":"assistant","timestamp":"2026-08-09T10:11:13Z","sessionId":"sess-9",` +
		`"message":{"model":"claude-opus-4-7","usage":{"input_tokens":1,"output_tokens":2}}}`
	writeFixture(t, root, "home-dev-projects-leak", "sess-9", []string{contentfulLine, noID})

	evs := collect(t, root)
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	path := filepath.Join(root, "projects", "home-dev-projects-leak", "sess-9.jsonl")
	want := map[string]string{
		"msg-7": "claude-code|msg-7",
		"":      fmt.Sprintf("claude-code|%s|%x", path, sha1.Sum([]byte(noID))),
	}
	for _, e := range evs {
		if got := e.DedupKey; got != want[e.MessageID] {
			t.Errorf("dedup key for message %q = %q, want %q", e.MessageID, got, want[e.MessageID])
		}
		if e.Raw != "" && strings.Contains(e.DedupKey, e.Raw) {
			t.Errorf("dedup key embeds the audit payload: %q", e.DedupKey)
		}
	}
}

// assertKeys fails unless m holds exactly the named keys.
func assertKeys(t *testing.T, label string, m map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Errorf("%s keys = [%s], want exactly [%s]", label, strings.Join(got, ","), strings.Join(sorted, ","))
	}
}

// child returns m[key] as a nested object, failing when it is missing.
func child(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object: %v", key, m[key])
	}
	return v
}
