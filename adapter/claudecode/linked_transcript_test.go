package claudecode

import (
	"context"
	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinkedTranscriptGrowth(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "seg")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real.jsonl")
	line := regressionLine("one", `"timestamp":"2026-09-01T00:00:00Z",`, "") + "\n"
	if err := os.WriteFile(target, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "session.jsonl")); err != nil {
		t.Fatal(err)
	}
	a := New().(Adapter)
	src := adapter.Source{Path: root}
	one, err := a.Collect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(line+strings.Replace(line, `"one"`, `"two"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	two, err := a.CollectIncremental(context.Background(), src, one.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(two.Events) != 2 {
		t.Fatalf("second pass events=%d, want 2 after symlink target grew", len(two.Events))
	}
	idle, err := a.CollectIncremental(context.Background(), src, two.Checkpoint)
	if err != nil || len(idle.Events) != 0 {
		t.Fatalf("idle replay=%+v %v", idle, err)
	}
}
