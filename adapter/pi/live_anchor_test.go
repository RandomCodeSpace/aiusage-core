package pi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
)

// Mutations target the known shape captured from each exact current writer.
func TestLiveRequiredAnchorsHoldCheckpoint(t *testing.T) {
	anchors := []string{"header.type", "header.id", "header.timestamp", "header.cwd", "assistant.type", "assistant.id", "assistant.timestamp", "assistant.message", "assistant.message.role", "assistant.message.usage", "assistant.message.usage.input", "assistant.message.usage.output"}
	for _, a := range []Adapter{NewPi().(Adapter), NewOpenClaw().(Adapter)} {
		t.Run(a.ID(), func(t *testing.T) {
			body := liveRead(t, a.ID()+"-before.jsonl")
			for _, anchor := range anchors {
				for _, mutation := range []string{"remove", "rename", "wrong-type", "null"} {
					t.Run(anchor+"/"+mutation, func(t *testing.T) {
						lines := strings.Split(strings.TrimSpace(string(body)), "\n")
						for i, line := range lines {
							var row map[string]any
							if err := json.Unmarshal([]byte(line), &row); err != nil {
								t.Fatal(err)
							}
							parts := strings.Split(anchor, ".")
							if parts[0] == "header" && i != 0 {
								continue
							}
							if parts[0] == "assistant" {
								m, ok := row["message"].(map[string]any)
								if !ok || m["role"] != "assistant" {
									continue
								}
							}
							target := row
							for _, key := range parts[1 : len(parts)-1] {
								target = target[key].(map[string]any)
							}
							key := parts[len(parts)-1]
							switch mutation {
							case "remove":
								delete(target, key)
							case "rename":
								target["renamed_"+key] = target[key]
								delete(target, key)
							case "wrong-type":
								target[key] = []any{}
							case "null":
								target[key] = nil
							}
							encoded, err := json.Marshal(row)
							if err != nil {
								t.Fatal(err)
							}
							lines[i] = string(encoded)
							break
						}
						dir := t.TempDir()
						writeSession(t, dir, "source.jsonl", lines)
						path := filepath.Join(dir, "source.jsonl")
						src := adapter.Source{Tool: a.ID(), Path: path}
						obs, err := a.Collect(context.Background(), src)
						if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil || !strings.Contains(err.Error(), path) {
							t.Fatalf("format_error=%v, checkpoint=%v, usage=%d", errors.Is(err, adapter.ErrSourceFormat), obs.Checkpoint != nil, len(obs.Events))
						}
						// Repair the same source; no failed checkpoint may hide its usage.
						liveWrite(t, path, body)
						recovered, err := a.CollectIncremental(context.Background(), src, obs.Checkpoint)
						if err != nil || len(recovered.Events) != 2 || recovered.Checkpoint == nil {
							t.Fatalf("recovery usage=%d, err=%v", len(recovered.Events), err)
						}
					})
				}
			}
		})
	}
}

func TestLiveValidEmptyAndIncompleteUsage(t *testing.T) {
	for _, a := range []Adapter{NewPi().(Adapter), NewOpenClaw().(Adapter)} {
		t.Run(a.ID(), func(t *testing.T) {
			body := liveRead(t, a.ID()+"-before.jsonl")
			header := strings.SplitN(string(body), "\n", 2)[0] + "\n"
			path := filepath.Join(t.TempDir(), "source.jsonl")
			liveWrite(t, path, []byte(header))
			src := adapter.Source{Tool: a.ID(), Path: path}
			empty, err := a.Collect(context.Background(), src)
			if err != nil || len(empty.Events) != 0 || empty.Checkpoint == nil {
				t.Fatalf("valid header-only source: %v", err)
			}
			// A well-formed JSON object without its usage or newline is still in flight.
			unfinished := `{"type":"message","id":"pending","timestamp":"2026-09-12T16:00:00Z","message":{"role":"assistant"}}`
			liveWrite(t, path, []byte(header+unfinished))
			partial, err := a.Collect(context.Background(), src)
			if err != nil || len(partial.Events) != 0 || partial.Checkpoint == nil || partial.Checkpoint.Offset != int64(len(header)) {
				t.Fatalf("incomplete usage: checkpoint=%v, err=%v", partial.Checkpoint, err)
			}
			if err := os.WriteFile(path, []byte(header+unfinished+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			finished, err := a.CollectIncremental(context.Background(), src, partial.Checkpoint)
			if !errors.Is(err, adapter.ErrSourceFormat) || finished.Checkpoint != nil {
				t.Fatalf("completed missing usage: %v", err)
			}
		})
	}
}
