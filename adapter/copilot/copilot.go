// Package copilot implements the event-level adapter for GitHub Copilot CLI.
//
// THREE SURFACES, ONE AUTHORITATIVE FACT EACH (issue #69). Copilot writes three
// unrelated local artefacts and each of them is the best record of exactly one
// thing; reading any of them for what another owns is how this adapter would
// double count.
//
//   - ~/.copilot/otel/**/*.jsonl — TOKENS. The OpenTelemetry JSONL the CLI
//     writes when the user enables file export, plus the single file named by
//     COPILOT_OTEL_FILE_EXPORTER_PATH. Its per-call counts are exact: measured
//     against the vendor's own ledger over one session, input 975,612, output
//     15,237, cache_read 811,374 and cache_write 56,061 agree TO THE UNIT on
//     both surfaces. It is also the only surface that carries the tool-call
//     spans this adapter's activity rows are keyed from. When it is absent the
//     adapter returns no usage at all — the export is opt-in, and doctor
//     surfaces that.
//   - ~/.copilot/session-store.db — COST, and nothing else here reads it.
//     See cost.go.
//   - ~/.copilot/session-state/<id>/events.jsonl — SKILLS AND HOOKS, which
//     OTEL names nowhere. See events.go.
//
// Each OTEL record can describe the same model call from several vantage points
// (chat span, inference log, agent-turn log, agent-summary span). We keep the
// highest-priority record per shared traceId / gen_ai.response.id and suppress
// the rest so a single call is counted once.
//
// ACTIVITY (tool calls) comes from `execute_tool` SPANS only, never from the
// metric records that share the file — see activity.go, which explains why the
// difference is a 226x one.
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY, the session store is
// opened mode=ro + query_only(1); nothing under the agent's directories is
// created, locked, or modified.
package copilot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/internal/tokenutil"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// attrMarker fast-skips JSONL lines that cannot carry usage attributes.
const attrMarker = `"attributes"`

// ExporterEnv names the single-file OTEL exporter override. It stays the ONLY
// environment variable this adapter consults: the CLI package ships no override
// for the session store or the session-state root (verified against 1.0.80), so
// there is nothing further for cmd.discoveryEnv to be taught about.
const ExporterEnv = "COPILOT_OTEL_FILE_EXPORTER_PATH"

// Layout under the discovery root. All three surfaces live side by side in one
// directory the CLI owns.
const (
	copilotDirName  = ".copilot"
	otelDirName     = "otel"
	costDBName      = "session-store.db"
	sessionStateDir = "session-state"
	eventsFileName  = "events.jsonl"
)

// Source kinds carried in Source.Meta["kind"], and the other Meta keys.
//
// The cost database rides on the OTEL source rather than being a source of its
// own, and that is forced by what cost IS here: a field of the usage row, not a
// record. A separate source could only report cost by minting a SECOND usage
// event for a call the OTEL source already reported — precisely the double
// count this split exists to avoid.
const (
	kindOTEL   = "otel"
	kindEvents = "events"

	metaKind    = "kind"
	metaCostDB  = "cost_db"
	metaSession = "session"
)

// recordSource classifies an OTEL record. Lower value = higher priority; a
// higher-priority record for the same trace/response suppresses lower ones.
type recordSource int

const (
	srcChatSpan recordSource = iota
	srcInferenceLog
	srcAgentTurnLog
	srcAgentSummarySpan
)

// model attribute keys, in preference order.
var modelAttrs = []string{"gen_ai.response.model", "gen_ai.request.model"}

// nanoAIUAttr is the vendor's per-call valuation in nano-AI-units, and
// agentNameAttr is the subagent name on an invoke_agent span. Both are read
// only from the span this adapter is already looking at.
const (
	nanoAIUAttr   = "github.copilot.nano_aiu"
	agentNameAttr = "gen_ai.agent.name"
)

// sessionAttr pairs a session attribute key with its priority (higher wins).
type sessionAttr struct {
	key      string
	priority int
}

var sessionAttrs = []sessionAttr{
	{"gen_ai.conversation.id", 3},
	{"copilot_chat.session_id", 3},
	{"copilot_chat.chat_session_id", 3},
	{"session.id", 3},
	{"github.copilot.interaction_id", 2},
	{"gen_ai.response.id", 1},
}

// Adapter reads GitHub Copilot CLI OpenTelemetry usage exports.
type Adapter struct{}

// New returns a Copilot adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolCopilot }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "GitHub Copilot" }

// Capabilities declares what this project can say about Copilot.
//
// Cost is VENDOR-reported: cost.go stamps PriceSourceAIU from the vendor's own
// nano-AI-unit valuation, which collect.stampCost is forbidden to overwrite
// with a rate-card estimate. Activity is RECORDED BUT UNATTRIBUTED because an
// execute_tool span's parent is the invoke_agent span, which makes it a SIBLING
// of the chat spans the usage rows are built from rather than their child.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolCopilot,
		Cost:      model.CostVendor,
		Activity:  model.ActivityUnattributed,
		Reasoning: model.ReasoningReportFor(model.ToolCopilot),
		Tier:      model.TierLive,
	}
}

// Discover finds every OTEL JSONL file under <root>/.copilot/otel (recursively)
// and, additively, the single file named by COPILOT_OTEL_FILE_EXPORTER_PATH,
// then every <root>/.copilot/session-state/<id>/events.jsonl. Each file becomes
// one Source; the session store rides along on the OTEL sources as
// Meta["cost_db"] rather than becoming one of its own (see the metaCostDB
// comment). Every absence is normal and reported as no source and no error: the
// OTEL export is opt-in, and events.jsonl is written LAZILY — a session opened
// and abandoned without a prompt has a directory and no events file at all
// (measured: 2 of 3 local session directories).
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	root := cfg.Root(model.ToolCopilot, "")
	seen := make(map[string]struct{})
	var sources []adapter.Source

	costDB := ""
	if root != "" {
		if p, ok := absFile(filepath.Join(root, copilotDirName, costDBName)); ok {
			costDB = p
		}
	}

	add := func(path string) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if _, dup := seen[path]; dup {
			return
		}
		seen[path] = struct{}{}
		meta := map[string]string{metaKind: kindOTEL}
		if costDB != "" {
			meta[metaCostDB] = costDB
		}
		sources = append(sources, adapter.Source{
			Tool:  model.ToolCopilot,
			Class: model.EventLevel,
			Path:  path,
			Label: "GitHub Copilot OTEL: " + path,
			Meta:  meta,
		})
	}

	if root != "" {
		otelDir := filepath.Join(root, copilotDirName, otelDirName)
		if fi, err := os.Stat(otelDir); err == nil && fi.IsDir() {
			_ = filepath.WalkDir(otelDir, func(p string, de fs.DirEntry, err error) error {
				if err != nil {
					return nil // skip unreadable subtrees, keep walking
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
					return nil
				}
				if !adapter.WalkEntryIsFile(de, p) {
					return nil
				}
				add(p)
				return nil
			})
		}
	}

	if env := strings.TrimSpace(os.Getenv(ExporterEnv)); env != "" {
		if fi, err := os.Stat(env); err == nil && !fi.IsDir() {
			add(env)
		}
	}

	if root != "" {
		stateDir := filepath.Join(root, copilotDirName, sessionStateDir)
		// os.ReadDir sorts by name, so the walk order is lexical and the same on
		// every pass. Nothing here depends on it — the dedup keys are the
		// provider's own event ids — but a deterministic order keeps a diff of
		// two runs readable.
		entries, err := os.ReadDir(stateDir)
		if err != nil {
			entries = nil // no session-state directory: normal, nothing to read
		}
		for _, de := range entries {
			if ctx.Err() != nil {
				break
			}
			if !de.IsDir() {
				continue
			}
			p, ok := absFile(filepath.Join(stateDir, de.Name(), eventsFileName))
			if !ok {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			sources = append(sources, adapter.Source{
				Tool:  model.ToolCopilot,
				Class: model.EventLevel,
				Path:  p,
				Label: "GitHub Copilot session events: " + p,
				// Marked as carrying no usage: this surface yields skills and
				// hooks and never a token, and Copilot's tokens come only from
				// the OPT-IN OTEL export. Without the mark, an install with the
				// export switched off would report a token source it does not
				// have and lose doctor's enablement checklist.
				Meta: map[string]string{
					metaKind:            kindEvents,
					metaSession:         de.Name(),
					adapter.MetaNoUsage: "session-state",
				},
			})
		}
	}

	return sources, nil
}

// absFile resolves path and reports whether it names an existing regular file.
func absFile(path string) (string, bool) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return path, true
}

// kindOf returns the source kind. An empty kind means the OTEL export, which is
// what every source was before the session-state surfaces existed.
func kindOf(src adapter.Source) string {
	if src.Meta != nil {
		if k := strings.TrimSpace(src.Meta[metaKind]); k != "" {
			return k
		}
	}
	return kindOTEL
}

// Collect parses one OTEL JSONL file, applies cross-record suppression, and
// returns the surviving usage events. Malformed lines are tolerated; a read
// that does not complete returns an error and no checkpoint so the next cycle
// retries.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental dispatches on the source kind and gates every file on
// size+mtime: unchanged files are not opened at all. A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	if kindOf(src) == kindEvents {
		return collectSessionEvents(ctx, src, cp)
	}
	return collectOTEL(ctx, src, cp)
}

// collectOTEL reads one OTEL JSONL file. Any change re-parses the whole file —
// the per-trace fallback model/session context spans all records of a file (a
// later record may supply the context an earlier usage record needs), so a tail
// read is not applicable.
func collectOTEL(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	fi, err := os.Stat(src.Path)
	if err != nil {
		return adapter.Observation{}, nil // absent/unreadable: nothing to report (opt-in feature)
	}
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()
	if cp != nil && cp.Size == size && cp.MTimeNS == mtimeNS {
		return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
	}

	newCp := &model.SourceCheckpoint{
		Tool: model.ToolCopilot, SourcePath: src.Path, Size: size, MTimeNS: mtimeNS,
	}

	records, err := readRecords(src.Path)
	if err != nil {
		// Failed or partial read: no checkpoint, so the next cycle retries even
		// if size+mtime are unchanged. Partial records are dropped too —
		// cross-record suppression needs the whole file, and an unsuppressed
		// lower-priority record would mint a distinct dedup key (double count).
		return adapter.Observation{}, fmt.Errorf("copilot: read %s: %w", src.Path, err)
	}
	if len(records) == 0 {
		return adapter.Observation{Checkpoint: newCp}, nil
	}

	fallbackTS := fi.ModTime().UTC()
	traceModels, traceSessions := collectTraceContexts(records)
	spans := spanIndex(records)

	cands := make([]*candidate, 0, len(records))
	var activity []model.ActivityEvent
	for i, rec := range records {
		if ctx.Err() != nil {
			// Partial parse: no checkpoint, so the next cycle re-reads in full.
			return adapter.Observation{
				Events:   candidatesToEvents(filterEmitted(cands), src.Path),
				Activity: activity,
			}, ctx.Err()
		}
		if c := toCandidate(rec, i, fallbackTS, traceModels, traceSessions); c != nil {
			cands = append(cands, c)
			continue // a usage record is never a tool-call span
		}
		if a, ok := toolCallActivity(rec, fallbackTS, traceSessions, src.Path); ok {
			activity = append(activity, a)
		}
	}

	emitted := filterEmitted(cands)
	attributeSubAgentCost(emitted, spans, costDBOf(src))
	events := candidatesToEvents(emitted, src.Path)

	return adapter.Observation{
		Events:       events,
		Activity:     activity,
		TurnContexts: turnContexts(emitted, events, spans, src.Path),
		Checkpoint:   newCp,
	}, nil
}

// costDBOf returns the session store path recorded on the source, if any.
func costDBOf(src adapter.Source) string {
	if src.Meta == nil {
		return ""
	}
	return strings.TrimSpace(src.Meta[metaCostDB])
}

// spanIndex maps spanId -> record for the SPAN records of one file, which is
// what the parent walk needs. Only spans have a parent chain; a log or metric
// record has none and is never a link in one.
func spanIndex(records []*otelRecord) map[string]*otelRecord {
	idx := make(map[string]*otelRecord, len(records))
	for _, rec := range records {
		if !isSpanRecord(rec) {
			continue
		}
		if id, ok := spanIDFromRecord(rec); ok {
			idx[id] = rec
		}
	}
	return idx
}

// maxSpanWalk bounds the parent walk. The chains this reads are two links long
// (chat -> invoke_agent -> execute_tool); the bound plus the visited set are
// there so a truncated or self-referential export cannot spin.
const maxSpanWalk = 8

// ancestorSpan walks up the parentSpanId chain from rec (rec itself first) and
// returns the first span matching want. A missing parent, a cycle or a chain
// longer than maxSpanWalk yields nil rather than a guess.
func ancestorSpan(rec *otelRecord, spans map[string]*otelRecord, want func(*otelRecord, map[string]json.RawMessage) bool) *otelRecord {
	seen := make(map[string]struct{}, maxSpanWalk)
	cur := rec
	for hop := 0; cur != nil && hop <= maxSpanWalk; hop++ {
		if want(cur, cur.Attributes) {
			return cur
		}
		parent, ok := rawString(cur.ParentSpanID)
		if !ok {
			return nil
		}
		if _, dup := seen[parent]; dup {
			return nil
		}
		seen[parent] = struct{}{}
		cur = spans[parent]
	}
	return nil
}

// turnContexts records the SUBAGENT a usage row ran as, along dimension
// 'agent'. It is the claude-code discipline applied to a span tree: the value
// comes from an EXACT structural join and never from adjacency.
//
// The join is the span's own parent chain, inside one file. Every `chat` span
// has an `invoke_agent` span as its direct parent (measured: 43 of 43, at zero
// hops), and that span carries `gen_ai.agent.name` when — and only when — the
// turn ran as a subagent: the session's own agent is `gen_ai.agent.id =
// github.copilot.default` with NO name attribute at all, while a subagent turn
// is `builtin:task` / name `task`. So the emptiness test IS the sentinel test,
// and there is no list of default-agent ids to keep in step with the vendor.
// Naming the default would invent an agent and collide with a real subagent of
// that name — the qwen-code "main" rule.
//
// One value per usage row per dimension, which is what usage_turn_context's
// primary key requires: a span has one parent, so a second value is not
// reachable rather than merely unlikely.
func turnContexts(cands []*candidate, events []model.UsageEvent, spans map[string]*otelRecord, path string) []model.TurnContext {
	if len(cands) != len(events) {
		return nil // candidatesToEvents is 1:1; a mismatch means the pairing is unsafe
	}
	var out []model.TurnContext
	for i, c := range cands {
		agent := ancestorSpan(c.rec, spans, isAgentSummarySpan)
		if agent == nil {
			continue
		}
		name := attrString(agent.Attributes, agentNameAttr)
		if name == "" {
			continue // the session's own agent names nothing; storing a sentinel would invent one
		}
		out = append(out, model.TurnContext{
			UsageDedupKey: events[i].DedupKey,
			Tool:          model.ToolCopilot,
			Dimension:     model.DimensionAgent,
			Value:         name,
			SessionID:     c.sessionID,
			Model:         c.model,
			EventTime:     c.eventTime,
			SourcePath:    path,
		})
	}
	return out
}

// otelRecord is the typed decode of one OTEL JSONL line. Every field stays raw
// JSON, decoded lazily only where read, so loosely-typed exporters (numeric
// strings, unexpected shapes in unread fields) never fail a line — matching
// the tolerance of the previous generic-map decode at a fraction of the
// allocations.
type otelRecord struct {
	Type         json.RawMessage            `json:"type"`
	Name         json.RawMessage            `json:"name"`
	Kind         json.RawMessage            `json:"kind"`
	TraceID      json.RawMessage            `json:"traceId"`
	SpanID       json.RawMessage            `json:"spanId"`
	ParentSpanID json.RawMessage            `json:"parentSpanId"`
	SpanContext  *spanContextObj            `json:"spanContext"`
	StartTime    json.RawMessage            `json:"startTime"`
	EndTime      json.RawMessage            `json:"endTime"`
	Duration     json.RawMessage            `json:"duration"`
	HrTime       json.RawMessage            `json:"hrTime"`
	HrTimeAlt    json.RawMessage            `json:"_hrTime"`
	Time         json.RawMessage            `json:"time"`
	Timestamp    json.RawMessage            `json:"timestamp"`
	ObservedTS   json.RawMessage            `json:"observedTimestamp"`
	TimeUnixNano json.RawMessage            `json:"timeUnixNano"`
	Body         json.RawMessage            `json:"body"`
	BodyAlt      json.RawMessage            `json:"_body"`
	Attributes   map[string]json.RawMessage `json:"attributes"`

	// ts/hasTS are resolved once at read time; stamp is the content-derived
	// dedup stamp, computed only for timestamp-less records.
	ts    time.Time
	hasTS bool
	stamp string
}

type spanContextObj struct {
	TraceID json.RawMessage `json:"traceId"`
	SpanID  json.RawMessage `json:"spanId"`
}

// readRecords streams the file line by line into typed records, keeping only
// lines that contain the "attributes" marker and decode as JSON objects. A
// non-nil error (open failure or scanner error) means the read did not
// complete; the caller must not checkpoint the file.
func readRecords(path string) ([]*otelRecord, error) {
	f, err := os.Open(path) // O_RDONLY
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []*otelRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // OTEL lines can be large
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(attrMarker)) {
			continue
		}
		rec := &otelRecord{}
		if err := json.Unmarshal(line, rec); err != nil {
			continue
		}
		rec.ts, rec.hasTS = timestampFromRecord(rec)
		if !rec.hasTS {
			rec.stamp = contentStamp(line)
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// traceContext accumulates fallback model/session for records sharing a traceId.
type traceContext struct {
	model           string
	sessionID       string
	sessionPriority int
}

// collectTraceContexts builds per-trace fallback model and session, mirroring
// ccusage: first non-empty model wins; highest-priority session wins.
func collectTraceContexts(records []*otelRecord) (map[string]string, map[string]string) {
	ctxs := make(map[string]*traceContext)
	for _, rec := range records {
		trace, ok := traceIDFromRecord(rec)
		if !ok {
			continue
		}
		if rec.Attributes == nil {
			continue
		}
		tc := ctxs[trace]
		if tc == nil {
			tc = &traceContext{}
			ctxs[trace] = tc
		}
		if tc.model == "" {
			tc.model = firstNonEmptyAttr(rec.Attributes, modelAttrs)
		}
		if sid, prio, ok := bestSessionAttr(rec.Attributes); ok && prio > tc.sessionPriority {
			tc.sessionID = sid
			tc.sessionPriority = prio
		}
	}
	models := make(map[string]string, len(ctxs))
	sessions := make(map[string]string, len(ctxs))
	for trace, tc := range ctxs {
		models[trace] = tc.model
		sessions[trace] = tc.sessionID
	}
	return models, sessions
}

// candidate is a parsed usage record awaiting suppression and conversion.
type candidate struct {
	rec        *otelRecord
	source     recordSource
	traceID    string
	hasTrace   bool
	responseID string
	model      string
	sessionID  string
	eventTime  time.Time

	input, output, cacheCreation, cacheRead, reasoning int64
	total                                              int64

	// nanoAiu is the VENDOR's own valuation of this call in nano-AI-units, from
	// the span's github.copilot.nano_aiu, or filled in from the session store
	// for the subagent calls OTEL leaves without one. 0 means unvalued, which is
	// unpriced and never free. See cost.go.
	nanoAiu int64

	dedupKey string
	raw      string
	emit     bool
}

// toCandidate classifies a record and maps its token attributes. Records that
// match no known shape, or that carry no tokens, are dropped (nil).
func toCandidate(rec *otelRecord, index int, fallbackTS time.Time, traceModels, traceSessions map[string]string) *candidate {
	attrs := rec.Attributes
	if attrs == nil {
		return nil
	}

	var source recordSource
	switch {
	case isChatSpan(rec, attrs):
		source = srcChatSpan
	case isInferenceLog(rec, attrs):
		source = srcInferenceLog
	case isAgentTurnLog(rec, attrs):
		source = srcAgentTurnLog
	case isAgentSummarySpan(rec, attrs):
		source = srcAgentSummarySpan
	default:
		return nil
	}

	input := attrNumber(attrs, "gen_ai.usage.input_tokens")
	output := attrNumber(attrs, "gen_ai.usage.output_tokens")
	cacheRead := attrNumber(attrs, "gen_ai.usage.cache_read.input_tokens")
	cacheCreation := attrNumberFirst(attrs,
		"gen_ai.usage.cache_write.input_tokens",
		"gen_ai.usage.cache_creation.input_tokens")
	reasoning := attrNumberFirst(attrs,
		"gen_ai.usage.reasoning.output_tokens",
		"gen_ai.usage.reasoning_tokens")
	totalAttr := attrNumberFirst(attrs,
		"gen_ai.usage.total_tokens",
		"gen_ai.usage.total.token_count")

	// Cache-read is a subset of input; never double-count it.
	if cacheRead > input {
		input = 0
	} else {
		input -= cacheRead
	}

	// Reconcile against the provider total. extra == reasoning per spec; the
	// fallback may fill output or grow reasoning, never shrink known parts.
	output, reasoning = tokenutil.ApplyTotalFallback(input, output, cacheCreation, cacheRead, reasoning, totalAttr)

	// Authoritative total: gen_ai.usage.total_tokens when the exporter reported
	// one. Otherwise use the component total, excluding reasoning because it is
	// already inside output_tokens. Copilot 1.0.82 live OTEL proves this directly:
	// input 15,987 + output 77 = response total 16,064 while reasoning is 64.
	// After splitting cache-read out of input, the same total is
	// 10,099 + 5,888 + 77. Adding reasoning again would overstate the call.
	total := totalAttr
	if total <= 0 {
		total = input + output + cacheCreation + cacheRead
	}
	if total == 0 {
		return nil
	}

	trace, hasTrace := traceIDFromRecord(rec)
	responseID := attrString(attrs, "gen_ai.response.id")

	mdl := firstNonEmptyAttr(attrs, modelAttrs)
	if mdl == "" && hasTrace {
		mdl = traceModels[trace]
	}
	if mdl == "" {
		mdl = "unknown"
	}

	session := sessionFor(attrs, trace, hasTrace, traceSessions)

	ts, hasTS := rec.ts, rec.hasTS
	if !hasTS {
		ts = fallbackTS
	}

	dedup := dedupKeyForRecord(source, rec, attrs, trace, hasTrace, session, ts, hasTS, index)

	return &candidate{
		rec:           rec,
		source:        source,
		traceID:       trace,
		hasTrace:      hasTrace,
		responseID:    responseID,
		model:         mdl,
		sessionID:     session,
		eventTime:     ts,
		input:         input,
		output:        output,
		cacheCreation: cacheCreation,
		cacheRead:     cacheRead,
		reasoning:     reasoning,
		total:         total,
		nanoAiu:       attrNumber(attrs, nanoAIUAttr),
		dedupKey:      dedup,
		raw:           rawAudit(rec, attrs),
	}
}

// filterEmitted marks survivors of cross-record suppression and returns them in
// original order. Suppression follows the priority chain: ChatSpan always wins;
// each lower source is dropped if a higher-priority source shares its trace or
// response id.
func filterEmitted(cands []*candidate) []*candidate {
	chatTraces := traceSet(cands, srcChatSpan)
	infTraces := traceSet(cands, srcInferenceLog)
	turnTraces := traceSet(cands, srcAgentTurnLog)
	chatResp := respSet(cands, srcChatSpan)
	infResp := respSet(cands, srcInferenceLog)
	turnResp := respSet(cands, srcAgentTurnLog)

	traceHit := func(c *candidate, set map[string]struct{}) bool {
		if !c.hasTrace {
			return false
		}
		_, ok := set[c.traceID]
		return ok
	}
	respHit := func(c *candidate, set map[string]struct{}) bool {
		if c.responseID == "" {
			return false
		}
		_, ok := set[c.responseID]
		return ok
	}

	var out []*candidate
	for _, c := range cands {
		switch c.source {
		case srcChatSpan:
			c.emit = true
		case srcInferenceLog:
			c.emit = !traceHit(c, chatTraces) && !respHit(c, chatResp)
		case srcAgentTurnLog:
			c.emit = !traceHit(c, chatTraces) && !traceHit(c, infTraces) &&
				!respHit(c, chatResp) && !respHit(c, infResp)
		case srcAgentSummarySpan:
			c.emit = !traceHit(c, chatTraces) && !traceHit(c, infTraces) && !traceHit(c, turnTraces) &&
				!respHit(c, chatResp) && !respHit(c, infResp) && !respHit(c, turnResp)
		}
		if c.emit {
			out = append(out, c)
		}
	}
	return out
}

func traceSet(cands []*candidate, src recordSource) map[string]struct{} {
	set := make(map[string]struct{})
	for _, c := range cands {
		if c.source == src && c.hasTrace {
			set[c.traceID] = struct{}{}
		}
	}
	return set
}

func respSet(cands []*candidate, src recordSource) map[string]struct{} {
	set := make(map[string]struct{})
	for _, c := range cands {
		if c.source == src && c.responseID != "" {
			set[c.responseID] = struct{}{}
		}
	}
	return set
}

// candidatesToEvents converts emitted candidates to immutable usage events.
func candidatesToEvents(cands []*candidate, path string) []model.UsageEvent {
	if len(cands) == 0 {
		return nil
	}
	evs := make([]model.UsageEvent, 0, len(cands))
	for _, c := range cands {
		ev := model.UsageEvent{
			Tool:                model.ToolCopilot,
			Model:               c.model,
			Provider:            model.ProviderGitHub,
			SessionID:           c.sessionID,
			Project:             "",
			EventTime:           c.eventTime,
			InputTokens:         c.input,
			OutputTokens:        c.output,
			CacheCreationTokens: c.cacheCreation,
			CacheReadTokens:     c.cacheRead,
			ReasoningTokens:     c.reasoning,
			TotalTokens:         c.total,
			RequestID:           c.responseID,
			SourcePath:          path,
			DedupKey:            model.ToolCopilot + "|" + c.dedupKey,
			Kind:                model.KindUsage,
			Raw:                 c.raw,
		}
		// A vendor-valued call is stamped with the vendor's number and never
		// goes near the LiteLLM ladder. A call nothing valued stays UNPRICED —
		// CostMicroUSD nil — because a stamped 0 in an append-only ledger claims
		// the request was free and can never be corrected in place.
		if micro := microUSDFromNanoAIU(c.nanoAiu); micro > 0 {
			ev.SetCost(micro, PriceSourceAIU)
		}
		evs = append(evs, ev)
	}
	return evs
}

// dedupKeyForRecord builds the per-record key string (without the tool prefix).
// Timestamp-less records (hasTS false) get a content-derived stamp instead of
// ts: ts is then the file mtime, which advances on every append and would mint
// a fresh key per poll — an unbounded recount.
func dedupKeyForRecord(source recordSource, rec *otelRecord, attrs map[string]json.RawMessage, trace string, hasTrace bool, session string, ts time.Time, hasTS bool, index int) string {
	span, hasSpan := spanIDFromRecord(rec)
	stamp := strconv.FormatInt(ts.UnixMilli(), 10)
	if !hasTS {
		stamp = rec.stamp
	}
	switch source {
	case srcChatSpan, srcAgentSummarySpan:
		if hasTrace && hasSpan {
			return trace + ":" + span
		}
		return "span:" + session + ":" + stamp + ":" + strconv.Itoa(index)
	case srcInferenceLog:
		if hasTrace && hasSpan {
			return "log:" + trace + ":" + span
		}
		return "log:" + session + ":" + stamp + ":" + strconv.Itoa(index)
	default: // srcAgentTurnLog
		turnIdx := ""
		if v, ok := numberValueRaw(attrs["turn.index"]); ok {
			turnIdx = strconv.FormatInt(v, 10)
		} else if v, ok := numberValueRaw(attrs["copilot_chat.turn.index"]); ok {
			turnIdx = strconv.FormatInt(v, 10)
		} else {
			turnIdx = "idx-" + strconv.Itoa(index)
		}
		if hasTrace {
			return "agent-turn:" + trace + ":" + turnIdx
		}
		return "agent-turn:" + session + ":" + turnIdx + ":" + strconv.Itoa(index)
	}
}

// contentStamp hashes the record's own content — never the file mtime — so a
// timestamp-less record keeps a stable dedup key across polls. The line is
// round-tripped through the generic map decode and json.Marshal (sorted keys)
// to stay byte-identical with the stamps of previously stored dedup keys; a
// direct hash of the raw line would re-key (and re-count) history.
func contentStamp(line []byte) string {
	var v map[string]any
	if err := json.Unmarshal(line, &v); err != nil {
		return "unhashable"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "unhashable"
	}
	sum := sha1.Sum(b)
	return fmt.Sprintf("c%x", sum[:8])
}

// --- record-shape detection (mirrors ccusage parser.rs) ---

func isSpanRecord(rec *otelRecord) bool {
	if t, ok := rawString(rec.Type); ok {
		return t == "span"
	}
	if _, ok := rawString(rec.Name); !ok {
		return false
	}
	if _, ok := rawString(rec.SpanID); ok {
		return true
	}
	if _, ok := rawString(rec.TraceID); ok {
		return true
	}
	// Presence checks: a present-but-null field still counts, matching the
	// previous generic-map decode.
	return len(rec.StartTime) > 0 || len(rec.EndTime) > 0 || len(rec.Duration) > 0 || len(rec.Kind) > 0
}

func isChatSpan(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if !isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "gen_ai.operation.name") == "chat" {
		return true
	}
	if name, ok := rawString(rec.Name); ok && strings.HasPrefix(name, "chat ") {
		return true
	}
	return false
}

func isAgentSummarySpan(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if !isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "gen_ai.operation.name") == "invoke_agent" {
		return true
	}
	if name, ok := rawString(rec.Name); ok && strings.HasPrefix(name, "invoke_agent ") {
		return true
	}
	return false
}

func isInferenceLog(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "event.name") == "gen_ai.client.inference.operation.details" {
		return true
	}
	if body, ok := recordBody(rec); ok && strings.HasPrefix(body, "GenAI inference:") {
		return true
	}
	return false
}

func isAgentTurnLog(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "event.name") == "copilot_chat.agent.turn" {
		return true
	}
	if body, ok := recordBody(rec); ok && strings.HasPrefix(body, "copilot_chat.agent.turn") {
		return true
	}
	return false
}

// --- field extraction helpers ---

func traceIDFromRecord(rec *otelRecord) (string, bool) {
	if v, ok := rawString(rec.TraceID); ok {
		return v, true
	}
	if rec.SpanContext != nil {
		return rawString(rec.SpanContext.TraceID)
	}
	return "", false
}

func spanIDFromRecord(rec *otelRecord) (string, bool) {
	if v, ok := rawString(rec.SpanID); ok {
		return v, true
	}
	if rec.SpanContext != nil {
		return rawString(rec.SpanContext.SpanID)
	}
	return "", false
}

func recordBody(rec *otelRecord) (string, bool) {
	if v, ok := rawString(rec.Body); ok {
		return v, true
	}
	return rawString(rec.BodyAlt)
}

// rawString decodes a raw JSON value as a trimmed non-empty string, else
// ok=false.
func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

// numberValue parses a non-negative integer from a JSON number or numeric
// string. encoding/json decodes numbers as float64; numeric strings are also
// accepted (OTEL exporters sometimes stringify values). Floats at or above
// MaxInt64 are rejected: Go's out-of-range conversion is implementation-
// specific (MinInt64 on amd64).
func numberValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || n < 0 || n >= math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			if i < 0 {
				return 0, false
			}
			return i, true
		}
		if f, err := n.Float64(); err == nil && f >= 0 && f < math.MaxInt64 {
			return int64(f), true
		}
		return 0, false
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			if i < 0 {
				return 0, false
			}
			return i, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// numberValueRaw decodes a raw JSON value (number or numeric string) through
// the same range rules as numberValue.
func numberValueRaw(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		return numberValue(num)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return numberValue(s)
	}
	return 0, false
}

func attrString(attrs map[string]json.RawMessage, key string) string {
	s, _ := rawString(attrs[key])
	return s
}

// attrNumber returns the attribute's integer value, defaulting to 0.
func attrNumber(attrs map[string]json.RawMessage, key string) int64 {
	if v, ok := numberValueRaw(attrs[key]); ok {
		return v
	}
	return 0
}

// attrNumberFirst returns the first key whose value parses to > 0, else 0.
func attrNumberFirst(attrs map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := numberValueRaw(attrs[k]); ok && v > 0 {
			return v
		}
	}
	return 0
}

func firstNonEmptyAttr(attrs map[string]json.RawMessage, keys []string) string {
	for _, k := range keys {
		if s := attrString(attrs, k); s != "" {
			return s
		}
	}
	return ""
}

// sessionFor resolves a record's session: its own highest-priority session
// attribute, else the per-trace fallback, else the trace id itself. Usage and
// activity rows share it so both ledgers place a call and its conversation
// under the same session id.
func sessionFor(attrs map[string]json.RawMessage, trace string, hasTrace bool, traceSessions map[string]string) string {
	session := ""
	if sid, _, ok := bestSessionAttr(attrs); ok {
		session = sid
	}
	if session == "" && hasTrace {
		session = traceSessions[trace]
	}
	if session == "" {
		session = trace
	}
	if session == "" {
		session = "unknown-session"
	}
	return session
}

// bestSessionAttr returns the highest-priority present session attribute.
func bestSessionAttr(attrs map[string]json.RawMessage) (string, int, bool) {
	best := ""
	bestPrio := -1
	for _, sa := range sessionAttrs {
		if s := attrString(attrs, sa.key); s != "" && sa.priority > bestPrio {
			best = s
			bestPrio = sa.priority
		}
	}
	if bestPrio < 0 {
		return "", 0, false
	}
	return best, bestPrio, true
}

// --- timestamp heuristics (mirrors ccusage parser.rs) ---

func timestampFromRecord(rec *otelRecord) (time.Time, bool) {
	for _, raw := range []json.RawMessage{rec.EndTime, rec.StartTime, rec.HrTime, rec.HrTimeAlt, rec.Time} {
		if t, ok := timestampFromParts(raw); ok {
			return t, true
		}
	}
	for _, raw := range []json.RawMessage{rec.Timestamp, rec.ObservedTS} {
		if t, ok := timestampFromScalar(raw); ok {
			return t, true
		}
	}
	if t, ok := timestampFromUnixNanos(rec.TimeUnixNano); ok {
		return t, true
	}
	return time.Time{}, false
}

// timestampFromParts reads an OTEL hrTime [seconds, nanos] pair -> ms.
func timestampFromParts(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) < 2 {
		return time.Time{}, false
	}
	sec, ok := numberValueRaw(arr[0])
	if !ok {
		return time.Time{}, false
	}
	nanos, ok := numberValueRaw(arr[1])
	if !ok {
		return time.Time{}, false
	}
	millis := sec*1000 + nanos/1_000_000
	return time.UnixMilli(millis).UTC(), true
}

// timestampFromScalar interprets a single numeric timestamp whose unit is
// inferred from magnitude (ns/us/ms/s), matching ccusage.
func timestampFromScalar(rawJSON json.RawMessage) (time.Time, bool) {
	raw, ok := numberValueRaw(rawJSON)
	if !ok {
		return time.Time{}, false
	}
	var millis int64
	switch {
	case raw >= 100_000_000_000_000_000: // nanoseconds
		millis = raw / 1_000_000
	case raw >= 100_000_000_000_000: // microseconds
		millis = raw / 1_000
	case raw >= 100_000_000_000: // milliseconds
		millis = raw
	default: // seconds
		millis = raw * 1_000
	}
	return time.UnixMilli(millis).UTC(), true
}

func timestampFromUnixNanos(rawJSON json.RawMessage) (time.Time, bool) {
	raw, ok := numberValueRaw(rawJSON)
	if !ok || raw <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(raw / 1_000_000).UTC(), true
}

// rawAttrKeys is the ALLOW-LIST of OTEL attributes persisted in
// UsageEvent.Raw: the usage counters an audit of the stored accounting needs,
// the model/provider identity behind them, and the identifiers the dedup key
// and session mapping are built from.
//
// It is an allow-list on purpose. An exporter told to capture message content
// (OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT and the Copilot-specific
// equivalents) writes prompt and completion text into the same attribute map,
// under keys that vary by exporter version — so denying known-bad keys would
// start leaking the day a new content attribute appears, while an unlisted key
// is simply never read (issue #42, closing the gap #17 left open).
var rawAttrKeys = []string{
	// Usage counters, every spelling toCandidate reads.
	"gen_ai.usage.input_tokens",
	"gen_ai.usage.output_tokens",
	"gen_ai.usage.cache_read.input_tokens",
	"gen_ai.usage.cache_write.input_tokens",
	"gen_ai.usage.cache_creation.input_tokens",
	"gen_ai.usage.reasoning.output_tokens",
	"gen_ai.usage.reasoning_tokens",
	"gen_ai.usage.total_tokens",
	"gen_ai.usage.total.token_count",
	// The vendor's own valuation of the call, which is what the stamped cost is
	// derived from and therefore part of auditing the stored accounting.
	nanoAIUAttr,
	// Model, provider and operation identity.
	"gen_ai.request.model",
	"gen_ai.response.model",
	"gen_ai.system",
	"gen_ai.provider.name",
	"gen_ai.operation.name",
	"event.name",
	// Call, conversation and turn identity: the dedup-key and session inputs.
	"gen_ai.response.id",
	"gen_ai.conversation.id",
	"copilot_chat.session_id",
	"copilot_chat.chat_session_id",
	"session.id",
	"github.copilot.interaction_id",
	"turn.index",
	"copilot_chat.turn.index",
}

// auditRecord is the stored audit payload. It mirrors the OTEL record's own
// nesting — span identity and timing at the top, the retained attributes
// below — so an auditor can compare it against the source line field for
// field, the same way the claudecode and gemini payloads mirror theirs.
type auditRecord struct {
	TraceID    string                     `json:"traceId,omitempty"`
	SpanID     string                     `json:"spanId,omitempty"`
	Timestamp  string                     `json:"timestamp,omitempty"`
	Attributes map[string]json.RawMessage `json:"attributes"`
}

// rawAudit builds the audit payload for one record from rawAttrKeys.
// Attribute values keep their original encoding (they are raw JSON) and keys
// are sorted by json.Marshal. Timestamp is the record's own resolved time and
// is omitted when the record carried none — the fallback there is the file
// mtime, which is a property of the poll, not of the record. Best-effort: an
// un-marshalable payload yields an empty raw rather than failing the parse.
func rawAudit(rec *otelRecord, attrs map[string]json.RawMessage) string {
	a := auditRecord{Attributes: make(map[string]json.RawMessage, len(rawAttrKeys))}
	if v, ok := traceIDFromRecord(rec); ok {
		a.TraceID = v
	}
	if v, ok := spanIDFromRecord(rec); ok {
		a.SpanID = v
	}
	if rec.hasTS {
		a.Timestamp = rec.ts.UTC().Format(time.RFC3339Nano)
	}
	for _, k := range rawAttrKeys {
		if v, ok := attrs[k]; ok && len(v) > 0 {
			a.Attributes[k] = v
		}
	}
	b, err := json.Marshal(a)
	if err != nil {
		return ""
	}
	return string(b)
}
