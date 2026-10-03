package model

import "testing"

// The reasoning column is DERIVED from reasoningModes rather than restated in
// the capability table, so the pricing engine and the dashboard can never
// disagree about what a tool reports. This pins the derivation, including the
// one state the map expresses by absence.
func TestReasoningReportFollowsTheBillingModes(t *testing.T) {
	for tool, mode := range reasoningModes {
		got := ReasoningReportFor(tool)
		want := ReasoningReportSubset
		if mode == ReasoningAdditive {
			want = ReasoningReportAdditive
		}
		if got != want {
			t.Errorf("ReasoningReportFor(%q) = %q, want %q (billing mode %q)", tool, got, want, mode)
		}
	}
}

// A tool ABSENT from reasoningModes reports no reasoning count at all. It must
// NOT inherit ReasoningModeFor's conservative subset fallback: "we bill it as a
// subset because we do not know" and "the source reports a subset" are
// different claims, and only the first is true of crush, kimi-code, goose and
// cline.
func TestToolsWithNoReasoningCounterSaySo(t *testing.T) {
	for _, tool := range []string{ToolCrush, ToolKimiCode, ToolGoose, ToolCline} {
		if _, ok := reasoningModes[tool]; ok {
			t.Fatalf("%q is now in reasoningModes; this test's premise is stale", tool)
		}
		if got := ReasoningReportFor(tool); got != ReasoningReportNone {
			t.Errorf("ReasoningReportFor(%q) = %q, want %q", tool, got, ReasoningReportNone)
		}
		// The pricing fallback is unchanged and deliberately different.
		if got := ReasoningModeFor(tool); got != ReasoningSubset {
			t.Errorf("ReasoningModeFor(%q) = %q, want the conservative %q", tool, got, ReasoningSubset)
		}
	}
}

// RetiredCapabilities fills Reasoning; a caller must never receive an entry with
// an empty one just because the literal in the table omits it. An empty field
// renders as an empty line in the detail card, which reads as a rendering bug
// rather than as a missing fact.
func TestRetiredCapabilitiesAreComplete(t *testing.T) {
	got := RetiredCapabilities()
	if len(got) != len(retiredCapabilities) {
		t.Fatalf("RetiredCapabilities returned %d entries, want %d", len(got), len(retiredCapabilities))
	}
	for _, c := range got {
		if c.Tool == "" || c.Cost == "" || c.Activity == "" || c.Reasoning == "" || c.Tier == "" {
			t.Errorf("retired declaration is incomplete: %+v", c)
		}
		if c.Reasoning != ReasoningReportFor(c.Tool) {
			t.Errorf("retired %q has Reasoning=%q, want %q", c.Tool, c.Reasoning, ReasoningReportFor(c.Tool))
		}
	}
}

// The returned slice is a copy: a caller that edits what it got must not be
// editing the declaration every other caller reads.
func TestRetiredCapabilitiesReturnsACopy(t *testing.T) {
	got := RetiredCapabilities()
	if len(got) == 0 {
		t.Skip("no retired tools to copy")
	}
	got[0].Tier = "tampered"
	if again := RetiredCapabilities(); again[0].Tier == "tampered" {
		t.Error("RetiredCapabilities handed out its own backing array")
	}
}
