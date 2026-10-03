// Package model holds the core domain types shared by adapters, storage,
// the collector and reporting. It depends on nothing else in the project.
//
// The core types model the ledger's shape: UsageEvent is one immutable observed
// record (deduplicated on DedupKey, stored append-only); AggregateSnapshot is
// one observation of a source's cumulative counters, materialized by the
// collector into synthetic usage events via monotonic-with-reset deltas;
// SourceCheckpoint is the incremental-collection state for a source (per-source
// seam so a source can skip unchanged reads).
//
// SourceClass partitions adapters by how they expose usage: EventLevel sources
// emit discrete records (one per request/message) and deduplicate cleanly;
// Aggregate sources expose only running counters, snapshots of which the
// collector turns into synthetic delta events.
//
// Dimensioning (Tool, Provider, ServiceTier) marks each usage record for
// categorization and pricing. Tool is the agent CLI (ToolClaudeCode,
// ToolCopilot, etc.). Provider is the billing vendor whose rate card applies
// (ProviderAnthropic, etc.), taken from source data when named or stamped for
// the vendor an adapter always talks to. ServiceTier names the requested
// priority or batch mode ("standard", "batch", etc.). ReasoningMode describes
// how a tool's reasoning tokens relate to output tokens (subset: already
// contained; additive: billed on top), and the pricing engine reads it to avoid
// double-charging.
//
// Token counting: Adapters normalize provider counts into disjoint buckets
// (InputTokens, OutputTokens, CacheCreationTokens, CacheReadTokens,
// ReasoningTokens), and must set TotalTokens as the provider's own authorized
// total, which varies by vendor (cache tokens additive for Anthropic, subset of
// input for OpenAI). The collector compares both and the cost engine prices
// according to ReasoningMode and the long-context tier crossed by the whole
// prompt. Cost is stamped at collect time (CostMicroUSD) or left nil when
// unpriced; 0 never appears (unpriced and free are distinguishable).
//
// Raw is the provider payload kept for audit, built by adapters from an
// explicit allow-list of usage/model/identity fields — never stripped from a
// whole record. It is an audit payload, not a backfill source (schema columns
// carry everything cost and reporting need), and it is not marshalled by
// default (export gates it behind --include-raw; history predates the
// allow-list and still holds whole transcript lines). config privacy.no_raw
// drops it entirely via the collector.
package model

import "time"

// SourceClass distinguishes how a source exposes usage data.
type SourceClass string

const (
	// EventLevel sources expose discrete, individually-identifiable usage
	// records (one per API request / message). They deduplicate cleanly via a
	// stable DedupKey and are immune to later file deletion once stored.
	EventLevel SourceClass = "event"
	// Aggregate sources expose only running/cumulative counters. The collector
	// snapshots them and materialises positive deltas as synthetic events using
	// a monotonic-with-reset accumulator (see PLAN.md).
	Aggregate SourceClass = "aggregate"
)

// Tool identifiers — the "tool" categorisation dimension (which agent CLI).
//
// ToolGemini is retired: Antigravity replaced the Gemini CLI, so no adapter
// collects it and nothing new is stamped with it. The identifier stays because
// usage_events is append-only and existing rows still carry it — deleting it
// would leave that history without a colour, a glyph or a reasoning mode.
const (
	ToolClaudeCode = "claude-code"
	ToolCodex      = "codex"
	ToolCopilot    = "copilot"
	ToolOpenCode   = "opencode"
	ToolHermes     = "hermes"
	ToolGemini     = "gemini"
	ToolAgy        = "agy"
	ToolPi         = "pi"
	ToolOpenClaw   = "openclaw"
	ToolCrush      = "crush"
	ToolKimiCode   = "kimi-code"
	ToolReasonix   = "reasonix"
	ToolDSH        = "dsh"
	ToolQwenCode   = "qwen-code"
	ToolGoose      = "goose"
	// ToolCline names the HARNESS, not the CLI surface the clinecli adapter
	// reads: a future adapter for the same product's editor extension shares
	// this id and mints its own, non-colliding dedup keys.
	ToolCline = "cline"
)

// Billing provider identities — the "provider" dimension of a priced event
// (whose price list applies to the request). Adapters whose source data names
// the provider pass it through verbatim (opencode's providerID, hermes'
// billing_provider); the rest stamp the constant for the vendor they always
// talk to. An empty provider means unknown and is rendered as such.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderGoogle    = "google"
	ProviderGitHub    = "github"
)

// ReasoningMode describes how a tool's reported reasoning tokens relate to its
// output tokens. The pricing engine reads it to decide whether reasoning is
// already paid for by the output count or must be billed on top of it.
type ReasoningMode string

const (
	// ReasoningSubset: reasoning tokens are already contained in the reported
	// output tokens. Price output only; billing reasoning again double-charges.
	ReasoningSubset ReasoningMode = "subset"
	// ReasoningAdditive: reasoning tokens are reported alongside output and are
	// NOT part of it. Price output and reasoning, both at the output rate.
	ReasoningAdditive ReasoningMode = "additive"
)

// reasoningModes maps a tool id to its reasoning billing mode.
//
// Verified against local captures and exact versioned writers:
//   - claude-code 2.1.269: observed thinking_tokens are a subset of
//     output_tokens; the live fixture preserves both counters.
//   - codex: reasoning_output_tokens is an OpenAI subset of output_tokens.
//   - opencode: current 1.18.29 writer output excludes reasoning. Legacy
//     1.1.65 may include it; the adapter removes only overlap proven by an
//     exact provider-total identity before exposing additive counters.
//   - gemini: tokens.thoughts is reported next to tokens.output and the
//     provider total is input + output + thoughts.
//   - agy 1.1.22 stream JSON: thinking_tokens is contained in output_tokens;
//     total_tokens is exactly input_tokens + output_tokens.
//   - copilot 1.0.82 OTEL: response usage reports nonzero reasoning while
//     total = input + output, proving reasoning is contained in output.
//   - hermes v2026.9.11: the exact writer maps reasoning from output details
//     and counts output once in CanonicalUsage.total_tokens. The live cumulative
//     capture and constructed edge are in adapter/hermes/testdata.
//
// Provider coverage limitation:
//   - copilot: the OTEL export reports gen_ai.usage.reasoning.output_tokens as
//     a distinct attribute, but Copilot proxies several vendors and the adapter's
//     own total handling already asserts that reasoning sits inside output_tokens
//     for Anthropic-backed models. Subset resolves that contradiction in the
//     conservative direction: it can only ever under-bill, never charge the same
//     token twice. Revisit per backing provider once real telemetry exists.
//
// Four tools are deliberately ABSENT rather than entered as subset: crush
// reports no tokens at all (its adapter emits cost only), and kimi-code's
// TokenUsage, goose's usage_ledger and cline's per-message metrics each carry no
// reasoning counter, so none of them has a reasoning relationship to state. They
// fall back to the conservative default like any unknown id.
var reasoningModes = map[string]ReasoningMode{
	ToolClaudeCode: ReasoningSubset,
	ToolCodex:      ReasoningSubset,
	ToolOpenCode:   ReasoningAdditive,
	ToolGemini:     ReasoningAdditive,
	ToolAgy:        ReasoningSubset,
	ToolHermes:     ReasoningSubset,
	ToolCopilot:    ReasoningSubset,
	// pi/openclaw share one session format, whose types state that output
	// already includes reasoning; reasonix labels the field a subset of
	// completion; dsh reports reasoning as a subdivision of output.
	ToolPi:       ReasoningSubset,
	ToolOpenClaw: ReasoningSubset,
	ToolReasonix: ReasoningSubset,
	ToolDSH:      ReasoningSubset,
	// qwen-code's ledger has Gemini's SHAPE but is filled by whichever wire
	// answered, and thoughtsTokens sits INSIDE the output count on the
	// OpenAI-compatible ones (authType openai and qwen-oauth), while the
	// anthropic converter writes no thoughts count at all. Only the native
	// Gemini wire reports it beside output, so subset is exact on three wires
	// and can only under-bill on the fourth. See qwencode.ReasoningMode.
	ToolQwenCode: ReasoningSubset,
}

// ReasoningModeFor returns the reasoning billing mode for a tool id. An unknown
// tool falls back to ReasoningSubset — the conservative direction, which can
// under-bill but never charges the same token twice.
func ReasoningModeFor(tool string) ReasoningMode {
	if m, ok := reasoningModes[tool]; ok {
		return m
	}
	return ReasoningSubset
}

// CacheWriteTTL splits a cache-creation write by the lifetime requested for the
// entry. Anthropic prices a 1h cache write above the 5m write, so the pricing
// stamp needs the split even though the ledger stores only the combined
// CacheCreationTokens count. A zero split means the source reported none: the
// whole cache write is priced at the 5m rate.
type CacheWriteTTL struct {
	Ephemeral5m int64
	Ephemeral1h int64
}

// EventKind marks a normal usage record vs an appended correction. History is
// never rewritten; corrections are appended as KindAdjustment rows.
type EventKind string

const (
	KindUsage      EventKind = "usage"
	KindAdjustment EventKind = "adjustment"
)

// UsageEvent is one immutable observed usage record. Stored append-only and
// deduplicated on DedupKey. All token counts are non-negative.
type UsageEvent struct {
	// ID is the ledger row id (usage_events.id), set by event listing and left
	// zero on an event that has not been read back from the store. It is the
	// keyset-pagination handle: ids are AUTOINCREMENT, so they never repeat and
	// their order agrees with insertion order. It stays out of the serialised
	// event shape (json:"-") because it is storage identity rather than
	// observed usage, and the export key set is a pinned contract.
	ID    int64  `json:"-"`
	Tool  string // agent CLI id (ToolClaudeCode, ...) — categorisation dim
	Model string // model id — categorisation dim
	// Provider is the billing identity behind the request (ProviderAnthropic,
	// ...), taken from the source data when it names one. Empty means unknown.
	Provider string
	// ServiceTier is the provider's service tier for the request ("standard",
	// "batch", "priority", ...). Empty when the source reports none.
	ServiceTier string
	SessionID   string    // provider session id
	Project     string    // workspace / cwd path
	EventTime   time.Time // when the usage actually occurred (from the source)
	// ObservedTime is when the daemon read/stored the record. For aggregate
	// deltas (no real event time) EventTime is set equal to ObservedTime.
	ObservedTime time.Time

	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	ReasoningTokens     int64 // optional subset of output (e.g. codex)
	// TotalTokens is provider-authoritative; each adapter sets it correctly for
	// its provider's accounting (cache tokens are separate for Anthropic but a
	// subset of input for OpenAI/codex — adapters must not double count).
	TotalTokens int64
	// CacheTTL splits CacheCreationTokens by requested cache lifetime. It is
	// TRANSIENT adapter enrichment consumed by the pricing stamp and is never
	// persisted: the ledger has no column for it and the insert statements list
	// their columns explicitly. json:"-" keeps it out of exports too.
	CacheTTL CacheWriteTTL `json:"-"`

	// CostMicroUSD is the cost stamped at collect time, in millionths of USD,
	// or nil when the event could not be priced. nil is the ONLY "unknown":
	// a stamped 0 would claim the request was free.
	CostMicroUSD *int64
	// PriceSource names the table that produced CostMicroUSD ("override",
	// "litellm-<fetch date>", "embedded-<snapshot date>"), or the "+"-joined
	// composite of the tables that produced it together
	// ("override+litellm-<fetch date>") when a partial override was completed
	// from the rung it displaced. A row billed off a model's long-context rate
	// card carries a "+long-context" suffix as well. Empty when unpriced, so a
	// later correction knows which table it corrects.
	//
	// The vocabulary is open: the value is stored, exported and displayed
	// verbatim and nothing parses it, so a new rung or a new composite may
	// appear without a schema change. Treat it as an opaque label.
	PriceSource string

	RequestID  string // provider request id (if any)
	MessageID  string // provider message id (if any)
	SourcePath string // file/db the record came from
	DedupKey   string // globally-unique stable key; inserts conflict-skip on this
	Kind       EventKind
	// Raw is the provider usage payload kept for audit (optional). Adapters
	// build it from an explicit allow-list of usage/model/identity fields, so
	// it never carries message content; config privacy.no_raw drops it
	// entirely. It is NOT a backfill source — the schema columns carry
	// everything cost and reporting need — and it is never marshalled by
	// default: export restores it only behind --include-raw, since rows
	// appended before the allow-list landed still hold whole transcript lines.
	Raw string `json:"-"`
}

// ComputedTotal sums the token components using Anthropic-style accounting
// (cache tokens additive). Adapters that lack a provider total may use it.
func (e UsageEvent) ComputedTotal() int64 {
	return e.InputTokens + e.OutputTokens + e.CacheCreationTokens + e.CacheReadTokens
}

// Cost returns the stamped cost in micro-USD and whether the event carries one.
// A false second result means unpriced — not free.
func (e UsageEvent) Cost() (int64, bool) {
	if e.CostMicroUSD == nil {
		return 0, false
	}
	return *e.CostMicroUSD, true
}

// SetCost stamps a priced cost and the price table that produced it. Callers
// that cannot price an event must leave both fields alone rather than stamp 0.
func (e *UsageEvent) SetCost(microUSD int64, source string) {
	c := microUSD
	e.CostMicroUSD = &c
	e.PriceSource = source
}

// AggregateSnapshot is one observation of a source's cumulative/growing
// counters for a single accumulator cell. The collector compares the new
// snapshot against the last stored state for the same (Tool, Key) to derive a
// positive delta (monotonic-with-reset), which it materialises as one immutable
// usage event. Used by sources whose per-record totals GROW between polls:
//   - hermes  — per-session running totals; Key = session_id
//   - gemini  — per-turn cumulative snapshots; Key = sourcePath + "|" + turn id
//   - agy     — cumulative result.usage from captured print-mode stream JSON
//
// Key is the accumulator identity (must be stable across polls and unique per
// growing cell). SessionID/Model/Project are the reportable attributes carried
// onto the synthetic event.
type AggregateSnapshot struct {
	Tool  string
	Key   string
	Model string
	// Provider is the billing identity behind the session (ProviderAnthropic,
	// ...), carried onto the synthetic event like the other attributes.
	Provider     string
	SessionID    string
	Project      string
	ObservedTime time.Time

	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	ReasoningTokens     int64
	TotalTokens         int64

	SourcePath string
	Raw        string
}

// SourceCheckpoint is mutable per-source incremental-collection state, keyed by
// (Tool, SourcePath). It lets an adapter skip or tail-read a source whose
// content is unchanged or append-only since the last cycle. It is NOT history:
// losing a checkpoint only costs a full re-read, never data (event dedup keys
// and aggregate_state make re-reads idempotent). The dangerous direction is a
// checkpoint that outruns its data — which is why the store persists it in the
// same transaction as the events it accounts for.
type SourceCheckpoint struct {
	Tool       string
	SourcePath string
	Size       int64  // file size in bytes at the last completed read
	MTimeNS    int64  // file mtime (unix nanoseconds) at the last completed read
	Offset     int64  // byte offset consumed (append-only JSONL tail reads)
	Watermark  int64  // max rowid consumed (database sources)
	State      string // adapter-specific JSON (baselines, manifests, gates)
}
