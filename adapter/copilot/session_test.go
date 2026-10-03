package copilot

// Tests for the two session-state surfaces, driven by fixtures scrubbed from
// this machine's real Copilot install (CLI 1.0.80): testdata/otel.jsonl (99
// spans plus 40 cumulative tool.call.count metric records),
// testdata/session-state/<id>/events.jsonl (302 records) and
// testdata/{schema,usage,secrets}.sql (the vendor's DDL verbatim plus all 45
// assistant_usage_events rows). Structure, identifiers' SHAPE and every counter
// are real; every piece of prompt, command, argument, path and description text
// is the canary below.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// canary is planted in every content-bearing field of every fixture.
const canary = "CANARY-4f1d9b02-SECRET"

// fixtureSession is the synthetic session id shared by all three fixtures — the
// session-state directory name, the OTEL gen_ai.conversation.id and the store's
// session_id, exactly as the real install shares one UUID across them.
const fixtureSession = "11111111-2222-4333-8444-555555555555"

// Facts measured on the source session and pinned here. They are the whole
// point of the fixture: a vendor change that moves any of them should fail a
// test rather than quietly move a bill.
const (
	fixtureChatSpans     = 43 // usage rows the OTEL export yields
	fixtureToolSpans     = 37 // execute_tool spans
	fixtureHookStarts    = 21 // hook.start records in events.jsonl
	fixtureSkills        = 1  // skill.invoked records
	fixtureSubAgentCalls = 3  // chat spans carrying no vendor valuation
	fixtureNanoTotal     = int64(18913170000)
	fixtureSubAgentNano  = int64(1663250000) // the 8.8% OTEL structurally omits
)

// buildFixture materialises the three surfaces under one home directory and
// returns it. The session store is built with journal_mode=WAL because that is
// how the CLI keeps it — on the reference machine the main file was 4,096 bytes
// against a 1.8 MiB WAL.
func buildFixture(t *testing.T, withStore bool) string {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, copilotDirName)
	if err := os.MkdirAll(filepath.Join(root, otelDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, filepath.Join("testdata", "otel.jsonl"),
		filepath.Join(root, otelDirName, "copilot-otel.jsonl"))

	stateDir := filepath.Join(root, sessionStateDir, fixtureSession)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, filepath.Join("testdata", "session-state", fixtureSession, eventsFileName),
		filepath.Join(stateDir, eventsFileName))
	// A second session directory with no events.jsonl: the file is written
	// lazily and its absence is normal, not an error.
	if err := os.MkdirAll(filepath.Join(root, sessionStateDir, "abandoned-session"), 0o755); err != nil {
		t.Fatal(err)
	}

	if withStore {
		buildStore(t, filepath.Join(root, costDBName))
	}
	return home
}

func copyFixture(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildStore(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"schema.sql", "secrets.sql", "usage.sql"} {
		stmt, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(stmt)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
}

// collectFixture runs one full pass over every discovered source and merges the
// observations, which is what the collector does.
func collectFixture(t *testing.T, home string) adapter.Observation {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var out adapter.Observation
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("collect %s: %v", s.Path, err)
		}
		out.Events = append(out.Events, obs.Events...)
		out.Activity = append(out.Activity, obs.Activity...)
		out.TurnContexts = append(out.TurnContexts, obs.TurnContexts...)
	}
	return out
}

func sumCost(evs []model.UsageEvent) (total int64, priced int) {
	for _, e := range evs {
		if c, ok := e.Cost(); ok {
			total += c
			priced++
		}
	}
	return total, priced
}

// TestCostIsVendorPricedAndNeverExceedsTheVendorTotal is the headline. Every
// usage row is valued from Copilot's own nano-AIU figure — no LiteLLM lookup
// anywhere — and the window's summed cost is the vendor's own total for it,
// from below.
func TestCostIsVendorPricedAndNeverExceedsTheVendorTotal(t *testing.T) {
	obs := collectFixture(t, buildFixture(t, true))
	if got := len(obs.Events); got != fixtureChatSpans {
		t.Fatalf("usage events = %d, want %d", got, fixtureChatSpans)
	}

	total, priced := sumCost(obs.Events)
	if priced != fixtureChatSpans {
		t.Errorf("priced events = %d, want all %d — a call the vendor valued must not land unpriced",
			priced, fixtureChatSpans)
	}
	ceiling := microUSDFromNanoAIU(fixtureNanoTotal)
	if total > ceiling {
		t.Errorf("stored cost = %d micro-USD, above the vendor's own %d for the same window",
			total, ceiling)
	}
	// Truncation is per row, so the sum may sit at most one micro-USD per row
	// below the vendor's total and never above it.
	if min := ceiling - int64(fixtureChatSpans); total < min {
		t.Errorf("stored cost = %d micro-USD, below %d — more than truncation can explain", total, min)
	}

	for _, e := range obs.Events {
		if e.PriceSource != PriceSourceAIU {
			t.Fatalf("price source = %q, want %q: Copilot must never be priced from the ladder",
				e.PriceSource, PriceSourceAIU)
		}
	}
}

// TestSubAgentCostComesFromTheSessionStore pins the 8.8% OTEL structurally
// loses. Sub-agent `chat` spans carry no github.copilot.nano_aiu at all, and
// the only surface that has their cost is assistant_usage_events, joined by the
// tool call that spawned them.
func TestSubAgentCostComesFromTheSessionStore(t *testing.T) {
	withStore, _ := sumCost(collectFixture(t, buildFixture(t, true)).Events)

	obs := collectFixture(t, buildFixture(t, false))
	withoutStore, priced := sumCost(obs.Events)
	if want := fixtureChatSpans - fixtureSubAgentCalls; priced != want {
		t.Fatalf("priced events without the store = %d, want %d — OTEL values every call but the sub-agent ones",
			priced, want)
	}
	// Truncation is per row, so the contribution lands within one micro-USD per
	// sub-agent span of the vendor's figure, and always below it.
	got := withStore - withoutStore
	want := microUSDFromNanoAIU(fixtureSubAgentNano)
	if got > want || got < want-fixtureSubAgentCalls {
		t.Errorf("store contributed %d micro-USD, want %d (%d nanoAIU) less at most %d of truncation",
			got, want, fixtureSubAgentNano, fixtureSubAgentCalls)
	}
	// Unattributed is UNKNOWN, never free: those rows carry no cost at all.
	for _, e := range obs.Events {
		if c, ok := e.Cost(); ok && c == 0 {
			t.Errorf("event %s stamped $0 — a missing valuation must stay nil", e.DedupKey)
		}
	}
}

// TestSubAgentCostRefusesAnAmbiguousSpawn: if a spawning tool call has more than
// one unvalued span beneath it, the store's total for that spawn belongs to no
// single span. Copying it onto each would multiply the sub-agent's cost and
// dividing would invent a split the source does not record, so neither happens.
func TestSubAgentCostRefusesAnAmbiguousSpawn(t *testing.T) {
	const tool = `{"type":"span","traceId":"t1","spanId":"tool1","name":"execute_tool task",` +
		`"startTime":[1786800000,0],"attributes":{"gen_ai.operation.name":"execute_tool",` +
		`"gen_ai.tool.name":"task","gen_ai.tool.call.id":"call_0023","gen_ai.conversation.id":"` + fixtureSession + `"}}`
	const agent = `{"type":"span","traceId":"t1","spanId":"agent1","parentSpanId":"tool1","name":"invoke_agent task",` +
		`"endTime":[1786800001,0],"attributes":{"gen_ai.operation.name":"invoke_agent",` +
		`"gen_ai.agent.name":"task","gen_ai.conversation.id":"` + fixtureSession + `"}}`
	chat := func(span string) string {
		return `{"type":"span","traceId":"t1","spanId":"` + span + `","parentSpanId":"agent1","name":"chat gpt-5-mini",` +
			`"endTime":[1786800002,0],"attributes":{"gen_ai.operation.name":"chat",` +
			`"gen_ai.response.model":"gpt-5-mini","gen_ai.conversation.id":"` + fixtureSession + `",` +
			`"gen_ai.usage.input_tokens":100,"gen_ai.usage.output_tokens":10}}`
	}

	home := t.TempDir()
	root := filepath.Join(home, copilotDirName)
	if err := os.MkdirAll(filepath.Join(root, otelDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := strings.Join([]string{tool, agent, chat("chatA"), chat("chatB")}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(root, otelDirName, "o.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	buildStore(t, filepath.Join(root, costDBName))

	obs := collectFixture(t, home)
	if len(obs.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(obs.Events))
	}
	for _, e := range obs.Events {
		if _, ok := e.Cost(); ok {
			t.Errorf("event %s was priced from an ambiguous spawn", e.DedupKey)
		}
	}
}

// TestTurnContextNamesOnlySubagentTurns. The agent dimension is populated from
// the span's own parent chain, and the session's own agent carries no
// gen_ai.agent.name — so the emptiness test is the sentinel test and no
// "default" value is ever stored.
func TestTurnContextNamesOnlySubagentTurns(t *testing.T) {
	obs := collectFixture(t, buildFixture(t, true))
	if got := len(obs.TurnContexts); got != fixtureSubAgentCalls {
		t.Fatalf("turn contexts = %d, want %d", got, fixtureSubAgentCalls)
	}
	keys := make(map[string]int)
	for _, c := range obs.TurnContexts {
		if c.Dimension != model.DimensionAgent {
			t.Errorf("dimension = %q, want %q", c.Dimension, model.DimensionAgent)
		}
		if c.Value != "task" {
			t.Errorf("value = %q, want the subagent's name", c.Value)
		}
		if c.SessionID != fixtureSession {
			t.Errorf("session = %q, want %q", c.SessionID, fixtureSession)
		}
		keys[c.UsageDedupKey]++
	}
	// (usage_dedup_key, dimension) is the store's PRIMARY KEY: two rows for one
	// turn on one dimension is unrepresentable there and must not be emitted.
	if len(keys) != fixtureSubAgentCalls {
		t.Fatalf("distinct usage keys = %d, want %d", len(keys), fixtureSubAgentCalls)
	}
	have := make(map[string]bool, len(obs.Events))
	for _, e := range obs.Events {
		have[e.DedupKey] = true
	}
	for k := range keys {
		if !have[k] {
			t.Errorf("turn context names %q, which this observation does not emit as usage", k)
		}
	}
}

// TestSkillAndHookActivityFromSessionState. OTEL names no skill and no hook;
// events.jsonl names both. And the tool calls are NOT read twice: events.jsonl
// records the same 37 calls under different ids, so only the OTEL spans produce
// tool rows.
func TestSkillAndHookActivityFromSessionState(t *testing.T) {
	obs := collectFixture(t, buildFixture(t, true))
	byKind := map[model.ActivityKind]int{}
	names := map[model.ActivityKind]map[string]int{}
	for _, a := range obs.Activity {
		byKind[a.Kind]++
		if names[a.Kind] == nil {
			names[a.Kind] = map[string]int{}
		}
		names[a.Kind][a.Name]++
	}
	for kind, want := range map[model.ActivityKind]int{
		model.ActivityTool:  fixtureToolSpans,
		model.ActivitySkill: fixtureSkills,
		model.ActivityHook:  fixtureHookStarts,
	} {
		if byKind[kind] != want {
			t.Errorf("%s rows = %d, want %d", kind, byKind[kind], want)
		}
	}
	if got := names[model.ActivitySkill]["context7-docs"]; got != 1 {
		t.Errorf("skill name counts = %v, want one context7-docs", names[model.ActivitySkill])
	}
	// Hook rows are named for the EVENT, which is the only name the record
	// carries; hook.end is deliberately not a second row.
	if got := names[model.ActivityHook]["preToolUse"]; got != fixtureHookStarts {
		t.Errorf("hook name counts = %v, want %d preToolUse", names[model.ActivityHook], fixtureHookStarts)
	}

	seen := map[string]bool{}
	for _, a := range obs.Activity {
		if seen[a.DedupKey] {
			t.Fatalf("duplicate activity dedup key %q", a.DedupKey)
		}
		seen[a.DedupKey] = true
		if a.SessionID == "" {
			t.Errorf("activity %q carries no session", a.DedupKey)
		}
		if a.UsageDedupKey != "" {
			t.Errorf("activity %q attributes cost; this surface offers no join", a.DedupKey)
		}
	}
}

// TestSessionActivityStableAcrossReReads: the keys are the provider's own event
// ids, so re-reading the whole file mints exactly the same set.
func TestSessionActivityStableAcrossReReads(t *testing.T) {
	home := buildFixture(t, true)
	first := collectFixture(t, home)
	second := collectFixture(t, home)
	keys := func(obs adapter.Observation) []string {
		var out []string
		for _, a := range obs.Activity {
			out = append(out, a.DedupKey)
		}
		for _, e := range obs.Events {
			out = append(out, e.DedupKey)
		}
		sort.Strings(out)
		return out
	}
	a, b := keys(first), keys(second)
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("dedup keys moved between passes: %d then %d keys", len(a), len(b))
	}
}

// TestSessionEventsGateOnSizeAndMtime: an unchanged file is not re-read.
func TestSessionEventsGateOnSizeAndMtime(t *testing.T) {
	home := buildFixture(t, true)
	a := New().(adapter.Incremental)
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	var src adapter.Source
	for _, s := range srcs {
		if kindOf(s) == kindEvents {
			src = s
		}
	}
	if src.Path == "" {
		t.Fatal("no session-events source discovered")
	}
	obs, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Checkpoint == nil || len(obs.Activity) == 0 {
		t.Fatalf("first pass: %d rows, checkpoint %v", len(obs.Activity), obs.Checkpoint)
	}
	again, err := a.CollectIncremental(context.Background(), src, obs.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Activity) != 0 || again.Checkpoint != nil {
		t.Fatalf("unchanged file re-read: %d rows, checkpoint %v", len(again.Activity), again.Checkpoint)
	}
}

// usageCheckpoint is the test's OWN decode of the cumulative counter. The
// production decoder has no field for it anywhere, which is why this struct has
// to exist here: the counter cannot reach the ledger even by accident.
type usageCheckpoint struct {
	Type string `json:"type"`
	Data struct {
		TotalNanoAiu int64 `json:"totalNanoAiu"`
	} `json:"data"`
}

// TestCheckpointsAreCumulativeAndAreOnlyEverAVerification is the one legitimate
// use of session.usage_checkpoint: proving our per-call sum equals the vendor's
// own running total. It also pins the trap — summing those checkpoints
// overstates by 6.6x.
func TestCheckpointsAreCumulativeAndAreOnlyEverAVerification(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "session-state", fixtureSession, eventsFileName))
	if err != nil {
		t.Fatal(err)
	}
	var last, summed int64
	var n int
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec usageCheckpoint
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Type != "session.usage_checkpoint" {
			continue
		}
		if rec.Data.TotalNanoAiu < last {
			t.Fatalf("checkpoint went backwards: %d after %d", rec.Data.TotalNanoAiu, last)
		}
		last = rec.Data.TotalNanoAiu
		summed += rec.Data.TotalNanoAiu
		n++
	}
	if n == 0 {
		t.Fatal("no checkpoints in the fixture")
	}
	if last != fixtureNanoTotal {
		t.Errorf("final checkpoint = %d, want the per-call sum %d", last, fixtureNanoTotal)
	}
	if summed <= fixtureNanoTotal*6 {
		t.Errorf("summed checkpoints = %d; the fixture no longer demonstrates the cumulative trap", summed)
	}

	// And the adapter's own figure is the final checkpoint, not the sum.
	total, _ := sumCost(collectFixture(t, buildFixture(t, true)).Events)
	if total > microUSDFromNanoAIU(last) {
		t.Errorf("stored cost %d exceeds the vendor's running total %d", total, microUSDFromNanoAIU(last))
	}
}

// TestNoSurfaceContentReachesAnyRecord is the plant-a-secret gate. Every
// content-bearing path the research names is filled with the canary in the
// fixtures — OTEL's gen_ai.tool.definitions / gen_ai.agent.description /
// enduser.pseudo.id / the span events array / an unlisted gen_ai.prompt;
// events.jsonl's user.message.data.content, assistant.message.data.content,
// tool.execution_start.data.arguments.{command,prompt,file_text,query},
// permission.requested.data.permissionRequest.toolArgs,
// hook.start.data.input.toolCalls[].args and
// skill.invoked.data.{content,path,description}; the store's turns.user_message,
// turns.assistant_response and sessions.summary — and none of it may appear in
// any field of any emitted record, raw payload included.
func TestNoSurfaceContentReachesAnyRecord(t *testing.T) {
	obs := collectFixture(t, buildFixture(t, true))
	if len(obs.Events) == 0 || len(obs.Activity) == 0 || len(obs.TurnContexts) == 0 {
		t.Fatal("fixture produced no records; the scan would prove nothing")
	}
	check := func(label string, v any, raw ...string) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", label, err)
		}
		parts := append([]string{string(b)}, raw...)
		for _, p := range parts {
			if strings.Contains(p, canary) {
				t.Fatalf("%s leaked fixture content: %s", label, p)
			}
		}
	}
	for _, e := range obs.Events {
		check("usage event", e, e.Raw)
	}
	for _, a := range obs.Activity {
		check("activity", a)
	}
	for _, c := range obs.TurnContexts {
		check("turn context", c)
	}
}

// TestSessionEventDecodeIsAnAllowList. The decoder reads two record types and,
// within them, one named field each. A record type it has not been taught
// about contributes nothing however much it carries, and a content key added
// beside a field it does read is discarded by encoding/json as it parses.
func TestSessionEventDecodeIsAnAllowList(t *testing.T) {
	lines := []string{
		// A type nobody here knows, stuffed with content.
		`{"type":"skill.something_else","id":"e1","timestamp":"2026-08-15T09:00:00.000Z",` +
			`"data":{"name":"` + canary + `","content":"` + canary + `"}}`,
		// The two known types, each carrying content beside the field read.
		`{"type":"skill.invoked","id":"e2","timestamp":"2026-08-15T09:00:01.000Z",` +
			`"data":{"name":"a-skill","content":"` + canary + `","path":"` + canary + `",` +
			`"arguments":{"command":"` + canary + `"}}}`,
		`{"type":"hook.start","id":"e3","timestamp":"2026-08-15T09:00:02.000Z",` +
			`"data":{"hookType":"preToolUse","input":{"toolCalls":[{"args":{"command":"` + canary + `"}}]}}}`,
		// No id: no stable key, so no row rather than a positional one.
		`{"type":"skill.invoked","timestamp":"2026-08-15T09:00:03.000Z","data":{"name":"unkeyed"}}`,
		// Named nothing: an unnamed invocation records nothing.
		`{"type":"hook.start","id":"e5","timestamp":"2026-08-15T09:00:04.000Z","data":{"input":{}}}`,
	}

	home := t.TempDir()
	dir := filepath.Join(home, copilotDirName, sessionStateDir, fixtureSession)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, eventsFileName),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	obs := collectFixture(t, home)
	if len(obs.Activity) != 2 {
		t.Fatalf("activity rows = %d, want 2 (the skill and the hook)", len(obs.Activity))
	}
	want := map[string]model.ActivityKind{
		"a-skill":    model.ActivitySkill,
		"preToolUse": model.ActivityHook,
	}
	for _, a := range obs.Activity {
		if want[a.Name] != a.Kind {
			t.Errorf("row %q has kind %q", a.Name, a.Kind)
		}
		b, _ := json.Marshal(a)
		if strings.Contains(string(b), canary) {
			t.Fatalf("activity leaked content: %s", b)
		}
		if !strings.HasPrefix(a.DedupKey, model.ToolCopilot+"|") ||
			!strings.Contains(a.DedupKey, fixtureSession) {
			t.Errorf("dedup key %q is not scoped to the tool and session", a.DedupKey)
		}
	}
}

// TestReadingTheSessionStoreIsObservational: a collection pass must leave every
// surface byte-identical. mode=ro cannot create or write and query_only(1)
// refuses a write statement, but the check that matters is the file itself.
func TestReadingTheSessionStoreIsObservational(t *testing.T) {
	home := buildFixture(t, true)
	root := filepath.Join(home, copilotDirName)
	dbPath := filepath.Join(root, costDBName)
	// Clear the sidecars the fixture's own writer left, so anything present
	// afterwards was materialised by the read.
	for _, s := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + s); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	paths := []string{
		dbPath,
		filepath.Join(root, otelDirName, "copilot-otel.jsonl"),
		filepath.Join(root, sessionStateDir, fixtureSession, eventsFileName),
	}
	before := make(map[string]string, len(paths))
	bytesBefore := make(map[string]string, len(paths))
	for _, p := range paths {
		before[p] = statOf(t, p)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		bytesBefore[p] = string(b)
	}

	collectFixture(t, home)

	for _, p := range paths {
		if got := statOf(t, p); got != before[p] {
			t.Errorf("%s changed during collection: %s -> %s", filepath.Base(p), before[p], got)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != bytesBefore[p] {
			t.Errorf("%s changed its bytes during collection", filepath.Base(p))
		}
	}
	// A rollback journal would mean a transaction was opened for writing.
	if _, err := os.Stat(dbPath + "-journal"); err == nil {
		t.Error("collection created a rollback journal")
	}
	// The WAL may be materialised by a reader, but it must hold no frames: no
	// frames means nothing was ever staged for writing.
	if fi, err := os.Stat(dbPath + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("collection left %d bytes in the write-ahead log", fi.Size())
	}
}

func statOf(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d bytes @ %s", fi.Size(), fi.ModTime().UTC())
}

// TestDiscoverFindsEverySurface pins what Discover returns, including the two
// absences that are normal: a session directory with no events.jsonl, and the
// store riding on the OTEL sources rather than being one.
func TestDiscoverFindsEverySurface(t *testing.T) {
	home := buildFixture(t, true)
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, s := range srcs {
		kinds[kindOf(s)]++
		if kindOf(s) == kindOTEL && costDBOf(s) == "" {
			t.Errorf("OTEL source %s carries no session store", s.Path)
		}
		if kindOf(s) == kindEvents && s.Meta[metaSession] != fixtureSession {
			t.Errorf("session-events source names session %q", s.Meta[metaSession])
		}
		if strings.HasSuffix(s.Path, costDBName) {
			t.Errorf("the session store became a source of its own: %s", s.Path)
		}
	}
	if kinds[kindOTEL] != 1 || kinds[kindEvents] != 1 {
		t.Fatalf("discovered kinds = %v, want one of each", kinds)
	}

	// Only the OTEL source can carry a token. Without that mark, an install with
	// the opt-in export switched OFF would report a token source it does not
	// have and lose doctor's enablement checklist.
	if n := adapter.CountUsageSources(srcs); n != 1 {
		t.Errorf("usage-carrying sources = %d, want 1 (the OTEL export)", n)
	}
	for _, s := range srcs {
		if got := s.CarriesUsage(); got != (kindOf(s) == kindOTEL) {
			t.Errorf("source %s (kind %s) CarriesUsage() = %v", s.Path, kindOf(s), got)
		}
	}

	// No store: the OTEL source is still discovered and simply carries none.
	plain := buildFixture(t, false)
	srcs, err = New().Discover(context.Background(), adapter.DiscoverConfig{Home: plain})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if costDBOf(s) != "" {
			t.Errorf("source %s names a store that does not exist", s.Path)
		}
	}
}
