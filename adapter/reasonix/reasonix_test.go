package reasonix

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// dayFile is the name every fixture is installed under, matching the writer's
// YYYY-MM-DD.jsonl daily convention.
const dayFile = "2026-08-16.jsonl"

// newRoot builds a Reasonix state root in a temp dir with one fixture installed
// as <root>/stats/2026-08-16.jsonl, and returns (root, ledgerPath).
func newRoot(t *testing.T, fixture string) (string, string) {
	t.Helper()
	root := t.TempDir()
	stats := filepath.Join(root, "stats")
	if err := os.MkdirAll(stats, 0o755); err != nil {
		t.Fatalf("mkdir stats: %v", err)
	}
	body, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	path := filepath.Join(stats, dayFile)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	return root, path
}

// collectAll discovers under an explicit override and collects every source.
// A collect error is non-fatal by contract — a malformed line must never cost
// the rest of the file — so the events are taken regardless and the error path
// is asserted where it belongs, in TestCollectShapes.
func collectAll(t *testing.T, root string) []model.UsageEvent {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{
		Overrides: map[string]string{model.ToolReasonix: root},
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var out []model.UsageEvent
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil && len(obs.Events) == 0 {
			t.Fatalf("collect %s: %v", s.Path, err)
		}
		out = append(out, obs.Events...)
	}
	return out
}

// TestCollectLiveLedgerExactEvents pins the adapter against the three records a
// real Reasonix install wrote on this machine, byte for byte. All three are
// cost_complete=false / incomplete_reason="no_price", which is the state this
// harness reports whenever it has no price table for the model — the shape the
// unpriced-is-not-$0 rule exists for.
func TestCollectLiveLedgerExactEvents(t *testing.T) {
	root, path := newRoot(t, "live-2026-08-16.jsonl")
	got := collectAll(t, root)

	want := []model.UsageEvent{{
		Tool:            model.ToolReasonix,
		Model:           "ollama/gemma4:31b-cloud",
		Provider:        "ollama",
		EventTime:       time.Date(2026, 8, 16, 4, 12, 0, 816750414, time.UTC),
		InputTokens:     6207,
		OutputTokens:    8,
		CacheReadTokens: 0,
		TotalTokens:     6215,
		SourcePath:      path,
		DedupKey:        "reasonix|4f396ae6bb6b0b909f2bf2ff64d38d2c",
		Kind:            model.KindUsage,
	}, {
		Tool:            model.ToolReasonix,
		Model:           "ollama/gemma4:31b-cloud",
		Provider:        "ollama",
		EventTime:       time.Date(2026, 8, 16, 4, 48, 16, 522162102, time.UTC),
		InputTokens:     5718,
		OutputTokens:    2,
		CacheReadTokens: 0,
		TotalTokens:     5720,
		SourcePath:      path,
		DedupKey:        "reasonix|af5e388886926e2d13e6359d318f3ba7",
		Kind:            model.KindUsage,
	}, {
		// Counter for counter identical to the record above — same model, same
		// 5718/2/5720 — and a different key, because the nanosecond ts is part
		// of the bytes being hashed. See TestIdenticalCountersKeepDistinctKeys.
		Tool:            model.ToolReasonix,
		Model:           "ollama/gemma4:31b-cloud",
		Provider:        "ollama",
		EventTime:       time.Date(2026, 8, 16, 5, 0, 8, 931387150, time.UTC),
		InputTokens:     5718,
		OutputTokens:    2,
		CacheReadTokens: 0,
		TotalTokens:     5720,
		SourcePath:      path,
		DedupKey:        "reasonix|cf5c641c2509f720f3e2a1ac24cad1e6",
		Kind:            model.KindUsage,
	}}

	if len(got) != len(want) {
		t.Fatalf("events = %d, want %d", len(got), len(want))
	}
	for i := range want {
		g := got[i]
		g.Raw = "" // asserted separately, in the audit-payload test
		if !g.EventTime.Equal(want[i].EventTime) {
			t.Errorf("event %d time = %s, want %s", i, g.EventTime, want[i].EventTime)
		}
		g.EventTime, want[i].EventTime = time.Time{}, time.Time{}
		if g != want[i] {
			t.Errorf("event %d =\n %+v\nwant\n %+v", i, g, want[i])
		}
	}
}

// TestLiveLedgerRecordsAreEventsNotCumulative is the cumulative-vs-event bug
// class, settled on live bytes: the second record's prompt is SMALLER than the
// first's, so these lines cannot be a running total, and the two are stored at
// their own values rather than diffed or maxed. Reasonix's own index of these
// files agrees — it SUMs the same columns into its day rollup.
func TestLiveLedgerRecordsAreEventsNotCumulative(t *testing.T) {
	root, _ := newRoot(t, "live-2026-08-16.jsonl")
	got := collectAll(t, root)
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3", len(got))
	}
	if got[1].InputTokens >= got[0].InputTokens {
		t.Fatalf("fixture no longer exercises the trap: inputs %d then %d",
			got[0].InputTokens, got[1].InputTokens)
	}
	if got[0].TotalTokens != 6215 || got[1].TotalTokens != 5720 {
		t.Errorf("totals = %d, %d; want the record's own 6215, 5720 (a diff would give 6215, -495)",
			got[0].TotalTokens, got[1].TotalTokens)
	}
}

// TestIdenticalCountersKeepDistinctKeys is the collision-safety claim behind a
// content-hash key, and it is not an argument — records 2 and 3 of the live
// ledger are the same model with the same 5718/2/5720 counters, produced by two
// separate real runs. They stay two events because the RFC3339 NANOSECOND
// timestamp is part of the bytes being hashed. If reasonix ever drops the
// sub-second precision, this test fails and the key needs rethinking.
func TestIdenticalCountersKeepDistinctKeys(t *testing.T) {
	root, _ := newRoot(t, "live-2026-08-16.jsonl")
	got := collectAll(t, root)
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3", len(got))
	}
	a, b := got[1], got[2]
	if a.InputTokens != b.InputTokens || a.OutputTokens != b.OutputTokens ||
		a.TotalTokens != b.TotalTokens || a.Model != b.Model {
		t.Fatalf("fixture no longer exercises the trap: %+v vs %+v", a, b)
	}
	if a.DedupKey == b.DedupKey {
		t.Fatalf("two distinct requests collapsed onto one key %q", a.DedupKey)
	}
	if a.EventTime.Equal(b.EventTime) {
		t.Fatal("the two records share a timestamp; the key has nothing left to separate them")
	}
}

// TestUnpricedIsNotZero: this surface carries cost HONESTY FLAGS and no amount,
// so no cost is stamped. A stamped 0 would claim the requests were free, and an
// append-only ledger could never take that back.
func TestUnpricedIsNotZero(t *testing.T) {
	root, _ := newRoot(t, "live-2026-08-16.jsonl")
	for i, ev := range collectAll(t, root) {
		if _, priced := ev.Cost(); priced {
			t.Errorf("event %d carries a cost; this surface reports no amount", i)
		}
		if ev.PriceSource != "" {
			t.Errorf("event %d price source = %q, want empty (unpriced)", i, ev.PriceSource)
		}
	}
}

// TestAuditPayloadKeepsTheHonestyFlags: an unpriced row has to be able to say
// WHY, and the flags are the only place that lives.
func TestAuditPayloadKeepsTheHonestyFlags(t *testing.T) {
	root, _ := newRoot(t, "live-2026-08-16.jsonl")
	got := collectAll(t, root)
	if len(got) == 0 {
		t.Fatal("no events")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(got[0].Raw), &raw); err != nil {
		t.Fatalf("raw is not JSON: %v (%q)", err, got[0].Raw)
	}
	want := map[string]any{
		"usage_source":      "executor",
		"cost_complete":     false,
		"cost_estimated":    true,
		"display_complete":  false,
		"display_status":    "unavailable",
		"incomplete_reason": "no_price",
		"cache_miss":        float64(6207),
	}
	for k, v := range want {
		if raw[k] != v {
			t.Errorf("raw[%q] = %v, want %v", k, raw[k], v)
		}
	}
}

// TestCollectShapes covers the counter arithmetic on constructed records: cache
// subset, reasoning, a disagreeing cache_miss, an over-large cache_hit, a
// missing total, an all-zero line, a blank line, a malformed line and negative
// counters.
func TestCollectShapes(t *testing.T) {
	root, _ := newRoot(t, "shapes-2026-08-16.jsonl")
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{
		Overrides: map[string]string{model.ToolReasonix: root},
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("sources = %d, want 1", len(srcs))
	}
	obs, err := a.Collect(context.Background(), srcs[0])
	// One malformed line is reported as a non-fatal error alongside the events.
	if err == nil {
		t.Fatal("want a non-fatal error naming the malformed line")
	}
	got := obs.Events

	type want struct {
		model, provider                          string
		input, output, reasoning, cacheRead, tot int64
	}
	wants := []want{
		{"deepseek/deepseek-reasoner", "deepseek", 360, 300, 220, 640, 1300},
		{"ollama/qwen3.5:cloud", "ollama", 400, 50, 0, 100, 550},
		{"bare-model", "", 0, 5, 0, 10, 15},
		{"x/y", "x", 7, 3, 0, 0, 10},
		{"x/y", "x", 0, 9, 0, 0, 9},
	}
	if len(got) != len(wants) {
		t.Fatalf("events = %d, want %d (all-zero and blank lines drop, malformed is skipped)",
			len(got), len(wants))
	}
	for i, w := range wants {
		g := got[i]
		if g.Model != w.model || g.Provider != w.provider {
			t.Errorf("event %d model/provider = %q/%q, want %q/%q", i, g.Model, g.Provider, w.model, w.provider)
		}
		if g.InputTokens != w.input || g.OutputTokens != w.output ||
			g.ReasoningTokens != w.reasoning || g.CacheReadTokens != w.cacheRead || g.TotalTokens != w.tot {
			t.Errorf("event %d counters = in %d out %d reas %d cache %d total %d; want %d %d %d %d %d",
				i, g.InputTokens, g.OutputTokens, g.ReasoningTokens, g.CacheReadTokens, g.TotalTokens,
				w.input, w.output, w.reasoning, w.cacheRead, w.tot)
		}
		if g.CacheCreationTokens != 0 {
			t.Errorf("event %d cache creation = %d, want 0 (reasonix reports hit/miss only)", i, g.CacheCreationTokens)
		}
	}
}

// TestInputIsDerivedFromPromptNotCacheMiss is the assigned-not-accumulated trap
// in its local form: two columns describe the same quantity and only one of them
// is what `total` was built from. Record 2 of the shapes fixture claims
// cache_miss=999 against prompt=500/cache_hit=100; deriving input from `prompt`
// keeps the stored components summing to the authoritative total, and the
// source's own claim survives in the audit payload rather than in the ledger.
func TestInputIsDerivedFromPromptNotCacheMiss(t *testing.T) {
	root, _ := newRoot(t, "shapes-2026-08-16.jsonl")
	got := collectAll(t, root)
	if len(got) < 2 {
		t.Fatalf("events = %d, want at least 2", len(got))
	}
	ev := got[1]
	if ev.InputTokens != 400 {
		t.Errorf("input = %d, want 400 (prompt 500 - cache_hit 100), not the claimed cache_miss 999", ev.InputTokens)
	}
	if sum := ev.ComputedTotal(); sum != ev.TotalTokens {
		t.Errorf("components sum to %d but the authoritative total is %d", sum, ev.TotalTokens)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(ev.Raw), &raw); err != nil {
		t.Fatalf("raw is not JSON: %v", err)
	}
	if raw["cache_miss"] != float64(999) {
		t.Errorf("raw cache_miss = %v, want the source's own 999", raw["cache_miss"])
	}
}

// TestBucketComesFromTheRecordNotTheFileName: the dailies are cut on the
// WRITER's local calendar, so a file named 2026-08-16.jsonl legitimately holds a
// record stamped 2026-08-15T22:30Z (any zone east of UTC crosses midnight
// early). Dating an event by its file would move usage between days for every
// user not on UTC.
func TestBucketComesFromTheRecordNotTheFileName(t *testing.T) {
	root, _ := newRoot(t, "shapes-2026-08-16.jsonl")
	got := collectAll(t, root)
	if len(got) == 0 {
		t.Fatal("no events")
	}
	want := time.Date(2026, 8, 15, 22, 30, 0, 1, time.UTC)
	if !got[0].EventTime.Equal(want) {
		t.Fatalf("event time = %s, want %s (the record's ts, not the file's day)", got[0].EventTime, want)
	}
	if got[0].EventTime.UTC().Day() == 16 {
		t.Error("the event was dated by its file name")
	}
}

// TestIncrementalSkipsUnchangedLedger: an unchanged size+mtime opens nothing and
// leaves the stored checkpoint alone.
func TestIncrementalSkipsUnchangedLedger(t *testing.T) {
	root, path := newRoot(t, "live-2026-08-16.jsonl")
	a := New().(Adapter)
	src := adapter.Source{Tool: model.ToolReasonix, Class: model.EventLevel, Path: path}
	_ = root

	first, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Checkpoint == nil {
		t.Fatal("first pass returned no checkpoint")
	}
	second, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Events) != 0 {
		t.Errorf("second pass re-read %d events from an unchanged file", len(second.Events))
	}
	if second.Checkpoint != nil {
		t.Error("second pass returned a checkpoint; an unchanged file must keep the stored one")
	}
}

// TestIncrementalTailReadsOnlyTheAppendedRecord is why a byte offset is safe on
// THIS surface: the dailies are append-only and every record is
// newline-terminated, so the stored offset lands exactly on a record boundary.
func TestIncrementalTailReadsOnlyTheAppendedRecord(t *testing.T) {
	_, path := newRoot(t, "live-2026-08-16.jsonl")
	a := New().(Adapter)
	src := adapter.Source{Tool: model.ToolReasonix, Class: model.EventLevel, Path: path}

	first, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Events) != 3 {
		t.Fatalf("first pass events = %d, want 3", len(first.Events))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if first.Checkpoint.Offset != fi.Size() {
		t.Fatalf("offset = %d, want the whole file %d", first.Checkpoint.Offset, fi.Size())
	}

	appendLine(t, path, `{"ts":"2026-08-16T06:00:00Z","model":"ollama/glm-5:cloud","source":"cli","prompt":11,"completion":4,"total":15,"requests":1}`)

	second, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Events) != 1 {
		t.Fatalf("second pass events = %d, want only the appended one", len(second.Events))
	}
	if second.Events[0].Model != "ollama/glm-5:cloud" || second.Events[0].TotalTokens != 15 {
		t.Errorf("appended event = %+v", second.Events[0])
	}
}

// TestIncrementalReReadsFromZeroOnShrink: a shrink or a same-size rewrite means
// unknown history, so the offset is not trusted. Re-reading is free of
// consequence precisely because the keys are content hashes.
func TestIncrementalReReadsFromZeroOnShrink(t *testing.T) {
	_, path := newRoot(t, "live-2026-08-16.jsonl")
	a := New().(Adapter)
	src := adapter.Source{Tool: model.ToolReasonix, Class: model.EventLevel, Path: path}

	first, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	keep := firstLine(t, path)
	if err := os.WriteFile(path, keep, 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Force a distinguishable mtime so the unchanged-file gate cannot fire.
	touch(t, path)

	second, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Events) != 1 {
		t.Fatalf("second pass events = %d, want 1 (a full re-read of the shrunken file)", len(second.Events))
	}
	if second.Events[0].DedupKey != first.Events[0].DedupKey {
		t.Errorf("re-read key = %q, want the identical %q; a re-read must collapse, not double count",
			second.Events[0].DedupKey, first.Events[0].DedupKey)
	}
	if second.Checkpoint == nil || second.Checkpoint.Offset != int64(len(keep)) {
		t.Errorf("checkpoint = %+v, want offset %d", second.Checkpoint, len(keep))
	}
}

// TestIncrementalReReadsFromZeroOnSameSizeRewrite is the OTHER half of the
// offset-trust rule, and the shrink test cannot reach it: there the stored
// offset is larger than the new file and would be rejected on its own bounds.
// A same-size rewrite leaves the offset perfectly in range and pointing at the
// end, so trusting it reads nothing and the replaced records are never seen —
// silent loss that looks exactly like an idle day. Only pure GROWTH is trusted.
func TestIncrementalReReadsFromZeroOnSameSizeRewrite(t *testing.T) {
	_, path := newRoot(t, "live-2026-08-16.jsonl")
	a := New().(Adapter)
	src := adapter.Source{Tool: model.ToolReasonix, Class: model.EventLevel, Path: path}

	first, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Three different records padded back out to the original byte count, so
	// size alone cannot tell the file was replaced. The padding is trailing
	// whitespace, which the reader already treats as padding rather than data.
	var rewritten []byte
	for i, n := range []int64{4001, 4002, 4003} {
		rewritten = append(rewritten, []byte(`{"ts":"2026-08-16T0`+string(rune('6'+i))+
			`:00:00.000000001Z","model":"ollama/gemma4:31b-cloud","source":"cli","prompt":`+
			strconv.FormatInt(n, 10)+`,"completion":8,"total":`+strconv.FormatInt(n+8, 10)+
			`,"requests":1}`+"\n")...)
	}
	if len(rewritten) > len(body) {
		t.Fatalf("rewrite is %d bytes, longer than the original %d", len(rewritten), len(body))
	}
	for len(rewritten) < len(body) {
		rewritten = append(rewritten, ' ')
	}
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	touch(t, path)

	second, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Events) != 3 {
		t.Fatalf("second pass events = %d, want 3 (a same-size rewrite must re-read from zero)", len(second.Events))
	}
	for i, ev := range second.Events {
		if ev.DedupKey == first.Events[i].DedupKey {
			t.Errorf("record %d kept the replaced record's key %q", i, ev.DedupKey)
		}
	}
}

// TestDedupKeyIsContentNotPosition is the reason the key is not the byte offset.
// Prepending a record shifts every later record's position; if the key rode on
// the offset, every shifted line would be counted a second time and an
// append-only ledger could never take it back.
func TestDedupKeyIsContentNotPosition(t *testing.T) {
	_, path := newRoot(t, "live-2026-08-16.jsonl")
	a := New().(Adapter)
	src := adapter.Source{Tool: model.ToolReasonix, Class: model.EventLevel, Path: path}

	before, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	head := []byte(`{"ts":"2026-08-16T00:00:00Z","model":"x/y","source":"cli","prompt":1,"completion":1,"total":2,"requests":1}` + "\n")
	if err := os.WriteFile(path, append(head, body...), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	after, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(after.Events) != len(before.Events)+1 {
		t.Fatalf("events = %d, want %d", len(after.Events), len(before.Events)+1)
	}
	for i, ev := range before.Events {
		if after.Events[i+1].DedupKey != ev.DedupKey {
			t.Errorf("record %d moved by %d bytes and changed key from %q to %q",
				i, len(head), ev.DedupKey, after.Events[i+1].DedupKey)
		}
	}
}

// TestUnterminatedTailIsNotConsumed: a final line with no newline is emitted,
// but the offset stops before it, so a write caught in progress is read whole on
// the next pass and its identical key collapses.
func TestUnterminatedTailIsNotConsumed(t *testing.T) {
	root := t.TempDir()
	stats := filepath.Join(root, "stats")
	if err := os.MkdirAll(stats, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(stats, dayFile)
	full := `{"ts":"2026-08-16T07:00:00Z","model":"x/y","source":"cli","prompt":4,"completion":1,"total":5,"requests":1}` + "\n"
	tail := `{"ts":"2026-08-16T07:01:00Z","model":"x/y","source":"cli","prompt":9,"completion":1,"total":10,"requests":1}`
	if err := os.WriteFile(path, []byte(full+tail), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := New().(Adapter)
	obs, err := a.CollectIncremental(context.Background(), adapter.Source{Tool: model.ToolReasonix, Path: path}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(obs.Events) != 2 {
		t.Fatalf("events = %d, want 2 (the unterminated tail is still emitted)", len(obs.Events))
	}
	if obs.Checkpoint.Offset != int64(len(full)) {
		t.Errorf("offset = %d, want %d — the unterminated line must stay unconsumed",
			obs.Checkpoint.Offset, len(full))
	}
}

// TestCollectDoesNotTouchTheLedger: strictly observational. Nothing about the
// file may change, including its access-independent metadata.
func TestCollectDoesNotTouchTheLedger(t *testing.T) {
	root, path := newRoot(t, "live-2026-08-16.jsonl")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	collectAll(t, root)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		t.Errorf("the ledger changed: %d/%s/%s -> %d/%s/%s",
			before.Size(), before.ModTime(), before.Mode(),
			after.Size(), after.ModTime(), after.Mode())
	}
}

// TestDiscoverIgnoresNonLedgerEntries: the stats directory also holds the
// writer's own .append.lock, which this adapter must never open.
func TestDiscoverIgnoresNonLedgerEntries(t *testing.T) {
	root, _ := newRoot(t, "live-2026-08-16.jsonl")
	stats := filepath.Join(root, "stats")
	for _, name := range []string{".append.lock", "index.db", "2026-08-15.jsonl.tmp"} {
		if err := os.WriteFile(filepath.Join(stats, name), nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(stats, "archive.jsonl"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{
		Overrides: map[string]string{model.ToolReasonix: root},
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 1 || filepath.Base(srcs[0].Path) != dayFile {
		t.Fatalf("sources = %+v, want only %s", srcs, dayFile)
	}
	if srcs[0].Class != model.EventLevel || srcs[0].Tool != model.ToolReasonix {
		t.Errorf("source = %+v, want an event-level %s source", srcs[0], model.ToolReasonix)
	}
	if srcs[0].Meta["day"] != "2026-08-16" {
		t.Errorf("meta day = %q, want 2026-08-16", srcs[0].Meta["day"])
	}
}

// TestDiscoverOnMissingRootIsNotAnError: Reasonix simply is not installed here.
func TestDiscoverOnMissingRootIsNotAnError(t *testing.T) {
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("sources = %d, want 0", len(srcs))
	}
}

// --- root resolution -------------------------------------------------------

// TestRootPrecedence pins the vendor's chain: REASONIX_STATE_HOME first, then
// REASONIX_HOME, then ~/.reasonix. Reading the wrong rung reports zero usage
// while looking perfectly healthy, which is the failure nobody notices.
func TestRootPrecedence(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	userHome := t.TempDir()

	t.Setenv(StateHomeEnv, state)
	t.Setenv(HomeEnv, home)
	if got := StatsDir(adapter.DiscoverConfig{Home: userHome}); got != filepath.Join(state, "stats") {
		t.Errorf("with both set, stats dir = %q, want %q", got, filepath.Join(state, "stats"))
	}

	t.Setenv(StateHomeEnv, "")
	if got := StatsDir(adapter.DiscoverConfig{Home: userHome}); got != filepath.Join(home, "stats") {
		t.Errorf("with only %s set, stats dir = %q, want %q", HomeEnv, got, filepath.Join(home, "stats"))
	}

	// Whitespace is unset on BOTH rungs, not just the lower one. A blank
	// REASONIX_STATE_HOME that stopped the chain here would resolve to nothing
	// at all and report zero usage while REASONIX_HOME names a good root.
	t.Setenv(StateHomeEnv, "   ")
	if got := StatsDir(adapter.DiscoverConfig{Home: userHome}); got != filepath.Join(home, "stats") {
		t.Errorf("with a blank %s, stats dir = %q, want the %s root %q",
			StateHomeEnv, got, HomeEnv, filepath.Join(home, "stats"))
	}

	t.Setenv(HomeEnv, "   ")
	want := filepath.Join(userHome, ".reasonix", "stats")
	if got := StatsDir(adapter.DiscoverConfig{Home: userHome}); got != want {
		t.Errorf("with both blank, stats dir = %q, want %q", got, want)
	}
}

// TestRootExpandsVarsThenTildeThenCwd is the three-step resolution the matrix
// singles out as this harness's own. Skipping any step points collection at a
// directory the harness never wrote to.
func TestRootExpandsVarsThenTildeThenCwd(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()
	cwd := t.TempDir()

	t.Run("braced default when the referenced variable is unset", func(t *testing.T) {
		t.Setenv(StateHomeEnv, "${RX_TEST_UNSET:-~/from-default}")
		t.Setenv(HomeEnv, "")
		want := filepath.Join(home, "from-default", "stats")
		if got := StatsDir(adapter.DiscoverConfig{Home: home}); got != want {
			t.Errorf("stats dir = %q, want %q", got, want)
		}
	})

	t.Run("braced reference wins over its default", func(t *testing.T) {
		t.Setenv("RX_TEST_ROOT", target)
		t.Setenv(StateHomeEnv, "${RX_TEST_ROOT:-~/from-default}")
		want := filepath.Join(target, "stats")
		if got := StatsDir(adapter.DiscoverConfig{Home: home}); got != want {
			t.Errorf("stats dir = %q, want %q", got, want)
		}
	})

	t.Run("nested defaults", func(t *testing.T) {
		t.Setenv(StateHomeEnv, "${RX_TEST_A:-${RX_TEST_B:-~/nested}}")
		want := filepath.Join(home, "nested", "stats")
		if got := StatsDir(adapter.DiscoverConfig{Home: home}); got != want {
			t.Errorf("stats dir = %q, want %q", got, want)
		}
	})

	t.Run("bare $VAR", func(t *testing.T) {
		t.Setenv("RX_TEST_ROOT", target)
		t.Setenv(StateHomeEnv, "$RX_TEST_ROOT/nested-root")
		want := filepath.Join(target, "nested-root", "stats")
		if got := StatsDir(adapter.DiscoverConfig{Home: home}); got != want {
			t.Errorf("stats dir = %q, want %q", got, want)
		}
	})

	t.Run("bare tilde", func(t *testing.T) {
		t.Setenv(StateHomeEnv, "~")
		want := filepath.Join(home, "stats")
		if got := StatsDir(adapter.DiscoverConfig{Home: home}); got != want {
			t.Errorf("stats dir = %q, want %q", got, want)
		}
	})

	t.Run("relative resolves against the working directory", func(t *testing.T) {
		t.Chdir(cwd)
		t.Setenv(StateHomeEnv, "rx-state")
		want := filepath.Join(cwd, "rx-state", "stats")
		if got := StatsDir(adapter.DiscoverConfig{Home: home}); got != want {
			t.Errorf("stats dir = %q, want %q", got, want)
		}
	})

	t.Run("an unset reference with no default collapses like a shell", func(t *testing.T) {
		t.Chdir(cwd)
		t.Setenv(StateHomeEnv, "${RX_TEST_UNSET}/reasonix")
		// Shell-faithful on purpose: ${UNSET} is the empty string, so the value
		// becomes the absolute "/reasonix" rather than something relative. The
		// resulting directory does not exist, Discover returns no sources, and
		// the user gets zero rows instead of somebody else's. Deviating here
		// would mean this adapter reading a path `cd` would not.
		want := filepath.Join("/reasonix", "stats")
		if got := StatsDir(adapter.DiscoverConfig{Home: home}); got != want {
			t.Errorf("stats dir = %q, want %q", got, want)
		}
	})

	t.Run("unbalanced brace stays literal", func(t *testing.T) {
		if got := expandVars("${RX_TEST_UNCLOSED/x", 0); got != "${RX_TEST_UNCLOSED/x" {
			t.Errorf("expandVars = %q, want the input unchanged", got)
		}
	})
}

// TestOverrideBeatsTheEnvironment: an explicit --path override is the operator
// speaking, and it outranks both variables.
func TestOverrideBeatsTheEnvironment(t *testing.T) {
	t.Setenv(StateHomeEnv, t.TempDir())
	t.Setenv(HomeEnv, t.TempDir())
	override := t.TempDir()
	want := filepath.Join(override, "stats")
	got := StatsDir(adapter.DiscoverConfig{
		Home:      t.TempDir(),
		Overrides: map[string]string{model.ToolReasonix: override},
	})
	if got != want {
		t.Errorf("stats dir = %q, want %q", got, want)
	}
}

// --- helpers ---------------------------------------------------------------

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	touch(t, path)
}

func firstLine(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for i, c := range body {
		if c == '\n' {
			return body[:i+1]
		}
	}
	return body
}

// touch moves the file's mtime forward so a size-and-mtime gate cannot mistake
// two writes inside one filesystem timestamp tick for an unchanged file.
func touch(t *testing.T, path string) {
	t.Helper()
	when := time.Now().Add(time.Second)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}
