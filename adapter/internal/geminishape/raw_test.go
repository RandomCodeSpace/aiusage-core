package geminishape

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// telemetryLine is a Gemini CLI telemetry record padded with the kind of
// payload the raw policy must refuse to store: the prompt, the response and a
// per-tool call log alongside the usage block.
const telemetryLine = `{"id":"turn-1","model":"gemini-2.5-pro","type":"turn","sessionId":"sess-g",` +
	`"timestamp":"2026-08-09T10:00:00Z",` +
	`"prompt":"LEAK-prompt merge the acme cap table","response":"LEAK-response here it is",` +
	`"toolCalls":[{"name":"write_file","args":{"path":"/tmp/LEAK-toolpath"}}],` +
	`"tokens":{"input":100,"output":50,"cached":20,"thoughts":10,"tool":5,"total":160}}`

// TestSnapshotRawIsUsageObjectOnly is the aggregate_state half of issue #17:
// the payload written to aggregate_state.raw (and carried onto the synthetic
// delta event) is re-marshalled from the allow-listed decode, so nothing else
// the telemetry record carries can reach the ledger.
func TestSnapshotRawIsUsageObjectOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(path, []byte(telemetryLine+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sh := Shape{Tool: model.ToolGemini, Provider: model.ProviderGoogle, Project: "gemini"}
	res, err := sh.ReadFile(path, time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(res.Snapshots) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(res.Snapshots))
	}
	raw := res.Snapshots[0].Raw
	if raw == "" {
		t.Fatal("Raw is empty; the audit payload should still be stored by default")
	}

	for _, marker := range []string{"LEAK-prompt", "LEAK-response", "LEAK-toolpath", "toolCalls", "cap table"} {
		if strings.Contains(raw, marker) {
			t.Errorf("Raw leaked %q:\n%s", marker, raw)
		}
	}

	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("Raw is not valid JSON: %v\n%s", err, raw)
	}
	assertKeys(t, "raw", top, "id", "model", "type", "sessionId", "timestamp", "tokens")
	if top["id"] != "turn-1" || top["model"] != "gemini-2.5-pro" || top["sessionId"] != "sess-g" {
		t.Errorf("identity fields = %v", top)
	}

	tokens, ok := top["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens is not an object: %v", top["tokens"])
	}
	assertKeys(t, "raw.tokens", tokens, "input", "output", "cached", "thoughts", "tool", "total")
	if tokens["input"] != float64(100) || tokens["total"] != float64(160) {
		t.Errorf("token block = %v", tokens)
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
