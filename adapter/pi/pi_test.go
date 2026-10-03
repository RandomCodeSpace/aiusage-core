package pi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// The fixtures under testdata are the REAL sessions this adapter was built
// against — recorded on 2026-08-16 from pi 0.84.2 and openclaw 2026.7.1-2, both
// driven against ollama — with every content field replaced by a synthetic
// placeholder. Structure, ids, timestamps, model names and every counter are
// verbatim; no prompt, response, tool argument or tool result is in the repo.

const (
	piRoot       = "testdata/pi/agent"
	piCompact    = "testdata/picompact/agent"
	openClawHome = "testdata/openclaw"

	// The three live pi sessions, by the uuid in their file names.
	sessErr  = "01a008bc-525d-7545-bcac-b3323ca79c5a" // a 410 from a retired model
	sessHi   = "01a008bc-b903-70e6-abd5-d625c9f14c27" // one plain turn
	sessTool = "01a008d1-48a0-7c7e-a4a2-1dda2e73c333" // a bash tool call, two turns
	sessFork = "01a008d5-4e1d-7dda-b38f-e53ade61213f" // `pi --fork` of sessTool

	// The dedup keys the fixtures produce. They are asserted literally because
	// they are the ledger's identity: a change here silently re-inserts history.
	keyHi        = "pi|1d0fe207|220d38568aa13706b14dba0b"
	keyToolCall  = "pi|c9f6aa91|a725e7648cea6696f6e02fff"
	keyToolFinal = "pi|558604b3|9618e115a8bace2ddabc78dd"
	keyOCCall    = "openclaw|f0401cbe|78010a3b5c9c480dcc2358b3"
	keyOCFinal   = "openclaw|5d2066a9|63c42736109ce9eeca48f8f4"
)

// collectAll discovers under cfg and collects every source, in discovery order.
func collectAll(t *testing.T, a adapter.Adapter, cfg adapter.DiscoverConfig) ([]adapter.Source, adapter.Observation) {
	t.Helper()
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var all adapter.Observation
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("collect %s: %v", s.Path, err)
		}
		all.Events = append(all.Events, obs.Events...)
		all.Activity = append(all.Activity, obs.Activity...)
		all.TurnContexts = append(all.TurnContexts, obs.TurnContexts...)
	}
	return srcs, all
}

func piCfg(t *testing.T, agentDir string) adapter.DiscoverConfig {
	t.Helper()
	t.Setenv(AgentDirEnv, agentDir)
	t.Setenv(SessionDirEnv, "")
	return adapter.DiscoverConfig{Home: t.TempDir()}
}

func eventByKey(evs []model.UsageEvent, key string) (model.UsageEvent, bool) {
	for _, e := range evs {
		if e.DedupKey == key {
			return e, true
		}
	}
	return model.UsageEvent{}, false
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

// TestPiFixtureEventsAreExact pins every usage row the live pi corpus produces,
// field by field. It is the adapter's contract: the dedup keys, the token split,
// the provider passthrough, the project taken from the header cwd.
func TestPiFixtureEventsAreExact(t *testing.T) {
	cfg := piCfg(t, piRoot)
	srcs, obs := collectAll(t, NewPi(), cfg)

	if len(srcs) != 4 {
		t.Fatalf("discovered %d sources, want 4 (three sessions + one fork)", len(srcs))
	}
	// The fork repeats sessTool's two turns under identical keys, so the corpus
	// holds five rows but only three distinct ones.
	if len(obs.Events) != 5 {
		t.Fatalf("collected %d events, want 5", len(obs.Events))
	}

	want := []struct {
		key      string
		session  string
		model    string
		provider string
		when     string
		in       int64
		out      int64
		total    int64
		msgID    string
	}{
		{keyHi, sessHi, "gemma4:31b-cloud", "ollama", "2026-08-16T04:03:01.760Z", 1048, 9, 1057, "chatcmpl-127"},
		{keyToolCall, sessTool, "gemma4:31b-cloud", "ollama", "2026-08-16T04:25:29.163Z", 1220, 14, 1234, "chatcmpl-938"},
		{keyToolFinal, sessTool, "gemma4:31b-cloud", "ollama", "2026-08-16T04:25:29.741Z", 1242, 7, 1249, "chatcmpl-956"},
	}
	for _, w := range want {
		e, ok := eventByKey(obs.Events, w.key)
		if !ok {
			t.Fatalf("no event with dedup key %q", w.key)
		}
		if e.Tool != model.ToolPi {
			t.Errorf("%s tool = %q, want %q", w.key, e.Tool, model.ToolPi)
		}
		if e.SessionID != w.session {
			t.Errorf("%s session = %q, want %q", w.key, e.SessionID, w.session)
		}
		if e.Model != w.model || e.Provider != w.provider {
			t.Errorf("%s model/provider = %q/%q, want %q/%q", w.key, e.Model, e.Provider, w.model, w.provider)
		}
		if got := e.EventTime; !got.Equal(mustTime(t, w.when)) {
			t.Errorf("%s time = %s, want %s", w.key, got, w.when)
		}
		if e.InputTokens != w.in || e.OutputTokens != w.out || e.TotalTokens != w.total {
			t.Errorf("%s tokens = %d/%d/%d, want %d/%d/%d",
				w.key, e.InputTokens, e.OutputTokens, e.TotalTokens, w.in, w.out, w.total)
		}
		if e.CacheReadTokens != 0 || e.CacheCreationTokens != 0 || e.ReasoningTokens != 0 {
			t.Errorf("%s cache/reasoning = %d/%d/%d, want zeros",
				w.key, e.CacheReadTokens, e.CacheCreationTokens, e.ReasoningTokens)
		}
		if e.MessageID != w.msgID {
			t.Errorf("%s messageID = %q, want %q", w.key, e.MessageID, w.msgID)
		}
		if e.Project != "/home/example/my-lab/pi" {
			t.Errorf("%s project = %q", w.key, e.Project)
		}
		if e.Kind != model.KindUsage {
			t.Errorf("%s kind = %q", w.key, e.Kind)
		}
		// ollama is unpriced in pi's catalogue: cost.total is 0, which is
		// unknown, not free.
		if e.CostMicroUSD != nil {
			t.Errorf("%s stamped a cost of %d from a zero cost object", w.key, *e.CostMicroUSD)
		}
	}

	// Exactly one tool call, joined to the record it rode in on.
	if len(obs.Activity) != 2 { // once in the session, once in its fork
		t.Fatalf("activity rows = %d, want 2", len(obs.Activity))
	}
	for _, c := range obs.Activity {
		if c.Kind != model.ActivityTool || c.Name != "bash" {
			t.Errorf("activity = %q/%q, want tool/bash", c.Kind, c.Name)
		}
		if c.DedupKey != "pi|call|call_7py5mn1h" {
			t.Errorf("activity dedup key = %q", c.DedupKey)
		}
		if c.UsageDedupKey != keyToolCall {
			t.Errorf("activity usage key = %q, want %q", c.UsageDedupKey, keyToolCall)
		}
		if c.CallsInTurn != 1 || c.TurnSeq != 0 {
			t.Errorf("activity divisor = %d seq %d, want 1/0", c.CallsInTurn, c.TurnSeq)
		}
	}

	// No turn context: the entry shape carries no attribution field at all.
	if len(obs.TurnContexts) != 0 {
		t.Errorf("turn contexts = %d, want 0 — the source records none", len(obs.TurnContexts))
	}
}

// TestForkedSessionCollapsesToTheSameKeys is THE trap of this format.
//
// `pi --fork <session>` writes a new file under a NEW session uuid and copies
// every entry of the source into it verbatim — same entry id, same timestamp,
// same usage object, same tool-call id. The fixture pair is a real fork of a
// real session, produced by pi's own SessionManager. A dedup key that included
// the session id would bill this machine twice for one API call.
func TestForkedSessionCollapsesToTheSameKeys(t *testing.T) {
	cfg := piCfg(t, piRoot)
	srcs, err := NewPi().Discover(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var origin, fork adapter.Observation
	for _, s := range srcs {
		obs, err := NewPi().Collect(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.Contains(s.Path, sessTool):
			origin = obs
		case strings.Contains(s.Path, sessFork):
			fork = obs
		}
	}
	if len(origin.Events) != 2 || len(fork.Events) != 2 {
		t.Fatalf("origin/fork events = %d/%d, want 2/2", len(origin.Events), len(fork.Events))
	}

	// Different sessions...
	if origin.Events[0].SessionID == fork.Events[0].SessionID {
		t.Fatalf("fixture is not a fork: both files report session %q", origin.Events[0].SessionID)
	}
	// ...identical ledger identity.
	for i := range origin.Events {
		if origin.Events[i].DedupKey != fork.Events[i].DedupKey {
			t.Errorf("event %d: origin key %q != fork key %q — a fork would be counted twice",
				i, origin.Events[i].DedupKey, fork.Events[i].DedupKey)
		}
		if origin.Events[i].TotalTokens != fork.Events[i].TotalTokens {
			t.Errorf("event %d: token totals differ across the fork", i)
		}
	}
	if origin.Activity[0].DedupKey != fork.Activity[0].DedupKey {
		t.Errorf("activity key %q != fork %q — a forked tool call would be counted twice",
			origin.Activity[0].DedupKey, fork.Activity[0].DedupKey)
	}
	if origin.Activity[0].UsageDedupKey != fork.Activity[0].UsageDedupKey {
		t.Errorf("forked activity attributes to a different usage row")
	}
}

// TestOpenClawFixtureEventsAreExact proves the byte-identical claim: the same
// parser, a different root and tool id, and nothing else.
func TestOpenClawFixtureEventsAreExact(t *testing.T) {
	t.Setenv(OpenClawStateDirEnv, "")
	t.Setenv(OpenClawHomeEnv, "")
	t.Setenv(OpenClawAgentDirEnv, "")
	t.Setenv(AgentDirEnv, "")
	srcs, obs := collectAll(t, NewOpenClaw(), adapter.DiscoverConfig{Home: openClawHome})

	if len(srcs) != 1 {
		t.Fatalf("discovered %d sources, want 1 (the sidecars must not be sources)", len(srcs))
	}
	if len(obs.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(obs.Events))
	}
	want := []struct {
		key   string
		in    int64
		out   int64
		total int64
	}{
		{keyOCCall, 20654, 14, 20668},
		{keyOCFinal, 20675, 6, 20681},
	}
	for i, w := range want {
		e := obs.Events[i]
		if e.DedupKey != w.key {
			t.Errorf("event %d key = %q, want %q", i, e.DedupKey, w.key)
		}
		if e.Tool != model.ToolOpenClaw {
			t.Errorf("event %d tool = %q, want %q", i, e.Tool, model.ToolOpenClaw)
		}
		if e.Model != "gemma4:31b" || e.Provider != "ollama-cloud" {
			t.Errorf("event %d model/provider = %q/%q", i, e.Model, e.Provider)
		}
		if e.SessionID != "7964daae-e624-4e5c-9d67-bcb0742de3e2" {
			t.Errorf("event %d session = %q", i, e.SessionID)
		}
		if e.Project != "/home/example/.openclaw/workspace" {
			t.Errorf("event %d project = %q", i, e.Project)
		}
		if e.InputTokens != w.in || e.OutputTokens != w.out || e.TotalTokens != w.total {
			t.Errorf("event %d tokens = %d/%d/%d, want %d/%d/%d",
				i, e.InputTokens, e.OutputTokens, e.TotalTokens, w.in, w.out, w.total)
		}
	}
	// The whole run's spend, which is exactly what the trajectory sidecar's
	// run-cumulative counter also reports. Reading both would double it.
	if got := obs.Events[0].TotalTokens + obs.Events[1].TotalTokens; got != 41349 {
		t.Errorf("session total = %d, want 41349 (the run total the sidecar repeats)", got)
	}

	if len(obs.Activity) != 1 {
		t.Fatalf("activity = %d, want 1", len(obs.Activity))
	}
	c := obs.Activity[0]
	if c.Name != "exec" || c.DedupKey != "openclaw|call|call_urfq7zf0" || c.UsageDedupKey != keyOCCall {
		t.Errorf("activity = %+v", c)
	}
	// OpenClaw's user messages are a plain STRING where pi writes an array. A
	// decoder that insisted on the array shape would fail the whole record.
	if len(obs.TurnContexts) != 0 {
		t.Errorf("turn contexts = %d, want 0", len(obs.TurnContexts))
	}
}

// TestTrajectorySidecarIsNeitherDiscoveredNorRead covers the cumulative-counter
// trap. `<session>.trajectory.jsonl` sits in the sessions directory, ends in
// `.jsonl`, and repeats the run's tokens four ways: a run-CUMULATIVE
// `data.usage` on two separate records, a `promptCache.lastCallUsage` that is
// the LAST call only (assigned, not accumulated), and a `messagesSnapshot` that
// re-embeds every assistant message with its own usage object.
func TestTrajectorySidecarIsNeitherDiscoveredNorRead(t *testing.T) {
	t.Setenv(OpenClawStateDirEnv, "")
	t.Setenv(OpenClawHomeEnv, "")
	t.Setenv(OpenClawAgentDirEnv, "")
	t.Setenv(AgentDirEnv, "")
	a := NewOpenClaw()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: openClawHome})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if strings.Contains(s.Path, ".trajectory") {
			t.Fatalf("discovery returned the trajectory sidecar: %s", s.Path)
		}
	}

	// Even handed the path directly, the header check refuses it: its first
	// record is `"type":"session.started"`, not `"type":"session"`.
	sidecar := filepath.Join(openClawHome, ".openclaw", "agents", "main", "sessions",
		"7964daae-e624-4e5c-9d67-bcb0742de3e2.trajectory.jsonl")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	obs, err := a.Collect(context.Background(), adapter.Source{
		Tool: model.ToolOpenClaw, Class: model.EventLevel, Path: sidecar,
	})
	if !errors.Is(err, adapter.ErrSourceFormat) {
		t.Fatalf("collect sidecar: %v, want format refusal", err)
	}
	if len(obs.Events) != 0 || len(obs.Activity) != 0 {
		t.Fatalf("sidecar produced %d events and %d activity rows, want none",
			len(obs.Events), len(obs.Activity))
	}
	if obs.Checkpoint != nil {
		t.Fatalf("rejected file advanced checkpoint: %+v", obs.Checkpoint)
	}
	// A rejected file that later GROWS must not be tail-read as though its
	// header had been accepted.
	obs2, err := a.(adapter.Incremental).CollectIncremental(context.Background(),
		adapter.Source{Tool: model.ToolOpenClaw, Path: sidecar},
		&model.SourceCheckpoint{Size: 1, MTimeNS: 1, Offset: 500, State: `{"rejected":true}`})
	if !errors.Is(err, adapter.ErrSourceFormat) || obs2.Checkpoint != nil {
		t.Fatalf("grown rejected file: checkpoint=%v, err=%v", obs2.Checkpoint, err)
	}
	if len(obs2.Events) != 0 {
		t.Fatalf("a grown rejected file yielded %d events", len(obs2.Events))
	}
}

// TestZeroUsageRecordIsNotAnEvent: a failed call (here a 410 for a retired
// model) records a usage object of all zeros. Nothing was billed, so nothing is
// a usage event — a zero row would claim a free request happened.
func TestZeroUsageRecordIsNotAnEvent(t *testing.T) {
	cfg := piCfg(t, piRoot)
	srcs, err := NewPi().Discover(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if !strings.Contains(s.Path, sessErr) {
			continue
		}
		obs, err := NewPi().Collect(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		if len(obs.Events) != 0 {
			t.Fatalf("the all-zero error record produced %d events", len(obs.Events))
		}
		// The model_change still carried forward, so the file is not simply
		// being skipped.
		if !strings.Contains(obs.Checkpoint.State, "ministral-3:3b-cloud") {
			t.Errorf("model carry-forward lost: %s", obs.Checkpoint.State)
		}
		return
	}
	t.Fatalf("fixture %s not discovered", sessErr)
}

// TestCompactionAndBranchSummaryCarryUsage covers the third usage-bearing entry
// kind. Pi charges for the LLM call that writes a compaction or branch summary
// and records that usage on the entry itself; a message-only parser would drop
// it. Neither entry names a model, so the model_change carry-forward answers.
func TestCompactionAndBranchSummaryCarryUsage(t *testing.T) {
	cfg := piCfg(t, piCompact)
	_, obs := collectAll(t, NewPi(), cfg)

	if len(obs.Events) != 2 {
		t.Fatalf("events = %d, want 2 (compaction + branch_summary)", len(obs.Events))
	}
	comp := obs.Events[0]
	if comp.Model != "claude-sonnet-4-5" || comp.Provider != "anthropic" {
		t.Errorf("compaction model/provider = %q/%q — the carry-forward did not apply",
			comp.Model, comp.Provider)
	}
	if comp.InputTokens != 900 || comp.OutputTokens != 300 ||
		comp.CacheReadTokens != 100 || comp.CacheCreationTokens != 50 || comp.TotalTokens != 1350 {
		t.Errorf("compaction tokens = %+v", comp)
	}
	// reasoning is a SUBSET of output: reported, never added.
	if comp.ReasoningTokens != 40 {
		t.Errorf("compaction reasoning = %d, want 40", comp.ReasoningTokens)
	}
	if got := comp.InputTokens + comp.OutputTokens + comp.CacheReadTokens + comp.CacheCreationTokens; got != comp.TotalTokens {
		t.Errorf("components sum to %d but total is %d — reasoning must not be additive here",
			got, comp.TotalTokens)
	}
	// The source's own cost, converted to micro-USD and stamped because it is
	// positive.
	cost, ok := comp.Cost()
	if !ok || cost != 7318 {
		t.Errorf("compaction cost = %d,%v, want 7318,true", cost, ok)
	}
	if comp.PriceSource != "pi-reported" {
		t.Errorf("price source = %q", comp.PriceSource)
	}
	// A zero cost object is UNKNOWN, not free.
	if _, ok := obs.Events[1].Cost(); ok {
		t.Errorf("branch_summary stamped a cost from a zero cost object")
	}
}

// TestReasoningIsClampedToOutput: reasoning sits inside output. A source that
// ever reported more reasoning than output would otherwise let a downstream
// additive rule bill tokens that do not exist.
func TestReasoningIsClampedToOutput(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"reasoning":99,"totalTokens":15,"cost":{"total":0}}}}`,
	})
	obs := collectFile(t, NewPi(), filepath.Join(dir, "s.jsonl"))
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d", len(obs.Events))
	}
	if obs.Events[0].ReasoningTokens != 5 {
		t.Errorf("reasoning = %d, want it clamped to output (5)", obs.Events[0].ReasoningTokens)
	}
}

// TestProjectComesFromTheHeaderNotTheDirectoryName. Pi encodes the cwd into the
// sessions subdirectory name by replacing `/`, `\` and `:` with `-` — and it
// does not escape a `-` that was already there, so the encoding is NOT
// invertible. The fixture's project is `/home/example/my-lab/pi`, whose
// directory is `--home-example-my-lab-pi--`; decoding that string would produce
// `/home/example/my/lab/pi`, a directory that does not exist. The header's cwd
// is the only honest source.
func TestProjectComesFromTheHeaderNotTheDirectoryName(t *testing.T) {
	cfg := piCfg(t, piRoot)
	srcs, obs := collectAll(t, NewPi(), cfg)

	var sawEncodedDir bool
	for _, s := range srcs {
		if strings.Contains(filepath.ToSlash(s.Path), "--home-example-my-lab-pi--") {
			sawEncodedDir = true
		}
	}
	if !sawEncodedDir {
		t.Fatalf("fixture layout changed: no cwd-encoded directory in discovery")
	}
	for _, e := range obs.Events {
		if e.Project != "/home/example/my-lab/pi" {
			t.Fatalf("project = %q, want the header cwd /home/example/my-lab/pi", e.Project)
		}
		if strings.Contains(e.Project, "/my/lab/") {
			t.Fatalf("project was decoded from the directory name: %q", e.Project)
		}
	}
}

// TestActivityDivisorIsCountedFromTheRecord. One assistant record commonly
// carries several toolCall blocks against a SINGLE usage object. The divisor is
// counted here, from the blocks actually present, never taken from anything the
// source claims — and all of them attribute to the one usage row.
func TestActivityDivisorIsCountedFromTheRecord(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"p","model":"m",` +
			`"content":[{"type":"text","text":"x"},` +
			`{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"/etc/passwd"}},` +
			`{"type":"toolCall","id":"c2","name":"exec","arguments":{"command":"rm -rf /"}},` +
			`{"type":"toolCall","name":"edit","namespace":"mcp__srv","arguments":{}}],` +
			`"usage":{"input":30,"output":6,"cacheRead":0,"cacheWrite":0,"totalTokens":36,"cost":{"total":0}}}}`,
	})
	obs := collectFile(t, NewPi(), filepath.Join(dir, "s.jsonl"))
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(obs.Events))
	}
	if len(obs.Activity) != 3 {
		t.Fatalf("activity = %d, want 3 (text blocks are not calls)", len(obs.Activity))
	}
	seen := map[string]bool{}
	for i, c := range obs.Activity {
		if c.CallsInTurn != 3 {
			t.Errorf("call %d divisor = %d, want 3", i, c.CallsInTurn)
		}
		if c.TurnSeq != i {
			t.Errorf("call %d seq = %d", i, c.TurnSeq)
		}
		if c.UsageDedupKey != obs.Events[0].DedupKey {
			t.Errorf("call %d attributes to %q, want the record's own usage row", i, c.UsageDedupKey)
		}
		if seen[c.DedupKey] {
			t.Errorf("duplicate activity key %q", c.DedupKey)
		}
		seen[c.DedupKey] = true
	}
	if obs.Activity[0].Name != "read" || obs.Activity[1].Name != "exec" {
		t.Errorf("names = %q/%q", obs.Activity[0].Name, obs.Activity[1].Name)
	}
	// A namespace qualifies the name; dropping it would merge two different
	// tools that share a bare name.
	if obs.Activity[2].Name != "mcp__srv/edit" {
		t.Errorf("namespaced name = %q, want mcp__srv/edit", obs.Activity[2].Name)
	}
	// An id-less block falls back to a hash of the usage key, its position among
	// THIS RECORD's blocks and its name — never a byte offset or line number,
	// which would recount the call on every re-read.
	if strings.HasPrefix(obs.Activity[2].DedupKey, "pi|call|") == false || len(obs.Activity[2].DedupKey) < 20 {
		t.Errorf("id-less block key = %q", obs.Activity[2].DedupKey)
	}
	obs2 := collectFile(t, NewPi(), filepath.Join(dir, "s.jsonl"))
	if obs2.Activity[2].DedupKey != obs.Activity[2].DedupKey {
		t.Errorf("id-less block key is not stable across reads")
	}
}

// TestCacheWrite1hIsSplitOutForPricing. `cacheWrite1h` is a SUBSET of
// `cacheWrite` that Anthropic alone reports, and it is the one counter whose
// price differs from its own siblings: pi-ai bills it at 2x the base input rate
// where a 5m write goes at the cacheWrite rate. The ledger stores only the
// combined count, so the split rides on the transient CacheTTL enrichment —
// dropping it charges every write at the 5m rate, an under-price that is silent
// because the totals still add up. It is a split, never an addition: the token
// columns must not move.
func TestCacheWrite1hIsSplitOutForPricing(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"anthropic","model":"claude-x",` +
			`"usage":{"input":10,"output":5,"cacheRead":7,"cacheWrite":100,"cacheWrite1h":40,"totalTokens":122,"cost":{"total":0}}}}`,
		// A source claiming more 1h than it wrote is clamped, not discarded:
		// pricing.ChargeFor throws away a split that does not add up.
		`{"type":"message","id":"e2","timestamp":"2026-08-16T00:00:02.000Z","message":{"role":"assistant","provider":"anthropic","model":"claude-x",` +
			`"usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":8,"cacheWrite1h":99,"totalTokens":10,"cost":{"total":0}}}}`,
	})
	obs := collectFile(t, NewPi(), filepath.Join(dir, "s.jsonl"))
	if len(obs.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(obs.Events))
	}
	e := obs.Events[0]
	if e.CacheCreationTokens != 100 || e.TotalTokens != 122 {
		t.Errorf("the split moved a token column: cacheWrite %d total %d", e.CacheCreationTokens, e.TotalTokens)
	}
	if e.CacheTTL.Ephemeral1h != 40 || e.CacheTTL.Ephemeral5m != 60 {
		t.Errorf("cache TTL split = %+v, want 60/40", e.CacheTTL)
	}
	if sum := e.CacheTTL.Ephemeral5m + e.CacheTTL.Ephemeral1h; sum != e.CacheCreationTokens {
		t.Errorf("split sums to %d, not the recorded %d — pricing would discard it", sum, e.CacheCreationTokens)
	}
	c := obs.Events[1]
	if c.CacheTTL.Ephemeral1h != 8 || c.CacheTTL.Ephemeral5m != 0 {
		t.Errorf("over-large 1h split = %+v, want it clamped to 0/8", c.CacheTTL)
	}
}

// TestToolResultUsageIsNeverAnEvent. pi's ToolResultMessage carries its OWN
// optional `usage` object ("usage from the tool execution itself ... not part of
// main LLM context accounting"). It sits in a `message` entry, in the same
// `.message.usage` position an assistant turn's usage sits in, so a parser that
// keyed on the field rather than the ROLE would bill a tool result as an API
// call.
func TestToolResultUsageIsNeverAnEvent(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"t1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"exec",` +
			`"content":[{"type":"text","text":"x"}],"isError":false,` +
			`"usage":{"input":9999,"output":9999,"cacheRead":0,"cacheWrite":0,"totalTokens":19998,"cost":{"total":1.5}}}}`,
		`{"type":"message","id":"u1","timestamp":"2026-08-16T00:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"x"}],` +
			`"usage":{"input":5,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":10,"cost":{"total":0}}}}`,
	})
	obs := collectFile(t, NewPi(), filepath.Join(dir, "s.jsonl"))
	if len(obs.Events) != 0 {
		t.Fatalf("non-assistant usage produced %d events: %+v", len(obs.Events), obs.Events)
	}
}

// TestActivityKeyIsScopedToItsRecord. An id-less toolCall block falls back to a
// hash, and that hash must identify the CALL: the usage row it rode in on plus
// its position among that record's blocks. A hash of the name alone would give
// every `edit` in the corpus one key and collapse thousands of calls into one
// row; a hash of a file offset or line number would mint a new key on every
// re-read, and the transcript is re-read in full whenever it changes.
func TestActivityKeyIsScopedToItsRecord(t *testing.T) {
	dir := t.TempDir()
	// Two id-less blocks with the SAME name in one record, and the same pair
	// again in a later record.
	block := `{"type":"toolCall","name":"edit","arguments":{}}`
	rec := func(id, ts string, in int64) string {
		return `{"type":"message","id":"` + id + `","timestamp":"` + ts + `","message":{"role":"assistant","provider":"p","model":"m",` +
			`"content":[` + block + `,` + block + `],` +
			`"usage":{"input":` + fmt.Sprint(in) + `,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":` + fmt.Sprint(in+1) + `,"cost":{"total":0}}}}`
	}
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		rec("e1", "2026-08-16T00:00:01.000Z", 10),
		rec("e2", "2026-08-16T00:00:02.000Z", 20),
	})
	obs := collectFile(t, NewPi(), filepath.Join(dir, "s.jsonl"))
	if len(obs.Activity) != 4 {
		t.Fatalf("activity = %d, want 4", len(obs.Activity))
	}
	seen := map[string]bool{}
	for i, c := range obs.Activity {
		if seen[c.DedupKey] {
			t.Errorf("call %d reuses key %q: an id-less block must be keyed by its record and its "+
				"position in it, not by its name", i, c.DedupKey)
		}
		seen[c.DedupKey] = true
	}

	// The same record at a different byte offset keeps its keys: prepending
	// entries moves every later line, and a key minted from a read position
	// would recount every call on the next pass.
	moved := t.TempDir()
	writeSession(t, moved, "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-08-16T00:00:00.500Z","provider":"p","modelId":"m"}`,
		`{"type":"message","id":"pad","timestamp":"2026-08-16T00:00:00.700Z","message":{"role":"user","content":[{"type":"text","text":"padding"}]}}`,
		rec("e1", "2026-08-16T00:00:01.000Z", 10),
		rec("e2", "2026-08-16T00:00:02.000Z", 20),
	})
	shifted := collectFile(t, NewPi(), filepath.Join(moved, "s.jsonl"))
	if len(shifted.Activity) != len(obs.Activity) {
		t.Fatalf("shifted activity = %d, want %d", len(shifted.Activity), len(obs.Activity))
	}
	for i := range obs.Activity {
		if shifted.Activity[i].DedupKey != obs.Activity[i].DedupKey {
			t.Errorf("call %d key moved with the byte offset: %q != %q — every pass would recount it",
				i, shifted.Activity[i].DedupKey, obs.Activity[i].DedupKey)
		}
	}
}

// TestCompleteButUnterminatedLineIsNotConsumed. A final line that is valid JSON
// but carries no newline is an append caught mid-flight: its records are emitted
// (they are complete), but the offset must stay BEFORE it. Advancing past it
// leaves the next tail read starting inside whatever the writer finishes, and
// the offset never rewinds while the file only grows — every record after it is
// lost for good.
func TestCompleteButUnterminatedLineIsNotConsumed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	header := `{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}` + "\n"
	whole := `{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":4,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":5,"cost":{"total":0}}}}`
	if err := os.WriteFile(path, []byte(header+whole), 0o600); err != nil {
		t.Fatal(err)
	}
	obs := collectFile(t, NewPi(), path)
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want 1 (the line is complete)", len(obs.Events))
	}
	if obs.Checkpoint.Offset != int64(len(header)) {
		t.Fatalf("offset = %d, want %d (before the unterminated line)", obs.Checkpoint.Offset, len(header))
	}

	// The writer finishes the line and appends another. A tail read from the
	// stored offset must see BOTH, and re-derive the first one's key unchanged.
	appendLines(t, path, []string{
		"",
		`{"type":"message","id":"e2","timestamp":"2026-08-16T00:00:02.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":6,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":7,"cost":{"total":0}}}}`,
	})
	touch(t, path)
	tail, err := NewPi().(adapter.Incremental).CollectIncremental(context.Background(),
		adapter.Source{Tool: model.ToolPi, Class: model.EventLevel, Path: path}, obs.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Events) != 2 {
		t.Fatalf("tail read = %d events, want 2 (the finished line and the new one)", len(tail.Events))
	}
	if tail.Events[0].DedupKey != obs.Events[0].DedupKey {
		t.Errorf("the re-read line minted a new key: %q != %q", tail.Events[0].DedupKey, obs.Events[0].DedupKey)
	}
}

// TestIncrementalSkipsTailsAndRestarts covers the checkpoint contract: an
// unchanged file is skipped, pure growth is tail-read with the header and model
// facts carried across the gap, and any other change re-reads from zero.
func TestIncrementalSkipsTailsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"SESS","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/proj"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-08-16T00:00:00.100Z","provider":"anthropic","modelId":"claude-x"}`,
		`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"anthropic","model":"claude-x","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":12,"cost":{"total":0}}}}`,
	})
	a := NewPi()
	src := adapter.Source{Tool: model.ToolPi, Class: model.EventLevel, Path: path}

	first, err := a.(adapter.Incremental).CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 || first.Checkpoint == nil {
		t.Fatalf("first pass = %d events, cp %v", len(first.Events), first.Checkpoint)
	}

	// Unchanged: skipped entirely, stored checkpoint left alone.
	again, err := a.(adapter.Incremental).CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Events) != 0 || again.Checkpoint != nil {
		t.Fatalf("unchanged pass returned %d events / cp %v", len(again.Events), again.Checkpoint)
	}

	// Growth: a compaction entry that names no model, appended past the header.
	// Only the tail is read, and the carried state supplies both.
	appendLines(t, path, []string{
		`{"type":"compaction","id":"e2","timestamp":"2026-08-16T00:00:02.000Z","summary":"s","firstKeptEntryId":"e1","tokensBefore":9,"usage":{"input":7,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":10,"cost":{"total":0}}}`,
	})
	touch(t, path)
	tail, err := a.(adapter.Incremental).CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Events) != 1 {
		t.Fatalf("tail read = %d events, want only the appended one", len(tail.Events))
	}
	e := tail.Events[0]
	if e.SessionID != "SESS" || e.Project != "/proj" {
		t.Errorf("tail lost the header: session %q project %q", e.SessionID, e.Project)
	}
	if e.Model != "claude-x" || e.Provider != "anthropic" {
		t.Errorf("tail lost the model carry-forward: %q/%q", e.Model, e.Provider)
	}

	// A full re-read must produce the same keys, so a restart is idempotent.
	full, err := a.(adapter.Incremental).CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Events) != 2 {
		t.Fatalf("full re-read = %d events, want 2", len(full.Events))
	}
	if full.Events[0].DedupKey != first.Events[0].DedupKey || full.Events[1].DedupKey != tail.Events[0].DedupKey {
		t.Errorf("re-read produced different dedup keys; a restart would double count")
	}

	// A shrink (pi rewrites a whole file when it migrates its session version)
	// is not a tail: it restarts from zero.
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"SESS","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/proj"}`,
	})
	shrunk, err := a.(adapter.Incremental).CollectIncremental(context.Background(), src, full.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(shrunk.Events) != 0 {
		t.Fatalf("shrink pass = %d events, want 0", len(shrunk.Events))
	}
	if shrunk.Checkpoint == nil || shrunk.Checkpoint.Offset >= full.Checkpoint.Offset {
		t.Errorf("shrink did not reset the offset: %+v", shrunk.Checkpoint)
	}
}

// TestUnterminatedTailLineIsNotConsumed: a line still being appended must not
// advance the offset past itself, or the finished line is never read.
func TestUnterminatedTailLineIsNotConsumed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	header := `{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}` + "\n"
	partial := `{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"ass`
	if err := os.WriteFile(path, []byte(header+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	obs := collectFile(t, NewPi(), path)
	if len(obs.Events) != 0 {
		t.Fatalf("partial line produced %d events", len(obs.Events))
	}
	if obs.Checkpoint.Offset != int64(len(header)) {
		t.Errorf("offset = %d, want %d (before the unterminated line)", obs.Checkpoint.Offset, len(header))
	}
}

// TestMalformedLineDoesNotDropTheRest: one unparseable line must cost that line
// and nothing else.
func TestMalformedLineDoesNotDropTheRest(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message" this is not json`,
		`{"type":"message","id":"e2","timestamp":"2026-08-16T00:00:02.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":4,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":5,"cost":{"total":0}}}}`,
	})
	path := filepath.Join(dir, "s.jsonl")
	obs, err := NewPi().Collect(context.Background(), adapter.Source{Tool: model.ToolPi, Path: path})
	if err == nil || errors.Is(err, adapter.ErrSourceFormat) || !strings.Contains(err.Error(), path) || obs.Checkpoint == nil {
		t.Fatalf("complete poison: checkpoint=%v, err=%v", obs.Checkpoint, err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(obs.Events))
	}
}

// TestSessionDirEnvOverridesAgentDir: PI_CODING_AGENT_SESSION_DIR names a
// sessions directory outright, the way --session-dir does, and it wins.
func TestSessionDirEnvOverridesAgentDir(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "flat.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"total":0}}}}`,
	})
	t.Setenv(AgentDirEnv, piRoot)
	t.Setenv(SessionDirEnv, dir)
	srcs, err := NewPi().Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || filepath.Base(srcs[0].Path) != "flat.jsonl" {
		t.Fatalf("discovery honoured %s over %s: %+v", AgentDirEnv, SessionDirEnv, srcs)
	}
}

// TestOpenClawProfileRootsAreDiscovered: `--profile x` / OPENCLAW_PROFILE=x
// relocates the whole state root to ~/.openclaw-x and resolves it into
// OPENCLAW_STATE_DIR only inside the CLI's own process. Globbing the sibling
// roots is the only way to see a profile that is not currently exported.
func TestOpenClawProfileRootsAreDiscovered(t *testing.T) {
	home := t.TempDir()
	for _, root := range []string{".openclaw", ".openclaw-dev"} {
		dir := filepath.Join(home, root, "agents", "main", "sessions")
		writeSession(t, dir, "s.jsonl", []string{
			`{"type":"session","id":"S-` + root + `","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
			`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"total":0}}}}`,
		})
	}
	t.Setenv(OpenClawStateDirEnv, "")
	t.Setenv(OpenClawHomeEnv, "")
	t.Setenv(OpenClawAgentDirEnv, "")
	t.Setenv(AgentDirEnv, "")
	srcs, err := NewOpenClaw().Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("discovered %d sources, want 2 (default + dev profile): %+v", len(srcs), srcs)
	}
}

// TestOpenClawHomeEnvMovesTheRoot: OPENCLAW_HOME replaces the home the
// `.openclaw` root is derived from, ahead of the discovery config's home.
func TestOpenClawHomeEnvMovesTheRoot(t *testing.T) {
	home := t.TempDir()
	writeSession(t, filepath.Join(home, ".openclaw", "agents", "main", "sessions"), "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"total":0}}}}`,
	})
	t.Setenv(OpenClawStateDirEnv, "")
	t.Setenv(OpenClawAgentDirEnv, "")
	t.Setenv(AgentDirEnv, "")
	t.Setenv(OpenClawHomeEnv, home)
	srcs, err := NewOpenClaw().Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 {
		t.Fatalf("%s did not move the root: %+v", OpenClawHomeEnv, srcs)
	}
}

// TestDiscoveryEnvNamesAreExact pins the variable names. They are what
// cmd.discoveryEnv parses out of this file to decide whether a supervised
// install would collect somewhere other than the shell that installed it, so a
// rename here is a silent behaviour change there.
//
// PI_AGENT_DIR is deliberately absent: third-party tooling uses that name, pi
// itself never reads it (pi 0.84.2 builds `<APP>_CODING_AGENT_DIR` from its own
// package name), and honouring it would point discovery at a directory the
// harness does not write.
func TestDiscoveryEnvNamesAreExact(t *testing.T) {
	want := map[string]string{
		AgentDirEnv:           "PI_CODING_AGENT_DIR",
		SessionDirEnv:         "PI_CODING_AGENT_SESSION_DIR",
		OpenClawStateDirEnv:   "OPENCLAW_STATE_DIR",
		OpenClawHomeEnv:       "OPENCLAW_HOME",
		OpenClawAgentDirEnv:   "OPENCLAW_AGENT_DIR",
		OpenClawConfigPathEnv: "OPENCLAW_CONFIG_PATH",
		OpenClawProfileEnv:    "OPENCLAW_PROFILE",
	}
	for got, exp := range want {
		if got != exp {
			t.Errorf("env const = %q, want %q", got, exp)
		}
	}
	src, err := os.ReadFile("pi.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `"PI_AGENT_DIR"`) {
		t.Errorf("pi.go reads PI_AGENT_DIR, which pi does not write")
	}
	// Every lookup must name its variable with one of these constants AT the
	// os.Getenv call. cmd.TestDiscoveryEnvCoversEveryAdapterVariable parses this
	// file and resolves the argument through the package's constants, so a read
	// routed through a helper's parameter — env(k) with k a string — is a
	// variable that guard cannot see, and a variable it cannot see is one the
	// automatic install cannot suppress.
	declared := map[string]bool{}
	for name := range want {
		declared[name] = true
	}
	lookups := 0
	for _, line := range strings.Split(string(src), "\n") {
		_, after, found := strings.Cut(line, "os.Getenv(")
		if !found {
			continue
		}
		arg, _, ok := strings.Cut(after, ")")
		if !ok {
			t.Errorf("unparseable os.Getenv call: %s", strings.TrimSpace(line))
			continue
		}
		lookups++
		if !declared[constValue(arg)] {
			t.Errorf("os.Getenv(%s) does not name a declared constant: %s", arg, strings.TrimSpace(line))
		}
	}
	if lookups == 0 {
		t.Error("found no os.Getenv calls at all; the scan is broken, not the source")
	}
}

// constValue resolves one of this package's env-name constants by identifier.
// An unknown identifier resolves to "", which no declared name equals.
func constValue(ident string) string {
	switch strings.TrimSpace(ident) {
	case "AgentDirEnv":
		return AgentDirEnv
	case "SessionDirEnv":
		return SessionDirEnv
	case "OpenClawStateDirEnv":
		return OpenClawStateDirEnv
	case "OpenClawHomeEnv":
		return OpenClawHomeEnv
	case "OpenClawAgentDirEnv":
		return OpenClawAgentDirEnv
	case "OpenClawConfigPathEnv":
		return OpenClawConfigPathEnv
	case "OpenClawProfileEnv":
		return OpenClawProfileEnv
	}
	return ""
}

// TestOpenClawAgentDirEnvFallsBackToPiVariable: OpenClaw resolves its agent-dir
// override as `OPENCLAW_AGENT_DIR || PI_CODING_AGENT_DIR`, so the pi variable
// moves BOTH harnesses' surfaces.
func TestOpenClawAgentDirEnvFallsBackToPiVariable(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agents", "main", "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSession(t, filepath.Join(root, "agents", "main", "sessions"), "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"total":0}}}}`,
	})
	t.Setenv(OpenClawStateDirEnv, "")
	t.Setenv(OpenClawHomeEnv, "")
	t.Setenv(OpenClawAgentDirEnv, "")
	t.Setenv(AgentDirEnv, agentDir)
	srcs, err := NewOpenClaw().Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 {
		t.Fatalf("%s did not move the OpenClaw surface: %+v", AgentDirEnv, srcs)
	}
}

// TestAdapterIsReadOnly: a collection pass must not create, modify or truncate
// anything under the discovery root.
func TestAdapterIsReadOnly(t *testing.T) {
	cfg := piCfg(t, piRoot)
	before := snapshotTree(t, piRoot)
	if _, obs := collectAll(t, NewPi(), cfg); len(obs.Events) == 0 {
		t.Fatal("nothing collected; the read-only claim would be vacuous")
	}
	if after := snapshotTree(t, piRoot); !sameTree(before, after) {
		t.Errorf("the collection pass changed the source tree\nbefore: %v\nafter:  %v", before, after)
	}
}

// --- helpers ---------------------------------------------------------------

func collectFile(t *testing.T, a adapter.Adapter, path string) adapter.Observation {
	t.Helper()
	obs, err := a.Collect(context.Background(), adapter.Source{
		Tool: a.ID(), Class: model.EventLevel, Path: path,
	})
	if err != nil {
		t.Fatalf("collect %s: %v", path, err)
	}
	return obs
}

func writeSession(t *testing.T, dir, name string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// touch moves the mtime forward so the size+mtime gate sees a change even when
// a test runs faster than the filesystem's timestamp resolution.
func touch(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

type fileStat struct {
	size int64
	mode os.FileMode
	mod  time.Time
}

func snapshotTree(t *testing.T, root string) map[string]fileStat {
	t.Helper()
	out := map[string]fileStat{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out[p] = fileStat{size: fi.Size(), mode: fi.Mode(), mod: fi.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(a, b map[string]fileStat) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		x, ok := b[k]
		if !ok || x != a[k] {
			return false
		}
	}
	return true
}
