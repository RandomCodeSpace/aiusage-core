package claudecode

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func regressionLine(id, timestampFields, details string) string {
	return fmt.Sprintf(`{"type":"assistant",%s"message":{"id":%q,"model":"claude-test","usage":{"input_tokens":10,"output_tokens":5%s}}}`, timestampFields, id, details)
}

func TestOversizedReadReportsPathAndReplays(t *testing.T) {
	root := t.TempDir()
	before := regressionLine("before", `"timestamp":"2026-09-01T00:00:00Z",`, "")
	after := regressionLine("after", `"timestamp":"2026-09-01T00:00:01Z",`, "")
	writeFixture(t, root, "seg", "session", []string{before, strings.Repeat("x", 9<<20), after})
	src := adapter.Source{Tool: model.ToolClaudeCode, Path: root}
	a := New().(Adapter)
	partial, err := a.CollectIncremental(context.Background(), src, nil)
	path := filepath.Join(root, "projects", "seg", "session.jsonl")
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "token too long") || partial.Checkpoint != nil || len(partial.Events) != 1 || partial.Events[0].MessageID != "before" {
		t.Fatalf("oversized read = %+v, %v", partial, err)
	}
	ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if n, err := ledger.InsertEvents(context.Background(), partial.Events); err != nil || n != 1 {
		t.Fatalf("prefix insert=%d,%v", n, err)
	}
	// Repair only the blocking line; the trailing usage record stays identical.
	writeFixture(t, root, "seg", "session", []string{before, `{"type":"user"}`, after})
	retry, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(retry.Events) != 2 || retry.Checkpoint == nil {
		t.Fatalf("retry=%+v,%v", retry, err)
	}
	if n, err := ledger.ApplyEvents(context.Background(), retry.Events, retry.Checkpoint); err != nil || n != 1 {
		t.Fatalf("retry duplicated/lost usage: %d,%v", n, err)
	}
	idle, err := a.CollectIncremental(context.Background(), src, retry.Checkpoint)
	if err != nil || len(idle.Events) != 0 || idle.Checkpoint != nil {
		t.Fatalf("idle=%+v,%v", idle, err)
	}
}

func TestMalformedTimestampsKeepValidNeighbors(t *testing.T) {
	for label, field := range map[string]string{"missing": "", "null": `"timestamp":null,`, "empty": `"timestamp":"",`, "malformed": `"timestamp":"bad",`, "numeric": `"timestamp":123,`, "zero": `"timestamp":"0001-01-01T00:00:00Z",`} {
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "seg", "session", []string{
				regressionLine("before", `"timestamp":"2026-09-01T00:00:00Z",`, ""),
				regressionLine("bad", field, ""),
				regressionLine("after", `"timestamp":"2026-09-01T00:00:01Z",`, ""),
			})
			obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolClaudeCode, Path: root})
			if err == nil || errors.Is(err, adapter.ErrSourceFormat) || !strings.Contains(err.Error(), "line 2") || len(obs.Events) != 2 || obs.Checkpoint == nil {
				t.Fatalf("timestamp diagnostic/progress=%+v,%v", obs, err)
			}
			for _, ev := range obs.Events {
				if ev.EventTime.IsZero() || ev.MessageID == "bad" {
					t.Fatalf("undated usage escaped: %+v", ev)
				}
			}
		})
	}
}

func TestOutputThinkingTokensAreASubset(t *testing.T) {
	for label, tc := range map[string]struct {
		details   string
		reasoning int64
	}{
		"absent":           {},
		"observed":         {`,"output_tokens_details":{"thinking_tokens":3,"content":"private-marker"}`, 3},
		"negative":         {`,"output_tokens_details":{"thinking_tokens":-1}`, 0},
		"null":             {`,"output_tokens_details":null`, 0},
		"unexpected-shape": {`,"output_tokens_details":"old-ignored-shape"`, 0},
	} {
		t.Run(label, func(t *testing.T) {
			cand, ok, err := parseLine([]byte(regressionLine("id", `"timestamp":"2026-09-01T00:00:00Z",`, tc.details)), "source.jsonl", "seg", "s")
			if err != nil || !ok {
				t.Fatalf("parse=%v,%v", ok, err)
			}
			ev := cand.event
			if ev.ReasoningTokens != tc.reasoning || ev.OutputTokens != 5 || ev.TotalTokens != 15 {
				t.Fatalf("reasoning changed accounting: %+v", ev)
			}
			if strings.Contains(ev.Raw, "private-marker") || strings.Contains(ev.Raw, "old-ignored-shape") {
				t.Fatalf("raw leaked unrecognized details: %s", ev.Raw)
			}
			if tc.reasoning > 0 && !strings.Contains(ev.Raw, `"thinking_tokens":3`) {
				t.Fatalf("raw omitted observed reasoning: %s", ev.Raw)
			}
		})
	}
}
