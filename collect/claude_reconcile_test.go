package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/claudecode"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func TestClaudeReconciliationRereadsLegacyCheckpoint(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	line := `{"type":"assistant","timestamp":"2026-10-04T00:00:00Z","message":{"id":"msg","model":"test-model","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ledger := realStore(t)
	reg := adapter.NewRegistry(claudecode.New())
	dc := adapter.DiscoverConfig{Overrides: map[string]string{model.ToolClaudeCode: root}}
	run := func(want int64, opts ...Option) {
		t.Helper()
		stats, err := RunOnce(ctx, reg, ledger, dc, opts...)
		if err != nil || len(stats.Errors) != 0 {
			t.Fatalf("cycle=%+v %v", stats, err)
		}
		summary, err := ledger.Summarize(ctx, store.Filter{})
		if err != nil || summary.Totals.Events != 1 || summary.Totals.Total != want {
			t.Fatalf("summary=%+v %v", summary, err)
		}
	}
	run(15)
	if err := os.WriteFile(path, []byte(line+strings.Replace(line, `"output_tokens":5`, `"output_tokens":50`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	run(15) // Old behavior checkpoints the larger file but keeps the first row.
	run(60, WithClaudeReconciliation())
	run(60, WithClaudeReconciliation())
	run(60) // Returning to default behavior does not undo reconciled history.
}

func TestClaudeReconciliationRequiresStoreCapability(t *testing.T) {
	stats, err := RunOnce(context.Background(), adapter.NewRegistry(), newFakeStore(), adapter.DiscoverConfig{}, WithClaudeReconciliation())
	if err == nil || !strings.Contains(err.Error(), "ReconcileClaudeBatch") || stats.EventsInserted != 0 {
		t.Fatalf("unsupported reconciliation: %+v %v", stats, err)
	}
}
