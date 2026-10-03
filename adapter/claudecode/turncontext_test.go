package claudecode

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// secretSkillInput is planted in the input of every Skill call and in the
// argument blobs a fixture carries. No field of any emitted record may contain
// it: the skill NAME is a fact worth recording, everything the skill was handed
// is content.
const secretSkillInput = "API_KEY=sk-live-4f2a deploy --to prod --db /home/dev/secret.db"

// attribution renders the five turn-attribution fields as transcript JSON,
// omitting the ones left empty. It mirrors what Claude Code actually writes:
// scalar strings at the TOP level of an assistant record, never nested.
type attribution struct {
	agent, skill, mcpTool, mcpServer, plugin string
}

func (a attribution) json() string {
	var b strings.Builder
	for _, f := range []struct{ key, val string }{
		{"attributionAgent", a.agent},
		{"attributionSkill", a.skill},
		{"attributionMcpTool", a.mcpTool},
		{"attributionMcpServer", a.mcpServer},
		{"attributionPlugin", a.plugin},
	} {
		if f.val != "" {
			b.WriteString(`"` + f.key + `":"` + f.val + `",`)
		}
	}
	return b.String()
}

// assistantAttributed builds an assistant record carrying a usage block, any
// subset of the five attribution fields, and optional tool_use blocks — the
// exact shape Claude Code writes for an attributed turn.
func assistantAttributed(msgID, ts string, at attribution, blocks string) string {
	return assistantAttributedReq(msgID, "req-"+msgID, false, ts, at, blocks)
}

// assistantAttributedReq is the same with the request id and sidechain flag
// under the caller's control, for the multi-record cases where a message id is
// deliberately shared across several transcript lines.
func assistantAttributedReq(msgID, reqID string, sidechain bool, ts string, at attribution, blocks string) string {
	side := "false"
	if sidechain {
		side = "true"
	}
	return `{"type":"assistant","uuid":"u-` + msgID + `-` + reqID + `","timestamp":"` + ts + `",` +
		`"sessionId":"sess-1","cwd":"/home/dev/proj","requestId":"` + reqID + `",` +
		`"isSidechain":` + side + `,` + at.json() +
		`"message":{"id":"` + msgID + `","model":"claude-sonnet-4-6","role":"assistant",` +
		`"content":[` + blocks + `],` +
		`"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10}}}`
}

// assistantUnderSkill is the single-skill shorthand the older tests read with.
func assistantUnderSkill(msgID, ts, skill, blocks string) string {
	return assistantAttributed(msgID, ts, attribution{skill: skill}, blocks)
}

// contextsByDimension indexes an observation's turn contexts, failing on a
// duplicate dimension for one usage key — which the store's primary key would
// reject anyway, and which is worth catching at the adapter where it is caused.
func contextsByDimension(t *testing.T, ctxs []model.TurnContext) map[model.TurnDimension]model.TurnContext {
	t.Helper()
	out := make(map[model.TurnDimension]model.TurnContext, len(ctxs))
	for _, c := range ctxs {
		if prev, dup := out[c.Dimension]; dup {
			t.Fatalf("two contexts for dimension %q on one turn: %q and %q",
				c.Dimension, prev.Value, c.Value)
		}
		out[c.Dimension] = c
	}
	return out
}

// TestEveryDimensionIsEmittedAndPairsWithItsUsageEvent pins the join for all
// five axes at once. Each context names the dedup key of the usage event from
// the SAME record, which is what makes the store's 1:1 join to the ledger sound,
// and a turn carrying all five produces FIVE rows rather than one composite.
func TestEveryDimensionIsEmittedAndPairsWithItsUsageEvent(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantAttributed("msg-1", "2026-05-29T17:14:22.354Z", attribution{
			agent:     "workflow-subagent",
			skill:     "adhd",
			mcpTool:   "browser_eval",
			mcpServer: "ruflo",
			plugin:    "mattpocock-skills",
		}, toolBlock("toolu_1", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 usage event, got %d", len(obs.Events))
	}
	if len(obs.TurnContexts) != 5 {
		t.Fatalf("want 5 turn contexts, got %d: %+v", len(obs.TurnContexts), obs.TurnContexts)
	}

	want := map[model.TurnDimension]string{
		model.DimensionAgent:     "workflow-subagent",
		model.DimensionSkill:     "adhd",
		model.DimensionMCPTool:   "browser_eval",
		model.DimensionMCPServer: "ruflo",
		model.DimensionPlugin:    "mattpocock-skills",
	}
	byDim := contextsByDimension(t, obs.TurnContexts)
	for dim, value := range want {
		c, ok := byDim[dim]
		if !ok {
			t.Fatalf("no context emitted for dimension %q", dim)
		}
		if c.Value != value {
			t.Errorf("%s value = %q, want %q", dim, c.Value, value)
		}
		if c.UsageDedupKey != obs.Events[0].DedupKey {
			t.Errorf("%s context names usage key %q, want the event's own %q",
				dim, c.UsageDedupKey, obs.Events[0].DedupKey)
		}
		if c.Tool != model.ToolClaudeCode {
			t.Errorf("%s tool = %q, want %q", dim, c.Tool, model.ToolClaudeCode)
		}
		if !c.EventTime.Equal(obs.Events[0].EventTime) {
			t.Errorf("%s context time %s != usage time %s; the two would not window together",
				dim, c.EventTime, obs.Events[0].EventTime)
		}
		if c.SessionID != obs.Events[0].SessionID || c.Project != obs.Events[0].Project ||
			c.Model != obs.Events[0].Model {
			t.Errorf("%s context dimensions %+v do not match the event they describe", dim, c)
		}
	}

	// The tool call is unaffected: turn context does not consume a call slot, so
	// the divisor stays at the number of real calls.
	if len(obs.Activity) != 1 || obs.Activity[0].CallsInTurn != 1 {
		t.Fatalf("activity = %+v, want one Bash call with CallsInTurn=1", obs.Activity)
	}
}

// TestTurnContextUnionsAcrossAMessagesRecords is the named-bug-class test, and
// the reason the deduper unions attributions instead of reading the winner's.
//
// One API response is streamed across several transcript records sharing a
// message id. Exactly one becomes the usage row, chosen on TOKEN metrics —
// non-sidechain first, then higher total, then higher cost — which have no
// relationship whatsoever to attribution. Here the record that WINS carries no
// attribution at all and a losing record carries all five. Reading the winner's
// copy would discard every one of them, silently, exactly as taking the winner's
// tool_use blocks discarded 19,682 of 60,832 calls.
//
// It also pins the other half: the union must DEDUPLICATE. Two records agreeing
// on a dimension are one fact, not two, and a second row for the same (turn,
// dimension) would be rejected by the store's primary key at best and
// double-count at worst.
func TestTurnContextUnionsAcrossAMessagesRecords(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		// A losing sidechain record carrying every attribution and few tokens.
		assistantAttributedReq("msg-1", "req-a", true, "2026-05-29T17:14:22.354Z", attribution{
			agent:     "workflow-subagent",
			skill:     "adhd",
			mcpTool:   "browser_eval",
			mcpServer: "ruflo",
			plugin:    "mattpocock-skills",
		}, ""),
		// Another loser, agreeing on the two axes it carries.
		assistantAttributedReq("msg-1", "req-a", true, "2026-05-29T17:14:23.354Z", attribution{
			agent: "workflow-subagent",
			skill: "adhd",
		}, ""),
		// The WINNER: non-sidechain, and carrying no attribution whatsoever.
		assistantAttributedReq("msg-1", "req-a", false, "2026-05-29T17:14:24.354Z",
			attribution{}, toolBlock("toolu_1", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("dedup left %d events, want 1", len(obs.Events))
	}
	if len(obs.TurnContexts) != 5 {
		t.Fatalf("union produced %d contexts, want 5; the winner carries none, so a "+
			"winner-only read would produce 0: %+v", len(obs.TurnContexts), obs.TurnContexts)
	}
	byDim := contextsByDimension(t, obs.TurnContexts)
	if len(byDim) != 5 {
		t.Fatalf("union did not deduplicate: %+v", obs.TurnContexts)
	}
	for _, c := range obs.TurnContexts {
		// Union for the FACT, winner for the KEY: a context naming any other
		// record's key would join nothing in the store.
		if c.UsageDedupKey != obs.Events[0].DedupKey {
			t.Errorf("context %s/%s names %q, but the surviving event is %q",
				c.Dimension, c.Value, c.UsageDedupKey, obs.Events[0].DedupKey)
		}
	}
	if byDim[model.DimensionAgent].Value != "workflow-subagent" {
		t.Errorf("agent = %q, want workflow-subagent", byDim[model.DimensionAgent].Value)
	}
}

// TestTurnContextDisagreementTakesTheFirstRecordDeterministically covers the
// case the local corpus does not contain and the format does not forbid.
//
// Measured over 1,275 transcripts and 102,887 usage-bearing assistant records,
// the records of a message NEVER disagree on any of the five dimensions and
// never partially agree — every record of a message carries the same value or
// none carries the field. That is what one version of one CLI happens to write,
// not a guarantee, so the rule is stated rather than assumed: first seen wins,
// and it wins the same way on every pass, because files are walked in lexical
// order and lines in file order. A rule that flapped between two values would
// re-derive a different row every poll against a table that only accepts the
// first.
func TestTurnContextDisagreementTakesTheFirstRecordDeterministically(t *testing.T) {
	lines := []string{
		assistantAttributedReq("msg-1", "req-a", true, "2026-05-29T17:14:22.354Z",
			attribution{skill: "first-seen"}, ""),
		assistantAttributedReq("msg-1", "req-a", true, "2026-05-29T17:14:23.354Z",
			attribution{skill: "second-seen"}, ""),
		assistantAttributedReq("msg-1", "req-a", false, "2026-05-29T17:14:24.354Z",
			attribution{skill: "third-seen-and-the-dedup-winner"}, ""),
	}

	// Collected twice into separate roots: the answer must be the same one, not
	// merely a stable-looking one.
	for i := range 2 {
		root := t.TempDir()
		writeFixture(t, root, "proj", "sess-1", lines)
		obs := collectObs(t, root)
		if len(obs.TurnContexts) != 1 {
			t.Fatalf("pass %d: got %d contexts, want 1", i, len(obs.TurnContexts))
		}
		if got := obs.TurnContexts[0].Value; got != "first-seen" {
			t.Fatalf("pass %d: skill = %q, want first-seen (the dedup winner carries "+
				"third-seen, so a winner-read would say that instead)", i, got)
		}
	}
}

// TestTurnContextOnTurnWithNoToolCalls is the case the activity ledger cannot
// see at all: a turn that ran under a context and called nothing. For skills
// these are 3,361 of 8,039 records locally, so losing them would lose 41.8% of
// that answer; for agents, which have no invoking call row in the ledger at all,
// it would lose nearly everything.
func TestTurnContextOnTurnWithNoToolCalls(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantAttributed("msg-1", "2026-05-29T17:14:22.354Z",
			attribution{agent: "Explore", skill: "repo-recon"},
			`{"type":"text","text":"thinking about it"}`),
	})

	obs := collectObs(t, root)
	if len(obs.Activity) != 0 {
		t.Fatalf("want 0 activity rows for a turn with no tool calls, got %d", len(obs.Activity))
	}
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 usage event, got %d", len(obs.Events))
	}
	byDim := contextsByDimension(t, obs.TurnContexts)
	if len(byDim) != 2 ||
		byDim[model.DimensionSkill].Value != "repo-recon" ||
		byDim[model.DimensionAgent].Value != "Explore" {
		t.Fatalf("context-only turn produced %+v, want repo-recon under Explore", obs.TurnContexts)
	}
}

// TestNoTurnContextWithoutTheFields checks the absence path. Plenty of records
// carry no attribution at all (17,684 of 120,571 locally), and none of them may
// acquire a context by inference from a nearby skill invocation — "the last skill
// I saw" would keep charging a skill long after it returned.
func TestNoTurnContextWithoutTheFields(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		// A turn that INVOKES a skill is not a turn running inside one.
		assistantWithTools("msg-1", "2026-05-29T17:14:22.354Z",
			`{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"adhd","args":"`+secretSkillInput+`"}}`),
		assistantWithTools("msg-2", "2026-05-29T17:15:22.354Z", toolBlock("toolu_2", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.TurnContexts) != 0 {
		t.Fatalf("records with no attribution fields produced %d contexts, want 0: %+v",
			len(obs.TurnContexts), obs.TurnContexts)
	}
	// The v5 behaviour is untouched: the invocation is still a kind=skill row.
	var found bool
	for _, a := range obs.Activity {
		if a.Kind == model.ActivitySkill && a.Name == "adhd" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the Skill invocation row disappeared: %+v", obs.Activity)
	}
}

// TestTurnContextNestingRecordsTheInnermost documents what the source gives us
// for a skill that invokes another. Once the inner skill is running, the field
// names ONLY the inner one, so its turns are charged to it and the outer skill
// is not credited with its callee's spend. The alternative — inventing a parent
// chain the transcript does not record — would be a guess wearing a number.
func TestTurnContextNestingRecordsTheInnermost(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		// Outer skill runs, and from inside it invokes another skill.
		assistantUnderSkill("msg-1", "2026-05-29T17:14:22.354Z", "superpowers:brainstorming",
			`{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"superpowers:writing-plans","args":"`+secretSkillInput+`"}}`),
		// The inner skill's own work: attributed to the inner skill alone.
		assistantUnderSkill("msg-2", "2026-05-29T17:15:22.354Z", "superpowers:writing-plans",
			toolBlock("toolu_2", "Write")),
	})

	obs := collectObs(t, root)
	if len(obs.TurnContexts) != 2 {
		t.Fatalf("want 2 contexts, got %d: %+v", len(obs.TurnContexts), obs.TurnContexts)
	}
	got := map[string]string{}
	for _, c := range obs.TurnContexts {
		if c.Dimension != model.DimensionSkill {
			t.Fatalf("unexpected dimension %q", c.Dimension)
		}
		got[c.UsageDedupKey] = c.Value
	}
	// Exactly one context per usage event, and the second turn belongs to the
	// inner skill only.
	if len(got) != 2 {
		t.Fatalf("contexts collided on a usage key: %+v", obs.TurnContexts)
	}
	var inner int
	for _, s := range got {
		if s == "superpowers:writing-plans" {
			inner++
		}
	}
	if inner != 1 {
		t.Fatalf("skills = %+v, want exactly one turn attributed to the inner skill", got)
	}
}

// TestTurnContextPrivacy is the hard privacy assertion: a secret planted in the
// inputs around every attribution field must not survive into ANY emitted field
// of ANY stream. The enforcement is at DECODE — the five attribution fields are
// typed as strings, so a nested object of arguments has no field to land in, and
// contentBlock.Input has one field, so encoding/json discards the rest as it
// parses. The secret never becomes a value in this process; this test is what
// proves the shape holds end to end.
func TestTurnContextPrivacy(t *testing.T) {
	root := t.TempDir()
	// Every attribution field is present AND the record is littered with
	// look-alike neighbours carrying the secret: an attributionSkillInput key
	// that a prefix match would swallow, and a nested object under a sixth
	// attribution-shaped name that a "copy every attribution* key" implementation
	// would take wholesale.
	leaky := `{"type":"assistant","uuid":"u-leak","timestamp":"2026-05-29T17:14:22.354Z",` +
		`"sessionId":"sess-1","cwd":"/home/dev/proj","requestId":"req-leak",` +
		`"attributionAgent":"workflow-subagent","attributionSkill":"adhd",` +
		`"attributionMcpTool":"browser_eval","attributionMcpServer":"ruflo",` +
		`"attributionPlugin":"mattpocock-skills",` +
		`"attributionSkillInput":"` + secretSkillInput + `",` +
		`"attributionContext":{"prompt":"` + secretSkillInput + `","command":"` + secretCommand + `"},` +
		`"message":{"id":"msg-leak","model":"claude-sonnet-4-6","role":"assistant",` +
		`"content":[{"type":"tool_use","id":"toolu_1","name":"Skill",` +
		`"input":{"skill":"nested","prompt":"` + secretSkillInput + `","command":"` + secretCommand + `"}}],` +
		`"usage":{"input_tokens":100,"output_tokens":50}}}`
	writeFixture(t, root, "proj", "sess-1", []string{
		leaky,
		assistantAttributed("msg-2", "2026-05-29T17:15:22.354Z",
			attribution{agent: "workflow-subagent", skill: "adhd"}, toolBlock("toolu_2", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.TurnContexts) == 0 {
		t.Fatal("fixture produced no turn contexts; the privacy assertion would be vacuous")
	}

	// Serialise every stream in full and search the bytes. A field-by-field
	// check would miss a leak into a field added later; this cannot.
	for name, v := range map[string]any{
		"turn contexts": obs.TurnContexts,
		"activity":      obs.Activity,
		"events":        obs.Events,
	} {
		blob, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		for _, secret := range []string{secretSkillInput, secretCommand} {
			if strings.Contains(string(blob), secret) {
				t.Fatalf("%s leaked input content: %s", name, blob)
			}
		}
		// Fragments too: a partial copy is still a leak.
		for _, frag := range []string{"sk-live-4f2a", "secret.db", "id_ed25519", "prompt"} {
			if strings.Contains(string(blob), frag) {
				t.Fatalf("%s leaked the fragment %q: %s", name, frag, blob)
			}
		}
	}

	// The names themselves DID survive on every axis — otherwise the checks
	// above pass by recording nothing at all.
	byDim := contextsByDimension(t, obs.TurnContexts[:5])
	for dim, want := range map[model.TurnDimension]string{
		model.DimensionAgent:     "workflow-subagent",
		model.DimensionSkill:     "adhd",
		model.DimensionMCPTool:   "browser_eval",
		model.DimensionMCPServer: "ruflo",
		model.DimensionPlugin:    "mattpocock-skills",
	} {
		if byDim[dim].Value != want {
			t.Fatalf("%s = %q, want %q", dim, byDim[dim].Value, want)
		}
	}
}

// TestTurnContextDecodeIsAnAllowList is the structural half of the privacy
// claim, asserted against the decode shape rather than against one fixture.
//
// The rule is that turn attribution reads exactly five named string fields and
// nothing else, so a transcript that grows a sixth attribution-shaped key —
// carrying an object, a prompt, anything — contributes nothing until this
// package is taught about it on purpose. A "take every key starting with
// attribution" implementation would pass every other test in this file and fail
// this one.
func TestTurnContextDecodeIsAnAllowList(t *testing.T) {
	var rl rawLine
	line := `{"attributionAgent":"a","attributionSkill":"s","attributionMcpTool":"t",` +
		`"attributionMcpServer":"v","attributionPlugin":"p",` +
		`"attributionFuture":"future-value","attributionNested":{"x":"y"}}`
	if err := json.Unmarshal([]byte(line), &rl); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got := recordContexts(&rl)
	if len(got) != 5 {
		t.Fatalf("recordContexts returned %d contexts, want exactly the five known dimensions: %+v", len(got), got)
	}
	// Fixed emission order, so a turn's rows land the same way on every pass.
	var dims []string
	for _, c := range got {
		dims = append(dims, string(c.dim))
		if strings.Contains(c.value, "future") || strings.Contains(c.value, "y") {
			t.Fatalf("an unknown attribution key reached a context value: %+v", c)
		}
	}
	want := []string{"agent", "skill", "mcp_tool", "mcp_server", "plugin"}
	if strings.Join(dims, ",") != strings.Join(want, ",") {
		t.Fatalf("dimension order = %v, want %v (a fixed order keeps re-derived rows stable)", dims, want)
	}

	// And the vocabulary the adapter can produce is exactly the one the store
	// closes with a CHECK constraint; a mismatch would be rows skipped at insert.
	known := map[string]bool{}
	for _, d := range model.TurnDimensions() {
		known[string(d)] = true
	}
	sorted := append([]string(nil), dims...)
	sort.Strings(sorted)
	for _, d := range sorted {
		if !known[d] {
			t.Fatalf("adapter emits dimension %q, which model.TurnDimensions() does not list", d)
		}
	}
}

// TestTurnContextRawIsUnaffected checks the audit payload allow-list did not
// quietly grow an attribution field. Raw is usage-object-only; a turn context
// has its own table and does not belong there.
func TestTurnContextRawIsUnaffected(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantAttributed("msg-1", "2026-05-29T17:14:22.354Z", attribution{
			agent: "workflow-subagent", skill: "adhd", mcpTool: "browser_eval",
			mcpServer: "ruflo", plugin: "mattpocock-skills",
		}, toolBlock("toolu_1", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(obs.Events))
	}
	raw := obs.Events[0].Raw
	for _, marker := range []string{
		"adhd", "workflow-subagent", "browser_eval", "ruflo", "mattpocock-skills",
		"attributionSkill", "attributionAgent", "attributionMcp", "attributionPlugin",
	} {
		if strings.Contains(raw, marker) {
			t.Fatalf("raw audit payload grew an attribution field (%q): %s", marker, raw)
		}
	}
}

// TestMalformedAttributionKeepsTheUsageRow is the degradation guarantee, applied
// to every one of the five fields. Enrichment must never become a way to LOSE a
// usage row: a null, a number or an object where a string was expected yields no
// context and leaves the event exactly as it would have been before these fields
// existed.
func TestMalformedAttributionKeepsTheUsageRow(t *testing.T) {
	fields := []string{
		"attributionAgent", "attributionSkill",
		"attributionMcpTool", "attributionMcpServer", "attributionPlugin",
	}
	shapes := []string{`null`, `123`, `{"name":"adhd"}`, `["adhd"]`, `""`, `"   "`}
	for _, field := range fields {
		for i, shape := range shapes {
			t.Run(field+"/"+shape, func(t *testing.T) {
				root := t.TempDir()
				line := `{"type":"assistant","uuid":"u-x","timestamp":"2026-05-29T17:14:22.354Z",` +
					`"sessionId":"sess-1","cwd":"/home/dev/proj","requestId":"req-x",` +
					`"` + field + `":` + shape + `,` +
					`"message":{"id":"msg-` + fmt.Sprint(i) + `","model":"claude-sonnet-4-6","role":"assistant",` +
					`"content":[],"usage":{"input_tokens":100,"output_tokens":50}}}`
				writeFixture(t, root, "proj", "sess-1", []string{line})

				obs := collectObs(t, root)
				if len(obs.Events) != 1 {
					t.Fatalf("%s=%s cost the usage row: got %d events", field, shape, len(obs.Events))
				}
				if obs.Events[0].TotalTokens != 150 {
					t.Fatalf("usage accounting changed: total = %d, want 150", obs.Events[0].TotalTokens)
				}
				if len(obs.TurnContexts) != 0 {
					t.Fatalf("%s=%s produced a context: %+v", field, shape, obs.TurnContexts)
				}
			})
		}
	}
}

// TestTurnContextFollowsTheDedupWinner checks a sidechain replay does not produce
// a context pointing at a usage row that events() never emitted. The context must
// name the WINNING candidate's key, or it would join nothing and the surviving
// turn would show no attribution at all.
func TestTurnContextFollowsTheDedupWinner(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantAttributed("msg-1", "2026-05-29T17:14:22.354Z",
			attribution{skill: "adhd", agent: "general-purpose"}, ""),
		// A sidechain replay of the same message under a different request id.
		assistantAttributedReq("msg-1", "req-other", true, "2026-05-29T17:14:22.354Z",
			attribution{skill: "adhd", agent: "general-purpose"}, ""),
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("dedup left %d events, want 1", len(obs.Events))
	}
	if len(obs.TurnContexts) != 2 {
		t.Fatalf("dedup left %d contexts, want 2 (one per dimension, replay absorbed): %+v",
			len(obs.TurnContexts), obs.TurnContexts)
	}
	for _, c := range obs.TurnContexts {
		if c.UsageDedupKey != obs.Events[0].DedupKey {
			t.Fatalf("context names %q but the surviving event is %q",
				c.UsageDedupKey, obs.Events[0].DedupKey)
		}
	}
}
