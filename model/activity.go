package model

import "time"

// ActivityKind marks what sort of invocation an ActivityEvent records. The
// vocabulary is closed and enforced by a CHECK constraint on activity_events:
// a new kind is a schema change, not a string an adapter may invent.
type ActivityKind string

const (
	// ActivityTool is one tool/function call the agent made (Bash, Read,
	// mcp__server__tool, codex's exec, opencode's bash, ...).
	ActivityTool ActivityKind = "tool"
	// ActivitySkill is a skill invocation. Claude Code models these as a tool
	// call named "Skill" whose input names the skill; the skill NAME is what
	// this row carries, so "which skill" is a group-by rather than a parse.
	ActivitySkill ActivityKind = "skill"
	// ActivityHook is a hook firing. It carries no usage of its own, so it
	// never attributes tokens.
	ActivityHook ActivityKind = "hook"
)

// ActivityEvent is one immutable observed AGENT ACTIVITY record: a tool call, a
// skill invocation or a hook firing. It is a sibling of UsageEvent, not a part
// of it — activity is not token accounting, and mixing the two would put rows
// that cost nothing into the ledger that answers "what did this cost".
//
// PRIVACY: this type is names and counts ONLY, by construction. There is no
// field for a tool's INPUT — no command string, no file path, no prompt, no
// leftover `.input` blob — and no raw payload of any kind, so `privacy.no_raw`
// has nothing to drop here. The one input field any adapter reads is the skill
// NAME of a Skill call, which is the fact being recorded rather than content.
// MCP tool names (mcp__server__tool) are names too and are kept verbatim: which
// server got called is the entire point.
//
// COST ATTRIBUTION. An activity row stores NO token counts. It carries
// UsageDedupKey, the dedup key of the usage_events row whose provider record
// contained this call, and CallsInTurn, how many calls shared that one usage
// object. Cost and tokens are derived on READ by joining to the ledger and
// dividing by CallsInTurn (see store.SummarizeActivity).
//
// That shape is the honest one. One assistant turn emits several tool_use
// blocks against a SINGLE usage object, so any per-call token count copied onto
// each row would multiply the turn's real cost by the number of calls in it.
// Storing the number once — in usage_events, where it already lives — and
// referencing it makes over-attribution structurally impossible rather than a
// rule someone has to keep remembering: the ledger stays the single source of
// cost truth, and the split is a documented read-time convention that can be
// changed without rewriting an append-only table.
type ActivityEvent struct {
	// ID is the activity row id (activity_events.id), set by listing queries and
	// left zero on a record that has not been read back from the store.
	ID   int64        `json:"-"`
	Tool string       // agent CLI id (ToolClaudeCode, ...)
	Kind ActivityKind // tool | skill | hook
	// Name is the invoked tool/skill/hook name, verbatim ("Bash", "Read",
	// "mcp__plugin__search", "artifact-design", "stop_hook_summary").
	Name string

	SessionID string    // provider session id
	Project   string    // workspace / cwd path
	Model     string    // model id of the turn, when the source names one
	EventTime time.Time // when the call happened (from the source)
	// ObservedTime is when the daemon read/stored the record.
	ObservedTime time.Time

	// UsageDedupKey is the dedup_key of the usage_events row this call is
	// attributable to, or "" when the source gives no join (codex records its
	// function calls and its token counts in unrelated records; hooks carry no
	// usage at all). It is deliberately NOT a foreign key: a call is an observed
	// fact even when its usage row was skipped as a poison row or predates
	// activity collection, and the read path left-joins, so a missing partner
	// contributes no cost instead of losing the call.
	UsageDedupKey string
	MessageID     string // provider message id (if any)
	RequestID     string // provider request id (if any)
	// TurnSeq is this call's 0-based index among the calls of its turn, and
	// CallsInTurn is how many calls that turn contained (>= 1). Together they
	// are the denominator and the tie-break of the read-time cost split.
	TurnSeq     int
	CallsInTurn int

	SourcePath string // file/db the record came from
	DedupKey   string // globally-unique stable key; inserts conflict-skip on this
}

// TurnDimension names one axis of turn attribution: the kind of thing a turn
// was running UNDER. The vocabulary is closed and enforced by a CHECK
// constraint on usage_turn_context — a new dimension is a schema change, not a
// string an adapter may invent.
//
// THE SIX PARTITIONS. These five, plus the tool-call attribution in
// activity_events, are SIX PARTITIONS OF THE SAME DOLLARS. Every one of them
// divides one window's spend a different way — which agent ran, which skill was
// active, which MCP server answered, which plugin supplied the code, which tool
// was called — the way cost-by-region and cost-by-product are two views of one
// budget. Each is honest alone. Any query that sums across two of them counts
// the same tokens twice, and no amount of care at the call site is a substitute
// for making that unexpressible, which is why the read API takes exactly one
// dimension per query and refuses to group by "dimension" at all.
type TurnDimension string

const (
	// DimensionAgent is the subagent type a turn ran as (claude-code's
	// `attributionAgent`: "workflow-subagent", "general-purpose", "Explore").
	// It is by far the largest of the five: measured over this machine's
	// transcripts, 79,816 of 102,887 usage-bearing assistant records carry it,
	// i.e. 77.6% of all token-bearing turns are subagent work that the tool-call
	// ledger describes with a couple of hundred `Agent` rows.
	DimensionAgent TurnDimension = "agent"
	// DimensionSkill is the skill a turn ran inside (`attributionSkill`). An
	// activity row of kind=skill records the turn that INVOKED a skill — one
	// call — while this records every turn the skill then spent: 44 invocation
	// rows against 8,039 records of actual work.
	DimensionSkill TurnDimension = "skill"
	// DimensionMCPTool is the MCP tool a turn was serving
	// (`attributionMcpTool`).
	DimensionMCPTool TurnDimension = "mcp_tool"
	// DimensionMCPServer is the MCP server that tool belongs to
	// (`attributionMcpServer`). Measured: it and DimensionMCPTool ALWAYS
	// co-occur — 7,084 records each, over the same 3,265 message ids, with no
	// record carrying one without the other. They are still two dimensions
	// rather than one composite value, because "what did the ruflo server cost"
	// and "what did browser_eval cost" are different questions and a composite
	// string would answer neither without parsing.
	DimensionMCPServer TurnDimension = "mcp_server"
	// DimensionPlugin is the plugin a turn's skill or agent came from
	// (`attributionPlugin`).
	DimensionPlugin TurnDimension = "plugin"
)

// turnDimensions is the closed vocabulary, in the order surfaces should offer
// it: the two that describe WHO was running first, then WHAT was being served,
// then WHERE the code came from.
var turnDimensions = []TurnDimension{
	DimensionAgent, DimensionSkill, DimensionMCPTool, DimensionMCPServer, DimensionPlugin,
}

// TurnDimensions returns the closed dimension vocabulary. The slice is a copy:
// a caller iterating it must not be able to edit the constant set.
func TurnDimensions() []TurnDimension {
	return append([]TurnDimension(nil), turnDimensions...)
}

// Valid reports whether d is one of the known dimensions. An unknown dimension
// is refused at the API boundary rather than passed to SQL, so a typo returns
// an error instead of an empty result that looks like "this agent cost nothing".
func (d TurnDimension) Valid() bool {
	for _, k := range turnDimensions {
		if k == d {
			return true
		}
	}
	return false
}

// TurnContext records that ONE usage event was produced while the agent was
// operating under ONE named thing along ONE dimension — inside a skill, as a
// subagent, serving an MCP tool, and so on. It answers "what did X cost", which
// the activity ledger alone cannot: an ActivityEvent of kind=skill records the
// turn that INVOKED a skill, not the thousands of turns the skill then went on
// to spend, and there is no activity row at all for "this turn ran as
// workflow-subagent".
//
// TURN CONTEXT IS A PROPERTY OF THE TURN, NOT A CALL WITHIN IT. That single
// distinction is why this is a separate record type rather than another
// ActivityEvent kind, and it is worth stating precisely.
//
// An ActivityEvent is one CALL. A turn emits several of them against a single
// usage object, so each takes a divided share (calls_in_turn) and the shares sum
// back to the turn. A turn context is not a call — it is the answer to "what was
// running when this turn happened", and along ANY ONE dimension a turn has AT
// MOST ONE. The source enforces that: every one of claude-code's five
// attribution fields is a scalar string on the record, verified over the whole
// local corpus (99,894 field occurrences across the five, 100% of them JSON
// strings, none an array or object), so a usage row cannot carry two agents or
// two skills. The store keys these rows by (UsageDedupKey, Dimension), which
// makes that a database constraint rather than an invariant a reader must trust.
//
// Therefore the cost of a value along a dimension is the sum over DISTINCT usage
// rows whose context is that value, with NO division and no divisor to share.
// Over-attribution WITHIN a dimension is not prevented by a rule, it is
// unrepresentable: the join to the ledger is 1:1 once the query is pinned to one
// dimension.
//
// ACROSS dimensions is the opposite, and it is the whole hazard of putting them
// in one table. A single turn commonly carries three or four contexts at once —
// measured: 3,816 records carry agent+mcp_tool+mcp_server, 2,201 carry
// agent+skill+plugin, 9 carry all five — and each of those rows names the turn's
// FULL cost, because each is a complete answer to a different question. A query
// that forgets to pin one dimension joins that turn once per context and reports
// up to five times its real cost. See store.SummarizeTurnContext, which takes the
// dimension as a required argument rather than as a filter that could be omitted.
//
// A turn context is deliberately NOT an ActivityEvent kind, for the same reason
// skill context never was: tool-call attribution and turn-context attribution
// are different partitions of the same dollars, each honest alone and
// meaningless added together. Sharing activity_events would have put that
// mistake one forgotten WHERE clause away — SummarizeActivity grouped by tool,
// with no kind filter, would silently have counted every attributed turn again.
// Nor is it a column on activity_events: 41.8% of skill-context records carry no
// tool_use block at all and emit no activity row to hang a column on, and the
// agent dimension covers turns that called nothing far more often still.
//
// NESTING is real and is recorded shallowly, because that is all the source
// offers. A skill may invoke another skill, but once the inner one is running
// the field names ONLY the inner skill, so cost lands on the innermost active
// one and an outer skill is not credited with what its callee spent. The same
// holds for an agent that spawns an agent. That is the source's own accounting,
// not a choice made here, and inventing a parent chain the transcript does not
// record would be a guess wearing a number.
//
// PRIVACY: the NAME and nothing else — agent type, skill name, MCP server and
// tool name, plugin name. There is no field for arguments, inputs, prompts or
// results, and there is no raw column, so `privacy.no_raw` has nothing to drop
// here: the discipline is satisfied by the shape rather than by a switch.
type TurnContext struct {
	// UsageDedupKey is the dedup_key of the usage_events row this context
	// describes. Together with Dimension it is the identity of the record: one
	// usage row, one value per dimension.
	UsageDedupKey string
	Tool          string // agent CLI id (ToolClaudeCode, ...)
	// Dimension names which axis this context is on. Must be one of the closed
	// vocabulary; the store's CHECK constraint refuses anything else.
	Dimension TurnDimension
	// Value is the name the turn ran under, verbatim ("workflow-subagent",
	// "adhd", "browser_eval", "ruflo", "mattpocock-skills"). Never empty — a
	// context with no value is not a context.
	Value string

	SessionID string    // provider session id
	Project   string    // workspace / cwd path
	Model     string    // model id of the turn
	EventTime time.Time // the usage event's own time, copied so both ledgers
	// place the turn in the same instant
	ObservedTime time.Time // when the daemon read/stored the record
	SourcePath   string    // file/db the record came from
}
