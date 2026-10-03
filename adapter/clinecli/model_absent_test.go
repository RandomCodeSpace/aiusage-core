package clinecli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestMissingModelKeepsUsageAndActivityIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.messages.json")
	raw := `{"version":1,"updated_at":"2026-08-16T05:00:00Z","agent":"lead","sessionId":"s","messages":[{"id":"m1","role":"assistant","ts":1786855600000,"metrics":{"inputTokens":5000,"outputTokens":250,"cacheReadTokens":0,"cacheWriteTokens":0},"content":[{"type":"tool_use","id":"call1","name":"read","input":{"secret":"private"}}]}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	src := adapter.Source{Tool: model.ToolCline, Path: path}
	obs, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil || len(obs.Events) != 1 || len(obs.Activity) != 1 || obs.Checkpoint == nil {
		t.Fatalf("missing-model observation=%+v,%v", obs, err)
	}
	ev := obs.Events[0]
	if ev.DedupKey != "cline|s|lead|m1" || ev.Model != "" || ev.Provider != "" || ev.TotalTokens != 5250 || ev.CostMicroUSD != nil {
		t.Fatalf("missing-model usage=%+v", ev)
	}
	if obs.Activity[0].UsageDedupKey != ev.DedupKey || obs.Activity[0].DedupKey != "cline|s|lead|m1|call|call1" {
		t.Fatalf("activity lost exact identity: %+v", obs.Activity)
	}
	again, err := (Adapter{}).Collect(context.Background(), src)
	if err != nil || len(again.Events) != 1 || again.Events[0].DedupKey != ev.DedupKey {
		t.Fatalf("reread identity=%+v,%v", again, err)
	}
}
