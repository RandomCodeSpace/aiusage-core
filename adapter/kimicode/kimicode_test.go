package kimicode

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// The fixtures under testdata/home are the real tree this adapter reads:
//
//   - wd_kimi_fixture01 is a VERBATIM STRUCTURAL COPY of a real Kimi Code
//     session recorded on this machine (kimi-code 0.36.1, protocol 1.5), with
//     every content field replaced by a placeholder. Record order, record
//     types, counters, timestamps and the model/alias pair are untouched.
//   - wd_kimi_fixture02 extends those same record shapes to the cases one
//     session cannot show: a model switch mid-file, a usage record with no
//     request before it, a session-scoped compaction charge, an all-zero
//     record, and a subagent whose records are identical to the main agent's.
//   - wd_kimi_fixture03 plants a secret in every content field the wire log
//     has.
const (
	fixtureHome = "testdata/home"

	// The real session: one turn, one request, one usage record.
	realSession = "session_00000000-1111-2222-3333-444444444444"
	realModel   = "gemma4:31b-cloud"
	realAlias   = "__kimi_env_model__"

	synthSession = "session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	privSession  = "session_ffffffff-0000-1111-2222-333333333333"

	secret = "KIMI-SECRET-DO-NOT-LEAK"
)

// clearEnv removes both discovery overrides so a developer's own shell cannot
// point the tests at a real Kimi tree.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv(HomeEnv, "")
	t.Setenv(DataDirEnv, "")
}

func discover(t *testing.T) []adapter.Source {
	t.Helper()
	clearEnv(t)
	srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: fixtureHome})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].Path < srcs[j].Path })
	return srcs
}

func sourceFor(t *testing.T, session, agent string) adapter.Source {
	t.Helper()
	for _, s := range discover(t) {
		if s.Meta["session"] == session && s.Meta["agent"] == agent {
			return s
		}
	}
	t.Fatalf("no source for session %q agent %q", session, agent)
	return adapter.Source{}
}

func collect(t *testing.T, src adapter.Source) []model.UsageEvent {
	t.Helper()
	obs, err := Adapter{}.Collect(context.Background(), src)
	if err != nil {
		t.Fatalf("Collect(%s): %v", src.Path, err)
	}
	if len(obs.Snapshots) != 0 {
		t.Fatalf("event-level adapter returned %d snapshots", len(obs.Snapshots))
	}
	return obs.Events
}

// --- discovery -------------------------------------------------------------

// TestDiscoverFindsEveryAgentWireLog pins the surface: the wire log lives four
// levels below the sessions root, and SUBAGENTS get their own directory beside
// main. Each agent owns its own recorder, so a scan that stopped at agents/main
// would silently drop every subagent's tokens.
func TestDiscoverFindsEveryAgentWireLog(t *testing.T) {
	srcs := discover(t)
	if len(srcs) != 4 {
		var got []string
		for _, s := range srcs {
			got = append(got, s.Path)
		}
		t.Fatalf("want 4 wire logs, got %d: %v", len(srcs), got)
	}
	for _, s := range srcs {
		if s.Tool != model.ToolKimiCode {
			t.Errorf("tool = %q, want %q", s.Tool, model.ToolKimiCode)
		}
		if s.Class != model.EventLevel {
			t.Errorf("class = %q, want event-level", s.Class)
		}
		if filepath.Base(s.Path) != fileWire {
			t.Errorf("discovered %q, which is not a wire log", s.Path)
		}
		if s.Meta["session"] == "" || s.Meta["agent"] == "" || s.Meta["workspace"] == "" {
			t.Errorf("%s: incomplete meta %v", s.Path, s.Meta)
		}
	}

	sub := sourceFor(t, synthSession, "subagent_7f3")
	if sub.Meta["workspace"] != "wd_kimi_fixture02" {
		t.Errorf("subagent workspace = %q", sub.Meta["workspace"])
	}
}

// TestDiscoverResolvesProjectFromWorkspaces checks the project dimension comes
// from the root's own workspaces.json, and that a workspace it does not name
// leaves the project unknown rather than guessed.
func TestDiscoverResolvesProjectFromWorkspaces(t *testing.T) {
	if got := sourceFor(t, realSession, "main").Meta["project"]; got != "/tmp/kimi-fixture-project" {
		t.Errorf("project = %q, want the workspaces.json root", got)
	}
	if got := sourceFor(t, privSession, "main").Meta["project"]; got != "" {
		t.Errorf("project = %q for an unlisted workspace, want empty", got)
	}
}

// TestEnvOverridesMoveTheRoot proves both exported variables actually move what
// is collected — which is why discoveryEnv has to name them.
func TestEnvOverridesMoveTheRoot(t *testing.T) {
	abs, err := filepath.Abs(filepath.Join(fixtureHome, ".kimi-code"))
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{HomeEnv, DataDirEnv} {
		t.Run(env, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(env, abs)
			// No Home at all: only the environment can find the tree.
			srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{})
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(srcs) != 4 {
				t.Fatalf("%s did not move the root: got %d sources", env, len(srcs))
			}
		})
	}
}

// TestDiscoverToleratesAMissingRoot: a machine without Kimi Code installed is
// not an error, it is zero sources.
func TestDiscoverToleratesAMissingRoot(t *testing.T) {
	clearEnv(t)
	srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("got %d sources from an empty home", len(srcs))
	}
}

// --- the real session ------------------------------------------------------

// TestRealSessionEmitsExactlyOneEvent pins every field of the event produced by
// the recorded session, dedup key included.
func TestRealSessionEmitsExactlyOneEvent(t *testing.T) {
	src := sourceFor(t, realSession, "main")
	evs := collect(t, src)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	got := evs[0]

	want := model.UsageEvent{
		Tool:                model.ToolKimiCode,
		Model:               realModel,
		Provider:            "",
		SessionID:           realSession,
		Project:             "/tmp/kimi-fixture-project",
		EventTime:           time.UnixMilli(1786853354607).UTC(),
		InputTokens:         23122,
		OutputTokens:        8,
		CacheCreationTokens: 0,
		CacheReadTokens:     0,
		ReasoningTokens:     0,
		TotalTokens:         23130,
		SourcePath:          src.Path,
		Kind:                model.KindUsage,
		DedupKey:            got.DedupKey, // pinned separately below
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("event mismatch:\n got %+v\nwant %+v", got, want)
	}
	// sha256 over
	// main|1786853354607|turn|__kimi_env_model__|gemma4:31b-cloud|__kimi_env_model__|1786853352822|"0.1"|23122|8|0|0
	// — agent, record time, scope, the alias the record claimed, the joined
	// request's model/alias/identity, then the four counters. Recomputed
	// independently; if this literal moves, every stored kimi-code row is
	// orphaned and re-collected.
	const wantKey = "kimi-code|f36bdc43b1747fd68db03b542a55179a577912afcde075bfc028b9a9a72302be"
	if got.DedupKey != wantKey {
		t.Errorf("dedup key = %q, want %q", got.DedupKey, wantKey)
	}
}

// TestModelIsTheRequestsNotTheUsageRecordsAlias is the headline trap.
// usage.record's `model` field holds the CONFIG ALIAS the profile was bound
// under (`__kimi_env_model__` here), never a model id; the real id is on the
// llm.request that preceded it. Reading the record's own field would file every
// session on this machine under one unpriceable pseudo-model.
func TestModelIsTheRequestsNotTheUsageRecordsAlias(t *testing.T) {
	evs := collect(t, sourceFor(t, realSession, "main"))
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].Model != realModel {
		t.Fatalf("model = %q, want the llm.request model %q", evs[0].Model, realModel)
	}
	if strings.Contains(marshal(t, evs[0]), realAlias) {
		t.Fatalf("the config alias %q reached the emitted event", realAlias)
	}
}

// TestStepEndUsageIsNotCountedTwice pins the double-count trap. The same
// counters appear twice in every loop turn: once as usage.record, and once
// inside the context.append_loop_event whose event.type is step.end. Counting
// both would exactly double every turn.
func TestStepEndUsageIsNotCountedTwice(t *testing.T) {
	raw, err := os.ReadFile(sourceFor(t, realSession, "main").Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"step.end"`) || strings.Count(string(raw), `"inputOther":23122`) != 2 {
		t.Fatal("fixture no longer carries the duplicated step.end usage; the trap is untested")
	}
	evs := collect(t, sourceFor(t, realSession, "main"))
	var total int64
	for _, e := range evs {
		total += e.TotalTokens
	}
	if len(evs) != 1 || total != 23130 {
		t.Fatalf("got %d events totalling %d, want 1 event totalling 23130", len(evs), total)
	}
}

// --- the joined-model and scope traps --------------------------------------

// TestNearestPriorRequestResolvesEachModel walks a file where the model changes
// mid-session: each usage record must resolve against the request BEFORE it,
// not against the first or the last one in the file.
func TestNearestPriorRequestResolvesEachModel(t *testing.T) {
	evs := collect(t, sourceFor(t, synthSession, "main"))

	type row struct {
		model string
		total int64
	}
	var got []row
	for _, e := range evs {
		got = append(got, row{e.Model, e.TotalTokens})
	}
	want := []row{
		{"", 14},                        // before any llm.request: model UNKNOWN
		{"kimi-k2-turbo-preview", 2104}, // first request
		{"kimi-k2-thinking", 2176},      // after the model switch
		{"kimi-k2-thinking", 528},       // the compaction call, scope="session"
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved models/totals:\n got %v\nwant %v", got, want)
	}
}

// TestUnknownModelIsEmptyNeverTheAlias: with no request before it, the model is
// unknown. Stamping the alias would put a pseudo-model in the model dimension
// and claim the source named a model it never named.
func TestUnknownModelIsEmptyNeverTheAlias(t *testing.T) {
	evs := collect(t, sourceFor(t, synthSession, "main"))
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	if evs[0].Model != "" {
		t.Fatalf("model = %q for a record with no prior request, want empty", evs[0].Model)
	}
	if evs[0].TotalTokens != 14 {
		t.Fatalf("an unattributable model must not cost the ledger its tokens: total = %d", evs[0].TotalTokens)
	}
}

// TestSessionScopedRecordsAreCounted: usageScope is "turn" or "session" and
// both are per-request deltas — kimi's own replayer SUMS every record into its
// scope bucket. Filtering by scope would drop compaction and summarisation
// charges, which only ever arrive as "session".
func TestSessionScopedRecordsAreCounted(t *testing.T) {
	evs := collect(t, sourceFor(t, synthSession, "main"))
	var total int64
	for _, e := range evs {
		total += e.TotalTokens
	}
	// 14 + 2104 + 2176 + 528, i.e. both session-scoped records included.
	if total != 4822 {
		t.Fatalf("total = %d, want 4822 (every scope counted once)", total)
	}
}

// TestZeroUsageRecordIsSkipped: kimi writes `usage ?? emptyUsage()`, so a
// provider that reported nothing still produces an all-zero record. It is not a
// spend and must not become a row.
func TestZeroUsageRecordIsSkipped(t *testing.T) {
	evs := collect(t, sourceFor(t, synthSession, "main"))
	if len(evs) != 4 {
		t.Fatalf("want 4 events (the all-zero record skipped), got %d", len(evs))
	}
	for _, e := range evs {
		if e.TotalTokens == 0 {
			t.Errorf("zero-token event emitted: %+v", e)
		}
	}
}

// TestCacheBucketsAreAdditiveNotSubsets pins the token mapping. kimi's own
// inputTotal is inputOther + inputCacheRead + inputCacheCreation, so inputOther
// EXCLUDES cache and the components sum to the total — Anthropic-style, not
// OpenAI-style where cached is a subset of input.
func TestCacheBucketsAreAdditiveNotSubsets(t *testing.T) {
	evs := collect(t, sourceFor(t, synthSession, "main"))
	e := evs[1]
	if e.InputTokens != 1200 || e.OutputTokens != 40 || e.CacheReadTokens != 800 || e.CacheCreationTokens != 64 {
		t.Fatalf("token mapping wrong: %+v", e)
	}
	if e.TotalTokens != 2104 || e.TotalTokens != e.ComputedTotal() {
		t.Fatalf("total = %d, want the additive sum 2104", e.TotalTokens)
	}
	if e.ReasoningTokens != 0 {
		t.Fatalf("reasoning = %d, but kimi's TokenUsage has no reasoning counter", e.ReasoningTokens)
	}
}

// TestProviderIsUnknownNotTheProtocol: llm.request.provider names the wire
// protocol the client speaks ("openai" is the OpenAI-legacy chat adapter, used
// for any compatible endpoint), not who bills the tokens. Passing it through
// would label a Moonshot or self-hosted request as OpenAI spend and send the
// pricing engine into the wrong namespace.
func TestProviderIsUnknownNotTheProtocol(t *testing.T) {
	for _, session := range []string{realSession, synthSession} {
		for _, e := range collect(t, sourceFor(t, session, "main")) {
			if e.Provider != "" {
				t.Fatalf("provider = %q, want unknown", e.Provider)
			}
		}
	}
}

// --- dedup keys ------------------------------------------------------------

// TestDedupKeysAreStableAcrossReads: the key is derived from record CONTENT
// only — never an offset, a line number or an ordinal — so re-reading the same
// file cannot recount its usage.
func TestDedupKeysAreStableAcrossReads(t *testing.T) {
	src := sourceFor(t, synthSession, "main")
	first, second := collect(t, src), collect(t, src)
	if len(first) != len(second) {
		t.Fatalf("read count changed: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].DedupKey != second[i].DedupKey {
			t.Fatalf("event %d key changed between reads: %q -> %q", i, first[i].DedupKey, second[i].DedupKey)
		}
	}
}

// TestDedupKeysAreDistinctPerRecord: four records, four keys.
func TestDedupKeysAreDistinctPerRecord(t *testing.T) {
	seen := make(map[string]bool)
	for _, e := range collect(t, sourceFor(t, synthSession, "main")) {
		if !strings.HasPrefix(e.DedupKey, model.ToolKimiCode+"|") {
			t.Errorf("key %q is not namespaced by the tool", e.DedupKey)
		}
		if seen[e.DedupKey] {
			t.Fatalf("duplicate dedup key %q", e.DedupKey)
		}
		seen[e.DedupKey] = true
	}
}

// TestSubagentRecordsDoNotCollideWithMain. The subagent fixture's usage record
// is byte-identical to the main agent's in every field the key hashes except
// the agent that wrote it: same millisecond, same counters, same scope, same
// alias, same request. Two agents really did spend those tokens; without the
// agent in the key one of the two would vanish.
func TestSubagentRecordsDoNotCollideWithMain(t *testing.T) {
	main := collect(t, sourceFor(t, synthSession, "main"))
	sub := collect(t, sourceFor(t, synthSession, "subagent_7f3"))
	if len(sub) != 1 {
		t.Fatalf("want 1 subagent event, got %d", len(sub))
	}
	if sub[0].TotalTokens != main[1].TotalTokens {
		t.Fatalf("fixture drifted: the subagent record is no longer identical to the main one")
	}
	if sub[0].DedupKey == main[1].DedupKey {
		t.Fatal("subagent and main agent records collapsed onto one dedup key")
	}
	if sub[0].SessionID != main[1].SessionID {
		t.Fatal("both agents belong to the same session and must report it")
	}
}

// TestDedupKeyExcludesSessionIdentity. Kimi forks a session by COPYING the wire
// log into a new session directory; the copied records keep their timestamps
// and counters. A key naming the session would count one spend once per fork.
func TestDedupKeyExcludesSessionIdentity(t *testing.T) {
	src := sourceFor(t, synthSession, "main")
	forked := src
	forked.Meta = map[string]string{
		"session":   "session_99999999-9999-9999-9999-999999999999",
		"agent":     src.Meta["agent"],
		"workspace": src.Meta["workspace"],
		"project":   src.Meta["project"],
	}
	orig, fork := collect(t, src), collect(t, forked)
	if len(orig) != len(fork) {
		t.Fatalf("event counts differ: %d vs %d", len(orig), len(fork))
	}
	for i := range orig {
		if orig[i].DedupKey != fork[i].DedupKey {
			t.Fatalf("event %d re-keyed under a forked session: %q -> %q",
				i, orig[i].DedupKey, fork[i].DedupKey)
		}
	}
}

// --- incremental reads -----------------------------------------------------

// TestAdapterIsIncremental. The collector reaches CollectIncremental through a
// type assertion, so losing the method is not a compile error — it is a silent
// fall back to re-reading every wire log in full on every pass.
func TestAdapterIsIncremental(t *testing.T) {
	if _, ok := New().(adapter.Incremental); !ok {
		t.Fatal("adapter no longer implements adapter.Incremental")
	}
}

// TestIncrementalTailReadMatchesFullRead. A tail read that resumed after the
// last llm.request would resolve every following usage record against nothing,
// changing both the model and the dedup key. The carry-forward lives in the
// checkpoint precisely so it does not.
func TestIncrementalTailReadMatchesFullRead(t *testing.T) {
	full := collect(t, sourceFor(t, synthSession, "main"))

	lines := readLines(t, sourceFor(t, synthSession, "main").Path)
	dir := t.TempDir()
	path := filepath.Join(dir, fileWire)
	// Split immediately AFTER the first llm.request, so the tail read STARTS on
	// a usage record whose model and dedup key can only come from a request the
	// tail never sees. That is the whole reason the carry-forward is persisted;
	// splitting any later leaves every tail record with its own request beside
	// it and proves nothing.
	const split = 4
	writeLines(t, path, lines[:split])

	src := adapter.Source{
		Tool: model.ToolKimiCode, Class: model.EventLevel, Path: path,
		Meta: map[string]string{"session": synthSession, "agent": "main", "project": "/tmp/kimi-fixture-project"},
	}
	a := Adapter{}
	obs, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if obs.Checkpoint == nil {
		t.Fatal("first pass returned no checkpoint")
	}
	got := obs.Events

	// Unchanged file: nothing new, and the stored checkpoint is left alone.
	again, err := a.CollectIncremental(context.Background(), src, obs.Checkpoint)
	if err != nil {
		t.Fatalf("unchanged pass: %v", err)
	}
	if len(again.Events) != 0 || again.Checkpoint != nil {
		t.Fatalf("unchanged file produced %d events / checkpoint %v", len(again.Events), again.Checkpoint)
	}

	writeLines(t, path, lines)
	obs2, err := a.CollectIncremental(context.Background(), src, obs.Checkpoint)
	if err != nil {
		t.Fatalf("tail pass: %v", err)
	}
	got = append(got, obs2.Events...)

	if len(got) != len(full) {
		t.Fatalf("incremental produced %d events, full read %d", len(got), len(full))
	}
	for i := range full {
		if got[i].DedupKey != full[i].DedupKey || got[i].Model != full[i].Model {
			t.Fatalf("event %d differs: incremental %q/%q vs full %q/%q",
				i, got[i].Model, got[i].DedupKey, full[i].Model, full[i].DedupKey)
		}
	}
}

// TestShrunkFileIsReReadFromZero. Kimi rewrites a wire log in place — a forked
// session is truncated at a turn, and a protocol migration rewrites the whole
// file — so a stale offset must never be trusted after a shrink.
func TestShrunkFileIsReReadFromZero(t *testing.T) {
	lines := readLines(t, sourceFor(t, synthSession, "main").Path)
	dir := t.TempDir()
	path := filepath.Join(dir, fileWire)
	writeLines(t, path, lines)

	src := adapter.Source{Tool: model.ToolKimiCode, Class: model.EventLevel, Path: path,
		Meta: map[string]string{"session": synthSession, "agent": "main"}}
	a := Adapter{}
	obs, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(obs.Events) != 4 {
		t.Fatalf("first pass: %d events", len(obs.Events))
	}

	writeLines(t, path, lines[:6]) // truncate: fewer bytes than the checkpoint
	obs2, err := a.CollectIncremental(context.Background(), src, obs.Checkpoint)
	if err != nil {
		t.Fatalf("shrunk pass: %v", err)
	}
	if len(obs2.Events) != 2 {
		t.Fatalf("shrunk file re-read produced %d events, want 2 from a full re-read", len(obs2.Events))
	}
	if obs2.Events[1].Model != "kimi-k2-turbo-preview" {
		t.Fatalf("re-read lost the model carry-forward: %q", obs2.Events[1].Model)
	}
}

// TestMalformedLineDoesNotDropTheRest: one corrupt line must cost one record,
// not the remainder of the session.
func TestMalformedLineDoesNotDropTheRest(t *testing.T) {
	lines := readLines(t, sourceFor(t, synthSession, "main").Path)
	broken := append([]string{}, lines...)
	broken[4] = `{"type":"usage.record","model":"k2","usage":{`
	dir := t.TempDir()
	path := filepath.Join(dir, fileWire)
	writeLines(t, path, broken)

	src := adapter.Source{Tool: model.ToolKimiCode, Class: model.EventLevel, Path: path,
		Meta: map[string]string{"session": synthSession, "agent": "main"}}
	obs, err := (Adapter{}).Collect(context.Background(), src)
	if err == nil || errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint == nil {
		t.Fatalf("malformed record error = %v, checkpoint present = %t", err, obs.Checkpoint != nil)
	}
	evs := obs.Events
	if len(evs) != 3 {
		t.Fatalf("want 3 events past the corrupt line, got %d", len(evs))
	}
	if evs[2].Model != "kimi-k2-thinking" {
		t.Fatalf("parsing did not recover: %q", evs[2].Model)
	}
}

// --- privacy ---------------------------------------------------------------

// TestNoContentReachesAnyEmittedField plants a secret in every content field
// the wire log has — system prompt, cwd, user input, message parts, tool call
// arguments, tool schemas, streamed text, tool results, message ids — and
// asserts none of it reaches any emitted field. Two content-only records also
// contain the strings "usage.record" and "llm.request"; their discriminators
// still exclude them from accounting.
func TestNoContentReachesAnyEmittedField(t *testing.T) {
	src := sourceFor(t, privSession, "main")
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), secret); n < 20 {
		t.Fatalf("fixture carries the secret only %d times; it is meant to be everywhere", n)
	}

	evs := collect(t, src)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	for _, e := range evs {
		if e.Raw != "" {
			t.Errorf("adapter built a raw audit payload; it must build none at all")
		}
		// Every string the event carries, whether exported or not.
		v := reflect.ValueOf(e)
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if f.Kind() != reflect.String {
				continue
			}
			if strings.Contains(f.String(), secret) {
				t.Errorf("field %s leaked content: %q", v.Type().Field(i).Name, f.String())
			}
		}
		if strings.Contains(marshal(t, e), secret) {
			t.Errorf("the serialised event leaked content")
		}
	}
}

// TestNoActivityOrTurnContextIsClaimed. Kimi's wire log has no record type for
// a tool call — calls live inside context.append_message alongside their
// arguments — and nothing in it shares an identity with usage.record, so any
// attribution would be positional. The adapter therefore claims neither
// stream rather than inventing one.
func TestNoActivityOrTurnContextIsClaimed(t *testing.T) {
	for _, s := range discover(t) {
		obs, err := Adapter{}.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("Collect(%s): %v", s.Path, err)
		}
		if len(obs.Activity) != 0 || len(obs.TurnContexts) != 0 {
			t.Fatalf("%s: adapter emitted %d activity / %d context rows it cannot support",
				s.Path, len(obs.Activity), len(obs.TurnContexts))
		}
	}
}

// TestWireDecodeIsAnAllowList reads this package's own source and fails when
// the wire line struct grows a field. The privacy guarantee is that
// encoding/json has nowhere to put a prompt, a message part, a tool argument or
// a tool schema: it holds only counters, identifiers, times and enums. A new
// field is how that stops being true, so it has to be a deliberate edit here
// and not a quiet one there.
func TestWireDecodeIsAnAllowList(t *testing.T) {
	want := map[string]bool{
		"type": true, "time": true, "model": true, "modelAlias": true,
		"turnStep": true, "usageScope": true, "usage": true,
	}
	got := jsonTagsOf(t, "wireLine")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wireLine json fields = %v, want %v", keys(got), keys(want))
	}

	wantUsage := map[string]bool{
		"inputOther": true, "output": true, "inputCacheRead": true, "inputCacheCreation": true,
	}
	if gotUsage := jsonTagsOf(t, "tokenUsage"); !reflect.DeepEqual(gotUsage, wantUsage) {
		t.Fatalf("tokenUsage json fields = %v, want %v", keys(gotUsage), keys(wantUsage))
	}
}

// --- helpers ---------------------------------------------------------------

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// jsonTagsOf returns the json tag names declared by a struct type in this
// package's source.
func jsonTagsOf(t *testing.T, typeName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "kimicode.go", nil, 0)
	if err != nil {
		t.Fatalf("parse kimicode.go: %v", err)
	}
	out := map[string]bool{}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typeName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		found = true
		for _, fld := range st.Fields.List {
			if fld.Tag == nil {
				t.Errorf("%s has an untagged field", typeName)
				continue
			}
			tag := reflect.StructTag(strings.Trim(fld.Tag.Value, "`")).Get("json")
			name, _, _ := strings.Cut(tag, ",")
			out[name] = true
		}
		return false
	})
	if !found {
		t.Fatalf("type %s not found in kimicode.go", typeName)
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
