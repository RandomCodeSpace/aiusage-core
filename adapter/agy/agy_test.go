package agy

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestTokenFreeFilesYieldNoSnapshots keeps content-only Antigravity artifacts
// valid and empty. Usage lives on the captured print-mode stream surface.
func TestTokenFreeFilesYieldNoSnapshots(t *testing.T) {
	dir := t.TempDir()
	// A conversation-style blob with no token usage anywhere.
	writeFile(t, dir, "conversation.json", `{"id":"c1","messages":[{"role":"user","content":"hi"}]}`)
	// A JSONL log with content only.
	writeFile(t, dir, "events.jsonl", `{"id":"e1","type":"agy","text":"hello"}`+"\n")

	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{model.ToolAgy: dir}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("want 2 sources (no usage pre-scan), got %d: %+v", len(srcs), srcs)
	}
	for _, src := range srcs {
		obs, err := a.Collect(context.Background(), src)
		if err != nil {
			t.Fatalf("collect %s: %v", src.Path, err)
		}
		if len(obs.Snapshots) != 0 || len(obs.Events) != 0 {
			t.Fatalf("token-free %s produced %d snapshots / %d events, want 0",
				src.Path, len(obs.Snapshots), len(obs.Events))
		}
	}
}

// TestCollectLiveStreamFixture is the exact oracle for the sanitized Agy 1.1.22
// two-turn capture in testdata. step_update usage is per-turn and must not be
// added to the cumulative result usage; the second result is the one aggregate
// snapshot for the conversation.
func TestCollectLiveStreamFixture(t *testing.T) {
	path := filepath.Join("testdata", "live-2026-08-31.jsonl")
	src := adapter.Source{Tool: model.ToolAgy, Class: model.Aggregate, Path: path}
	obs, err := Adapter{}.Collect(context.Background(), src)
	if err != nil {
		t.Fatalf("collect live fixture: %v", err)
	}
	if obs.Checkpoint == nil {
		t.Fatal("complete live stream did not advance its checkpoint")
	}
	if len(obs.Snapshots) != 1 {
		t.Fatalf("snapshots = %d, want one cumulative conversation: %+v", len(obs.Snapshots), obs.Snapshots)
	}
	s := obs.Snapshots[0]
	if s.Tool != model.ToolAgy || s.Provider != model.ProviderGoogle || s.Model != "gemini-3.7-flash-high" {
		t.Errorf("identity = tool %q provider %q model %q", s.Tool, s.Provider, s.Model)
	}
	if s.SessionID != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("SessionID = %q", s.SessionID)
	}
	if s.Key != s.SessionID {
		t.Errorf("Key = %q, want path-independent conversation id %q", s.Key, s.SessionID)
	}
	if s.InputTokens != 35192 || s.OutputTokens != 6618 || s.ReasoningTokens != 6593 || s.CacheReadTokens != 0 || s.TotalTokens != 41810 {
		t.Errorf("usage = in %d out %d thinking %d cache %d total %d",
			s.InputTokens, s.OutputTokens, s.ReasoningTokens, s.CacheReadTokens, s.TotalTokens)
	}
	if s.TotalTokens != s.InputTokens+s.OutputTokens {
		t.Errorf("provider total = %d, input + output = %d", s.TotalTokens, s.InputTokens+s.OutputTokens)
	}
	if strings.Contains(s.Raw, "SANITIZED") || strings.Contains(s.Raw, "response") {
		t.Errorf("raw audit payload retained response content: %s", s.Raw)
	}

	unchanged, err := Adapter{}.CollectIncremental(context.Background(), src, obs.Checkpoint)
	if err != nil {
		t.Fatalf("unchanged collect: %v", err)
	}
	if len(unchanged.Snapshots) != 0 || unchanged.Checkpoint != nil {
		t.Fatalf("unchanged source was reopened: %+v", unchanged)
	}
}

func TestStreamFormatDriftWithholdsCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "stream.jsonl", `{"event":"init","conversation_id":"c1","init":{"model":"gemini-3.7-flash-high"}}
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","num_turns":1,"usage":{"input_tokens":10,"output_tokens":4,"thinking_tokens":3,"total_tokens":14}}}
`)
	src := adapter.Source{Tool: model.ToolAgy, Class: model.Aggregate, Path: path}
	obs, err := Adapter{}.Collect(context.Background(), src)
	if !errors.Is(err, adapter.ErrSourceFormat) {
		t.Fatalf("error = %v, want ErrSourceFormat", err)
	}
	if obs.Checkpoint != nil {
		t.Fatalf("format drift advanced checkpoint: %+v", obs.Checkpoint)
	}
}

func TestStreamCompletePoisonKeepsValidSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "stream.jsonl", `{"event":"init","conversation_id":"c1","init":{"model":"gemini-3.7-flash-high"}}
not-json
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","num_turns":1,"usage":{"input_tokens":10,"output_tokens":4,"thinking_tokens":3,"cache_read_tokens":2,"total_tokens":14}}}
`)
	src := adapter.Source{Tool: model.ToolAgy, Class: model.Aggregate, Path: path}
	obs, err := Adapter{}.Collect(context.Background(), src)
	if err == nil || !strings.Contains(err.Error(), "1 unparseable record") {
		t.Fatalf("error = %v, want poison-record report", err)
	}
	if len(obs.Snapshots) != 1 || obs.Snapshots[0].TotalTokens != 14 {
		t.Fatalf("valid snapshot was lost: %+v", obs.Snapshots)
	}
	if obs.Checkpoint == nil {
		t.Fatal("fully consumed poison record must not hold back the checkpoint")
	}
}

func TestStreamRejectsContradictoryCounters(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "stream.jsonl", `{"event":"init","conversation_id":"c1","init":{"model":"gemini-3.7-flash-high"}}
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","num_turns":1,"usage":{"input_tokens":10,"output_tokens":4,"thinking_tokens":5,"cache_read_tokens":0,"total_tokens":14}}}
`)
	_, err := Adapter{}.Collect(context.Background(), adapter.Source{Tool: model.ToolAgy, Class: model.Aggregate, Path: path})
	if !errors.Is(err, adapter.ErrSourceFormat) {
		t.Fatalf("error = %v, want ErrSourceFormat", err)
	}
}

// TestDiscoverEmptyDir verifies an empty Antigravity dir yields no sources.
func TestDiscoverEmptyDir(t *testing.T) {
	dir := t.TempDir()
	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{model.ToolAgy: dir}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources for empty dir, got %d", len(srcs))
	}
}

// TestDiscoverMissingRoots verifies that a home with none of the candidate dirs
// present yields no sources and no error.
func TestDiscoverMissingRoots(t *testing.T) {
	home := t.TempDir() // no .gemini/antigravity-cli, .antigravitycli, .cache/antigravity
	a := New()
	cfg := adapter.DiscoverConfig{Home: home}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources, got %d", len(srcs))
	}
}

// TestParseGeminiShapedUsage verifies that IF Antigravity ever emits a Gemini-
// shaped usage file, it is discovered and parsed with tool="agy". This exercises
// the forward-looking parser without asserting the live (token-free) state.
func TestParseGeminiShapedUsage(t *testing.T) {
	dir := t.TempDir()
	// Two cumulative records for the same id -> one max snapshot.
	content := `{"id":"t1","model":"antigravity","type":"gemini","tokens":{"input":50,"output":10,"thoughts":2}}
{"id":"t1","model":"antigravity","type":"gemini","tokens":{"input":150,"output":40,"thoughts":8}}
`
	writeFile(t, dir, "usage.jsonl", content)

	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{model.ToolAgy: dir}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("want 1 source for usage-bearing file, got %d", len(srcs))
	}

	obs, _ := a.Collect(context.Background(), srcs[0])
	if len(obs.Snapshots) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(obs.Snapshots))
	}
	s := obs.Snapshots[0]
	if s.Tool != model.ToolAgy {
		t.Errorf("Tool = %q, want %q", s.Tool, model.ToolAgy)
	}
	if s.Provider != model.ProviderGoogle {
		t.Errorf("Provider = %q, want %q", s.Provider, model.ProviderGoogle)
	}
	if s.InputTokens != 150 || s.OutputTokens != 40 || s.ReasoningTokens != 8 {
		t.Errorf("max snapshot wrong: in=%d out=%d thoughts=%d", s.InputTokens, s.OutputTokens, s.ReasoningTokens)
	}
	// Total = input+output+thoughts = 150+40+8 = 198.
	if want := int64(198); s.TotalTokens != want {
		t.Errorf("TotalTokens = %d, want %d", s.TotalTokens, want)
	}
	if want := s.SourcePath + "|t1"; s.Key != want {
		t.Errorf("Key = %q, want %q", s.Key, want)
	}
}

// TestDiscoverResolvesSymlinkedRoot: aggregate keys embed absolute file paths,
// so a symlinked root must resolve to its target or a re-point would mint new
// identities and re-add full cumulative totals.
func TestDiscoverResolvesSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeFile(t, real, "s.jsonl", `{"id":"t1","model":"m","type":"agy","tokens":{"input":5,"output":2}}`+"\n")
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("eval real root: %v", err)
	}

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Overrides: map[string]string{model.ToolAgy: link}})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("want 1 source, got %d", len(srcs))
	}
	if want := filepath.Join(resolved, "s.jsonl"); srcs[0].Path != want {
		t.Errorf("Path = %q, want resolved %q", srcs[0].Path, want)
	}
}

// TestScanAbortWithholdsCheckpoint: a JSONL line exceeding the scanner buffer
// aborts the scan mid-file. The records read so far are still returned
// (best-effort), but the checkpoint must NOT advance — advancing it would skip
// the unread remainder until the next size/mtime change, a permanent data loss.
func TestScanAbortWithholdsCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.jsonl")
	good := `{"id":"t1","model":"m","tokens":{"input":10,"output":5}}` + "\n"
	// One line larger than the 8 MiB scanner cap, then a record that the
	// aborted scan never reaches.
	huge := `{"id":"big","pad":"` + strings.Repeat("x", 9<<20) + `"}` + "\n"
	tail := `{"id":"t2","model":"m","tokens":{"input":1,"output":1}}` + "\n"
	if err := os.WriteFile(path, []byte(good+huge+tail), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := Adapter{}
	src := adapter.Source{Tool: model.ToolAgy, Class: model.Aggregate, Path: path}
	obs, err := a.CollectIncremental(context.Background(), src, nil)
	if err == nil {
		t.Fatal("scan abort must surface a non-fatal error")
	}
	if len(obs.Snapshots) != 1 {
		t.Fatalf("want the 1 snapshot read before the abort, got %d", len(obs.Snapshots))
	}
	if obs.Checkpoint != nil {
		t.Fatalf("checkpoint advanced past an incomplete read: %+v", obs.Checkpoint)
	}
}

// TestScanAbortAlsoReportsSkippedCount: when unparseable lines and a scan abort
// land in the same read, ONE error reports both — the skip count is not dropped
// in favour of the partial-read error, and the wrapped scanner error stays
// inspectable.
func TestScanAbortAlsoReportsSkippedCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.jsonl")
	good := `{"id":"t1","model":"m","tokens":{"input":10,"output":5}}` + "\n"
	bad := "not json at all\n"
	huge := `{"id":"big","pad":"` + strings.Repeat("x", 9<<20) + `"}` + "\n"
	if err := os.WriteFile(path, []byte(good+bad+huge), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := Adapter{}
	src := adapter.Source{Tool: model.ToolAgy, Class: model.Aggregate, Path: path}
	obs, err := a.CollectIncremental(context.Background(), src, nil)
	if err == nil {
		t.Fatal("want an error reporting both the partial read and the skipped line")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("error must still wrap the scanner error, got %v", err)
	}
	if !strings.Contains(err.Error(), "partial read") {
		t.Errorf("error must report the partial read: %v", err)
	}
	if !strings.Contains(err.Error(), "1 unparseable record(s) skipped") {
		t.Errorf("error must report the skipped count: %v", err)
	}
	if obs.Checkpoint != nil {
		t.Fatalf("checkpoint advanced past an incomplete read: %+v", obs.Checkpoint)
	}
}

// Live case that motivated this: ~/.antigravitycli held a *.json symlink into a
// ~/.gemini project directory that no longer exists. The walk reports the link
// by its own metadata — not a directory, usage extension — so discovery kept
// handing the collector a file that is not there, and the daemon logged an
// error every cycle for as long as the link survived. A link that resolves is
// still a real telemetry file and must be kept.
func TestDiscoverSkipsDanglingSymlinks(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".antigravitycli")
	writeFile(t, dir, "real.json", `{"id":"c1","messages":[]}`)
	if err := os.Symlink(filepath.Join(dir, "gone.json"), filepath.Join(dir, "dangling.json")); err != nil {
		t.Fatalf("symlink dangling: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "real.json"), filepath.Join(dir, "linked.json")); err != nil {
		t.Fatalf("symlink linked: %v", err)
	}

	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		got[filepath.Base(s.Path)] = true
	}
	if got["dangling.json"] {
		t.Error("discovery returned a dangling symlink; every collect of it fails with ENOENT")
	}
	if !got["real.json"] {
		t.Error("discovery dropped the real telemetry file")
	}
	if !got["linked.json"] {
		t.Error("discovery dropped a symlink that resolves to a real telemetry file")
	}
}
