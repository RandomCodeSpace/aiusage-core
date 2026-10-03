package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func TestOpenClawReasoningFieldAlias(t *testing.T) {
	for _, a := range []Adapter{NewPi().(Adapter), NewOpenClaw().(Adapter)} {
		t.Run(a.ID(), func(t *testing.T) {
			cases := []struct {
				name, fields string
				pi, openclaw int64
			}{
				{"legacy", `"reasoning":4`, 4, 4},
				{"native", `"reasoningTokens":4`, 0, 4},
				{"both", `"reasoning":4,"reasoningTokens":2`, 4, 4},
				{"explicit-zero", `"reasoning":4,"reasoningTokens":0`, 4, 4},
				{"primary-zero", `"reasoning":0,"reasoningTokens":4`, 0, 0},
				{"null-native", `"reasoning":4,"reasoningTokens":null`, 4, 4},
				{"subset", `"reasoningTokens":20`, 0, 7},
				{"negative", `"reasoningTokens":-4`, 0, 0},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, "source.jsonl")
					header := `{"type":"session","id":"session","timestamp":"2026-09-12T00:00:00Z","cwd":"/fixture"}`
					row := func(fields string) string {
						return fmt.Sprintf(`{"type":"message","id":"entry","timestamp":"2026-09-12T00:00:01Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":10,"output":7,"totalTokens":17,%s}}}`, fields)
					}
					writeSession(t, dir, "source.jsonl", []string{header, row(tc.fields)})
					src := adapter.Source{Tool: a.ID(), Path: path}
					obs, err := a.Collect(context.Background(), src)
					if err != nil || len(obs.Events) != 1 {
						t.Fatalf("collect = %+v, %v", obs, err)
					}
					want := tc.pi
					if a.ID() == "openclaw" {
						want = tc.openclaw
					}
					ev := obs.Events[0]
					if ev.ReasoningTokens != want || ev.OutputTokens != 7 || ev.TotalTokens != 17 || ev.InputTokens != 10 {
						t.Fatalf("reasoning mapping = %+v, want %d", ev, want)
					}
					var raw auditPayload
					if err := json.Unmarshal([]byte(ev.Raw), &raw); err != nil || raw.Reasoning != want {
						t.Fatalf("audit reasoning = %d, %v", raw.Reasoning, err)
					}
					// Preserve the previous parser identity for this exact source. Its key
					// used only reasoning, even when reasoningTokens was present.
					writeSession(t, dir, "source.jsonl", []string{header, row(fmt.Sprintf(`"reasoning":%d`, tc.pi))})
					equivalent, err := a.Collect(context.Background(), src)
					if err != nil || len(equivalent.Events) != 1 || equivalent.Events[0].DedupKey != ev.DedupKey {
						t.Fatalf("alias changed the previous parser identity: %+v, %v", equivalent, err)
					}
					ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer ledger.Close()
					first, err := ledger.ApplyEvents(context.Background(), equivalent.Events, nil)
					if err != nil || first != 1 {
						t.Fatalf("old insert=%d, %v", first, err)
					}
					repeat, err := ledger.ApplyEvents(context.Background(), obs.Events, nil)
					if err != nil || repeat != 0 {
						t.Fatalf("corrected parser recounted old source: %d, %v", repeat, err)
					}
					stored, err := ledger.ListEvents(context.Background(), store.Filter{})
					if err != nil || len(stored) != 1 || stored[0].ReasoningTokens != tc.pi {
						t.Fatalf("history changed: %+v, %v", stored, err)
					}

				})
			}
		})
	}
}
