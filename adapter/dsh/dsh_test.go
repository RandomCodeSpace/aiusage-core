package dsh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// TestMain keeps DSH_HOME from redirecting fixture discovery into an ambient
// installation. The env-precedence test sets it explicitly after this reset.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(HomeEnv)
	os.Exit(m.Run())
}

// The fixture is a REAL DSH session captured on this machine — a two-step turn
// that called `bash` and `glob` — with every content field replaced by a
// synthetic placeholder. Structure, sequence numbers, timestamps, identities and
// token counts are the harness's own; the prose, the arguments and the results
// are not.
const fixture = "testdata/session.jsonl"

// Identities and counters the fixture is asserted against, copied from the
// captured session rather than recomputed by the code under test.
const (
	fixSession = "session-4a2d4601-43c9-4b11-8b85-c59c899a2a8a"
	fixProject = "/home/agent/project"
	fixModel   = "gemma4:31b-cloud"
	fixProv    = "ollama-local"

	fixMsg1  = "d9019f4d-e5c4-4ce8-b1b5-d63ec187d30d"
	fixMsg2  = "93731e2a-fc44-4301-a362-b25986488448"
	fixCall1 = "call_cnl5fude"
	fixCall2 = "call_l93arpbi"
)

// --- helpers -------------------------------------------------------------

// dshHome returns the default DSH home for a user home dir, matching production
// discovery (DSH_HOME unset => ~/.dsh).
func dshHome(home string) string { return filepath.Join(home, defaultHomeDir) }

// plantSession writes logical JSONL lines as a plain (uncompressed) transcript
// in the session-directory layout DSH uses.
func plantSession(t *testing.T, home, project, session string, lines []string) string {
	t.Helper()
	dir := filepath.Join(dshHome(home), dirSessions, project, session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, transcriptPlain)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// plantZstd writes the same logical lines as DSH's DEFAULT artifact: a
// concatenation of independent Zstandard frames — one for the header line, then
// one per append batch. Reading it back is the whole point of the zstd path, so
// the fixture deliberately reproduces the multi-frame layout rather than one
// tidy frame.
func plantZstd(t *testing.T, home, project, session string, batches [][]string) string {
	t.Helper()
	dir := filepath.Join(dshHome(home), dirSessions, project, session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, transcriptZstd)
	if err := os.WriteFile(path, zstdFrames(t, batches), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// zstdFrames encodes each batch as its own complete frame and concatenates them.
func zstdFrames(t *testing.T, batches [][]string) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, batch := range batches {
		var frame bytes.Buffer
		w, err := zstd.NewWriter(&frame)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		if _, err := w.Write([]byte(strings.Join(batch, "\n") + "\n")); err != nil {
			t.Fatalf("zstd write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("zstd close: %v", err)
		}
		out.Write(frame.Bytes())
	}
	return out.Bytes()
}

// fixtureLines reads the committed fixture as logical lines.
func fixtureLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// collectAll discovers under cfg and collects every source, failing on error.
func collectAll(t *testing.T, cfg adapter.DiscoverConfig) adapter.Observation {
	t.Helper()
	obs, err := collectAllErr(t, cfg)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return obs
}

func collectAllErr(t *testing.T, cfg adapter.DiscoverConfig) (adapter.Observation, error) {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var all adapter.Observation
	var firstErr error
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		all.Events = append(all.Events, obs.Events...)
		all.Activity = append(all.Activity, obs.Activity...)
		all.TurnContexts = append(all.TurnContexts, obs.TurnContexts...)
	}
	return all, firstErr
}

func ms(v int64) time.Time { return time.UnixMilli(v).UTC() }

// --- the fixture, end to end ---------------------------------------------

// TestFixtureSessionEmitsExactEvents pins the whole observation of one real
// captured session: two usage rows and two tool calls, with the exact dedup keys
// the store will conflict-skip on.
func TestFixtureSessionEmitsExactEvents(t *testing.T) {
	home := t.TempDir()
	path := plantSession(t, home, "--home-agent-project--", fixSession, fixtureLines(t))

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})

	if len(obs.Events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(obs.Events), obs.Events)
	}
	want := []model.UsageEvent{
		{
			Tool: model.ToolDSH, Model: fixModel, Provider: fixProv,
			SessionID: fixSession, Project: fixProject,
			EventTime:   ms(1786855857939),
			InputTokens: 10065, OutputTokens: 44, TotalTokens: 10109,
			MessageID: fixMsg1, RequestID: "chatcmpl-988",
			SourcePath: path, DedupKey: model.ToolDSH + "|msg|" + fixMsg1, Kind: model.KindUsage,
		},
		{
			Tool: model.ToolDSH, Model: fixModel, Provider: fixProv,
			SessionID: fixSession, Project: fixProject,
			EventTime:   ms(1786855859054),
			InputTokens: 10136, OutputTokens: 55, TotalTokens: 10191,
			MessageID: fixMsg2, RequestID: "chatcmpl-958",
			SourcePath: path, DedupKey: model.ToolDSH + "|msg|" + fixMsg2, Kind: model.KindUsage,
		},
	}
	for i, w := range want {
		got := obs.Events[i]
		got.Raw = "" // asserted separately
		if got != w {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, got, w)
		}
	}

	if len(obs.Activity) != 2 {
		t.Fatalf("activity = %d, want 2: %+v", len(obs.Activity), obs.Activity)
	}
	wantAct := []model.ActivityEvent{
		{
			Tool: model.ToolDSH, Kind: model.ActivityTool, Name: "bash",
			SessionID: fixSession, Project: fixProject, Model: fixModel,
			EventTime:     ms(1786855857940),
			UsageDedupKey: model.ToolDSH + "|msg|" + fixMsg1, MessageID: fixMsg1,
			TurnSeq: 0, CallsInTurn: 2,
			SourcePath: path, DedupKey: model.ToolDSH + "|call|" + fixMsg1 + "|" + fixCall1,
		},
		{
			Tool: model.ToolDSH, Kind: model.ActivityTool, Name: "glob",
			SessionID: fixSession, Project: fixProject, Model: fixModel,
			EventTime:     ms(1786855857976),
			UsageDedupKey: model.ToolDSH + "|msg|" + fixMsg1, MessageID: fixMsg1,
			TurnSeq: 1, CallsInTurn: 2,
			SourcePath: path, DedupKey: model.ToolDSH + "|call|" + fixMsg1 + "|" + fixCall2,
		},
	}
	for i, w := range wantAct {
		if obs.Activity[i] != w {
			t.Errorf("activity %d:\n got %+v\nwant %+v", i, obs.Activity[i], w)
		}
	}

	// The captured session composes no agent preset, so there is no turn
	// context to record and none is invented.
	if len(obs.TurnContexts) != 0 {
		t.Errorf("turn contexts = %d, want 0: %+v", len(obs.TurnContexts), obs.TurnContexts)
	}
}

// TestFixtureAuditPayloadKeepsTheCollapseEvidence checks that Raw carries the
// provider counters and the sourceEventSeqs list — the record's own statement of
// which chunk seqs it collapsed, including the duplicate usage chunk.
func TestFixtureAuditPayloadKeepsTheCollapseEvidence(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--home-agent-project--", fixSession, fixtureLines(t))

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	raw := obs.Events[0].Raw
	for _, want := range []string{
		`"messageId":"` + fixMsg1 + `"`,
		`"responseModel":"gemma4:31b"`,
		`"usage":{"inputTokens":10065,"outputTokens":44}`,
		`"sourceEventSeqs":[16,17,18,19,20,21,22,23]`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("audit payload missing %s:\n%s", want, raw)
		}
	}
}

// --- zstd, the default artifact ------------------------------------------

// TestZstdMultiFrameTranscriptMatchesPlain reads the same logical session from
// DSH's default artifact — a concatenation of independent frames — and from the
// plain spelling, and requires the two to agree on everything but the path.
//
// This is the load-bearing test for the whole adapter: DSH compresses BY
// DEFAULT, so a reader without a working multi-frame decode sees nothing at all
// rather than a partial history.
func TestZstdMultiFrameTranscriptMatchesPlain(t *testing.T) {
	lines := fixtureLines(t)

	plainHome := t.TempDir()
	plantSession(t, plainHome, "--home-agent-project--", fixSession, lines)
	plain := collectAll(t, adapter.DiscoverConfig{Home: plainHome})

	// Header frame, then several append frames, as the backend writes them.
	zHome := t.TempDir()
	zPath := plantZstd(t, zHome, "--home-agent-project--", fixSession, [][]string{
		lines[:1], lines[1:12], lines[12:26], lines[26:],
	})
	zst := collectAll(t, adapter.DiscoverConfig{Home: zHome})

	if len(zst.Events) != len(plain.Events) || len(zst.Activity) != len(plain.Activity) {
		t.Fatalf("zstd read %d events / %d activity, plain read %d / %d",
			len(zst.Events), len(zst.Activity), len(plain.Events), len(plain.Activity))
	}
	for i := range plain.Events {
		a, b := plain.Events[i], zst.Events[i]
		a.SourcePath, b.SourcePath = "", ""
		if a != b {
			t.Errorf("event %d differs:\nplain %+v\n zstd %+v", i, a, b)
		}
	}
	for i := range plain.Activity {
		a, b := plain.Activity[i], zst.Activity[i]
		a.SourcePath, b.SourcePath = "", ""
		if a != b {
			t.Errorf("activity %d differs:\nplain %+v\n zstd %+v", i, a, b)
		}
	}
	if zst.Events[0].SourcePath != zPath {
		t.Errorf("SourcePath = %q, want %q", zst.Events[0].SourcePath, zPath)
	}
}

// TestTornZstdTailKeepsCompleteFramesAndWithholdsCheckpoint models DSH's own
// crash case: the last append frame is incomplete on disk. Everything in the
// complete frames must survive, and the checkpoint must NOT advance — advancing
// it over an unread remainder loses those records forever.
func TestTornZstdTailKeepsCompleteFramesAndWithholdsCheckpoint(t *testing.T) {
	lines := fixtureLines(t)
	whole := zstdFrames(t, [][]string{lines[:1], lines[1:26], lines[26:]})
	// Cut into the final frame, leaving the first two complete.
	torn := whole[:len(whole)-8]

	home := t.TempDir()
	dir := filepath.Join(dshHome(home), dirSessions, "--home-agent-project--", fixSession)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, transcriptZstd)
	if err := os.WriteFile(path, torn, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover: %v, %d sources", err, len(srcs))
	}
	obs, err := a.Collect(context.Background(), srcs[0])
	if err == nil {
		t.Fatal("a torn tail must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "partial read") {
		t.Errorf("error = %v, want it to name a partial read", err)
	}
	if obs.Checkpoint != nil {
		t.Error("checkpoint advanced over an unread remainder")
	}
	if len(obs.Events) != 1 || obs.Events[0].DedupKey != model.ToolDSH+"|msg|"+fixMsg1 {
		t.Fatalf("complete frames lost: %d events %+v", len(obs.Events), obs.Events)
	}
}

// --- trap: the same call reports usage twice ------------------------------

// TestUsageChunkIsNeverCounted is the split-identity trap in its DSH form. The
// assistant/chunk of type "usage" and the step's assistant/message describe ONE
// provider call. Here the chunk is given deliberately different numbers so a
// reader that counted it — or preferred it — cannot pass by coincidence.
func TestUsageChunkIsNeverCounted(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--home-agent-project--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/chunk","seq":0,"time":1100,"data":{"turn":1,"step":1,"chunk":{"type":"usage","usage":{"inputTokens":999999,"outputTokens":888888}}}}`,
		`{"type":"assistant/message","seq":1,"time":1200,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":100,"outputTokens":20}},"sourceEventSeqs":[0]}`,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want exactly 1 (the chunk is the same call): %+v", len(obs.Events), obs.Events)
	}
	got := obs.Events[0]
	if got.InputTokens != 100 || got.OutputTokens != 20 || got.TotalTokens != 120 {
		t.Errorf("read the chunk instead of the message: %+v", got)
	}
}

// TestPackedChunkRowsAreIgnored covers the other chunk spelling: DSH packs runs
// of delta chunks into `text-chunks` / `reasoning-chunks` / `tool-call-chunks`
// rows. None of them is a usage record and none may become one.
func TestPackedChunkRowsAreIgnored(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--home-agent-project--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"text-chunks","seq0":0,"time0":1100,"data":{"turn":1,"step":1,"index":0,"dt":[0,1],"texts":["a","b"]}}`,
		`{"type":"reasoning-chunks","seq0":2,"time0":1150,"data":{"turn":1,"step":1,"index":0,"dt":[0],"texts":["r"]}}`,
		`{"type":"tool-call-chunks","seq0":3,"time0":1160,"data":{"turn":1,"step":1,"index":0,"dt":[0],"argumentsDeltas":["{}"]}}`,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 0 || len(obs.Activity) != 0 {
		t.Fatalf("packed chunk rows produced %d events / %d activity", len(obs.Events), len(obs.Activity))
	}
}

// --- trap: a forked session replays its parent's records ------------------

// TestForkedSessionReplaysCollapseOntoTheParent covers the second face of
// split identity. A DSH fork seeds the child's log with the parent's leading
// events copied verbatim under a NEW session id. Keys built from the session
// would make those copies new rows and charge the parent's history once per
// fork; keys built from the message identity collapse them.
func TestForkedSessionReplaysCollapseOntoTheParent(t *testing.T) {
	body := []string{
		`{"type":"assistant/message","seq":1,"time":1200,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":100,"outputTokens":20}},"sourceEventSeqs":[0]}`,
		`{"type":"tool/call","seq":2,"time":1300,"data":{"turn":1,"step":1,"callId":"call-1","name":"bash","arguments":"{}"}}`,
	}
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-parent", append([]string{
		`{"type":"session","version":0,"id":"session-parent","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
	}, body...))
	plantSession(t, home, "--w--", "session-child", append([]string{
		`{"type":"session","version":0,"id":"session-child","createdAt":2000,"cwd":"/w","parentSession":"session-parent","seedLength":3,"delegationDepth":1}`,
	}, body...))

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 2 || len(obs.Activity) != 2 {
		t.Fatalf("want both sessions read: %d events, %d activity", len(obs.Events), len(obs.Activity))
	}
	if obs.Events[0].DedupKey != obs.Events[1].DedupKey {
		t.Errorf("fork replay minted a second usage row: %q vs %q",
			obs.Events[0].DedupKey, obs.Events[1].DedupKey)
	}
	if obs.Activity[0].DedupKey != obs.Activity[1].DedupKey {
		t.Errorf("fork replay minted a second activity row: %q vs %q",
			obs.Activity[0].DedupKey, obs.Activity[1].DedupKey)
	}
	// The rows still carry their own session, so the collapse is the store's
	// conflict-skip rather than a loss of provenance in the adapter.
	if obs.Events[0].SessionID == obs.Events[1].SessionID {
		t.Error("both rows report the same session id; the fork's provenance was lost")
	}
}

// TestMissingMessageIDFallsBackToSessionAndSeq documents the fallback: a record
// with no message id (which DSH itself never writes) is still keyed stably
// across polls, at the cost of not collapsing across a fork.
func TestMissingMessageIDFallsBackToSessionAndSeq(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":7,"time":1200,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"}},"usage":{"inputTokens":100,"outputTokens":20}}}`,
		`{"type":"tool/call","seq":8,"time":1300,"data":{"turn":1,"step":1,"callId":"call-1","name":"bash","arguments":"{}"}}`,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 1 || obs.Events[0].DedupKey != "dsh|session-x|seq|7" {
		t.Fatalf("usage key = %+v", obs.Events)
	}
	if len(obs.Activity) != 1 || obs.Activity[0].DedupKey != "dsh|call|session-x|seq|8" {
		t.Fatalf("activity key = %+v", obs.Activity)
	}
	// The step still holds exactly one usage row, so the join is still 1:1 and
	// the call is still attributed: what the missing id costs is the collapse
	// across a fork, not the attribution.
	if obs.Activity[0].UsageDedupKey != "dsh|session-x|seq|7" {
		t.Errorf("UsageDedupKey = %q, want the fallback usage key", obs.Activity[0].UsageDedupKey)
	}
	if obs.Activity[0].MessageID != "" {
		t.Errorf("MessageID = %q, want empty", obs.Activity[0].MessageID)
	}
}

// --- attribution ----------------------------------------------------------

// TestCallsAttributeToTheirOwnStep checks the join is per (turn, step) — one
// model call plus the tools it requested — and not "the nearest usage row".
func TestCallsAttributeToTheirOwnStep(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
		`{"type":"tool/call","seq":2,"time":1110,"data":{"turn":1,"step":1,"callId":"c1","name":"bash","arguments":"{}"}}`,
		`{"type":"tool/call","seq":3,"time":1120,"data":{"turn":1,"step":1,"callId":"c2","name":"read","arguments":"{}"}}`,
		`{"type":"tool/call","seq":4,"time":1130,"data":{"turn":1,"step":1,"callId":"c3","name":"mcp__github__create_issue","arguments":"{}"}}`,
		`{"type":"assistant/message","seq":5,"time":1200,"data":{"turn":1,"step":2,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-2"},"usage":{"inputTokens":20,"outputTokens":2}}}`,
		`{"type":"tool/call","seq":6,"time":1210,"data":{"turn":1,"step":2,"callId":"c4","name":"glob","arguments":"{}"}}`,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Activity) != 4 {
		t.Fatalf("activity = %d, want 4", len(obs.Activity))
	}
	type got struct {
		name, key string
		seq, n    int
	}
	want := []got{
		{"bash", "dsh|msg|msg-1", 0, 3},
		{"read", "dsh|msg|msg-1", 1, 3},
		{"mcp__github__create_issue", "dsh|msg|msg-1", 2, 3},
		{"glob", "dsh|msg|msg-2", 0, 1},
	}
	for i, w := range want {
		a := obs.Activity[i]
		g := got{a.Name, a.UsageDedupKey, a.TurnSeq, a.CallsInTurn}
		if g != w {
			t.Errorf("activity %d = %+v, want %+v", i, g, w)
		}
	}
}

// TestAmbiguousStepAttributesNothing: two usage-bearing assistant messages for
// one step break the 1:1 join, so the calls are recorded as UNATTRIBUTED rather
// than charged to whichever row happened to come first. Both usage rows survive
// — the ledger never loses a cost because the attribution was unclear.
func TestAmbiguousStepAttributesNothing(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-a"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
		`{"type":"assistant/message","seq":2,"time":1150,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-b"},"usage":{"inputTokens":30,"outputTokens":3}}}`,
		`{"type":"tool/call","seq":3,"time":1200,"data":{"turn":1,"step":1,"callId":"c1","name":"bash","arguments":"{}"}}`,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 2 {
		t.Fatalf("events = %d, want both rows kept", len(obs.Events))
	}
	if len(obs.Activity) != 1 {
		t.Fatalf("activity = %d, want 1", len(obs.Activity))
	}
	if obs.Activity[0].UsageDedupKey != "" {
		t.Errorf("UsageDedupKey = %q, want empty on an ambiguous step", obs.Activity[0].UsageDedupKey)
	}
	if obs.Activity[0].DedupKey != "dsh|call|session-x|seq|3" {
		t.Errorf("DedupKey = %q, want the anchorless spelling", obs.Activity[0].DedupKey)
	}
}

// --- token accounting -----------------------------------------------------

// TestDisjointTokenMapping pins the arithmetic DSH documents: inputTokens is
// UNCACHED input, cache read and write are the rest of billed input, reasoning
// is a subdivision of output that is never added again, and the total is the
// sum of the four disjoint buckets.
func TestDisjointTokenMapping(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":100,"outputTokens":40,"cacheReadTokens":7000,"cacheWriteTokens":300,"reasoningTokens":25}}}`,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d", len(obs.Events))
	}
	e := obs.Events[0]
	switch {
	case e.InputTokens != 100:
		t.Errorf("InputTokens = %d, want 100 (uncached input only)", e.InputTokens)
	case e.OutputTokens != 40:
		t.Errorf("OutputTokens = %d, want 40", e.OutputTokens)
	case e.CacheReadTokens != 7000:
		t.Errorf("CacheReadTokens = %d, want 7000", e.CacheReadTokens)
	case e.CacheCreationTokens != 300:
		t.Errorf("CacheCreationTokens = %d, want 300 (cacheWriteTokens)", e.CacheCreationTokens)
	case e.ReasoningTokens != 25:
		t.Errorf("ReasoningTokens = %d, want 25", e.ReasoningTokens)
	case e.TotalTokens != 7440:
		t.Errorf("TotalTokens = %d, want 7440 = 100+40+7000+300 with reasoning not added again", e.TotalTokens)
	}
}

// TestReasoningIsClampedToOutput: reasoning is a subdivision of output by
// contract, so a source reporting more of it than output is clamped rather than
// stored as a number that cannot be true.
func TestReasoningIsClampedToOutput(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":5,"reasoningTokens":900}}}`,
	})
	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if got := obs.Events[0].ReasoningTokens; got != 5 {
		t.Errorf("ReasoningTokens = %d, want 5", got)
	}
}

// TestNegativeCountersAreClamped: a negative counter would violate the ledger's
// CHECK constraint and poison the whole insert batch it rides in.
func TestNegativeCountersAreClamped(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":-5,"outputTokens":20}}}`,
	})
	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	e := obs.Events[0]
	if e.InputTokens != 0 || e.TotalTokens != 20 {
		t.Errorf("negative input not clamped: %+v", e)
	}
}

// TestStepsWithoutAccountingEmitNoUsage covers the two shapes of a step that
// cost nothing this adapter can report: an absent usage object, and an
// all-zero one (a known-empty provider stream). Both still anchor their calls.
func TestStepsWithoutAccountingEmitNoUsage(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"}}}`,
		`{"type":"tool/call","seq":2,"time":1110,"data":{"turn":1,"step":1,"callId":"c1","name":"bash","arguments":"{}"}}`,
		`{"type":"assistant/message","seq":3,"time":1200,"data":{"turn":1,"step":2,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-2"},"usage":{"inputTokens":0,"outputTokens":0}},"sourceEventSeqs":[]}`,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 0 {
		t.Fatalf("events = %d, want 0: %+v", len(obs.Events), obs.Events)
	}
	if len(obs.Activity) != 1 {
		t.Fatalf("activity = %d, want 1", len(obs.Activity))
	}
	if got := obs.Activity[0].UsageDedupKey; got != "" {
		t.Errorf("UsageDedupKey = %q; a step with no accounting has no row to name", got)
	}
	if got := obs.Activity[0].MessageID; got != "msg-1" {
		t.Errorf("MessageID = %q, want the step's message as the key anchor", got)
	}
}

// TestTitleLLMRequestProducesNoUsage documents a KNOWN UNDER-RECORD: DSH's
// session-title plugin issues a real, billed model call and the log records the
// REQUEST (`session/title-llm-request`) with no usage anywhere. Nothing in the
// transcript carries its token counts, so it is absent from the ledger rather
// than estimated. The fixture contains one.
func TestTitleLLMRequestProducesNoUsage(t *testing.T) {
	lines := fixtureLines(t)
	var found bool
	for _, l := range lines {
		if strings.Contains(l, `"type":"session/title-llm-request"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture no longer contains a session/title-llm-request; this documented gap is untested")
	}
	home := t.TempDir()
	plantSession(t, home, "--home-agent-project--", fixSession, lines)
	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 2 {
		t.Fatalf("events = %d, want 2 — the title call must neither be counted nor estimated", len(obs.Events))
	}
}

// --- turn context ---------------------------------------------------------

// TestAgentPresetBecomesTurnContext: SessionHeader.agentPreset names the agent
// composition every turn of the session ran under, so it is recorded on the
// agent dimension for each usage row. It is a declared header field, not an
// inference from adjacency.
func TestAgentPresetBecomesTurnContext(t *testing.T) {
	home := t.TempDir()
	path := plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","origin":"subagent","delegationDepth":1,"agentPreset":"code-reviewer"}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	want := model.TurnContext{
		UsageDedupKey: "dsh|msg|msg-1",
		Tool:          model.ToolDSH,
		Dimension:     model.DimensionAgent,
		Value:         "code-reviewer",
		SessionID:     "session-x",
		Project:       "/w",
		Model:         "m",
		EventTime:     ms(1100),
		SourcePath:    path,
	}
	if len(obs.TurnContexts) != 1 || obs.TurnContexts[0] != want {
		t.Fatalf("turn contexts = %+v, want exactly [%+v]", obs.TurnContexts, want)
	}
}

// TestForkSeedRecordsNoTurnContext is the fork trap applied to the THIRD fact
// type. Usage and activity survive a replayed prefix because their keys are the
// message's; turn context cannot, because its value is the session HEADER's.
//
// DSH writes a child's agentPreset from the parent's LIVE composition rather
// than the parent's header — a parent that recomposed while blank runs on a
// newer preset than its header names — so the two logs CAN disagree about the
// records they share. A replayed record derives the same usage dedup key in
// both, and usage_turn_context is keyed (usage_dedup_key, dimension) ON
// CONFLICT DO NOTHING: whichever transcript the walk reaches first wins in
// silence. Here the child sorts FIRST, so a header-blind adapter labels the
// parent's own turn with the fork's preset.
func TestForkSeedRecordsNoTurnContext(t *testing.T) {
	// seq 0 is the parent's; seq 1 is the child's own work. seedLength=1.
	parentTurn := `{"type":"assistant/message","seq":0,"time":1200,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-parent"},"usage":{"inputTokens":100,"outputTokens":20}}}`

	home := t.TempDir()
	// "session-a..." sorts before "session-z...", so the CHILD is walked first.
	plantSession(t, home, "--w--", "session-a-child", []string{
		`{"type":"session","version":0,"id":"session-a-child","createdAt":2000,"cwd":"/w","parentSession":"session-z-parent","seedLength":1,"origin":"subagent","delegationDepth":1,"agentPreset":"reviewer"}`,
		parentTurn,
		`{"type":"assistant/message","seq":1,"time":1300,"data":{"turn":2,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-child"},"usage":{"inputTokens":10,"outputTokens":2}}}`,
	})
	plantSession(t, home, "--w--", "session-z-parent", []string{
		`{"type":"session","version":0,"id":"session-z-parent","createdAt":1000,"cwd":"/w","delegationDepth":0,"agentPreset":"planner"}`,
		parentTurn,
	})

	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 3 {
		t.Fatalf("events = %d, want 3 (the replay still yields its row): %+v", len(obs.Events), obs.Events)
	}

	// One context per usage key: the parent's turn from the parent's log, the
	// child's own turn from the child's. The replay contributes none.
	got := map[string]string{}
	for _, c := range obs.TurnContexts {
		if prev, dup := got[c.UsageDedupKey]; dup {
			t.Errorf("two contexts for %s: %q and %q — the fork seed re-labelled a turn it does not own",
				c.UsageDedupKey, prev, c.Value)
		}
		got[c.UsageDedupKey] = c.Value
	}
	want := map[string]string{
		"dsh|msg|msg-parent": "planner",
		"dsh|msg|msg-child":  "reviewer",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("context[%s] = %q, want %q", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("contexts = %+v, want exactly %+v", got, want)
	}
}

// TestNoAgentPresetEmitsNoContext: a session that composes no preset has no
// context to record, and a subagent origin alone is not a name.
func TestNoAgentPresetEmitsNoContext(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","origin":"subagent","delegationDepth":1}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	})
	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(obs.TurnContexts) != 0 {
		t.Fatalf("turn contexts = %+v, want none", obs.TurnContexts)
	}
}

// --- model and provider ---------------------------------------------------

// TestRequestContextSuppliesTheModelFallback: a message whose provenance names
// no route falls back to the newest request/context, which carries route
// metadata and no content of any kind.
func TestRequestContextSuppliesTheModelFallback(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"request/context","seq":0,"time":1050,"data":{"provider":"ollama-local","model":"gemma4:31b-cloud","contextWindow":262144}}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	})
	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	e := obs.Events[0]
	if e.Model != "gemma4:31b-cloud" || e.Provider != "ollama-local" {
		t.Errorf("model/provider = %q/%q, want the request/context route", e.Model, e.Provider)
	}
}

// TestProviderIsPassedThroughVerbatim: the provider is a ROUTE key the source
// names, and it is stored as written rather than mapped onto a vendor guess.
func TestProviderIsPassedThroughVerbatim(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"deepseek","model":"deepseek-chat"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	})
	obs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if got := obs.Events[0].Provider; got != "deepseek" {
		t.Errorf("Provider = %q, want the route key verbatim", got)
	}
}

// --- discovery ------------------------------------------------------------

// TestDiscoverAcceptsBothSpellingsAndIgnoresEverythingElse: DSH's own discovery
// reads the fixed transcript name and rejects flat artifacts instead of ignoring
// them, so a stray .jsonl in a session directory must not become a source.
func TestDiscoverAcceptsBothSpellingsAndIgnoresEverythingElse(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(dshHome(home), dirSessions)
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mk("--w--/session-a/" + transcriptPlain)
	mk("--w--/session-b/" + transcriptZstd)
	mk("--w--/session-c/notes.jsonl")      // a session-owned artifact, not a transcript
	mk("--w--/session-d.jsonl")            // the flat layout DSH rejects
	mk("--w--/session-e/session.jsonl.gz") // wrong encoding

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %d, want 2: %+v", len(srcs), srcs)
	}
	for _, s := range srcs {
		if s.Tool != model.ToolDSH || s.Class != model.EventLevel {
			t.Errorf("source %+v has the wrong tool/class", s)
		}
	}
	if !strings.Contains(srcs[0].Label, "session-a") || !strings.Contains(srcs[1].Label, "session-b") {
		t.Errorf("labels = %q, %q; want the session directory names", srcs[0].Label, srcs[1].Label)
	}
}

// TestHomeEnvMovesDiscovery: DSH_HOME moves the whole surface, which is why it
// is an exported constant the supervision guard can find.
func TestHomeEnvMovesDiscovery(t *testing.T) {
	real := t.TempDir()
	plantSession(t, real, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	})
	t.Setenv(HomeEnv, dshHome(real))

	// A different (empty) home: without the variable there would be nothing.
	obs := collectAll(t, adapter.DiscoverConfig{Home: t.TempDir()})
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want 1 read through %s", len(obs.Events), HomeEnv)
	}
}

// --- incremental ----------------------------------------------------------

// TestIncrementalSkipsUnchangedAndRereadsOnChange: the checkpoint is a
// work-avoidance gate, never a correctness input — a re-read re-derives the
// same dedup keys and the store collapses them.
func TestIncrementalSkipsUnchangedAndRereadsOnChange(t *testing.T) {
	home := t.TempDir()
	lines := []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	}
	path := plantSession(t, home, "--w--", "session-x", lines)

	a := New().(Adapter)
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover: %v, %d", err, len(srcs))
	}

	first, err := a.CollectIncremental(context.Background(), srcs[0], nil)
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if len(first.Events) != 1 || first.Checkpoint == nil {
		t.Fatalf("first collect = %d events, checkpoint %v", len(first.Events), first.Checkpoint)
	}

	again, err := a.CollectIncremental(context.Background(), srcs[0], first.Checkpoint)
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if len(again.Events) != 0 || again.Checkpoint != nil {
		t.Fatalf("unchanged file was re-read: %d events, checkpoint %v", len(again.Events), again.Checkpoint)
	}

	lines = append(lines,
		`{"type":"assistant/message","seq":2,"time":1200,"data":{"turn":1,"step":2,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-2"},"usage":{"inputTokens":20,"outputTokens":2}}}`)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Force an mtime change even on a coarse clock.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	third, err := a.CollectIncremental(context.Background(), srcs[0], first.Checkpoint)
	if err != nil {
		t.Fatalf("third collect: %v", err)
	}
	if len(third.Events) != 2 {
		t.Fatalf("grown file = %d events, want the whole file re-read", len(third.Events))
	}
}

// --- robustness -----------------------------------------------------------

// TestMalformedLineIsSkippedNotFatal: one unreadable record must not cost the
// rest of the session.
func TestMalformedLineIsSkippedNotFatal(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--w--", "session-x", []string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0}`,
		`{"type":"assistant/message","seq":1,` + "\uFFFD" + `broken`,
		`{"type":"assistant/message","seq":2,"time":1200,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-2"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	})

	_, err := collectAllErr(t, adapter.DiscoverConfig{Home: home})
	if err == nil || !strings.Contains(err.Error(), "skipped 1") {
		t.Fatalf("error = %v, want it to report one skipped record", err)
	}
	obs, _ := collectAllErr(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 1 || obs.Events[0].DedupKey != "dsh|msg|msg-2" {
		t.Fatalf("the good record was lost: %+v", obs.Events)
	}
}

// TestHeaderlessTranscriptStillYieldsNoIdentityGuess: a log whose first line is
// not a header has no session id and no cwd, and none is invented from the
// directory name — the project directory encoding is lossy by design.
func TestHeaderlessTranscriptStillYieldsNoIdentityGuess(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--home-agent-project--", "session-x", []string{
		`{"type":"turn/start","seq":0,"time":1000,"data":{"turn":1}}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	})
	obs, _ := collectAllErr(t, adapter.DiscoverConfig{Home: home})
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d", len(obs.Events))
	}
	if obs.Events[0].SessionID != "" || obs.Events[0].Project != "" {
		t.Errorf("identity guessed from the path: %+v", obs.Events[0])
	}
	if obs.Events[0].DedupKey != "dsh|msg|msg-1" {
		t.Errorf("DedupKey = %q; the message identity must survive a missing header", obs.Events[0].DedupKey)
	}
}

// TestBlankLineBeforeTheHeaderKeepsTheIdentity: the header is the first
// NON-EMPTY line, not physical line 0. Keying it on a read position makes one
// stray blank line — an empty append frame, a re-encoded tail — silently cost
// the transcript its session id, its cwd and its agentPreset, with no skipped
// record and no error to show for it.
func TestBlankLineBeforeTheHeaderKeepsTheIdentity(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(dshHome(home), dirSessions, "--w--", "session-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "\n" + strings.Join([]string{
		`{"type":"session","version":0,"id":"session-x","createdAt":1000,"cwd":"/w","delegationDepth":0,"agentPreset":"reviewer"}`,
		`{"type":"assistant/message","seq":1,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"p","model":"m"},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":1}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, transcriptPlain), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	obs, err := collectAllErr(t, adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(obs.Events))
	}
	if got := obs.Events[0].SessionID; got != "session-x" {
		t.Errorf("SessionID = %q, want session-x: the header was read as a record", got)
	}
	if got := obs.Events[0].Project; got != "/w" {
		t.Errorf("Project = %q, want /w", got)
	}
	if len(obs.TurnContexts) != 1 || obs.TurnContexts[0].Value != "reviewer" {
		t.Errorf("turn contexts = %+v, want the header's agent preset", obs.TurnContexts)
	}
}

// TestBoundedReaderErrorsInsteadOfTruncating: the decompression bound must end
// the scan with an ERROR. An io.LimitReader would end it with EOF, which looks
// exactly like a complete file and would advance the checkpoint over whatever
// was left — the one direction a checkpoint must never move.
func TestBoundedReaderErrorsInsteadOfTruncating(t *testing.T) {
	b := &boundedReader{r: strings.NewReader(strings.Repeat("x", 100)), left: 10}

	got, err := io.ReadAll(b)
	if len(got) != 10 {
		t.Errorf("read %d bytes, want the bound of 10", len(got))
	}
	if !errors.Is(err, errTooLarge) {
		t.Errorf("err = %v, want errTooLarge", err)
	}
}
