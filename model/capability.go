package model

// capability.go carries the VOCABULARY of the per-tool capability declaration:
// where a cost figure came from, whether a tool call can be joined to the turn
// that paid for it, how the source reports reasoning tokens, and how well the
// adapter behind it is verified. It is the "capability declaration" of
// CONTEXT.md, in machine-readable form.
//
// THE VALUES ARE NOT HERE. Each adapter declares its own through the required
// adapter.Adapter.Capabilities method (issue #72, decision 1): a table in this
// package was a SECOND statement of facts whose first statement is the adapter
// source, and it drifted the moment an adapter learned to read a vendor price
// without anyone opening it. A sixteenth adapter now fails to COMPILE until it
// declares itself, which beats a guard test reminding someone to edit a map.
//
// The TYPES stay here because the dashboard is what shows them, and internal/tui
// may not import adapter (layering: model < adapter, store < collect, report,
// tui). model is the shared floor both sides already stand on, which is the same
// reason FormatCost and the reasoning modes are here.
//
// One table remains, and it is deliberately the one no adapter can own:
// retiredCapabilities, the tools nothing collects any more whose rows are still
// in the append-only ledger.

// CostProvenance for a whole tool is declared here; PriceProvenance in
// provenance.go answers the same question for one stored row. A tool declared
// CostVendor is one whose adapter stamps a cost — its rows carry that stamp and
// collect.stampCost is forbidden from overwriting it. A tool declared
// CostComputed has no such adapter code at all, so every row it produces is
// valued from the public rate card or left unpriced.

// ActivityCapture says what a tool's adapter can tell you about its tool calls.
type ActivityCapture string

const (
	// ActivityExact: the adapter emits activity rows AND names the usage row
	// that paid for each call, so the call's share of the turn is derivable.
	ActivityExact ActivityCapture = "exact join"
	// ActivityUnattributed: the adapter emits activity rows but the source gives
	// it no handle onto the usage record, so every call's cost is UNKNOWN — not
	// zero. Codex's token_count records share no identity with its call records;
	// copilot's execute_tool span is a sibling of the chat span, not its parent;
	// goose's usage_ledger rows carry no message id.
	ActivityUnattributed ActivityCapture = "recorded, unattributed"
	// ActivityNone: the adapter emits no activity rows at all. Its surface
	// exposes usage and nothing else.
	ActivityNone ActivityCapture = "none"
)

// ReasoningReport says how a tool's source reports reasoning tokens. It is the
// display face of reasoningModes plus the one state that map expresses by
// ABSENCE: a source with no reasoning counter at all.
type ReasoningReport string

const (
	// ReasoningReportSubset: reasoning is already inside the output count.
	ReasoningReportSubset ReasoningReport = "subset"
	// ReasoningReportAdditive: reasoning is reported beside output, not in it.
	ReasoningReportAdditive ReasoningReport = "additive"
	// ReasoningReportNone: the source carries no reasoning counter, so there is
	// no relationship to state. Distinct from "subset": subset is a claim about
	// a number that exists.
	ReasoningReportNone ReasoningReport = "not reported"
)

// VerificationTier is how well the adapter behind a tool is verified, in
// CONTEXT.md's vocabulary.
type VerificationTier string

const (
	// TierLive: verified against sessions actually run on a real install.
	TierLive VerificationTier = "live"
	// TierFixture: the surface format comes from a trusted source and is
	// verified against constructed fixtures, not against a real log.
	TierFixture VerificationTier = "fixture"
)

// ToolCapability is one tool's declaration.
type ToolCapability struct {
	Tool      string
	Cost      CostProvenance
	Activity  ActivityCapture
	Reasoning ReasoningReport
	Tier      VerificationTier
}

// retiredCapabilities declares the tools NO adapter collects any more. It is
// what a per-adapter declaration structurally cannot express: there is no
// adapter left to hold the statement, and usage_events is append-only, so stored
// rows still carry the id and the dashboard still has to describe where their
// numbers came from. Reasoning is deliberately absent from the literals, exactly
// as it was in the table this replaces — RetiredCapabilities fills it from
// reasoningModes, so the pricing engine and this display can never disagree
// about what a source reported.
//
// It is not a place to park a live tool. An entry here is a claim that nothing
// discovers the tool, which the composition root checks against the registry
// (TestRetiredToolsHaveNoAdapter).
var retiredCapabilities = []ToolCapability{
	// gemini is RETIRED — Antigravity replaced the Gemini CLI and no adapter
	// collects it (see the ToolGemini comment). Its declaration is exactly what
	// the deleted table stated for it.
	{Tool: ToolGemini, Cost: CostComputed, Activity: ActivityNone, Tier: TierLive},
}

// ReasoningReportFor says how a tool's source reports reasoning tokens. It reads
// reasoningModes rather than a second table: a tool present there reports the
// count with the stated relationship to output, and a tool ABSENT from it
// reports no count at all — which is why ReasoningModeFor's conservative
// subset fallback must not be used here. "Falls back to subset for pricing" and
// "reports a subset" are different statements, and only the first is true of
// crush, kimi-code, goose and cline.
func ReasoningReportFor(tool string) ReasoningReport {
	m, ok := reasoningModes[tool]
	if !ok {
		return ReasoningReportNone
	}
	if m == ReasoningAdditive {
		return ReasoningReportAdditive
	}
	return ReasoningReportSubset
}

// RetiredCapabilities returns the declarations of every tool no adapter collects
// any more, each with its Reasoning filled. The slice is a copy.
//
// The composition root merges these UNDER the registry's own declarations, so a
// tool that comes back to life is described by its adapter and not by this list.
func RetiredCapabilities() []ToolCapability {
	out := make([]ToolCapability, 0, len(retiredCapabilities))
	for _, c := range retiredCapabilities {
		c.Reasoning = ReasoningReportFor(c.Tool)
		out = append(out, c)
	}
	return out
}
