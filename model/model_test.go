package model

import "testing"

// TestReasoningModeForCoversEveryTool pins the reasoning billing mode of every
// tool id. The pricing engine bills reasoning on top of output only for the
// additive tools; flipping one of these silently double-bills (or drops) a
// whole provider's reasoning tokens, so the table is a deliberate tripwire.
func TestReasoningModeForCoversEveryTool(t *testing.T) {
	want := map[string]ReasoningMode{
		ToolClaudeCode: ReasoningSubset,
		ToolCodex:      ReasoningSubset,
		ToolHermes:     ReasoningSubset,
		// copilot: reasoning is billed as a subset of output (issue #28). The
		// adapter's total handling already assumes reasoning sits inside
		// output_tokens for Anthropic-backed models, and subset can only
		// under-bill, never charge the same token twice.
		ToolCopilot:  ReasoningSubset,
		ToolOpenCode: ReasoningAdditive,
		ToolGemini:   ReasoningAdditive,
		// Agy 1.1.22 stream JSON reports total = input + output while
		// thinking is nonzero, proving thinking is already inside output.
		ToolAgy: ReasoningSubset,
		// pi/openclaw, reasonix and dsh all report reasoning inside output, and
		// so does qwen-code on the OpenAI-compatible wires its ledger mixes in
		// under Gemini's field names. crush (no tokens), kimi-code, goose and
		// cline (no reasoning counter on any of their surfaces) are absent on
		// purpose and must keep falling back to the conservative default.
		ToolPi:       ReasoningSubset,
		ToolOpenClaw: ReasoningSubset,
		ToolReasonix: ReasoningSubset,
		ToolDSH:      ReasoningSubset,
		ToolQwenCode: ReasoningSubset,
	}
	for tool, mode := range want {
		if got := ReasoningModeFor(tool); got != mode {
			t.Errorf("ReasoningModeFor(%q) = %q, want %q", tool, got, mode)
		}
	}
	if len(reasoningModes) != len(want) {
		t.Errorf("reasoningModes has %d entries, the table asserts %d: a new tool needs a verified mode",
			len(reasoningModes), len(want))
	}
	// Unknown tools fall back to the conservative direction: never bill twice.
	if got := ReasoningModeFor("not-a-tool"); got != ReasoningSubset {
		t.Errorf("ReasoningModeFor(unknown) = %q, want %q", got, ReasoningSubset)
	}
}

// TestTransientFieldsAreNotExported guards the pricing-only enrichment that the
// ledger has no column for.
func TestTransientFieldsAreNotExported(t *testing.T) {
	e := UsageEvent{CacheTTL: CacheWriteTTL{Ephemeral5m: 1, Ephemeral1h: 2}}
	if e.ComputedTotal() != 0 {
		t.Errorf("ComputedTotal = %d, want 0: the TTL split must not enter token math", e.ComputedTotal())
	}
}
