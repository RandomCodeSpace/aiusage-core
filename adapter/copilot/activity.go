package copilot

// Activity extraction for the Copilot OTEL export.
//
// SPANS ONLY, AND THAT IS THE WHOLE POINT. The export interleaves two record
// shapes, and only one of them counts anything. A span is written once per
// operation: one `execute_tool` span is one tool call. A METRIC record
// (`github.copilot.tool.call.count` and friends) is a cumulative counter
// re-exported on the exporter's timer, so the same single call reappears as a
// fresh dataPoint every interval — measured on a live export, one `view` call
// had produced 226 dataPoints, every one of them value 1, all carrying the
// identical attribute set. Counting or summing those dataPoints would report
// 226 tool calls where the session made one. Nothing here reads dataPoints;
// activity is derived from `type: "span"` records exclusively.
//
// NO COST IS ATTRIBUTED, and that is a property of the source. See
// toolCallActivity for the evidence.
//
// PRIVACY: names and identity only. An OTEL span can carry argument text in its
// attributes and arbitrary payloads in its `events` array. The `events` array
// never reaches this code at all — otelRecord has no field for it, so
// encoding/json discards it while parsing — and of the attribute map only the
// keys named by toolNameAttr / toolCallIDAttr (plus the session identity keys
// of sessionAttrs) are ever read. An exporter configured to capture message content writes it under
// keys nothing here asks for.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// opExecuteTool is the gen_ai.operation.name of a tool-call span. It is the
// primary detector: the span NAME ("execute_tool view") also carries it as a
// prefix, but the attribute is the semantic-convention field and the name is a
// display string the exporter is free to reword.
const opExecuteTool = "execute_tool"

// toolNameAttr and toolCallIDAttr are the ENTIRE allow-list of span attributes
// the activity path reads, beyond the session identity keys sessionAttrs
// already names. Both are identity: which tool, and the provider's own id for
// the call.
//
// It is an allow-list for the same reason UsageEvent.Raw uses one (issue #42):
// an exporter told to capture content writes prompts, arguments and completions
// into this same attribute map under keys that vary by exporter version, so
// denying known-bad keys would start leaking the day a new one appears. An
// unlisted key is simply never read.
const (
	toolNameAttr   = "gen_ai.tool.name"
	toolCallIDAttr = "gen_ai.tool.call.id"
)

// toolCall is the allow-listed view of one execute_tool span: those two fields
// and nothing else. Arguments, results and span events have no field here to
// land in.
type toolCall struct {
	name   string
	callID string
}

// readToolCall extracts exactly the allow-listed attributes.
func readToolCall(attrs map[string]json.RawMessage) toolCall {
	return toolCall{
		name:   attrString(attrs, toolNameAttr),
		callID: attrString(attrs, toolCallIDAttr),
	}
}

// isToolCallSpan reports whether the record is a tool-call SPAN. Metric records
// fail it twice over: they are not spans, and their attributes live inside
// dataPoints, so the top-level attribute map this reads is nil for them.
func isToolCallSpan(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if !isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "gen_ai.operation.name") == opExecuteTool {
		return true
	}
	name, ok := rawString(rec.Name)
	return ok && strings.HasPrefix(name, opExecuteTool+" ")
}

// toolCallActivity converts one execute_tool span into an activity row.
// Records that are not tool-call spans, and spans with no tool name or nothing
// stable to deduplicate on, are skipped (ok=false) rather than guessed at.
//
// NO COST IS ATTRIBUTED. Copilot's export gives no identity linking a tool call
// to the model response that requested it, verified on a live export:
//
//   - The execute_tool span's parent is the `invoke_agent` span, NOT a `chat`
//     span. It is a SIBLING of the chat spans, so the span tree names no turn.
//   - `gen_ai.tool.call.id` (toolu_…) occurs exactly once in the whole file, on
//     the call span itself. No chat span, log record or metric repeats it.
//   - The chat spans carry `github.copilot.turn_id` and `gen_ai.response.id`;
//     the execute_tool span carries neither.
//   - The only shared handle is the traceId, and a trace holds MANY chat spans
//     (two in the measured session, one usage row each), so it identifies a
//     conversation, not a turn.
//
// The remaining option would be to pick the chat span whose time window
// contains the call — a positional guess dressed as a join, which would start
// mis-attributing the moment copilot overlapped a call with the next turn (in
// the measured session the call already ran BEFORE its own chat span closed).
// So UsageDedupKey stays empty and these rows are reported as unattributed
// calls, exactly as codex's are, which is what they are. Note also that the
// parent invoke_agent span is itself SUPPRESSED as a usage record whenever a
// chat span shares its trace (filterEmitted), so even attributing to the parent
// would name a dedup key that is not in the ledger.
func toolCallActivity(rec *otelRecord, fallbackTS time.Time, traceSessions map[string]string, path string) (model.ActivityEvent, bool) {
	attrs := rec.Attributes
	if attrs == nil || !isToolCallSpan(rec, attrs) {
		return model.ActivityEvent{}, false
	}
	call := readToolCall(attrs)
	if call.name == "" {
		return model.ActivityEvent{}, false // an unnamed call is not a record of anything
	}

	trace, hasTrace := traceIDFromRecord(rec)
	span, hasSpan := spanIDFromRecord(rec)

	// Span identity first: it is what the usage dedup key of a chat span is
	// built from, it is unique per operation, and it is stable across the full
	// re-reads this adapter performs whenever the file grows. The provider's own
	// call id is the fallback for an exporter that omits span ids. A span with
	// neither is dropped: a key minted from the read (index, mtime) would mint a
	// fresh one every poll, which is an unbounded recount.
	key := ""
	switch {
	case hasTrace && hasSpan:
		key = trace + ":" + span
	case call.callID != "":
		key = "call:" + call.callID
	default:
		return model.ActivityEvent{}, false
	}

	return model.ActivityEvent{
		Tool:      model.ToolCopilot,
		Kind:      model.ActivityTool,
		Name:      call.name,
		SessionID: sessionFor(attrs, trace, hasTrace, traceSessions),
		// Model is left empty on purpose. The span names none, and the turn it
		// belongs to is exactly the fact the export does not record — naming the
		// trace's first model would be the same positional guess UsageDedupKey
		// refuses, wearing a different field.
		EventTime:   toolCallTime(rec, fallbackTS),
		TurnSeq:     0,
		CallsInTurn: 1,
		SourcePath:  path,
		DedupKey:    model.ToolCopilot + "|tool|" + key,
	}, true
}

// toolCallTime resolves when the call happened. startTime is the fact for a
// tool call — the instant the agent did the thing — where the usage path wants
// endTime (a response is complete when it ends). The record's own resolved
// timestamp and finally the file mtime are fallbacks; unlike the usage path
// that is harmless here, because the dedup key is derived from span identity
// and never from the timestamp.
func toolCallTime(rec *otelRecord, fallbackTS time.Time) time.Time {
	if t, ok := timestampFromParts(rec.StartTime); ok {
		return t
	}
	if t, ok := timestampFromScalar(rec.StartTime); ok {
		return t
	}
	if rec.hasTS {
		return rec.ts
	}
	return fallbackTS
}
