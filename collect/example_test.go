package collect_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/codex"
	"github.com/RandomCodeSpace/aiusage-core/collect"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/pricing"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func ExampleRunOnce() {
	// This example supplies a fixture root. An application supplies the root
	// of its existing source and keeps its output ledger between passes.
	dir, err := os.MkdirTemp("", "aiusage-core-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	root := filepath.Join(dir, "codex")
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		panic(err)
	}
	fixture := `{"type":"turn_context","payload":{"model":"gpt-5-codex"}}
{"type":"event_msg","timestamp":"2026-05-29T10:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"output_tokens":200,"reasoning_output_tokens":50,"total_tokens":1200}}}}
`
	if err := os.WriteFile(filepath.Join(root, "sessions", "example.jsonl"), []byte(fixture), 0o600); err != nil {
		panic(err)
	}
	ledger, err := store.Open(filepath.Join(dir, "usage.db"))
	if err != nil {
		panic(err)
	}
	defer ledger.Close()

	ctx := context.Background()
	registry := adapter.NewRegistry(codex.New())
	discovery := adapter.DiscoverConfig{Overrides: map[string]string{model.ToolCodex: root}}
	prices := pricing.New(pricing.Options{})
	stats, err := collect.RunOnce(ctx, registry, ledger, discovery,
		collect.WithPricer(prices), collect.WithoutRaw())
	if err != nil || len(stats.Errors) != 0 {
		panic(fmt.Sprintf("collection failed: %v %v", err, stats.Errors))
	}
	summary, err := ledger.Summarize(ctx, store.Filter{})
	if err != nil {
		panic(err)
	}
	fmt.Printf("inserted=%d tokens=%d\n", stats.EventsInserted, summary.Totals.Total)
	// Output: inserted=1 tokens=1200
}
