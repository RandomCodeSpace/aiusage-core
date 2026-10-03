package pi

import "encoding/json"

// The decode layer is the privacy boundary, and it is an ALLOW-LIST rather than
// a filter applied afterwards. Every struct here names exactly the fields this
// adapter reads — ids, timestamps, model and provider names, tool names and
// counters — so encoding/json DISCARDS everything else while it parses. Prompt
// text, assistant text, thinking blocks, tool arguments, tool results, patch
// bodies and compaction summaries never become a value in this process: there
// is no field for them to land in.
//
// That is why `contentBlock` has no `arguments` and `entry` has no `summary`,
// even though both exist on disk and would have been convenient to keep.

// entry is one line of a pi/OpenClaw session transcript. The header
// (`type:"session"`) and the tree entries share this shape; unused variants
// (label, session_info, custom, custom_message) decode into it harmlessly and
// are ignored by apply.
type entry struct {
	Type      string   `json:"type"`
	ID        string   `json:"id"`
	Timestamp string   `json:"timestamp"`
	CWD       string   `json:"cwd"`      // session header only: the project path
	Provider  string   `json:"provider"` // model_change only
	ModelID   string   `json:"modelId"`  // model_change only
	Message   *message `json:"message"`  // message entries only
	// Usage on compaction / branch_summary entries: the cost of the LLM call
	// that generated the summary. `summary` itself is deliberately not decoded.
	Usage *usage `json:"usage"`
}

// message is one assistant/user/toolResult message. Only assistant messages
// carry usage; the rest are decoded to be recognised and dropped.
//
// Content is kept as raw bytes and decoded lazily by Blocks(), because the
// field is polymorphic: Pi writes a user message's content as an ARRAY of
// blocks and OpenClaw writes the same field as a plain STRING (both verified on
// live sessions). A struct that insisted on one shape would fail to decode the
// record and lose its usage object with it.
type message struct {
	Role          string          `json:"role"`
	Provider      string          `json:"provider"`
	Model         string          `json:"model"`
	API           string          `json:"api"`
	ResponseModel string          `json:"responseModel"`
	ResponseID    string          `json:"responseId"`
	StopReason    string          `json:"stopReason"`
	Usage         *usage          `json:"usage"`
	Content       json.RawMessage `json:"content"`
}

// Blocks decodes the content array, or returns nothing when the field is a
// string, absent, or anything else. A decode failure is not an error: it means
// this message has no tool calls to report.
func (m *message) Blocks() []contentBlock {
	if m == nil || len(m.Content) == 0 || m.Content[0] != '[' {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// contentBlock is one element of an assistant message's content array. Only the
// identity of a tool call survives the decode.
//
// There is NO `arguments` field, and that is the whole privacy story for
// activity: pi writes `arguments` as an arbitrary object (a shell command, a
// file path, a patch), and because nothing here names it, encoding/json throws
// it away as it parses rather than this package holding it and choosing not to
// use it. `text` and `thinking` are absent for the same reason.
type contentBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// usage is pi's usage object. input/output/cacheRead/cacheWrite are DISJOINT
// and totalTokens is their sum; reasoning is a SUBSET of output; cost is pi's
// own per-call computation in USD.
type usage struct {
	Input      *int64 `json:"input"`
	Output     *int64 `json:"output"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
	// CacheWrite1h is the SUBSET of CacheWrite written with 1h retention. Only
	// Anthropic reports the split, and it is the one token count whose price
	// differs from its own siblings: pi-ai bills it at 2x the base INPUT rate
	// while a 5m write goes at the cacheWrite rate. Dropping it would leave the
	// pricing engine to charge every write at the 5m rate — an under-price that
	// is silent because the token totals still add up.
	CacheWrite1h int64  `json:"cacheWrite1h"`
	Reasoning    *int64 `json:"reasoning"`
	// OpenClaw native transports use this spelling. Presence distinguishes
	// an explicit zero from older Pi-shaped rows using reasoning.
	ReasoningTokens *int64 `json:"reasoningTokens"`
	TotalTokens     int64  `json:"totalTokens"`
	Cost            cost   `json:"cost"`
}

// cost is the USD breakdown pi stamps beside the token counts.
type cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// auditPayload is the ALLOW-LIST persisted in UsageEvent.Raw. It is built by
// naming fields, never by stripping content out of the original line, so a new
// field the harness starts writing cannot leak into the ledger by default. It is
// an audit record, not a backfill source: everything cost and reporting need is
// already in the event's own columns.
type auditPayload struct {
	Entry         string  `json:"entry"`
	Type          string  `json:"type"`
	Timestamp     string  `json:"timestamp,omitempty"`
	Session       string  `json:"session,omitempty"`
	Provider      string  `json:"provider,omitempty"`
	Model         string  `json:"model,omitempty"`
	API           string  `json:"api,omitempty"`
	ResponseModel string  `json:"responseModel,omitempty"`
	ResponseID    string  `json:"responseId,omitempty"`
	StopReason    string  `json:"stopReason,omitempty"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cacheRead"`
	CacheWrite    int64   `json:"cacheWrite"`
	CacheWrite1h  int64   `json:"cacheWrite1h,omitempty"`
	Reasoning     int64   `json:"reasoning"`
	TotalTokens   int64   `json:"totalTokens"`
	CostUSD       float64 `json:"costUSD"`
}

// auditJSON renders the allow-listed payload. Best-effort: an un-marshalable
// payload yields an empty raw rather than dropping the event.
func auditJSON(p auditPayload) string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}
