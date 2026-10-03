package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func liveRC6Lines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("testdata/live-0.1.0-rc.6.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func mutateRC6Record(t *testing.T, line string, field string, change func(map[string]any, string)) string {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatal(err)
	}
	keys := strings.Split(field, ".")
	parent := record
	for _, key := range keys[:len(keys)-1] {
		parent = parent[key].(map[string]any)
	}
	change(parent, keys[len(keys)-1])
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func collectRC6Lines(t *testing.T, lines []string) (adapter.Observation, error) {
	t.Helper()
	path := plantSession(t, t.TempDir(), "sample", "live", lines)
	return (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolDSH, Class: model.EventLevel, Path: path})
}

func TestLiveRC6RequiredAnchorsHoldCheckpoint(t *testing.T) {
	for _, field := range []string{"type", "seq", "time", "data.message.id", "data.usage", "data.usage.inputTokens", "data.usage.outputTokens"} {
		for _, mutation := range []string{"deleted", "renamed", "wrong-type", "null"} {
			t.Run(field+"/"+mutation, func(t *testing.T) {
				lines := liveRC6Lines(t)
				lines[3] = mutateRC6Record(t, lines[3], field, func(parent map[string]any, key string) {
					switch mutation {
					case "deleted":
						delete(parent, key)
					case "renamed":
						parent["future_"+key] = parent[key]
						delete(parent, key)
					case "wrong-type":
						parent[key] = false
					case "null":
						parent[key] = nil
					}
				})
				obs, err := collectRC6Lines(t, lines)
				if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil {
					t.Fatalf("checkpoint = %+v, error = %v", obs.Checkpoint, err)
				}
				if len(obs.Events) != 1 || obs.Events[0].DedupKey != "dsh|msg|10f3eecd-50d3-4fa9-bd3c-cd6fb13f6e00" {
					t.Fatalf("valid neighbor was lost or bad record emitted: %+v", obs.Events)
				}
				if len(obs.Activity) != 1 || obs.Activity[0].UsageDedupKey != "" {
					t.Fatalf("call was lost or guessed onto a different usage: %+v", obs.Activity)
				}
			})
		}
	}
}

func TestLiveRC6OptionalUsageAndAdditiveFields(t *testing.T) {
	t.Run("writer omitted usage without an accounting chunk", func(t *testing.T) {
		lines := liveRC6Lines(t)
		lines[3] = mutateRC6Record(t, lines[3], "data.usage", func(parent map[string]any, key string) { delete(parent, key) })
		lines = append(lines[:2], lines[3:]...) // remove the corresponding usage chunk
		obs, err := collectRC6Lines(t, lines)
		if err != nil || obs.Checkpoint == nil || len(obs.Events) != 1 || len(obs.Activity) != 1 || obs.Activity[0].UsageDedupKey != "" {
			t.Fatalf("legitimate no-accounting response = %+v, %v", obs, err)
		}
	})
	t.Run("additive fields and unrelated collapsed records", func(t *testing.T) {
		lines := liveRC6Lines(t)
		lines[3] = mutateRC6Record(t, lines[3], "data.usage", func(parent map[string]any, key string) { parent["future_usage"] = map[string]any{"extra": true} })
		lines = append(lines,
			`{"type":"tool/result","seq":19,"time":1789230047416,"sourceEventSeqs":[18],"data":{"message":{"id":"result-1"}}}`,
			`{"type":"future_record","seq":29,"time":1789230048003,"data":{"future":true}}`)
		obs, err := collectRC6Lines(t, lines)
		if err != nil || obs.Checkpoint == nil || len(obs.Events) != 2 || len(obs.Activity) != 1 || obs.Activity[0].UsageDedupKey != "dsh|msg|858ae9ba-fa62-42f1-8eb8-e4415f0ecbc9" {
			t.Fatalf("additive data changed accounting = %+v, %v", obs, err)
		}
	})
}

func TestRejectedRC6MessageKeepsStepAmbiguous(t *testing.T) {
	for _, field := range []string{"data.message.id", "data.usage.inputTokens"} {
		t.Run(field, func(t *testing.T) {
			lines := liveRC6Lines(t)
			lines[6] = mutateRC6Record(t, lines[6], "data.step", func(parent map[string]any, key string) { parent[key] = 1 })
			lines[3] = mutateRC6Record(t, lines[3], field, func(parent map[string]any, key string) { delete(parent, key) })
			obs, err := collectRC6Lines(t, lines)
			if !errors.Is(err, adapter.ErrSourceFormat) || len(obs.Events) != 1 || len(obs.Activity) != 1 || obs.Activity[0].UsageDedupKey != "" || obs.Activity[0].MessageID != "" {
				t.Fatalf("rejected message must not make another message an exact join: %+v, %v", obs, err)
			}
		})
	}
}

func TestRejectedRC6UsageRecoveryKeepsCallIdentity(t *testing.T) {
	for _, field := range []string{"data.usage.inputTokens", "data.usage"} {
		t.Run(field, func(t *testing.T) {
			complete := liveRC6Lines(t)
			broken := append([]string(nil), complete...)
			broken[3] = mutateRC6Record(t, broken[3], field, func(parent map[string]any, key string) {
				if key == "usage" {
					parent[key] = false
				} else {
					delete(parent, key)
				}
			})
			path := plantSession(t, t.TempDir(), "sample", "live", broken)
			src := adapter.Source{Tool: model.ToolDSH, Class: model.EventLevel, Path: path}
			ctx := context.Background()
			a := Adapter{}
			partial, err := a.Collect(ctx, src)
			if !errors.Is(err, adapter.ErrSourceFormat) || partial.Checkpoint != nil || len(partial.Events) != 1 || len(partial.Activity) != 1 || partial.Activity[0].UsageDedupKey != "" {
				t.Fatalf("rejected accounting = %+v, %v", partial, err)
			}
			ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			inserted, err := ledger.ApplyBatch(ctx, store.ObservationBatch{Events: partial.Events, Activity: partial.Activity, Checkpoint: partial.Checkpoint})
			if err != nil || inserted.Events != 1 || inserted.Activity != 1 {
				t.Fatalf("partial apply = %+v, %v", inserted, err)
			}
			if err := os.WriteFile(path, []byte(strings.Join(complete, "\n")+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			recovered, err := a.CollectIncremental(ctx, src, partial.Checkpoint)
			if err != nil || recovered.Checkpoint == nil || len(recovered.Events) != 2 || len(recovered.Activity) != 1 || recovered.Activity[0].UsageDedupKey != recovered.Events[0].DedupKey {
				t.Fatalf("recovered accounting = %+v, %v", recovered, err)
			}
			inserted, err = ledger.ApplyBatch(ctx, store.ObservationBatch{Events: recovered.Events, Activity: recovered.Activity, Checkpoint: recovered.Checkpoint})
			if err != nil || inserted.Events != 1 || inserted.Activity != 0 {
				t.Fatalf("recovery must insert repaired usage without another call: %+v, %v", inserted, err)
			}
			before, after := partial.Activity[0], recovered.Activity[0]
			if before.DedupKey != after.DedupKey || before.MessageID != after.MessageID || before.Model != after.Model {
				t.Fatalf("accounting rejection changed call identity: before=%+v, after=%+v", before, after)
			}
		})
	}
}

func TestDSHUnknownFormatAndIncompleteTail(t *testing.T) {
	for _, lines := range [][]string{{`{"future":"header"}`, `{"type":"future_record"}`}, {`{"future":"header"}`}} {
		obs, err := collectRC6Lines(t, lines)
		if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil {
			t.Fatalf("unknown format = %+v, %v", obs, err)
		}
	}
	for _, lines := range [][]string{nil, liveRC6Lines(t)[:1]} {
		obs, err := collectRC6Lines(t, lines)
		if err != nil || obs.Checkpoint == nil || len(obs.Events) != 0 {
			t.Fatalf("empty/header-only source = %+v, %v", obs, err)
		}
	}
	for _, prefix := range [][]string{nil, liveRC6Lines(t)[:5]} {
		path := plantSession(t, t.TempDir(), "sample", "live", prefix)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(`{"type":"future_record"`)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("write tail: %v, close: %v", err, closeErr)
		}
		obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolDSH, Class: model.EventLevel, Path: path})
		if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil {
			t.Fatalf("incomplete tail = %+v, %v", obs, err)
		}
		want := 0
		if len(prefix) > 0 {
			want = 1
		}
		if len(obs.Events) != want || len(obs.Activity) != want {
			t.Fatalf("complete prefix was lost: %+v", obs)
		}
	}
}

// Constructed edge, not a live reasoning capture: DSH 0.1.0-rc.6's DeepSeek
// provider maps completion_tokens_details.reasoning_tokens to reasoningTokens,
// while its token meter excludes that output subset from the disjoint total.
func TestRC6WriterReasoningSubset(t *testing.T) {
	lines := liveRC6Lines(t)
	lines[3] = mutateRC6Record(t, lines[3], "data.usage", func(parent map[string]any, key string) {
		parent[key].(map[string]any)["reasoningTokens"] = 7
	})
	obs, err := collectRC6Lines(t, lines)
	if err != nil || len(obs.Events) != 2 || obs.Events[0].ReasoningTokens != 7 || obs.Events[0].OutputTokens != 17 || obs.Events[0].TotalTokens != 1172 {
		t.Fatalf("constructed reasoning subset = %+v, %v", obs, err)
	}
}
