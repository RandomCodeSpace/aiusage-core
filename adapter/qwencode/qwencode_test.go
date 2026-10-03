package qwencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// The live fixture is the two records this machine's Qwen Code install actually
// wrote (~/.qwen/usage/token-usage-2026-08.jsonl), with the two UUIDs replaced
// by synthetic ones. Every counter, timestamp, model id, auth type and source is
// the real value.
const (
	liveSession = "9d1f7a24-0000-4000-8000-000000000000"
	liveID1     = "9d1f7a24-0001-4000-8000-000000000001"
	liveID2     = "9d1f7a24-0002-4000-8000-000000000002"
)

// discover runs discovery against a testdata root passed as an explicit
// override, so an ambient QWEN_* variable on the developer's machine cannot
// redirect the test.
func discover(t *testing.T, root string) []adapter.Source {
	t.Helper()
	srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{
		Home:      t.TempDir(),
		Overrides: map[string]string{model.ToolQwenCode: root},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return srcs
}

// collectRoot discovers and collects every source under root, returning the
// merged observation and the last error.
func collectRoot(t *testing.T, root string) (adapter.Observation, error) {
	t.Helper()
	var (
		out  adapter.Observation
		lerr error
	)
	for _, src := range discover(t, root) {
		obs, err := Adapter{}.Collect(context.Background(), src)
		if err != nil {
			lerr = err
		}
		out.Events = append(out.Events, obs.Events...)
		out.Activity = append(out.Activity, obs.Activity...)
		out.TurnContexts = append(out.TurnContexts, obs.TurnContexts...)
	}
	return out, lerr
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

// TestCollectLiveFixtureEmitsExactEvents pins every field of the events parsed
// from the real ledger records, dedup keys and audit payload included.
func TestCollectLiveFixtureEmitsExactEvents(t *testing.T) {
	root := filepath.Join("testdata", "live")
	path := filepath.Join(root, "usage", "token-usage-2026-08.jsonl")

	obs, err := collectRoot(t, root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	want := []model.UsageEvent{
		{
			Tool:         model.ToolQwenCode,
			Model:        "gemma4:31b",
			SessionID:    liveSession,
			EventTime:    mustTime(t, "2026-08-16T04:04:34.251Z"),
			InputTokens:  33550,
			OutputTokens: 8,
			TotalTokens:  33558,
			MessageID:    liveID1,
			SourcePath:   path,
			DedupKey:     "qwen-code|" + liveID1,
			Kind:         model.KindUsage,
			Raw: `{"schemaVersion":1,"id":"` + liveID1 + `","timestamp":"2026-08-16T04:04:34.251Z",` +
				`"localDate":"2026-08-16","localMonth":"2026-08","sessionId":"` + liveSession + `",` +
				`"model":"gemma4:31b","authType":"openai","source":"main","inputTokens":33550,` +
				`"outputTokens":8,"cachedTokens":0,"thoughtsTokens":0,"totalTokens":33558,"apiDurationMs":2503}`,
		},
		{
			Tool:         model.ToolQwenCode,
			Model:        "gemma4:31b",
			SessionID:    liveSession,
			EventTime:    mustTime(t, "2026-08-16T04:04:34.965Z"),
			InputTokens:  11105,
			OutputTokens: 19,
			TotalTokens:  11124,
			MessageID:    liveID2,
			SourcePath:   path,
			DedupKey:     "qwen-code|" + liveID2,
			Kind:         model.KindUsage,
			Raw: `{"schemaVersion":1,"id":"` + liveID2 + `","timestamp":"2026-08-16T04:04:34.965Z",` +
				`"localDate":"2026-08-16","localMonth":"2026-08","sessionId":"` + liveSession + `",` +
				`"model":"gemma4:31b","authType":"openai","source":"managed-auto-memory-extractor",` +
				`"inputTokens":11105,"outputTokens":19,"cachedTokens":0,"thoughtsTokens":0,` +
				`"totalTokens":11124,"apiDurationMs":688}`,
		},
	}
	if !reflect.DeepEqual(obs.Events, want) {
		t.Fatalf("events mismatch:\n got %#v\nwant %#v", obs.Events, want)
	}
}

// TestProviderStaysUnknown pins the deliberate refusal to translate authType
// into a billing provider: "openai" names the wire protocol, and this machine's
// own records carry it against a model served from localhost.
func TestProviderStaysUnknown(t *testing.T) {
	obs, err := collectRoot(t, filepath.Join("testdata", "live"))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, e := range obs.Events {
		if e.Provider != "" {
			t.Errorf("event %s: Provider = %q, want unknown (empty)", e.DedupKey, e.Provider)
		}
		if !strings.Contains(e.Raw, `"authType":"openai"`) {
			t.Errorf("event %s: audit payload dropped authType: %s", e.DedupKey, e.Raw)
		}
	}
}

// TestSubagentSourceBecomesAgentTurnContext covers the one attribution this
// surface supports: `source` is subagent_name || "main", so the second live
// record (written by the managed-auto-memory-extractor subagent) produces an
// agent-dimension context and the "main" record produces none.
func TestSubagentSourceBecomesAgentTurnContext(t *testing.T) {
	root := filepath.Join("testdata", "live")
	obs, err := collectRoot(t, root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []model.TurnContext{{
		UsageDedupKey: "qwen-code|" + liveID2,
		Tool:          model.ToolQwenCode,
		Dimension:     model.DimensionAgent,
		Value:         "managed-auto-memory-extractor",
		SessionID:     liveSession,
		Model:         "gemma4:31b",
		EventTime:     mustTime(t, "2026-08-16T04:04:34.965Z"),
		SourcePath:    filepath.Join(root, "usage", "token-usage-2026-08.jsonl"),
	}}
	if !reflect.DeepEqual(obs.TurnContexts, want) {
		t.Fatalf("turn contexts mismatch:\n got %#v\nwant %#v", obs.TurnContexts, want)
	}
}

// TestTurnContextKeysAnEmittedEvent enforces the contract rule that a context
// may only describe a usage event the same observation emitted.
func TestTurnContextKeysAnEmittedEvent(t *testing.T) {
	for _, root := range []string{"live", "edge", "planted"} {
		obs, _ := collectRoot(t, filepath.Join("testdata", root))
		keys := make(map[string]bool, len(obs.Events))
		for _, e := range obs.Events {
			keys[e.DedupKey] = true
		}
		seen := make(map[string]bool, len(obs.TurnContexts))
		for _, c := range obs.TurnContexts {
			if !keys[c.UsageDedupKey] {
				t.Errorf("%s: context %q names no emitted event", root, c.UsageDedupKey)
			}
			k := c.UsageDedupKey + "|" + string(c.Dimension)
			if seen[k] {
				t.Errorf("%s: two contexts for %s", root, k)
			}
			seen[k] = true
			if c.Value == "" {
				t.Errorf("%s: empty context value for %s", root, c.UsageDedupKey)
			}
		}
	}
}

// TestNoActivityIsEmitted pins the coverage claim: the ledger records API
// responses, never invocations, so there is nothing to put in the activity
// stream and nothing is invented from adjacency.
func TestNoActivityIsEmitted(t *testing.T) {
	for _, root := range []string{"live", "edge", "planted"} {
		obs, _ := collectRoot(t, filepath.Join("testdata", root))
		if len(obs.Activity) != 0 {
			t.Errorf("%s: got %d activity rows, want none", root, len(obs.Activity))
		}
	}
}

// TestBucketsFromTimestampNotFileNameOrLocalDate is TRAP 1. The edge fixture is
// named token-usage-2020-01.jsonl and every record inside claims localDate
// 2020-01-15 / localMonth 2020-01, while the timestamps are August 2026. Both
// the file name and those two fields are the WRITER's local calendar; a ledger
// copied from another machine keeps them. Only `timestamp` may decide the
// bucket.
func TestBucketsFromTimestampNotFileNameOrLocalDate(t *testing.T) {
	root := filepath.Join("testdata", "edge")
	obs, _ := collectRoot(t, root)
	if len(obs.Events) == 0 {
		t.Fatal("no events parsed from the edge fixture")
	}
	for _, e := range obs.Events {
		if e.MessageID == "e-7" {
			continue // the unparseable-timestamp case, asserted separately
		}
		if got := e.EventTime.UTC().Format("2006-01"); got != "2026-08" {
			t.Errorf("event %s bucketed at %s, want 2026-08 (from timestamp, not the "+
				"2020-01 file name or localMonth)", e.MessageID, got)
		}
	}
	// And the writer-local claims are still kept verbatim in the audit payload,
	// where they describe what the writer said rather than deciding anything.
	if !strings.Contains(obs.Events[0].Raw, `"localMonth":"2020-01"`) {
		t.Errorf("audit payload dropped the writer's localMonth: %s", obs.Events[0].Raw)
	}
}

// TestTokenMapping pins the accounting this surface reports: cached is a subset
// of the prompt count, thoughts are a subset of the output count (the
// OpenAI-compatible wires write candidatesTokenCount = completion_tokens beside
// thoughtsTokenCount = completion_tokens_details.reasoning_tokens, which is one
// of them), and a provider total below the components that are NOT subsets is
// raised rather than stored.
func TestTokenMapping(t *testing.T) {
	obs, err := collectRoot(t, filepath.Join("testdata", "edge"))
	if err == nil {
		t.Fatal("want a non-fatal error reporting the skipped records")
	}
	byID := map[string]model.UsageEvent{}
	for _, e := range obs.Events {
		byID[e.MessageID] = e
	}

	type want struct{ in, out, cacheRead, reasoning, total int64 }
	cases := map[string]want{
		// Plain record: nothing to subtract.
		"e-1": {in: 100, out: 10, total: 110},
		// cachedTokens is a SUBSET of inputTokens: 5000 prompt tokens of which
		// 4000 came from cache. Adding them beside input would report 9000.
		"e-2": {in: 1000, out: 20, cacheRead: 4000, total: 5020},
		// The harness's own fallback when the API omits prompt tokens:
		// inputTokens == cachedTokens, so Input is legitimately zero.
		"e-3": {in: 0, out: 5, cacheRead: 700, total: 705},
		// thoughtsTokens is INSIDE outputTokens: 100 completion tokens of which
		// 70 were reasoning, totalling 300 with the prompt. The provider's own
		// total is kept — adding reasoning to the floor would store 370 for a
		// turn that cost 300, an overstatement no later row can take back.
		"e-4": {in: 200, out: 100, reasoning: 70, total: 300},
		// cached > input cannot happen upstream but must not produce a negative
		// Input if it ever does: the clamp caps cached at the prompt count.
		"e-6": {in: 0, out: 4, cacheRead: 10, total: 14},
	}
	for id, w := range cases {
		e, ok := byID[id]
		if !ok {
			t.Errorf("record %s produced no event", id)
			continue
		}
		got := want{e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.ReasoningTokens, e.TotalTokens}
		if got != w {
			t.Errorf("record %s mapped to %+v, want %+v", id, got, w)
		}
		if e.CacheCreationTokens != 0 {
			t.Errorf("record %s: CacheCreationTokens = %d, but the surface reports none",
				id, e.CacheCreationTokens)
		}
	}
	if _, ok := byID["e-5"]; ok {
		t.Error("the all-zero record produced an event; a record with no tokens is not usage")
	}
}

// TestTotalIsTheProviderFigureOrTheComponentFloor pins BOTH directions of the
// stored total over every fixture, because each has its own way of being wrong.
//
// Below: the issue #49 invariant — a row may not claim a total under the counts
// stored beside it. Above: the total may not exceed the provider's own figure
// for any other reason, and reasoning is the one that would. thoughtsTokens is
// inside outputTokens on the OpenAI-compatible wires, so a floor that added it
// would inflate every reasoning turn by its reasoning count and append that to
// an immutable ledger. Cache read is outside the floor for the same reason,
// which is why the equality is exact rather than a pair of bounds.
func TestTotalIsTheProviderFigureOrTheComponentFloor(t *testing.T) {
	for _, root := range []string{"live", "edge", "planted"} {
		obs, _ := collectRoot(t, filepath.Join("testdata", root))
		if len(obs.Events) == 0 {
			t.Errorf("%s: no events", root)
		}
		for _, e := range obs.Events {
			var raw struct {
				TotalTokens int64 `json:"totalTokens"`
			}
			if err := json.Unmarshal([]byte(e.Raw), &raw); err != nil {
				t.Fatalf("%s/%s: audit payload unreadable: %v", root, e.MessageID, err)
			}
			want := e.InputTokens + e.CacheReadTokens + e.CacheCreationTokens + e.OutputTokens
			if raw.TotalTokens > want {
				want = raw.TotalTokens
			}
			if e.TotalTokens != want {
				t.Errorf("%s/%s: total %d, want max(provider %d, floor in+cache+out) = %d "+
					"(reasoning %d is inside output and must not move it)",
					root, e.MessageID, e.TotalTokens, raw.TotalTokens, want, e.ReasoningTokens)
			}
		}
	}
}

// TestRecordWithoutProviderUUIDIsSkipped covers the refusal to invent a dedup
// key: the id-less record and the malformed line are both dropped and both
// reported, while every well-formed record around them still lands.
func TestRecordWithoutProviderUUIDIsSkipped(t *testing.T) {
	obs, err := collectRoot(t, filepath.Join("testdata", "edge"))
	if err == nil {
		t.Fatal("want a non-fatal error naming the skipped records")
	}
	if !strings.Contains(err.Error(), "skipped 2 unusable record(s)") {
		t.Errorf("error = %v, want it to name the 2 skipped records", err)
	}
	for _, e := range obs.Events {
		if e.DedupKey == "" || e.DedupKey == model.ToolQwenCode+"|" {
			t.Errorf("event with an empty provider id got key %q", e.DedupKey)
		}
		if e.MessageID == "" {
			t.Errorf("event %q carries no provider id", e.DedupKey)
		}
	}
	if got := len(obs.Events); got != 6 {
		t.Errorf("got %d events, want 6 (8 usable-looking records minus the id-less one and the all-zero one)", got)
	}
}

// TestUnparseableTimestampFallsBackToMtime pins the fallback and, more
// importantly, that it cannot cause a recount: the dedup key is the provider
// UUID and contains no time at all.
func TestUnparseableTimestampFallsBackToMtime(t *testing.T) {
	root := filepath.Join("testdata", "edge")
	path := filepath.Join(root, "usage", "token-usage-2020-01.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	obs, _ := collectRoot(t, root)
	for _, e := range obs.Events {
		if e.MessageID != "e-7" {
			continue
		}
		if !e.EventTime.Equal(fi.ModTime().UTC()) {
			t.Errorf("EventTime = %s, want the file mtime %s", e.EventTime, fi.ModTime().UTC())
		}
		if e.DedupKey != "qwen-code|e-7" {
			t.Errorf("DedupKey = %q, want the provider id alone so the mtime cannot move it", e.DedupKey)
		}
		return
	}
	t.Fatal("record e-7 produced no event")
}

// TestDedupKeyIsTheProviderUUIDAlone checks the key survives the two things that
// move a ledger: a different directory, and a full re-read after a tail read. It
// carries no path, no session and no offset, so a copied ledger counts once.
func TestDedupKeyIsTheProviderUUIDAlone(t *testing.T) {
	dir := t.TempDir()
	copyLedger(t, filepath.Join("testdata", "live", "usage", "token-usage-2026-08.jsonl"),
		filepath.Join(dir, "usage", "token-usage-2026-08.jsonl"))

	from := func(root string) []string {
		obs, err := collectRoot(t, root)
		if err != nil {
			t.Fatalf("collect %s: %v", root, err)
		}
		var keys []string
		for _, e := range obs.Events {
			keys = append(keys, e.DedupKey)
		}
		return keys
	}
	orig, copied := from(filepath.Join("testdata", "live")), from(dir)
	if !reflect.DeepEqual(orig, copied) {
		t.Fatalf("keys differ after copying the ledger:\n got %v\nwant %v", copied, orig)
	}
	want := []string{"qwen-code|" + liveID1, "qwen-code|" + liveID2}
	if !reflect.DeepEqual(orig, want) {
		t.Fatalf("keys = %v, want %v", orig, want)
	}
}

// TestIncrementalTailReadsOnlyNewRecords exercises the three checkpoint paths:
// an unchanged file is not read at all, growth is tailed from the stored offset,
// and a rewrite restarts from zero.
func TestIncrementalTailReadsOnlyNewRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage", "token-usage-2026-08.jsonl")
	copyLedger(t, filepath.Join("testdata", "live", "usage", "token-usage-2026-08.jsonl"), path)

	srcs := discover(t, dir)
	if len(srcs) != 1 {
		t.Fatalf("discovered %d sources, want 1", len(srcs))
	}
	src := srcs[0]

	first, err := Adapter{}.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if len(first.Events) != 2 {
		t.Fatalf("first collect got %d events, want 2", len(first.Events))
	}
	cp := first.Checkpoint
	if cp == nil {
		t.Fatal("first collect returned no checkpoint")
	}

	// Unchanged: the file must not be parsed again, and the stored checkpoint
	// must be left alone.
	same, err := Adapter{}.CollectIncremental(context.Background(), src, cp)
	if err != nil {
		t.Fatalf("unchanged collect: %v", err)
	}
	if len(same.Events) != 0 || same.Checkpoint != nil {
		t.Fatalf("unchanged file produced %d events / checkpoint %v", len(same.Events), same.Checkpoint)
	}

	// Growth: only the appended record comes back.
	appendLine(t, path, `{"schemaVersion":1,"id":"tail-1","timestamp":"2026-08-16T05:00:00.000Z",`+
		`"localDate":"2026-08-16","localMonth":"2026-08","sessionId":"`+liveSession+`",`+
		`"model":"gemma4:31b","authType":"openai","source":"main","inputTokens":7,"outputTokens":2,`+
		`"cachedTokens":0,"thoughtsTokens":0,"totalTokens":9,"apiDurationMs":10}`)
	grown, err := Adapter{}.CollectIncremental(context.Background(), src, cp)
	if err != nil {
		t.Fatalf("tail collect: %v", err)
	}
	if len(grown.Events) != 1 || grown.Events[0].MessageID != "tail-1" {
		t.Fatalf("tail collect got %+v, want only tail-1", grown.Events)
	}

	// A rewrite (same size, different content) is unknown history: restart from
	// zero. Re-derived keys collapse in the store, so re-reading is always safe.
	rewritten := grown.Checkpoint
	if rewritten == nil {
		t.Fatal("tail collect returned no checkpoint")
	}
	stale := *rewritten
	stale.Size = 1 << 30 // pretend the file shrank
	full, err := Adapter{}.CollectIncremental(context.Background(), src, &stale)
	if err != nil {
		t.Fatalf("re-read collect: %v", err)
	}
	if len(full.Events) != 3 {
		t.Fatalf("shrink re-read got %d events, want all 3", len(full.Events))
	}
}

// TestCollectDoesNotTouchTheLedger is the observational invariant: the adapter
// may not change the size, mode or modification time of a file the agent owns.
func TestCollectDoesNotTouchTheLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage", "token-usage-2026-08.jsonl")
	copyLedger(t, filepath.Join("testdata", "live", "usage", "token-usage-2026-08.jsonl"), path)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, err := collectRoot(t, dir); err != nil {
		t.Fatalf("collect: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		t.Errorf("the ledger changed under collection: %v/%v/%v -> %v/%v/%v",
			before.Size(), before.ModTime(), before.Mode(),
			after.Size(), after.ModTime(), after.Mode())
	}
}

// TestDiscoverIsSilentWithoutAUsageDir is TRAP 2. The harness only writes the
// ledger while privacy.usageStatisticsEnabled is on, so a missing usage/
// directory means "never ran OR opted out" — never a fault, and never a reason
// to go looking at the transcripts the user did not consent to have counted.
func TestDiscoverIsSilentWithoutAUsageDir(t *testing.T) {
	empty := t.TempDir() // exists, no usage/ inside
	srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{
		Home:      t.TempDir(),
		Overrides: map[string]string{model.ToolQwenCode: empty},
	})
	if err != nil {
		t.Fatalf("Discover on an opted-out root returned %v, want no error", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("Discover found %d sources under a root with no usage/ dir", len(srcs))
	}

	missing, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{
		Home:      t.TempDir(),
		Overrides: map[string]string{model.ToolQwenCode: filepath.Join(empty, "nope")},
	})
	if err != nil || len(missing) != 0 {
		t.Fatalf("Discover on a missing root = %v, %v", missing, err)
	}
}

// TestDiscoverMatchesOnlyLedgerFiles keeps the sibling files the runtime
// directory accumulates out of the source list.
func TestDiscoverMatchesOnlyLedgerFiles(t *testing.T) {
	dir := t.TempDir()
	usage := filepath.Join(dir, "usage")
	if err := os.MkdirAll(usage, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"token-usage-2026-08.jsonl", "token-usage-2026-07.jsonl",
		"token-usage-2026-08.jsonl.tmp", "usage_record.jsonl", "notes.txt", "token-usage-2026-06.json",
	} {
		if err := os.WriteFile(filepath.Join(usage, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(usage, "token-usage-dir.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, s := range discover(t, dir) {
		got = append(got, filepath.Base(s.Path))
		if s.Tool != model.ToolQwenCode || s.Class != model.EventLevel {
			t.Errorf("source %s: tool/class = %s/%s", s.Path, s.Tool, s.Class)
		}
		if s.Label == "" {
			t.Errorf("source %s has no label", s.Path)
		}
	}
	want := []string{"token-usage-2026-07.jsonl", "token-usage-2026-08.jsonl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("discovered %v, want %v", got, want)
	}
}

// TestRootPrecedence pins the harness's own resolution order:
// override > QWEN_RUNTIME_DIR > QWEN_HOME > ~/.qwen.
func TestRootPrecedence(t *testing.T) {
	home := t.TempDir()
	runtime := t.TempDir()
	qwenHome := t.TempDir()
	override := t.TempDir()
	for _, d := range []string{home, runtime, qwenHome, override} {
		if err := os.MkdirAll(filepath.Join(d, "usage"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "usage", "token-usage-2026-08.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The home default lives at <home>/.qwen, not <home>.
	dotQwen := filepath.Join(home, ".qwen", "usage")
	if err := os.MkdirAll(dotQwen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotQwen, "token-usage-2026-08.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	rootOf := func(t *testing.T, cfg adapter.DiscoverConfig) string {
		t.Helper()
		srcs, err := Adapter{}.Discover(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(srcs) != 1 {
			t.Fatalf("discovered %d sources, want 1", len(srcs))
		}
		return srcs[0].Meta["root"]
	}

	t.Run("home default", func(t *testing.T) {
		t.Setenv(RuntimeDirEnv, "")
		t.Setenv(HomeEnv, "")
		if got := rootOf(t, adapter.DiscoverConfig{Home: home}); got != filepath.Join(home, ".qwen") {
			t.Errorf("root = %s, want <home>/.qwen", got)
		}
	})
	t.Run("QWEN_HOME beats the default", func(t *testing.T) {
		t.Setenv(RuntimeDirEnv, "")
		t.Setenv(HomeEnv, qwenHome)
		if got := rootOf(t, adapter.DiscoverConfig{Home: home}); got != qwenHome {
			t.Errorf("root = %s, want %s", got, qwenHome)
		}
	})
	t.Run("QWEN_RUNTIME_DIR beats QWEN_HOME", func(t *testing.T) {
		t.Setenv(RuntimeDirEnv, runtime)
		t.Setenv(HomeEnv, qwenHome)
		if got := rootOf(t, adapter.DiscoverConfig{Home: home}); got != runtime {
			t.Errorf("root = %s, want %s", got, runtime)
		}
	})
	t.Run("an explicit override beats both", func(t *testing.T) {
		t.Setenv(RuntimeDirEnv, runtime)
		t.Setenv(HomeEnv, qwenHome)
		cfg := adapter.DiscoverConfig{Home: home, Overrides: map[string]string{model.ToolQwenCode: override}}
		if got := rootOf(t, cfg); got != override {
			t.Errorf("root = %s, want %s", got, override)
		}
	})
	t.Run("a relative value is dropped, not resolved against our cwd", func(t *testing.T) {
		t.Setenv(RuntimeDirEnv, "relative/runtime")
		t.Setenv(HomeEnv, qwenHome)
		if got := rootOf(t, adapter.DiscoverConfig{Home: home}); got != qwenHome {
			t.Errorf("root = %s, want the next candidate %s: the harness resolves a relative "+
				"value against its OWN cwd, which this process cannot know", got, qwenHome)
		}
	})
	t.Run("a tilde is expanded like the harness expands it", func(t *testing.T) {
		t.Setenv(RuntimeDirEnv, "~/.qwen")
		t.Setenv(HomeEnv, qwenHome)
		if got := rootOf(t, adapter.DiscoverConfig{Home: home}); got != filepath.Join(home, ".qwen") {
			t.Errorf("root = %s, want <home>/.qwen", got)
		}
	})
}

// copyLedger writes src's bytes to dst, creating dst's parent.
func copyLedger(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// appendLine appends one JSONL record, the way the harness does.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}
