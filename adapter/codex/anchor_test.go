package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func anchorFixture(t *testing.T) (map[string]any, []string) {
	t.Helper()
	b, err := os.ReadFile("testdata/live-0.153.4.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var usage map[string]any
	if err := json.Unmarshal([]byte(lines[3]), &usage); err != nil {
		t.Fatal(err)
	}
	return usage, lines
}
func anchorJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUsageAnchorDriftHoldsCheckpoint(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"missing-type":         func(v map[string]any) { delete(v, "type") },
		"wrong-type":           func(v map[string]any) { v["type"] = "renamed-event" },
		"numeric-type":         func(v map[string]any) { v["type"] = 42 },
		"missing-payload":      func(v map[string]any) { delete(v, "payload") },
		"wrong-payload":        func(v map[string]any) { v["payload"] = "bad" },
		"missing-payload-type": func(v map[string]any) { delete(v["payload"].(map[string]any), "type") },
		"wrong-payload-type":   func(v map[string]any) { v["payload"].(map[string]any)["type"] = "renamed-count" },
		"missing-info":         func(v map[string]any) { delete(v["payload"].(map[string]any), "info") },
		"wrong-info":           func(v map[string]any) { v["payload"].(map[string]any)["info"] = "bad" },
		"missing-usage": func(v map[string]any) {
			info := v["payload"].(map[string]any)["info"].(map[string]any)
			delete(info, "last_token_usage")
			delete(info, "total_token_usage")
		},
		"wrong-usage": func(v map[string]any) {
			v["payload"].(map[string]any)["info"].(map[string]any)["last_token_usage"] = "bad"
		},
		"missing-input": func(v map[string]any) {
			delete(v["payload"].(map[string]any)["info"].(map[string]any)["last_token_usage"].(map[string]any), "input_tokens")
		},
		"wrong-output": func(v map[string]any) {
			v["payload"].(map[string]any)["info"].(map[string]any)["last_token_usage"].(map[string]any)["output_tokens"] = "bad"
		},
	} {
		t.Run(name, func(t *testing.T) {
			row, lines := anchorFixture(t)
			mutate(row)
			path := filepath.Join(t.TempDir(), "source.jsonl")
			body := append(append([]string{}, lines[:4]...), anchorJSON(t, row), lines[4])
			writeSession(t, path, body)
			obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolCodex, Path: path})
			if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil || len(obs.Events) != 2 || len(obs.Activity) != 1 {
				t.Fatalf("anchor drift=%+v,%v", obs, err)
			}
		})
	}
}

func TestRejectedCumulativeAnchorDoesNotMutateState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	writeSession(t, path, []string{
		`{"type":"turn_context","payload":{"model":"safe-model"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","model":"rejected-model","info":{"last_token_usage":{"input_tokens":500,"output_tokens":50,"total_tokens":550},"total_token_usage":{"input_tokens":1000,"output_tokens":"bad","total_tokens":1100}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":200,"output_tokens":20,"total_tokens":220}}}}`,
	})
	obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolCodex, Path: path})
	if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil || len(obs.Events) != 2 {
		t.Fatalf("rejected cumulative anchor=%+v,%v", obs, err)
	}
	for _, event := range obs.Events {
		if event.Model != "safe-model" || event.TotalTokens != 110 {
			t.Fatalf("rejected record changed model or cumulative baseline: %+v", event)
		}
	}
}

func TestUnknownAdditionsAliasesAndCumulativeUsage(t *testing.T) {
	row, lines := anchorFixture(t)
	path := filepath.Join(t.TempDir(), "source.jsonl")
	src := adapter.Source{Tool: model.ToolCodex, Path: path}
	writeSession(t, path, lines)
	base, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	row["future_field"] = "unknown-content"
	payload := row["payload"].(map[string]any)
	payload["future_payload"] = true
	info := payload["info"].(map[string]any)
	info["future_info"] = true
	for _, key := range []string{"last_token_usage", "total_token_usage"} {
		info[key].(map[string]any)["future_tokens"] = 123
	}
	updated := append(append([]string{}, lines[:3]...), anchorJSON(t, row), `{"type":"future-kind","future_payload":true}`, lines[4])
	writeSession(t, path, updated)
	added, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil || !reflect.DeepEqual(base.Events, added.Events) || !reflect.DeepEqual(base.Activity, added.Activity) {
		t.Fatalf("unknown additions changed normalized output: %+v,%v", added, err)
	}
	// The established parser accepts string-numeric aliases and total-only history.
	for _, key := range []string{"last_token_usage", "total_token_usage"} {
		u := info[key].(map[string]any)
		u["prompt_tokens"] = u["input_tokens"]
		u["completion_tokens"] = u["output_tokens"]
		delete(u, "input_tokens")
		delete(u, "output_tokens")
	}
	writeSession(t, path, append(append([]string{}, lines[:3]...), anchorJSON(t, row), lines[4]))
	aliases, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil || !reflect.DeepEqual(base.Events, aliases.Events) {
		t.Fatalf("numeric aliases changed usage: %+v,%v", aliases, err)
	}
	delete(info, "last_token_usage")
	writeSession(t, path, []string{anchorJSON(t, row)})
	cumulative, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil || len(cumulative.Events) != 1 || cumulative.Events[0].TotalTokens != 98865 {
		t.Fatalf("cumulative fallback=%+v,%v", cumulative, err)
	}
}

func TestEmptyBookkeepingUnknownSourceAndPoison(t *testing.T) {
	for name, tc := range map[string]struct {
		lines          []string
		format, poison bool
	}{
		"empty":            {},
		"bookkeeping":      {lines: []string{`{"type":"session_meta","payload":{"id":"s","cli_version":"0.153.4"}}`, `{"type":"event_msg","payload":{"type":"task_started"}}`}},
		"rate-limits-only": {lines: []string{`{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{}}}`}},
		"zero":             {lines: []string{`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":0,"output_tokens":0}}}}`}},
		"unknown":          {lines: []string{`{"renamed_type":"event","renamed_payload":{}}`}, format: true},
		"poison":           {lines: []string{`{"type":"session_meta","payload":{"id":"s"}}`, `not-json`}, poison: true},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.jsonl")
			writeSession(t, path, tc.lines)
			obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolCodex, Path: path})
			if errors.Is(err, adapter.ErrSourceFormat) != tc.format || (err != nil) != (tc.format || tc.poison) || (obs.Checkpoint == nil) != tc.format {
				t.Fatalf("empty/poison contract=%+v,%v", obs, err)
			}
		})
	}
}

func TestIncompleteFirstRecordRetriedAfterAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	src := adapter.Source{Tool: model.ToolCodex, Path: path}
	partial := `{"type":"event_msg"`
	if err := os.WriteFile(path, []byte(partial), 0600); err != nil {
		t.Fatal(err)
	}
	a := Adapter{}
	first, err := a.Collect(context.Background(), src)
	if err != nil || first.Checkpoint == nil || first.Checkpoint.Offset != 0 || len(first.Events) != 0 {
		t.Fatalf("unfinished first record = %+v, %v", first, err)
	}
	complete := partial + `,"timestamp":"2026-09-12T09:00:00Z","payload":{"type":"token_count","model":"gpt-5","info":{"last_token_usage":{"input_tokens":100,"output_tokens":20}}}}` + "\n"
	if err := os.WriteFile(path, []byte(complete), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil || next.Checkpoint == nil || next.Checkpoint.Offset != int64(len(complete)) || len(next.Events) != 1 || next.Events[0].TotalTokens != 120 {
		t.Fatalf("completed first record = %+v, %v", next, err)
	}
	idle, err := a.CollectIncremental(context.Background(), src, next.Checkpoint)
	if err != nil || len(idle.Events) != 0 || idle.Checkpoint != nil {
		t.Fatalf("unchanged completed record = %+v, %v", idle, err)
	}
}

func TestIncompleteUnknownFirstRecordKeepsCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	for _, partial := range []string{`{"type":"future_record"`, `{"future_key":`, `{"type":"response_item","payload":`} {
		if err := os.WriteFile(path, []byte(partial), 0600); err != nil {
			t.Fatal(err)
		}
		obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolCodex, Path: path})
		if err != nil || obs.Checkpoint == nil || obs.Checkpoint.Offset != 0 || len(obs.Events) != 0 {
			t.Fatalf("partial %q = %+v, %v", partial, obs, err)
		}
	}
}
