package geminishape

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// The two shapes this parser is parameterised for today. Both consumers must
// keep working off the same parse; a change that only satisfies one of them is
// the failure this file exists to catch.
var (
	geminiShape = Shape{Tool: model.ToolGemini, Provider: model.ProviderGoogle, Project: "gemini"}
	agyShape    = Shape{Tool: model.ToolAgy, Provider: model.ProviderGoogle, Project: "agy"}
)

// fixedNow is the ObservedTime fallback used wherever a record carries no
// parseable timestamp.
var fixedNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// writeTemp writes content to a fresh temp dir under name and returns the path.
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestReadFileStampsShapeIdentity checks the record-parsing contract for BOTH
// shapes: cumulative records collapse to the max snapshot per id, one snapshot
// per (file, id) in first-seen order, and every snapshot carries the shape's
// tool/provider/project identity.
//
// The ids sort in the OPPOSITE order to their first appearance ("zulu" is seen
// first, "alpha" second) so that first-seen order is the only order that
// satisfies the assertions below. With ids that sort the way they appear, a
// ReadFile that walked its map through sorted keys - or Go's randomised map
// order, half the time - would pass while having dropped the ordering the
// contract promises.
func TestReadFileStampsShapeIdentity(t *testing.T) {
	content := `{"id":"zulu","model":"m-1","type":"gemini","sessionId":"s1","timestamp":"2026-08-09T10:00:00Z","tokens":{"input":100,"output":20,"thoughts":5}}
{"id":"alpha","model":"m-2","type":"gemini","sessionId":"s1","timestamp":"2026-08-09T10:00:01Z","tokens":{"input":7,"output":3}}
{"id":"zulu","model":"m-1","type":"gemini","sessionId":"s1","timestamp":"2026-08-09T10:00:02Z","tokens":{"input":300,"output":80,"thoughts":15}}
`
	for _, sh := range []Shape{geminiShape, agyShape} {
		t.Run(sh.Tool, func(t *testing.T) {
			path := writeTemp(t, "turns.jsonl", content)
			res, err := sh.ReadFile(path, fixedNow)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if res.Skipped != 0 || res.ScanErr != nil {
				t.Fatalf("clean file reported Skipped=%d ScanErr=%v", res.Skipped, res.ScanErr)
			}
			if len(res.Snapshots) != 2 {
				t.Fatalf("want 2 snapshots (zulu, alpha), got %d: %+v", len(res.Snapshots), res.Snapshots)
			}
			first, second := res.Snapshots[0], res.Snapshots[1]
			if want := path + "|zulu"; first.Key != want {
				t.Errorf("first Key = %q, want %q (first-seen id order, not sorted)", first.Key, want)
			}
			if want := path + "|alpha"; second.Key != want {
				t.Errorf("second Key = %q, want %q (first-seen id order, not sorted)", second.Key, want)
			}
			for _, s := range res.Snapshots {
				if s.Tool != sh.Tool {
					t.Errorf("Tool = %q, want %q", s.Tool, sh.Tool)
				}
				if s.Provider != sh.Provider {
					t.Errorf("Provider = %q, want %q", s.Provider, sh.Provider)
				}
				if s.Project != sh.Project {
					t.Errorf("Project = %q, want %q", s.Project, sh.Project)
				}
				if s.SourcePath != path {
					t.Errorf("SourcePath = %q, want %q", s.SourcePath, path)
				}
				if s.SessionID != "s1" {
					t.Errorf("SessionID = %q, want s1", s.SessionID)
				}
			}
			// Max (final) cumulative record wins for zulu.
			if first.InputTokens != 300 || first.OutputTokens != 80 || first.ReasoningTokens != 15 {
				t.Errorf("zulu not the max snapshot: in=%d out=%d thoughts=%d",
					first.InputTokens, first.OutputTokens, first.ReasoningTokens)
			}
			if want := int64(395); first.TotalTokens != want {
				t.Errorf("zulu TotalTokens = %d, want %d (derived input+output+thoughts)", first.TotalTokens, want)
			}
			if got := first.ObservedTime.Format(time.RFC3339); got != "2026-08-09T10:00:02Z" {
				t.Errorf("zulu ObservedTime = %q, want the chosen record's stamp", got)
			}
		})
	}
}

// TestToSnapshotTokenMapping pins the documented mapping: Input is
// (input+tool) minus the cached overlap clamped at zero, cached lands in
// CacheRead and is never added on top of the total, CacheCreation is always
// zero, a reported provider total wins wherever it covers the components the
// snapshot stores and is raised to them where it does not, negative counters
// clamp, and an all-zero record is dropped.
func TestToSnapshotTokenMapping(t *testing.T) {
	cases := []struct {
		name                                 string
		tok                                  tokenBlock
		wantOK                               bool
		in, out, cacheRead, reasoning, total int64
	}{
		{
			// The provider total covers input+tool+output+thoughts (290), so it
			// stands as reported and cached is still subtracted from Input.
			name:   "reported total wins and cached is subtracted from input",
			tok:    tokenBlock{Input: 200, Output: 30, Cached: 40, Thoughts: 10, Tool: 50, Total: 400},
			wantOK: true, in: 210, out: 30, cacheRead: 40, reasoning: 10, total: 400,
		},
		{
			// Real records total input+output+thoughts and leave tool out of it
			// (see telemetryLine in raw_test.go). Those tool tokens are folded
			// into Input, so the stored total is raised to cover them.
			name:   "reported total below the components it stores is raised to them",
			tok:    tokenBlock{Input: 200, Output: 30, Cached: 40, Thoughts: 10, Tool: 50, Total: 240},
			wantOK: true, in: 210, out: 30, cacheRead: 40, reasoning: 10, total: 290,
		},
		{
			name:   "cached is not added on top of the derived total",
			tok:    tokenBlock{Input: 100, Output: 20, Cached: 90, Thoughts: 5},
			wantOK: true, in: 10, out: 20, cacheRead: 90, reasoning: 5, total: 125,
		},
		{
			// Input clamps, but the 15 tokens the record itemised are still
			// itemised: the total reports them rather than the smaller 10.
			name:   "cached larger than input+tool clamps Input to zero",
			tok:    tokenBlock{Input: 10, Cached: 500, Tool: 5, Total: 10},
			wantOK: true, in: 0, out: 0, cacheRead: 500, reasoning: 0, total: 15,
		},
		{
			// Every negative counter clamps to zero, and tool tokens reach both
			// Input and the total, so a tool-only record totals its tool tokens.
			name:   "negative counters clamp and the derived total includes tool",
			tok:    tokenBlock{Input: -5, Output: -7, Cached: -1, Thoughts: -2, Tool: 9, Total: -3},
			wantOK: true, in: 9, out: 0, cacheRead: 0, reasoning: 0, total: 9,
		},
		{
			name:   "all-zero record is dropped",
			tok:    tokenBlock{},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, ok := geminiShape.toSnapshot(rawRecord{ID: "r", Tokens: tc.tok}, "/tmp/x.jsonl", fixedNow)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if snap.InputTokens != tc.in {
				t.Errorf("InputTokens = %d, want %d", snap.InputTokens, tc.in)
			}
			if snap.OutputTokens != tc.out {
				t.Errorf("OutputTokens = %d, want %d", snap.OutputTokens, tc.out)
			}
			if snap.CacheReadTokens != tc.cacheRead {
				t.Errorf("CacheReadTokens = %d, want %d", snap.CacheReadTokens, tc.cacheRead)
			}
			if snap.CacheCreationTokens != 0 {
				t.Errorf("CacheCreationTokens = %d, want 0 (this shape never reports creation)", snap.CacheCreationTokens)
			}
			if snap.ReasoningTokens != tc.reasoning {
				t.Errorf("ReasoningTokens = %d, want %d", snap.ReasoningTokens, tc.reasoning)
			}
			if snap.TotalTokens != tc.total {
				t.Errorf("TotalTokens = %d, want %d", snap.TotalTokens, tc.total)
			}
		})
	}
}

// TestTotalCoversEveryComponentFoldedIntoFields is the invariant issue #49
// broke: TotalTokens must account for every component the shape folds into the
// snapshot's own token fields, so a stored row can never disagree with the
// counts stored beside it. Tool tokens are folded into InputTokens, so a record
// carrying nothing but tool tokens must total them, and adding tool tokens to a
// record must move the total by exactly that many - with or without a
// provider-reported total.
//
// Cache read is deliberately outside the invariant: tokens.cached is the slice
// of tokens.input already counted there, so it is subtracted out of Input and
// never added on top of the total.
func TestTotalCoversEveryComponentFoldedIntoFields(t *testing.T) {
	const n = 9

	snapOf := func(t *testing.T, tok tokenBlock) model.AggregateSnapshot {
		t.Helper()
		snap, ok := agyShape.toSnapshot(rawRecord{ID: "r", Tokens: tok}, "/tmp/x.jsonl", fixedNow)
		if !ok {
			t.Fatalf("record %+v carries usage but was dropped", tok)
		}
		if sum := snap.InputTokens + snap.OutputTokens + snap.ReasoningTokens; snap.TotalTokens < sum {
			t.Errorf("TotalTokens = %d, below the %d tokens held by the fields it covers (in=%d out=%d reasoning=%d)",
				snap.TotalTokens, sum, snap.InputTokens, snap.OutputTokens, snap.ReasoningTokens)
		}
		return snap
	}

	// Each component that reaches a counting field, on its own: the total is
	// exactly the tokens the record carries.
	for _, tc := range []struct {
		name string
		tok  tokenBlock
	}{
		{"input only", tokenBlock{Input: n}},
		{"output only", tokenBlock{Output: n}},
		{"thoughts only", tokenBlock{Thoughts: n}},
		{"tool only", tokenBlock{Tool: n}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if snap := snapOf(t, tc.tok); snap.TotalTokens != n {
				t.Errorf("TotalTokens = %d, want %d (the only component this record carries)", snap.TotalTokens, n)
			}
		})
	}

	// Folding tool tokens into a record must move both InputTokens and the
	// total by exactly that many, whether the total is derived or reported.
	for _, tc := range []struct {
		name string
		base tokenBlock
	}{
		{"derived total", tokenBlock{Input: 100, Output: 20, Cached: 30, Thoughts: 5}},
		{"reported total", tokenBlock{Input: 100, Output: 20, Cached: 30, Thoughts: 5, Total: 125}},
	} {
		t.Run("adding tool tokens moves the "+tc.name, func(t *testing.T) {
			before := snapOf(t, tc.base)
			with := tc.base
			with.Tool = n
			after := snapOf(t, with)
			if got := after.InputTokens - before.InputTokens; got != n {
				t.Errorf("InputTokens moved by %d, want %d", got, n)
			}
			if got := after.TotalTokens - before.TotalTokens; got != n {
				t.Errorf("TotalTokens moved by %d, want %d: tool tokens reach InputTokens (%d) but not the total (%d)",
					got, n, after.InputTokens, after.TotalTokens)
			}
		})
	}
}

// TestRecordTotalOrdering covers the grouping key: total() reports the provider
// total when it covers the components, else the derived sum with negatives
// clamped. It must follow the same accounting toSnapshot stores, or the max
// snapshot picked per id is not the one with the largest stored total.
func TestRecordTotalOrdering(t *testing.T) {
	cases := []struct {
		name string
		tok  tokenBlock
		want int64
	}{
		{"reported total", tokenBlock{Input: 1, Output: 1, Total: 999}, 999},
		{"derived when total absent", tokenBlock{Input: 10, Output: 20, Thoughts: 5, Cached: 77}, 35},
		{"derived clamps negatives", tokenBlock{Input: -10, Output: 20, Thoughts: -5}, 20},
		{"non-positive total falls back to derived", tokenBlock{Input: 4, Output: 6, Total: -1}, 10},
		{"derived counts tool tokens", tokenBlock{Input: 10, Output: 20, Tool: 7}, 37},
		{"reported total is raised to cover tool tokens", tokenBlock{Input: 10, Output: 20, Tool: 7, Total: 30}, 37},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (rawRecord{Tokens: tc.tok}).total(); got != tc.want {
				t.Errorf("total() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDecodeValueShapes covers the decode helpers: plain object, array, $set
// envelope, messages[] blob (including a top-level stats summary alongside it),
// and inputs that are not JSON at all.
func TestDecodeValueShapes(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantOK  bool
		wantIDs []string
	}{
		{"plain object", `{"id":"a","tokens":{"input":1}}`, true, []string{"a"}},
		{"array of objects", `[{"id":"a"},{"id":"b"}]`, true, []string{"a", "b"}},
		{"nested array", `[[{"id":"a"}],{"id":"b"}]`, true, []string{"a", "b"}},
		{"set wrapper", `{"$set":{"id":"w","tokens":{"input":3}}}`, true, []string{"w"}},
		{"set wrapper holding an array", `{"$set":[{"id":"w1"},{"id":"w2"}]}`, true, []string{"w1", "w2"}},
		{
			"messages blob with top-level summary",
			`{"id":"conv","messages":[{"id":"m1","tokens":{"input":1}},{"id":"m2","tokens":{"input":2}}],"tokens":{"input":3,"total":3}}`,
			true,
			[]string{"m1", "m2", "conv"},
		},
		{
			"messages blob without a summary keeps only the messages",
			`{"id":"conv","messages":[{"id":"m1","tokens":{"input":1}}]}`,
			true,
			[]string{"m1"},
		},
		{"not json", `this is not json`, false, nil},
		{"empty", ``, false, nil},
		{"whitespace only", "  \t\r\n ", false, nil},
		{"bare scalar", `42`, false, nil},
		{"malformed array", `[{"id":"a"}`, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recs, ok := decodeValue([]byte(tc.data))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (records %+v)", ok, tc.wantOK, recs)
			}
			if !ok {
				return
			}
			var ids []string
			for _, r := range recs {
				ids = append(ids, r.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

// TestHasUsageExt pins the extension flag both adapters' Discover walks gate on.
func TestHasUsageExt(t *testing.T) {
	cases := map[string]bool{
		"/a/b/usage.json":      true,
		"/a/b/usage.jsonl":     true,
		"/a/b/USAGE.JSONL":     true,
		"/a/b/Usage.Json":      true,
		"/a/b/usage.json.gz":   false,
		"/a/b/usage.pb":        false,
		"/a/b/usage":           false,
		"/a/b/.json":           true,
		"/a/b/chats/":          false,
		"/a/b/usage.jsonlines": false,
	}
	for path, want := range cases {
		if got := HasUsageExt(path); got != want {
			t.Errorf("HasUsageExt(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestIdentityFallbacks pins project derivation and the identity fallbacks: the
// project label always comes from the Shape (the telemetry records no cwd), a
// record without an id becomes "turn", a record without a sessionId inherits
// the file stem, and an unparseable timestamp falls back to now.
func TestIdentityFallbacks(t *testing.T) {
	path := writeTemp(t, "session-7.jsonl", `{"model":"m","timestamp":"nonsense","tokens":{"input":5,"output":5}}`+"\n")
	for _, sh := range []Shape{geminiShape, agyShape} {
		t.Run(sh.Tool, func(t *testing.T) {
			res, err := sh.ReadFile(path, fixedNow)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if len(res.Snapshots) != 1 {
				t.Fatalf("want 1 snapshot, got %d", len(res.Snapshots))
			}
			s := res.Snapshots[0]
			if want := path + "|turn"; s.Key != want {
				t.Errorf("Key = %q, want %q (id fallback)", s.Key, want)
			}
			if s.SessionID != "session-7" {
				t.Errorf("SessionID = %q, want the file stem session-7", s.SessionID)
			}
			if s.Project != sh.Project {
				t.Errorf("Project = %q, want %q (project comes from the shape, not the record)", s.Project, sh.Project)
			}
			if !s.ObservedTime.Equal(fixedNow) {
				t.Errorf("ObservedTime = %v, want the now fallback %v", s.ObservedTime, fixedNow)
			}
		})
	}
}

// TestParseTimeRFC3339 covers the timestamp helper directly: an RFC3339 stamp
// is normalised to UTC with its full precision kept, and anything else is the
// zero time.
//
// It does NOT distinguish one layout constant from another, and no test can:
// time.Parse accepts a fractional second under the plain time.RFC3339 layout,
// so the fractional case below passes on that layout alone. That is why
// parseTime has a single layout - a second, nanosecond-specific pass could
// never be the one that succeeds.
func TestParseTimeRFC3339(t *testing.T) {
	if got := parseTime("2026-08-09T10:00:00Z"); got.Format(time.RFC3339) != "2026-08-09T10:00:00Z" {
		t.Errorf("whole-second parse = %v", got)
	}
	// Offset and sub-second digits both have to survive: the offset is what
	// makes the stamp comparable across sources, and truncating the fraction
	// would collapse turns logged inside the same second onto one timestamp.
	got := parseTime("2026-08-09T12:00:00.123456789+02:00")
	if want := time.Date(2026, 8, 9, 10, 0, 0, 123456789, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("fractional-second parse = %v (%v), want %v UTC", got, got.Location(), want)
	}
	for _, s := range []string{"", "   ", "not a time", "2026-08-09"} {
		if ts := parseTime(s); !ts.IsZero() {
			t.Errorf("parseTime(%q) = %v, want zero time", s, ts)
		}
	}
}

// TestSkippedCountsOnlyUnparseableInput: non-usage records (session headers,
// user turns, $set mutations, all-zero records) are dropped silently, while
// genuinely unparseable lines are counted in Skipped.
func TestSkippedCountsOnlyUnparseableInput(t *testing.T) {
	content := `{"sessionId":"s1","kind":"session"}
{"id":"u1","type":"user","content":"hi"}
{"$set":{"lastUpdated":"2026-08-09T10:00:00Z"}}
not json at all
{"id":"g1","type":"gemini","tokens":{"input":10,"output":2,"total":12}}
<html>nope</html>
`
	path := writeTemp(t, "mixed.jsonl", content)
	res, err := geminiShape.ReadFile(path, fixedNow)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(res.Snapshots) != 1 || res.Snapshots[0].TotalTokens != 12 {
		t.Fatalf("want the single usage-bearing snapshot, got %+v", res.Snapshots)
	}
	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (only the two unparseable lines)", res.Skipped)
	}
	if res.ScanErr != nil {
		t.Errorf("ScanErr = %v, want nil (the scan completed)", res.ScanErr)
	}
}

// TestJSONFileUnparseableCountsOnce: a whole .json file that is not JSON counts
// as one skip, not a fatal error.
func TestJSONFileUnparseableCountsOnce(t *testing.T) {
	path := writeTemp(t, "broken.json", "not json\n")
	res, err := geminiShape.ReadFile(path, fixedNow)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(res.Snapshots) != 0 {
		t.Errorf("want 0 snapshots, got %d", len(res.Snapshots))
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", res.Skipped)
	}
}

// TestScanAbortReportsScanErrAndPartialSnapshots: an over-long JSONL line
// aborts the scan. Records read before the abort are still returned, the skips
// seen so far are still counted, and ScanErr is set so the caller withholds its
// checkpoint.
func TestScanAbortReportsScanErrAndPartialSnapshots(t *testing.T) {
	good := `{"id":"t1","model":"m","tokens":{"input":10,"output":5}}` + "\n"
	bad := "not json\n"
	huge := `{"id":"big","pad":"` + strings.Repeat("x", maxLineBytes+1) + `"}` + "\n"
	tail := `{"id":"t2","model":"m","tokens":{"input":1,"output":1}}` + "\n"
	path := writeTemp(t, "huge.jsonl", good+bad+huge+tail)

	res, err := geminiShape.ReadFile(path, fixedNow)
	if err != nil {
		t.Fatalf("ReadFile returned a fatal error for a recoverable scan abort: %v", err)
	}
	if !errors.Is(res.ScanErr, bufio.ErrTooLong) {
		t.Fatalf("ScanErr = %v, want bufio.ErrTooLong", res.ScanErr)
	}
	if len(res.Snapshots) != 1 {
		t.Fatalf("want the 1 snapshot read before the abort, got %d: %+v", len(res.Snapshots), res.Snapshots)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the unparseable line seen before the abort)", res.Skipped)
	}
}

// TestReadFileMissingFileIsFatal: an unopenable file is the one fatal case.
func TestReadFileMissingFileIsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.jsonl")
	if _, err := geminiShape.ReadFile(path, fixedNow); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

// TestRawAllowListDropsMessageContent is the messages[] half of the raw policy
// (issue #17): records lifted out of a messages blob are re-marshalled from the
// allow-listed decode too, so message text never reaches the ledger.
func TestRawAllowListDropsMessageContent(t *testing.T) {
	blob := `{"id":"conv","messages":[{"id":"m1","model":"gemini-2.5-pro","role":"assistant",` +
		`"content":"LEAK-message body","tokens":{"input":11,"output":7,"total":18}}]}`
	path := writeTemp(t, "conversation.json", blob)

	res, err := agyShape.ReadFile(path, fixedNow)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(res.Snapshots) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(res.Snapshots))
	}
	raw := res.Snapshots[0].Raw
	for _, marker := range []string{"LEAK-message body", "content", "role", "messages"} {
		if strings.Contains(raw, marker) {
			t.Errorf("Raw leaked %q:\n%s", marker, raw)
		}
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("Raw is not valid JSON: %v\n%s", err, raw)
	}
	assertKeys(t, "messages raw", top, "id", "model", "type", "sessionId", "timestamp", "tokens")
}
