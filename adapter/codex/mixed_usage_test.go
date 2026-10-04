package codex

import (
	"context"
	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMixedLastAndTotalUsage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mixed.jsonl")
	first := `{"type":"event_msg","timestamp":"2026-09-01T00:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110},"total_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}}` + "\n"
	second := `{"type":"event_msg","timestamp":"2026-09-01T00:00:01Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"output_tokens":20,"total_tokens":170}}}}` + "\n"
	if err := os.WriteFile(p, []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	a := Adapter{}
	src := adapter.Source{Path: p}
	one, err := a.Collect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(first+second), 0600); err != nil {
		t.Fatal(err)
	}
	two, err := a.CollectIncremental(context.Background(), src, one.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(two.Events) != 1 || two.Events[0].TotalTokens != 60 {
		t.Fatalf("second pass=%+v; want delta=60", two.Events)
	}
	full, err := a.Collect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Events) != 2 || full.Events[0].TotalTokens+full.Events[1].TotalTokens != 170 {
		t.Fatalf("full pass=%+v; want 170 tokens", full.Events)
	}
	idle, err := a.CollectIncremental(context.Background(), src, two.Checkpoint)
	if err != nil || len(idle.Events) != 0 {
		t.Fatalf("idle replay=%+v %v", idle, err)
	}
}

func TestMixedUsageUpgradesLegacyCheckpointWithoutReemittingHistory(t *testing.T) {
	for _, legacyState := range []string{`{}`, `{"havePrev":true,"input":20,"output":5,"total":25}`} {
		t.Run(legacyState, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "mixed.jsonl")
			prefix := `{"type":"event_msg","timestamp":"2026-09-01T00:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110},"total_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}}` + "\n"
			tail := `{"type":"event_msg","timestamp":"2026-09-01T00:00:01Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"output_tokens":20,"total_tokens":170}}}}` + "\n"
			if err := os.WriteFile(p, []byte(prefix), 0o600); err != nil {
				t.Fatal(err)
			}
			a := Adapter{}
			src := adapter.Source{Path: p}
			initial, err := a.Collect(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			initial.Checkpoint.State = legacyState
			if err := os.WriteFile(p, []byte(prefix+tail), 0o600); err != nil {
				t.Fatal(err)
			}
			next, err := a.CollectIncremental(context.Background(), src, initial.Checkpoint)
			if err != nil || len(next.Events) != 1 || next.Events[0].TotalTokens != 60 {
				t.Fatalf("legacy checkpoint emitted history or wrong delta: %+v %v", next, err)
			}
			if next.Checkpoint == nil || !strings.Contains(next.Checkpoint.State, `"version":1`) {
				t.Fatalf("checkpoint not upgraded: %+v", next.Checkpoint)
			}
		})
	}
}
