package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func anchorFixture(t *testing.T) (map[string]any, string) {
	t.Helper()
	b, err := os.ReadFile("testdata/live-2.1.269.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	return first, lines[len(lines)-1]
}

func anchorJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVersionedUsageAnchorDriftHoldsManifest(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"missing-type":    func(v map[string]any) { delete(v, "type") },
		"wrong-type":      func(v map[string]any) { v["type"] = "renamed-assistant" },
		"numeric-type":    func(v map[string]any) { v["type"] = 42 },
		"missing-message": func(v map[string]any) { delete(v, "message") },
		"wrong-message":   func(v map[string]any) { v["message"] = "bad" },
		"missing-id":      func(v map[string]any) { delete(v["message"].(map[string]any), "id") },
		"wrong-id":        func(v map[string]any) { v["message"].(map[string]any)["id"] = 42 },
		"missing-model":   func(v map[string]any) { delete(v["message"].(map[string]any), "model") },
		"wrong-model":     func(v map[string]any) { v["message"].(map[string]any)["model"] = 42 },
		"missing-usage":   func(v map[string]any) { delete(v["message"].(map[string]any), "usage") },
		"wrong-usage":     func(v map[string]any) { v["message"].(map[string]any)["usage"] = "bad" },
		"renamed-input": func(v map[string]any) {
			u := v["message"].(map[string]any)["usage"].(map[string]any)
			u["renamed_input"] = u["input_tokens"]
			delete(u, "input_tokens")
		},
		"wrong-output": func(v map[string]any) {
			v["message"].(map[string]any)["usage"].(map[string]any)["output_tokens"] = "bad"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			row, after := anchorFixture(t)
			before := anchorJSON(t, row)
			mutate(row)
			root := t.TempDir()
			writeFixture(t, root, "seg", "session", []string{before, anchorJSON(t, row), after})
			obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolClaudeCode, Path: root})
			if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil || len(obs.Events) != 2 {
				t.Fatalf("anchor drift=%+v,%v", obs, err)
			}
		})
	}
}

func TestUnknownFieldsAndLegacyShapesRemainSupported(t *testing.T) {
	root := t.TempDir()
	src := adapter.Source{Tool: model.ToolClaudeCode, Path: root}
	row, after := anchorFixture(t)
	writeFixture(t, root, "seg", "session", []string{anchorJSON(t, row), after})
	base, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	row["future_field"] = map[string]any{"private": "unknown-content"}
	msg := row["message"].(map[string]any)
	msg["future_message_field"] = true
	msg["usage"].(map[string]any)["future_counter"] = 123
	writeFixture(t, root, "seg", "session", []string{anchorJSON(t, row), `{"type":"future-kind","future_field":true}`, after})
	added, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil || !reflect.DeepEqual(base.Events, added.Events) || !reflect.DeepEqual(base.Activity, added.Activity) || !reflect.DeepEqual(base.TurnContexts, added.TurnContexts) {
		t.Fatalf("unknown additions changed normalized observations: %+v,%v", added, err)
	}
	// These supported legacy records predate the versioned direct-record contract.
	delete(row, "version")
	delete(row, "type")
	delete(msg, "id")
	legacy := anchorJSON(t, row)
	writeFixture(t, root, "seg", "session", []string{legacy})
	old, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil || len(old.Events) != 1 || old.Events[0].DedupKey != persistedKey("", root+"/projects/seg/session.jsonl", []byte(legacy)) {
		t.Fatalf("legacy idless fallback=%+v,%v", old, err)
	}
	wrapped := map[string]any{"type": "progress", "version": "2.1.269", "timestamp": row["timestamp"], "data": map[string]any{"message": msg}}
	writeFixture(t, root, "seg", "session", []string{anchorJSON(t, wrapped)})
	progress, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil || len(progress.Events) != 1 || progress.Events[0].TotalTokens != old.Events[0].TotalTokens {
		t.Fatalf("AgentProgress=%+v,%v", progress, err)
	}
}

func TestEmptyBookkeepingUnknownSourceAndPoison(t *testing.T) {
	for name, tc := range map[string]struct {
		lines          []string
		format, poison bool
	}{
		"empty": {}, "bookkeeping": {lines: []string{`{"type":"user"}`, `{"type":"system"}`}},
		"unknown": {lines: []string{`{"renamed_type":"assistant","renamed_payload":{}}`}, format: true},
		"poison":  {lines: []string{regressionLine("good", `"timestamp":"2026-09-01T00:00:00Z",`, ""), `not-json`}, poison: true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "seg", "session", tc.lines)
			obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolClaudeCode, Path: root})
			if errors.Is(err, adapter.ErrSourceFormat) != tc.format || (err != nil) != (tc.format || tc.poison) || (obs.Checkpoint == nil) != tc.format {
				t.Fatalf("empty/poison contract=%+v,%v", obs, err)
			}
		})
	}
}
