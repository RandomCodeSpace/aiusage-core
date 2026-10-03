package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestResultStatusPreservesCumulativeUsage(t *testing.T) {
	init := `{"event":"init","conversation_id":"c1","init":{"model":"gemini-test"}}` + "\n"
	usage := `"usage":{"input_tokens":1000,"output_tokens":200,"thinking_tokens":100,"cache_read_tokens":900,"total_tokens":1200}`
	for _, status := range []string{"SUCCESS", "ERROR"} {
		t.Run(status, func(t *testing.T) {
			result := fmt.Sprintf(`{"event":"result","result":{"conversation_id":"c1","status":%q,"num_turns":2,%s}}`, status, usage)
			// The second result repeats the conversation's cumulative totals.
			path := writeFile(t, t.TempDir(), "stream.jsonl", init+result+"\n"+result+"\n")
			src := adapter.Source{Tool: model.ToolAgy, Class: model.Aggregate, Path: path}
			obs, err := (Adapter{}).Collect(context.Background(), src)
			if err != nil || len(obs.Snapshots) != 1 || obs.Checkpoint == nil {
				t.Fatalf("result=%+v,%v", obs, err)
			}
			s := obs.Snapshots[0]
			if s.InputTokens != 100 || s.CacheReadTokens != 900 || s.OutputTokens != 200 || s.TotalTokens != 1200 || s.ReasoningTokens != 100 {
				t.Fatalf("cached/cumulative accounting=%+v", s)
			}
			var raw struct {
				Usage streamUsage `json:"usage"`
			}
			if err := json.Unmarshal([]byte(s.Raw), &raw); err != nil || raw.Usage.InputTokens != 1000 {
				t.Fatalf("raw provider input=%+v,%v", raw, err)
			}
			idle, err := (Adapter{}).CollectIncremental(context.Background(), src, obs.Checkpoint)
			if err != nil || len(idle.Snapshots) != 0 || idle.Checkpoint != nil {
				t.Fatalf("idle=%+v,%v", idle, err)
			}
		})
	}
	for _, tc := range []struct {
		name, status, usage, init string
		formatError               bool
	}{
		{"error-without-usage", "ERROR", "", init, false},
		{"success-without-usage", "SUCCESS", "", init, true},
		{"error-missing-init", "ERROR", "," + usage, "", true},
		{"error-malformed-usage", "ERROR", `,"usage":{"input_tokens":1000}`, init, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.init + fmt.Sprintf(`{"event":"result","result":{"conversation_id":"c1","status":%q,"num_turns":1%s}}`, tc.status, tc.usage) + "\n"
			path := writeFile(t, t.TempDir(), "stream.jsonl", body)
			obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolAgy, Class: model.Aggregate, Path: path})
			if errors.Is(err, adapter.ErrSourceFormat) != tc.formatError || len(obs.Snapshots) != 0 || (obs.Checkpoint == nil) != tc.formatError {
				t.Fatalf("status/required anchors=%+v,%v", obs, err)
			}
		})
	}
}
