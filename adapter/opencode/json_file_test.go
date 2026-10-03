//go:build linux || darwin

package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
)

func TestJSONSkipsFIFO(t *testing.T) {
	const sourceEnv = "AIUSAGE_CORE_TEST_OPENCODE_FIFO_SOURCE"
	if root := os.Getenv(sourceEnv); root != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		obs, err := New().Collect(ctx, adapter.Source{
			Path: root,
			Meta: map[string]string{"kind": kindJSON},
		})
		if err != nil || len(obs.Events) != 0 {
			t.Fatalf("FIFO collection = %d events, %v; want no events or error", len(obs.Events), err)
		}
		return
	}

	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "blocked.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A blocked FIFO open cannot be canceled by Collect's context. Keep the
	// regression in a child process so failure cannot strand a test goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJSONSkipsFIFO$")
	cmd.Env = append(os.Environ(), sourceEnv+"="+root)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("Collect blocked on a JSON FIFO after its context deadline")
	}
	if err != nil {
		t.Fatalf("FIFO collection: %v\n%s", err, output)
	}
}
