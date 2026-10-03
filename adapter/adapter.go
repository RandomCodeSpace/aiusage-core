// Package adapter defines the read-only interface every agent-CLI integration
// implements, plus the registry that wires them together.
//
// It imports no concrete adapter and must not: a consumer who only wants the
// interface should not link fifteen file-format parsers, and the packages that
// implement this one importing it back is a cycle besides. The fan-out lives in
// adapter/all (Default). Plumbing our own adapters share hides under
// adapter/internal, so no helper becomes API surface by accident.
//
// # STRICTLY OBSERVATIONAL
//
// This is the rule the whole package exists to keep. An adapter reads local
// files and databases the harness has ALREADY written, and does nothing else:
// no writes, no modification, no write locks, no rotation, no compaction, no
// deletion, not even a lock that would make the harness wait. A user's coding
// session must be unable to notice that this project is running.
//
// Every SQLite source is opened mode=ro with query_only(1) and a busy timeout;
// immutable=1 is used NOWHERE, deliberately. A harness holds its own database
// open and writes it live, so an immutable reader can read a stale or empty
// picture rather than the file's real contents - measured on one install, a
// 4096-byte main file holding no rows at all while every row lived in the WAL
// beside it. mode=ro is the flag that cannot write; immutable=1 is the flag
// that lies.
//
// # THE SHAPE OF A READ
//
// Discover locates sources under the configured roots; Collect reads one source
// and returns an Observation. An Observation carries up to three INDEPENDENT
// streams - usage events, activity, turn contexts - plus a checkpoint that must
// only be set once a read completed, since a partial read that advances the
// checkpoint skips its own remainder forever. The streams are independent
// because a source may report calls it reports no usage for, and usage it
// reports no calls for; none of them is derived from another.
//
// Nothing in an Observation has a field for a prompt, a command, an argument or
// a file's contents. Activity and turn context are NAMES and counts, enforced by
// the model types rather than by a switch, and the raw audit payload is built
// from an allow-list of usage/model/identity fields. Content that has nowhere
// to land cannot leak.
//
// Missing, partial and corrupt inputs are normal: an adapter returns what it
// could read plus a non-fatal error, and never aborts the collection cycle for
// the other fourteen.
//
// # WHAT A CAPABILITIES DECLARATION MEANS
//
// Capabilities is a required method, so an adapter declares what this project
// can honestly say about its harness: where a cost figure came from (vendor
// stamp vs computed from a public rate card), whether a tool call can be joined
// to the turn that paid for it (exact join / recorded-but-unattributed / no
// activity at all), how the source reports reasoning tokens, and how well the
// adapter itself is verified (live against a real install, or fixture against
// constructed data). Tool MUST equal ID(), and every field MUST be set - a
// surface renders an empty field as an empty line, which reads as a rendering
// fault rather than as a missing fact.
//
// It is a compiled declaration beside the code it describes, not a table
// somewhere else, because a second statement of a fact drifts from the first:
// a sixteenth adapter does not compile until it declares itself, which beats a
// guard test reminding someone to edit a map (issue #72, decision 1). Reasoning
// is filled from model.ReasoningReportFor rather than restated, so the pricing
// engine and this declaration cannot disagree about what a source reports.
//
// It describes the ADAPTER, not an install. A declaration of "exact join" still
// yields no activity on a machine where the surface that carries it is switched
// off, the way Copilot's opt-in OTEL export can be.
//
// # THE INTERFACE IS FROZEN
//
// Capabilities was the last required method (issue #72, decision 4). Every
// future capability arrives as an OPTIONAL interface discovered by type
// assertion, the way Incremental already is: the collector asserts for it and
// falls back to a full Collect when it is absent. The reason is external
// implementers - a new required method breaks every implementation outside this
// module at once, while an optional one costs an existing adapter nothing.
//
// # THE THREE NAMED BUG CLASSES
//
// Every adapter is checked against these before it merges (CONTEXT.md). Each is
// named for a mistake that already happened here, and each misreports in the
// one direction this project promises never to go.
//
//   - SPLIT-IDENTITY RECORDS. One usage identity spans several source records.
//     Claude Code streams a single API response across transcript records that
//     share a message id and PARTITIONS its tool_use blocks between them, so
//     usage must collapse per message (keep-best) while calls and turn contexts
//     UNION across it - reading the winning record's copy silently drops what
//     its siblings carried. Cline is the same class inverted: its message
//     document is rewritten whole on every save, so no byte offset survives,
//     while the message ids inside it do not change.
//
//   - CUMULATIVE-VS-EVENT COUNTING. A surface re-exports a running counter on a
//     timer, and summing the exports multiplies the truth. Measured on a live
//     Copilot export, a session that made exactly ONE tool call had produced
//     226 identical metric dataPoints; a span is written once per operation, so
//     events come from spans and never from summing re-exports. The same trap
//     sits on that harness's cost counters, which are session-wide totals.
//
//   - ASSIGNED-NOT-ACCUMULATED COLUMNS. A column holds the LAST value rather
//     than a running total, and reading it as a total misreports. Goose's
//     sessions.total_tokens is assigned, so its adapter reads the purpose-built
//     usage_ledger and never a token column of sessions; Crush names the two
//     columns it refuses in the struct field names themselves.
//
// The standing rule underneath all three: when a source offers no honest join,
// attribute NOTHING. Codex's token counts share no identity with its call
// records, so its calls are recorded unattributed - a timestamp-nearest match
// would invent an attribution the source does not support, and an invented
// number is worse than a missing one.
package adapter

import (
	"context"
	"errors"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// ErrSourceFormat marks a discovered source whose required writer/schema
// anchors no longer match a supported surface. Adapters wrap it with the tool,
// surface and incompatible anchor so callers can distinguish upstream drift
// from valid zero usage without learning a new error type per harness.
var ErrSourceFormat = errors.New("adapter source format incompatible")

// Source is a concrete usage source discovered by an adapter — typically a
// file, directory, or database belonging to one agent CLI.
type Source struct {
	Tool  string            // owning tool id (model.ToolXxx)
	Class model.SourceClass // EventLevel or Aggregate
	Path  string            // primary path (file or db)
	Label string            // human-friendly label for `sources`
	Meta  map[string]string // adapter-specific extras (e.g. session id, dir)
}

// MetaNoUsage marks a discovered source that carries NO token usage — activity
// or turn context only.
//
// It exists because "does this tool have a data source" is asked by surfaces
// that mean TOKENS: doctor decides whether to print a tool's enablement
// checklist from it, and the TUI's By-Tool footnote states whether a token
// source exists at all (issue #44). Copilot discovers one session-state source
// per session for its skills and hooks while its tokens come only from an
// OPT-IN OTEL export, so counting those would tell a user with the export
// switched off that their token source is present and would suppress the
// checklist that is the only way to turn it on. The value is unread; presence
// of the key is the mark.
const MetaNoUsage = "no_usage"

// CarriesUsage reports whether this source can produce usage events. Sources
// say yes unless they mark themselves otherwise, so an adapter that never
// thinks about it behaves exactly as before.
func (s Source) CarriesUsage() bool {
	if s.Meta == nil {
		return true
	}
	_, marked := s.Meta[MetaNoUsage]
	return !marked
}

// CountUsageSources returns how many of these sources can produce usage events.
func CountUsageSources(srcs []Source) int {
	n := 0
	for _, s := range srcs {
		if s.CarriesUsage() {
			n++
		}
	}
	return n
}

// Observation is the result of reading a Source once. EventLevel adapters fill
// Events; Aggregate adapters fill Snapshots. An adapter may return both.
type Observation struct {
	Events    []model.UsageEvent
	Snapshots []model.AggregateSnapshot
	// Activity is the agent ACTIVITY observed in the same read: which tool was
	// called, which skill was invoked, which hook fired. It is a second,
	// independent output stream, not a derivative of Events — a source may
	// report calls it reports no usage for, and vice versa.
	//
	// An adapter that can tie a call to the usage record it rode in on MUST set
	// ActivityEvent.UsageDedupKey to that event's DedupKey and CallsInTurn to
	// the number of calls sharing it; that pairing is the whole cost
	// attribution, and it is only sound when both come from the SAME provider
	// record. An adapter that cannot must leave the key empty rather than
	// guess: a timestamp-nearest match would invent an attribution the source
	// does not support.
	//
	// PRIVACY: names and counts only. Never put a tool's input anywhere in
	// here — model.ActivityEvent has no field that would hold one.
	Activity []model.ActivityEvent
	// TurnContexts records what each observed usage event was produced UNDER —
	// which subagent, which skill, which MCP tool and server, which plugin. It
	// is a third independent stream and a property of the TURN, not of a call:
	// at most one value per (usage event, dimension), keyed by that event's
	// DedupKey, never divided among anything.
	//
	// One usage event may appear here several times, once per DIMENSION, and
	// each of those rows names the turn's FULL cost because each answers a
	// different question. They are partitions, not shares. A consumer must pin
	// exactly one dimension per query; summing across them counts the same
	// tokens once per context the turn carried. See model.TurnContext.
	//
	// An adapter MUST only emit one for a usage event it is also emitting in
	// Events, and must leave the stream empty when its source records no such
	// thing rather than inferring a context from adjacency — "the last skill
	// call I saw" is a guess that would keep charging a skill long after it
	// returned.
	//
	// PRIVACY: the NAME only — agent type, skill, MCP server/tool, plugin.
	// Never inputs, arguments, prompts or results; model.TurnContext has no
	// field that would hold one.
	TurnContexts []model.TurnContext
	// Checkpoint, when non-nil, is the source's new incremental state and MUST
	// only be set once the read completed (a partial read that advances the
	// checkpoint would skip the unread remainder forever). The collector
	// persists it in the same transaction as this observation's data; nil
	// leaves any stored checkpoint untouched.
	Checkpoint *model.SourceCheckpoint
	// CodeChanges carries the latest per-turn line-count snapshots reported
	// by a harness. These mutable counts are independent of token accounting;
	// missing data must remain unknown. No source patch text is retained.
	CodeChanges []model.CodeChange
}

// DiscoverConfig carries discovery roots and per-tool path overrides.
type DiscoverConfig struct {
	Home      string            // user home directory
	Overrides map[string]string // tool id -> explicit root path (optional)
}

// Root returns the discovery root for a tool: an explicit override if present,
// otherwise the user's home directory.
func (c DiscoverConfig) Root(tool, def string) string {
	if c.Overrides != nil {
		if v, ok := c.Overrides[tool]; ok && v != "" {
			return v
		}
	}
	if def != "" {
		return def
	}
	return c.Home
}

// Adapter reads one agent CLI's local usage data. Implementations MUST be
// read-only and must tolerate missing/partial/corrupt files without erroring
// the whole collection cycle (return best-effort results + a non-fatal error).
type Adapter interface {
	// ID is the stable tool identifier (model.ToolXxx).
	ID() string
	// DisplayName is the human-friendly name ("Claude Code").
	DisplayName() string
	// Discover locates sources under the configured roots. Read-only.
	Discover(ctx context.Context, cfg DiscoverConfig) ([]Source, error)
	// Collect reads a single source and returns its observations. Read-only.
	Collect(ctx context.Context, src Source) (Observation, error)
	// Capabilities declares what this project can actually say about the tool:
	// where a cost figure came from, whether a tool call can be joined to the
	// turn that paid for it, how the source reports reasoning tokens, and how
	// well the adapter is verified. Tool MUST equal ID(), and every field MUST
	// be set — a surface renders an empty field as an empty line, which reads as
	// a rendering fault rather than as a missing fact.
	//
	// It is a REQUIRED method rather than a table somewhere else because the
	// declaration is a statement about this code, and a statement kept beside
	// the code it describes cannot drift from it. A sixteenth adapter does not
	// compile until it declares itself, which beats a guard test reminding
	// someone to edit a map (issue #72, decision 1). The value type lives in
	// model so the dashboard reads it without importing this package.
	//
	// Reasoning is filled from model.ReasoningReportFor rather than restated:
	// there is ONE table of reasoning behaviour, and the pricing engine and this
	// declaration must never disagree about what a source reports.
	Capabilities() model.ToolCapability
}

// Incremental is an optional Adapter capability: given the checkpoint stored
// after the previous cycle, the adapter may skip unchanged data and read only
// what is new, returning the updated checkpoint on the Observation. A nil
// checkpoint (first cycle, or checkpoint lost) MUST behave exactly like
// Collect — a full read. Correctness never depends on the checkpoint; it is
// purely a work-avoidance gate.
type Incremental interface {
	CollectIncremental(ctx context.Context, src Source, cp *model.SourceCheckpoint) (Observation, error)
}

// Registry holds the set of available adapters.
type Registry struct{ adapters []Adapter }

// NewRegistry builds a registry from the given adapters.
func NewRegistry(as ...Adapter) *Registry { return &Registry{adapters: as} }

// All returns every registered adapter.
func (r *Registry) All() []Adapter { return r.adapters }

// Capabilities returns every registered adapter's own declaration, keyed by
// tool id. The adapter DECLARES and the registry AGGREGATES (issue #72,
// decision 1): there is no table anywhere that has to be edited when a
// sixteenth adapter arrives, because an adapter that does not declare itself
// does not compile.
//
// Keyed by ad.ID() rather than by the declaration's own Tool field: the registry
// is the authority on which tool an adapter IS, and the two agreeing is a
// property worth testing rather than assuming. A caller that also has to
// describe tools NO adapter collects any more - a ledger is append-only, so
// rows outlive the adapter that wrote them - lays model.RetiredCapabilities()
// down first and lets this map overwrite it, so a tool that comes back to life
// is described by its adapter rather than by the list of the departed.
func (r *Registry) Capabilities() map[string]model.ToolCapability {
	out := make(map[string]model.ToolCapability, len(r.adapters))
	for _, a := range r.adapters {
		out[a.ID()] = a.Capabilities()
	}
	return out
}

// Get returns the adapter for id, if registered.
func (r *Registry) Get(id string) (Adapter, bool) {
	for _, a := range r.adapters {
		if a.ID() == id {
			return a, true
		}
	}
	return nil, false
}
