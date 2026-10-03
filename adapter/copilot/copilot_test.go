package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// writeOTEL writes the given JSONL lines to <home>/.copilot/otel/<name> and
// returns the home directory.
func writeOTEL(t *testing.T, lines ...string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".copilot", "otel")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir otel: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copilot.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return home
}

func collectAll(t *testing.T, home string) []model.UsageEvent {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var evs []model.UsageEvent
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		evs = append(evs, obs.Events...)
	}
	return evs
}

// TestChatSpanAndDuplicateInferenceSuppression is the headline case: a chat span
// and an inference log that share a traceId and response.id. The chat span wins;
// the inference log is suppressed. Token mapping is verified on the survivor.
func TestChatSpanAndDuplicateInferenceSuppression(t *testing.T) {
	chatSpan := `{"type":"span","traceId":"trace-1","spanId":"span-1","name":"chat claude-sonnet-4",` +
		`"endTime":[1775934264,967317833],"attributes":{` +
		`"gen_ai.operation.name":"chat",` +
		`"gen_ai.request.model":"claude-sonnet-4",` +
		`"gen_ai.response.model":"claude-sonnet-4",` +
		`"gen_ai.response.id":"resp-1",` +
		`"gen_ai.conversation.id":"conv-1",` +
		`"gen_ai.usage.input_tokens":19452,` +
		`"gen_ai.usage.output_tokens":281,` +
		`"gen_ai.usage.cache_read.input_tokens":123,` +
		`"gen_ai.usage.cache_creation.input_tokens":25,` +
		`"gen_ai.usage.reasoning.output_tokens":128}}`

	// Same trace + response id, lower priority -> must be suppressed.
	inferenceLog := `{"_body":"GenAI inference: claude-sonnet-4","hrTime":[1775934263,0],"attributes":{` +
		`"event.name":"gen_ai.client.inference.operation.details",` +
		`"gen_ai.response.model":"claude-sonnet-4",` +
		`"gen_ai.response.id":"resp-1",` +
		`"gen_ai.conversation.id":"conv-1",` +
		`"gen_ai.usage.input_tokens":80,` +
		`"gen_ai.usage.output_tokens":20}}`

	home := writeOTEL(t, chatSpan, inferenceLog)
	evs := collectAll(t, home)

	if len(evs) != 1 {
		t.Fatalf("want 1 surviving event, got %d: %+v", len(evs), evs)
	}
	e := evs[0]

	// Token map: Input = 19452 - min(19452,123) = 19329; cache_read separate.
	if e.InputTokens != 19329 {
		t.Errorf("InputTokens = %d, want 19329", e.InputTokens)
	}
	if e.OutputTokens != 281 {
		t.Errorf("OutputTokens = %d, want 281", e.OutputTokens)
	}
	if e.CacheReadTokens != 123 {
		t.Errorf("CacheReadTokens = %d, want 123", e.CacheReadTokens)
	}
	if e.CacheCreationTokens != 25 {
		t.Errorf("CacheCreationTokens = %d, want 25", e.CacheCreationTokens)
	}
	if e.ReasoningTokens != 128 {
		t.Errorf("ReasoningTokens = %d, want 128", e.ReasoningTokens)
	}
	// Copilot reports reasoning inside output, so it is not added again.
	wantTotal := int64(19329 + 281 + 25 + 123)
	if e.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", e.TotalTokens, wantTotal)
	}
	if e.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q, want claude-sonnet-4", e.Model)
	}
	if e.SessionID != "conv-1" {
		t.Errorf("SessionID = %q, want conv-1", e.SessionID)
	}
	if e.Project != "" {
		t.Errorf("Project = %q, want empty", e.Project)
	}
	if e.DedupKey != "copilot|trace-1:span-1" {
		t.Errorf("DedupKey = %q, want copilot|trace-1:span-1", e.DedupKey)
	}
	if e.Tool != model.ToolCopilot {
		t.Errorf("Tool = %q, want %q", e.Tool, model.ToolCopilot)
	}
	if e.EventTime.IsZero() {
		t.Error("EventTime is zero; expected parsed endTime")
	}
	if got := e.EventTime.UTC().Format("2006-01-02T15:04:05.000Z"); got != "2026-04-11T19:04:24.967Z" {
		t.Errorf("EventTime = %s, want 2026-04-11T19:04:24.967Z", got)
	}
}

// TestLiveReasoningIsInsideOutput replays a sanitized Copilot CLI 1.0.82 span.
// The exporter omits total_tokens, while its response usage states
// total=input+output and reports nonzero reasoning inside output. This pins the
// fallback that would otherwise inflate the call by reasoning a second time.
func TestLiveReasoningIsInsideOutput(t *testing.T) {
	path := filepath.Join("testdata", "otel-1.0.82.jsonl")
	obs, err := New().Collect(context.Background(), adapter.Source{
		Tool: model.ToolCopilot, Class: model.EventLevel, Path: path,
	})
	if err != nil {
		t.Fatalf("collect live fixture: %v", err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(obs.Events), obs.Events)
	}
	e := obs.Events[0]
	if e.Model != "mai-code-1.1-flash" || e.Provider != model.ProviderGitHub {
		t.Errorf("identity = model %q provider %q", e.Model, e.Provider)
	}
	if e.InputTokens != 10099 || e.CacheReadTokens != 5888 || e.OutputTokens != 77 || e.ReasoningTokens != 64 {
		t.Errorf("usage = in %d cache %d out %d reasoning %d",
			e.InputTokens, e.CacheReadTokens, e.OutputTokens, e.ReasoningTokens)
	}
	if e.TotalTokens != 16064 || e.TotalTokens != e.InputTokens+e.CacheReadTokens+e.OutputTokens {
		t.Errorf("total = %d, want 16064 without adding reasoning twice", e.TotalTokens)
	}
	if e.ReasoningTokens > e.OutputTokens {
		t.Errorf("reasoning = %d exceeds output = %d", e.ReasoningTokens, e.OutputTokens)
	}
	if cost, ok := e.Cost(); !ok || cost != 2229 || e.PriceSource != PriceSourceAIU {
		t.Errorf("vendor cost = %d present %t source %q", cost, ok, e.PriceSource)
	}
}

// TestProviderTotalWins pins the exporter's own gen_ai.usage.total_tokens as
// authoritative. Copilot proxies several vendors and for Anthropic-backed
// models the reasoning attribute is already inside output_tokens, so a
// recomputed component sum overstates the call and would double-bill it.
func TestProviderTotalWins(t *testing.T) {
	span := `{"type":"span","traceId":"t","spanId":"s","name":"chat claude-sonnet-4",` +
		`"endTime":[1775934264,0],"attributes":{"gen_ai.operation.name":"chat",` +
		`"gen_ai.response.model":"claude-sonnet-4","gen_ai.conversation.id":"c",` +
		`"gen_ai.usage.input_tokens":1000,` +
		`"gen_ai.usage.output_tokens":500,` +
		`"gen_ai.usage.reasoning.output_tokens":200,` +
		`"gen_ai.usage.total_tokens":1500}}`
	home := writeOTEL(t, span)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	e := evs[0]
	// Components are untouched; only the total defers to the exporter.
	if e.InputTokens != 1000 || e.OutputTokens != 500 || e.ReasoningTokens != 200 {
		t.Errorf("components = in %d / out %d / reasoning %d; want 1000/500/200",
			e.InputTokens, e.OutputTokens, e.ReasoningTokens)
	}
	if e.TotalTokens != 1500 {
		t.Errorf("TotalTokens = %d, want 1500 (exporter total, not the 1700 sum)", e.TotalTokens)
	}
	if e.Provider != model.ProviderGitHub {
		t.Errorf("Provider = %q, want %q", e.Provider, model.ProviderGitHub)
	}
}

// TestTotalTokenFallbackFillsOutput verifies that when only a grand total is
// present, the missing remainder fills the empty output gap.
func TestTotalTokenFallbackFillsOutput(t *testing.T) {
	span := `{"type":"span","traceId":"t","spanId":"s","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c","gen_ai.usage.total_tokens":567}}`
	home := writeOTEL(t, span)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].OutputTokens != 567 {
		t.Errorf("OutputTokens = %d, want 567 (total fallback)", evs[0].OutputTokens)
	}
	if evs[0].ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d, want 0", evs[0].ReasoningTokens)
	}
	if evs[0].TotalTokens != 567 {
		t.Errorf("TotalTokens = %d, want 567", evs[0].TotalTokens)
	}
}

// TestNumericStringValues confirms that stringified numbers are parsed.
func TestNumericStringValues(t *testing.T) {
	span := `{"type":"span","traceId":"t2","spanId":"s2","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c2",` +
		`"gen_ai.usage.input_tokens":"100",` +
		`"gen_ai.usage.output_tokens":"50",` +
		`"gen_ai.usage.cache_read.input_tokens":"10"}}`
	home := writeOTEL(t, span)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].InputTokens != 90 { // 100 - min(100,10)
		t.Errorf("InputTokens = %d, want 90", evs[0].InputTokens)
	}
	if evs[0].OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", evs[0].OutputTokens)
	}
	if evs[0].CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens = %d, want 10", evs[0].CacheReadTokens)
	}
}

// TestEmptyWhenNoOtelDir verifies graceful behaviour when ~/.copilot/otel is
// absent: no sources, no error.
func TestEmptyWhenNoOtelDir(t *testing.T) {
	home := t.TempDir() // no .copilot/otel
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("discover returned error: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources, got %d", len(srcs))
	}
}

// TestExporterEnvAdditive verifies the COPILOT_OTEL_FILE_EXPORTER_PATH single
// file is discovered additively to the default directory.
func TestExporterEnvAdditive(t *testing.T) {
	home := writeOTEL(t,
		`{"type":"span","traceId":"a","spanId":"a1","name":"chat m","endTime":[1775934264,0],`+
			`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",`+
			`"gen_ai.conversation.id":"ca","gen_ai.usage.input_tokens":10,"gen_ai.usage.output_tokens":5}}`)

	extraDir := t.TempDir()
	extraFile := filepath.Join(extraDir, "extra.jsonl")
	if err := os.WriteFile(extraFile, []byte(
		`{"type":"span","traceId":"b","spanId":"b1","name":"chat m","endTime":[1775934264,0],`+
			`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",`+
			`"gen_ai.conversation.id":"cb","gen_ai.usage.input_tokens":7,"gen_ai.usage.output_tokens":3}}`+"\n"), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}
	t.Setenv(ExporterEnv, extraFile)

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("want 2 sources (dir file + env file), got %d: %+v", len(srcs), srcs)
	}

	var total int64
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		for _, e := range obs.Events {
			total += e.TotalTokens
		}
	}
	if total != (10+5)+(7+3) {
		t.Errorf("aggregate TotalTokens = %d, want 25", total)
	}
}

// TestAgentTurnSuppressedByInference verifies a deeper rung of the priority
// chain: an agent-turn log sharing a trace with an inference log is suppressed.
func TestAgentTurnSuppressedByInference(t *testing.T) {
	// Both records carry the shared traceId at the record top level (where the
	// parser looks). Inference outranks agent-turn, so the turn is suppressed.
	inference := `{"traceId":"shared","_body":"GenAI inference: m","hrTime":[1775934263,0],"attributes":{` +
		`"event.name":"gen_ai.client.inference.operation.details",` +
		`"gen_ai.response.model":"m","gen_ai.conversation.id":"c",` +
		`"gen_ai.usage.input_tokens":80,"gen_ai.usage.output_tokens":20}}`
	agentTurn := `{"traceId":"shared","_body":"copilot_chat.agent.turn","hrTime":[1775934262,0],"attributes":{` +
		`"event.name":"copilot_chat.agent.turn","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c","turn.index":3,` +
		`"gen_ai.usage.input_tokens":40,"gen_ai.usage.output_tokens":10}}`

	home := writeOTEL(t, inference, agentTurn)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 surviving event (inference wins), got %d: %+v", len(evs), evs)
	}
	if !strings.HasPrefix(evs[0].DedupKey, "copilot|log:") {
		t.Errorf("survivor DedupKey = %q, want inference log key", evs[0].DedupKey)
	}
}

// TestMalformedLinesSkipped ensures bad JSON and lines without "attributes" do
// not fail the cycle and do not produce events.
func TestMalformedLinesSkipped(t *testing.T) {
	good := `{"type":"span","traceId":"g","spanId":"g1","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"cg","gen_ai.usage.input_tokens":10,"gen_ai.usage.output_tokens":5}}`
	home := writeOTEL(t,
		`{not valid json "attributes"}`,
		`{"type":"metric","name":"gen_ai.client.token.usage"}`, // no "attributes" marker
		`{"attributes":{"some":"thing"}}`,                      // attributes but no usage shape
		good,
	)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event (only the good span), got %d: %+v", len(evs), evs)
	}
	if evs[0].TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", evs[0].TotalTokens)
	}
}

// TestZeroTokenRecordDropped ensures records with no tokens are dropped.
func TestZeroTokenRecordDropped(t *testing.T) {
	span := `{"type":"span","traceId":"z","spanId":"z1","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"cz"}}`
	home := writeOTEL(t, span)
	evs := collectAll(t, home)
	if len(evs) != 0 {
		t.Fatalf("want 0 events (no tokens), got %d", len(evs))
	}
}

// TestTimestampLessDedupStableAcrossPolls: a record carrying no timestamp
// fields must keep the same dedup key when the file mtime advances — the OTEL
// file's mtime moves on every append, and an mtime-derived key would recount
// the record each poll.
func TestTimestampLessDedupStableAcrossPolls(t *testing.T) {
	recA := `{"_body":"GenAI inference: m1","attributes":{` +
		`"event.name":"gen_ai.client.inference.operation.details",` +
		`"gen_ai.response.model":"m1","gen_ai.conversation.id":"c1",` +
		`"gen_ai.usage.input_tokens":10,"gen_ai.usage.output_tokens":5}}`
	recB := `{"_body":"GenAI inference: m1","attributes":{` +
		`"event.name":"gen_ai.client.inference.operation.details",` +
		`"gen_ai.response.model":"m1","gen_ai.conversation.id":"c1",` +
		`"gen_ai.usage.input_tokens":99,"gen_ai.usage.output_tokens":7}}`
	home := writeOTEL(t, recA, recB)

	evs1 := collectAll(t, home)
	if len(evs1) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs1))
	}
	if evs1[0].DedupKey == evs1[1].DedupKey {
		t.Fatalf("distinct records share dedup key %q", evs1[0].DedupKey)
	}

	// Advance the file mtime — what a later poll of an appended-to file sees.
	path := filepath.Join(home, ".copilot", "otel", "copilot.jsonl")
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	evs2 := collectAll(t, home)
	if len(evs2) != 2 {
		t.Fatalf("want 2 events after mtime change, got %d", len(evs2))
	}
	for i := range evs1 {
		if evs1[i].DedupKey != evs2[i].DedupKey {
			t.Errorf("event %d dedup key changed across polls: %q -> %q",
				i, evs1[i].DedupKey, evs2[i].DedupKey)
		}
	}
}

// TestNumberValueOverflowRejected verifies floats at or above MaxInt64 are
// rejected instead of converting to an implementation-specific value.
func TestNumberValueOverflowRejected(t *testing.T) {
	if v, ok := numberValue(float64(1e300)); ok {
		t.Errorf("numberValue(1e300) = %d, ok=true; want rejected", v)
	}
	if v, ok := numberValue(json.Number("1e300")); ok {
		t.Errorf(`numberValue(json.Number("1e300")) = %d, ok=true; want rejected`, v)
	}
	if v, ok := numberValue(float64(12)); !ok || v != 12 {
		t.Errorf("numberValue(12) = %d,%v; want 12,true", v, ok)
	}
}

// TestIncrementalSizeMtimeGate: an unchanged OTEL file is skipped without
// opening; any change re-parses the whole file (trace contexts are whole-file
// scope) and refreshes the checkpoint.
func TestIncrementalSizeMtimeGate(t *testing.T) {
	span := `{"type":"span","traceId":"t1","spanId":"s1","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c1","gen_ai.usage.input_tokens":100,"gen_ai.usage.output_tokens":50}}`
	home := writeOTEL(t, span)
	path := filepath.Join(home, ".copilot", "otel", "copilot.jsonl")

	a := Adapter{}
	src := adapter.Source{Tool: model.ToolCopilot, Class: model.EventLevel, Path: path}

	obs1, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(obs1.Events) != 1 || obs1.Checkpoint == nil {
		t.Fatalf("full read: err=%v events=%d cp=%v", err, len(obs1.Events), obs1.Checkpoint)
	}

	obs2, err := a.CollectIncremental(context.Background(), src, obs1.Checkpoint)
	if err != nil || len(obs2.Events) != 0 || obs2.Checkpoint != nil {
		t.Fatalf("unchanged skip: err=%v events=%d cp=%+v", err, len(obs2.Events), obs2.Checkpoint)
	}

	// Append a second span: full re-parse emits both; the store dedups the old.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	span2 := `{"type":"span","traceId":"t2","spanId":"s2","name":"chat m","endTime":[1775934300,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c2","gen_ai.usage.input_tokens":7,"gen_ai.usage.output_tokens":3}}` + "\n"
	if _, err := f.WriteString(span2); err != nil {
		t.Fatalf("write append: %v", err)
	}
	f.Close()

	obs3, err := a.CollectIncremental(context.Background(), src, obs1.Checkpoint)
	if err != nil || len(obs3.Events) != 2 || obs3.Checkpoint == nil {
		t.Fatalf("changed reparse: err=%v events=%d cp=%v", err, len(obs3.Events), obs3.Checkpoint)
	}
}

// TestFailedReadWithholdsCheckpoint guards the checkpoint contract: a file
// that stats but cannot be read must not be checkpointed, or the size+mtime
// gate would skip its content forever once the file goes quiet.
func TestFailedReadWithholdsCheckpoint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based open failure does not apply to root")
	}
	span := `{"type":"span","traceId":"t1","spanId":"s1","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c1","gen_ai.usage.input_tokens":100,"gen_ai.usage.output_tokens":50}}`
	home := writeOTEL(t, span)
	path := filepath.Join(home, ".copilot", "otel", "copilot.jsonl")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	a := Adapter{}
	src := adapter.Source{Tool: model.ToolCopilot, Class: model.EventLevel, Path: path}

	obs, err := a.CollectIncremental(context.Background(), src, nil)
	if err == nil {
		t.Fatal("want error from unreadable file")
	}
	if obs.Checkpoint != nil {
		t.Fatalf("checkpoint must be withheld on a failed read, got %+v", obs.Checkpoint)
	}

	// Once readable again, the same (unchanged) file must still be collected.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	obs2, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(obs2.Events) != 1 || obs2.Checkpoint == nil {
		t.Fatalf("recovery read: err=%v events=%d cp=%v", err, len(obs2.Events), obs2.Checkpoint)
	}
}
