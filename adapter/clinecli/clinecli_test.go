package clinecli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// clearEnv blanks every variable that moves this adapter's surface, so an
// ambient Cline install on the developer's machine can never reach a test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{DirEnv, DataDirEnv, SessionDataDirEnv, DBDataDirEnv} {
		t.Setenv(name, "")
	}
}

// discover runs discovery against a fixture root passed as an aiusage override.
func discover(t *testing.T, root string) []adapter.Source {
	t.Helper()
	clearEnv(t)
	srcs, err := New().Discover(context.Background(),
		adapter.DiscoverConfig{Overrides: map[string]string{model.ToolCline: root}})
	if err != nil {
		t.Fatalf("Discover(%s): %v", root, err)
	}
	return srcs
}

// collectAll discovers under root and collects every source, concatenating the
// observations in discovery order.
func collectAll(t *testing.T, root string) adapter.Observation {
	t.Helper()
	var all adapter.Observation
	for _, src := range discover(t, root) {
		obs, err := New().Collect(context.Background(), src)
		if err != nil {
			t.Fatalf("Collect(%s): %v", src.Path, err)
		}
		all.Events = append(all.Events, obs.Events...)
		all.Activity = append(all.Activity, obs.Activity...)
		all.TurnContexts = append(all.TurnContexts, obs.TurnContexts...)
	}
	return all
}

func eventByKey(t *testing.T, evs []model.UsageEvent, key string) model.UsageEvent {
	t.Helper()
	for _, e := range evs {
		if e.DedupKey == key {
			return e
		}
	}
	t.Fatalf("no event with dedup key %q; got %d events", key, len(evs))
	return model.UsageEvent{}
}

func activityByKey(t *testing.T, as []model.ActivityEvent, key string) model.ActivityEvent {
	t.Helper()
	for _, a := range as {
		if a.DedupKey == key {
			return a
		}
	}
	t.Fatalf("no activity with dedup key %q; got %d rows", key, len(as))
	return model.ActivityEvent{}
}

// TestLiveFixtureExactEvents pins every field of every event derived from the
// scrubbed capture of two real `cline` 3.0.55 sessions. The counters and the
// identifiers are exactly what the CLI wrote; only the conversation text was
// replaced.
func TestLiveFixtureExactEvents(t *testing.T) {
	obs := collectAll(t, "testdata/live")

	want := []model.UsageEvent{
		{
			Tool: model.ToolCline, Model: "gemma4:31b-cloud", Provider: "openai-compatible",
			SessionID: "1786853448040_dhd6i", Project: "/workspace/demo",
			EventTime:   time.UnixMilli(1786853448987).UTC(),
			InputTokens: 4758, OutputTokens: 8, TotalTokens: 4766,
			MessageID: "msg_BZEHy5S0",
			DedupKey:  "cline|1786853448040_dhd6i|lead|msg_BZEHy5S0",
			Kind:      model.KindUsage,
		},
		{
			Tool: model.ToolCline, Model: "gemma4:31b-cloud", Provider: "openai-compatible",
			SessionID: "1786855504226_rafg6", Project: "/workspace/probe",
			EventTime:   time.UnixMilli(1786855505886).UTC(),
			InputTokens: 4770, OutputTokens: 20, TotalTokens: 4790,
			MessageID: "msg_0P7wghlz",
			DedupKey:  "cline|1786855504226_rafg6|lead|msg_0P7wghlz",
			Kind:      model.KindUsage,
		},
		{
			Tool: model.ToolCline, Model: "gemma4:31b-cloud", Provider: "openai-compatible",
			SessionID: "1786855504226_rafg6", Project: "/workspace/probe",
			EventTime:   time.UnixMilli(1786855506538).UTC(),
			InputTokens: 4822, OutputTokens: 17, TotalTokens: 4839,
			MessageID: "msg_7R6ON9ZS",
			DedupKey:  "cline|1786855504226_rafg6|lead|msg_7R6ON9ZS",
			Kind:      model.KindUsage,
		},
	}
	if len(obs.Events) != len(want) {
		t.Fatalf("event count = %d, want %d", len(obs.Events), len(want))
	}
	for i, w := range want {
		got := obs.Events[i]
		got.SourcePath = "" // absolute and machine-specific
		got.Raw = ""        // asserted separately
		if !reflect.DeepEqual(got, w) {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, got, w)
		}
	}

	// The tool call rode in the same message as the usage it is charged to.
	want1 := model.ActivityEvent{
		Tool: model.ToolCline, Kind: model.ActivityTool, Name: "run_commands",
		SessionID: "1786855504226_rafg6", Project: "/workspace/probe",
		Model:         "gemma4:31b-cloud",
		EventTime:     time.UnixMilli(1786855505886).UTC(),
		UsageDedupKey: "cline|1786855504226_rafg6|lead|msg_0P7wghlz",
		MessageID:     "msg_0P7wghlz",
		TurnSeq:       0, CallsInTurn: 1,
		DedupKey: "cline|1786855504226_rafg6|lead|msg_0P7wghlz|call|call_vj271jx5",
	}
	if len(obs.Activity) != 1 {
		t.Fatalf("activity count = %d, want 1: %+v", len(obs.Activity), obs.Activity)
	}
	got := obs.Activity[0]
	got.SourcePath = ""
	if !reflect.DeepEqual(got, want1) {
		t.Errorf("activity:\n got %+v\nwant %+v", got, want1)
	}

	if len(obs.TurnContexts) != 0 {
		t.Errorf("TurnContexts = %d, want 0: this adapter attributes no turn context",
			len(obs.TurnContexts))
	}
}

// TestLive306FixtureExactEvent replays a bounded Cline CLI 3.0.60 session
// captured in an isolated data directory. The prompt, response, system prompt,
// process identity, and workspace path were sanitized; the writer shape,
// timestamps, model identity, message identity, and counters are unchanged.
func TestLive306FixtureExactEvent(t *testing.T) {
	obs := collectAll(t, "testdata/live-3.0.60")
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(obs.Events), obs.Events)
	}

	got := obs.Events[0]
	got.SourcePath = ""
	got.Raw = ""
	want := model.UsageEvent{
		Tool: model.ToolCline, Model: "gemma4:31b-cloud", Provider: "openai-compatible",
		SessionID: "1788188806371_l6qgw", Project: "/workspace/capture",
		EventTime:   time.UnixMilli(1788188809496).UTC(),
		InputTokens: 4860, OutputTokens: 2, TotalTokens: 4862,
		MessageID: "msg_BFDjZVbl",
		DedupKey:  "cline|1788188806371_l6qgw|lead|msg_BFDjZVbl",
		Kind:      model.KindUsage,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event:\n got %+v\nwant %+v", got, want)
	}
	if len(obs.Activity) != 0 || len(obs.TurnContexts) != 0 {
		t.Fatalf("activity/turn context = %d/%d, want 0/0", len(obs.Activity), len(obs.TurnContexts))
	}
}

// TestRawIsTheUsageObjectAllowList checks the audit payload is re-marshalled
// from the allow-list rather than carried as source bytes: it holds the usage
// object, the model identity and the message identity, and nothing else.
func TestRawIsTheUsageObjectAllowList(t *testing.T) {
	obs := collectAll(t, "testdata/live")
	ev := eventByKey(t, obs.Events, "cline|1786853448040_dhd6i|lead|msg_BZEHy5S0")

	var got map[string]any
	if err := json.Unmarshal([]byte(ev.Raw), &got); err != nil {
		t.Fatalf("raw is not JSON (%q): %v", ev.Raw, err)
	}
	wantKeys := map[string]bool{"id": true, "role": true, "ts": true, "modelInfo": true, "metrics": true}
	for k := range got {
		if !wantKeys[k] {
			t.Errorf("raw carries un-allow-listed key %q: %s", k, ev.Raw)
		}
	}
	for k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("raw is missing allow-listed key %q: %s", k, ev.Raw)
		}
	}
}

// TestSessionAccumulatorIsNeverRead is the cumulative-vs-event trap.
//
// The session sidecar carries `metadata.usage` and `metadata.aggregateUsage`,
// and both are running totals of the very per-message metrics this adapter
// reads: measured on a live two-turn session, metadata.usage was 9592/37 while
// the two messages reported 4770/20 and 4822/17. Reading either alongside the
// messages counts every token twice. The trap fixture's sidecar declares
// absurd accumulator values, and none of them may reach any emitted number.
func TestSessionAccumulatorIsNeverRead(t *testing.T) {
	obs := collectAll(t, "testdata/traps")

	// The live capture's own arithmetic, restated so the claim is checked and
	// not merely asserted in a comment.
	live := collectAll(t, "testdata/live")
	var sumIn, sumOut int64
	for _, e := range live.Events {
		if e.SessionID == "1786855504226_rafg6" {
			sumIn += e.InputTokens
			sumOut += e.OutputTokens
		}
	}
	if sumIn != 9592 || sumOut != 37 {
		t.Fatalf("per-message metrics sum to %d/%d, want 9592/37 — the number the "+
			"session sidecar reports as metadata.usage", sumIn, sumOut)
	}

	sentinels := []int64{999999999, 888888888, 222222, 111111}
	for _, e := range obs.Events {
		for _, s := range sentinels {
			for name, v := range map[string]int64{
				"InputTokens":         e.InputTokens,
				"OutputTokens":        e.OutputTokens,
				"CacheReadTokens":     e.CacheReadTokens,
				"CacheCreationTokens": e.CacheCreationTokens,
				"TotalTokens":         e.TotalTokens,
			} {
				if v == s {
					t.Errorf("%s of %s == %d: a session accumulator or a non-assistant "+
						"message reached the ledger", name, e.DedupKey, s)
				}
			}
		}
	}

	var total int64
	for _, e := range obs.Events {
		total += e.TotalTokens
	}
	// 1100 (m1) + 30064 (m2) + 505 (m3) + 10 (m6) + 16 (nosidecar n1).
	if want := int64(31695); total != want {
		t.Errorf("trap fixture total = %d, want %d", total, want)
	}
}

// TestCacheTokensNeverDoubleCount covers both branches of the cache-accounting
// decision. Cline copies each upstream provider's usage fields verbatim, so
// whether a cached token already sits inside inputTokens is the provider's
// convention: the components must sum to the total in either case.
func TestCacheTokensNeverDoubleCount(t *testing.T) {
	obs := collectAll(t, "testdata/traps")

	// Subset branch: 200+50 fits inside 1000, so the cache counts are carved out
	// of the input component and the total stays inputTokens+outputTokens.
	subset := eventByKey(t, obs.Events, "cline|trapsess|lead|m1")
	if subset.InputTokens != 750 || subset.CacheReadTokens != 200 ||
		subset.CacheCreationTokens != 50 || subset.OutputTokens != 100 {
		t.Errorf("subset split = in %d / read %d / write %d / out %d, want 750/200/50/100",
			subset.InputTokens, subset.CacheReadTokens, subset.CacheCreationTokens, subset.OutputTokens)
	}
	if subset.TotalTokens != 1100 {
		t.Errorf("subset total = %d, want 1100 (inputTokens+outputTokens)", subset.TotalTokens)
	}

	// Additive branch: 30000 cannot be a subset of an input count of 4, so the
	// cache read is added on top instead of being clamped away.
	additive := eventByKey(t, obs.Events, "cline|trapsess|lead|m2")
	if additive.InputTokens != 4 || additive.CacheReadTokens != 30000 {
		t.Errorf("additive split = in %d / read %d, want 4/30000",
			additive.InputTokens, additive.CacheReadTokens)
	}
	if additive.TotalTokens != 30064 {
		t.Errorf("additive total = %d, want 30064", additive.TotalTokens)
	}

	for _, e := range obs.Events {
		sum := e.InputTokens + e.OutputTokens + e.CacheReadTokens + e.CacheCreationTokens
		if sum != e.TotalTokens {
			t.Errorf("%s: components sum to %d but total is %d", e.DedupKey, sum, e.TotalTokens)
		}
	}
}

// TestOnlyAssistantMessagesWithMetricsBecomeEvents pins the record filter: a
// user message is never usage even when it carries a metrics block, a message
// with no id has no honest key and is dropped whole, and a message with no
// metrics contributes activity but no tokens.
//
// The trap fixture's user message carries a modelInfo as well as a metrics
// block deliberately, and that is what makes this assertion bite: without one
// the message is refused for naming no model and the role filter could be
// deleted with every test still passing. Its 111111 counters are one of the
// sentinels TestSessionAccumulatorIsNeverRead scans for, so a role filter that
// stopped filtering fails there too.
func TestOnlyAssistantMessagesWithMetricsBecomeEvents(t *testing.T) {
	obs := collectAll(t, "testdata/traps")

	got := make(map[string]bool, len(obs.Events))
	for _, e := range obs.Events {
		got[e.DedupKey] = true
	}
	want := []string{
		"cline|trapsess|lead|m1", "cline|trapsess|lead|m2",
		"cline|trapsess|lead|m3", "cline|trapsess|lead|m6",
		"cline|nosidecar|researcher|n1",
	}
	if len(got) != len(want) {
		t.Fatalf("event keys = %v, want exactly %v", got, want)
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("missing event %q", k)
		}
	}

	// The metrics-less turn's call is observed, and its cost is unknown rather
	// than zero: the key it would have been charged to is left empty.
	orphan := activityByKey(t, obs.Activity, "cline|trapsess|lead|m4|call|call_orphan")
	if orphan.UsageDedupKey != "" {
		t.Errorf("orphan call attributes to %q; a call in a message with no metrics "+
			"has no usage row to be charged to", orphan.UsageDedupKey)
	}
	if orphan.CallsInTurn != 1 {
		t.Errorf("orphan CallsInTurn = %d, want 1", orphan.CallsInTurn)
	}
}

// TestTimestampFallsBackToTheDocumentStamp: a message with no ts is placed at
// the document's last-write stamp rather than at the unix epoch, which would
// otherwise put real tokens in 1970 and poison every all-time total.
func TestTimestampFallsBackToTheDocumentStamp(t *testing.T) {
	obs := collectAll(t, "testdata/traps")
	ev := eventByKey(t, obs.Events, "cline|trapsess|lead|m6")
	want := time.Date(2026, 8, 16, 5, 0, 0, 0, time.UTC)
	if !ev.EventTime.Equal(want) {
		t.Errorf("EventTime = %s, want %s (the document's updated_at)", ev.EventTime, want)
	}
}

// TestActivityDivisorCountsOnlyEmittedRows: calls_in_turn must equal the number
// of rows actually emitted for the turn, so an unusable block (a tool_use with
// no name, which no query could group by) lowers neither the count nor the rows
// out of step with each other.
func TestActivityDivisorCountsOnlyEmittedRows(t *testing.T) {
	obs := collectAll(t, "testdata/traps")

	var rows []model.ActivityEvent
	for _, a := range obs.Activity {
		if a.MessageID == "m3" {
			rows = append(rows, a)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("m3 emitted %d activity rows, want 2 (the named tool_use blocks)", len(rows))
	}
	for _, a := range rows {
		if a.CallsInTurn != len(rows) {
			t.Errorf("%s: CallsInTurn = %d but the turn emitted %d rows",
				a.DedupKey, a.CallsInTurn, len(rows))
		}
		if a.UsageDedupKey != "cline|trapsess|lead|m3" {
			t.Errorf("%s attributes to %q, want the same message's usage row",
				a.DedupKey, a.UsageDedupKey)
		}
	}
	if rows[0].DedupKey != "cline|trapsess|lead|m3|call|call_named" {
		t.Errorf("first call key = %q", rows[0].DedupKey)
	}
	// The id-less block is keyed by its position among the turn's calls, never
	// by a read position: the document is rewritten whole on every save.
	if rows[1].DedupKey != "cline|trapsess|lead|m3|call|idx1" {
		t.Errorf("id-less call key = %q, want the positional fallback", rows[1].DedupKey)
	}
	if rows[0].TurnSeq != 0 || rows[1].TurnSeq != 1 {
		t.Errorf("TurnSeq = %d,%d, want 0,1", rows[0].TurnSeq, rows[1].TurnSeq)
	}
}

// TestDedupKeysSurviveTheWholeFileRewrite is the split-identity class inverted.
// Cline rewrites the entire message document on every save rather than
// appending to it, so a byte-offset tail read would be meaningless and a key
// minted from a read position would recount every earlier turn. The keys are
// minted from the persisted message ids, so a rewritten document re-derives the
// old keys exactly and only the new message is new.
func TestDedupKeysSurviveTheWholeFileRewrite(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "data", "sessions", "s1")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sess, "s1.messages.json")

	write := func(msgs string) {
		doc := `{"version":1,"updated_at":"2026-08-16T05:00:00.000Z","agent":"lead",` +
			`"sessionId":"s1","messages":[` + msgs + `]}`
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	msgA := `{"id":"a","role":"assistant","ts":1786855600000,` +
		`"modelInfo":{"id":"m","provider":"p"},` +
		`"metrics":{"inputTokens":10,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}}`
	msgB := `{"id":"b","role":"assistant","ts":1786855601000,` +
		`"modelInfo":{"id":"m","provider":"p"},` +
		`"metrics":{"inputTokens":20,"outputTokens":2,"cacheReadTokens":0,"cacheWriteTokens":0}}`

	write(msgA)
	first := collectAll(t, dir)
	if len(first.Events) != 1 || first.Events[0].DedupKey != "cline|s1|lead|a" {
		t.Fatalf("first pass = %+v", first.Events)
	}

	write(msgA + "," + msgB) // the CLI rewrites the whole document
	second := collectAll(t, dir)
	if len(second.Events) != 2 {
		t.Fatalf("second pass produced %d events, want 2", len(second.Events))
	}
	if second.Events[0].DedupKey != "cline|s1|lead|a" || second.Events[1].DedupKey != "cline|s1|lead|b" {
		t.Fatalf("keys after rewrite = %q, %q",
			second.Events[0].DedupKey, second.Events[1].DedupKey)
	}
	if second.Events[0].TotalTokens != first.Events[0].TotalTokens {
		t.Errorf("the re-derived event changed: %d then %d",
			first.Events[0].TotalTokens, second.Events[0].TotalTokens)
	}
}

// TestDedupKeysAreScopedToTheSessionAndAgent: message ids are minted by the CLI,
// not by the provider, so two sessions can and eventually will mint the same
// one. The key carries the session and the agent stream, which makes the
// collision unrepresentable instead of merely unlikely.
func TestDedupKeysAreScopedToTheSessionAndAgent(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []struct{ id, agent string }{{"s1", "lead"}, {"s2", "researcher"}} {
		sub := filepath.Join(dir, "data", "sessions", s.id)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		doc := `{"version":1,"updated_at":"2026-08-16T05:00:00.000Z","agent":"` + s.agent + `",` +
			`"sessionId":"` + s.id + `","messages":[` +
			`{"id":"msg_COLLIDE","role":"assistant","ts":1786855600000,` +
			`"modelInfo":{"id":"m","provider":"p"},` +
			`"metrics":{"inputTokens":10,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}}]}`
		if err := os.WriteFile(filepath.Join(sub, s.id+".messages.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	obs := collectAll(t, dir)
	if len(obs.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(obs.Events))
	}
	if obs.Events[0].DedupKey == obs.Events[1].DedupKey {
		t.Fatalf("two sessions minting the same message id collapsed to one key: %q",
			obs.Events[0].DedupKey)
	}
	if got := obs.Events[0].DedupKey; got != "cline|s1|lead|msg_COLLIDE" {
		t.Errorf("key = %q", got)
	}
	if got := obs.Events[1].DedupKey; got != "cline|s2|researcher|msg_COLLIDE" {
		t.Errorf("key = %q", got)
	}
}

// TestCheckpointGatesOnSizeAndMtime: an untouched document is not even opened,
// and a changed one is re-read in full.
func TestCheckpointGatesOnSizeAndMtime(t *testing.T) {
	srcs := discover(t, "testdata/live")
	if len(srcs) == 0 {
		t.Fatal("no sources")
	}
	src := srcs[0]

	obs, err := New().(adapter.Incremental).CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if obs.Checkpoint == nil {
		t.Fatal("first collect returned no checkpoint")
	}
	if obs.Checkpoint.Size == 0 || obs.Checkpoint.MTimeNS == 0 {
		t.Fatalf("checkpoint carries no file stamp: %+v", obs.Checkpoint)
	}
	if obs.Checkpoint.Tool != model.ToolCline || obs.Checkpoint.SourcePath != src.Path {
		t.Errorf("checkpoint identity = %s/%s", obs.Checkpoint.Tool, obs.Checkpoint.SourcePath)
	}
	if len(obs.Events) == 0 {
		t.Fatal("first collect produced no events")
	}

	again, err := New().(adapter.Incremental).CollectIncremental(context.Background(), src, obs.Checkpoint)
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if len(again.Events) != 0 || len(again.Activity) != 0 {
		t.Errorf("unchanged document was re-parsed: %d events, %d activity",
			len(again.Events), len(again.Activity))
	}
	if again.Checkpoint != nil {
		t.Errorf("a skipped read must leave the stored checkpoint alone, got %+v", again.Checkpoint)
	}

	stale := *obs.Checkpoint
	stale.Size--
	changed, err := New().(adapter.Incremental).CollectIncremental(context.Background(), src, &stale)
	if err != nil {
		t.Fatalf("third collect: %v", err)
	}
	if len(changed.Events) != len(obs.Events) {
		t.Errorf("changed document produced %d events, want the full %d",
			len(changed.Events), len(obs.Events))
	}
}

// TestProjectFallsBackToSidecarThenLabel: discovery metadata wins, the session
// sidecar beside the document is next, and a session with neither is labelled
// rather than left blank.
func TestProjectFallsBackToSidecarThenLabel(t *testing.T) {
	obs := collectAll(t, "testdata/traps")

	if got := eventByKey(t, obs.Events, "cline|trapsess|lead|m1").Project; got != "/workspace/traps" {
		t.Errorf("sidecar project = %q, want /workspace/traps", got)
	}
	if got := eventByKey(t, obs.Events, "cline|nosidecar|researcher|n1").Project; got != defaultProject {
		t.Errorf("project with no sidecar = %q, want %q", got, defaultProject)
	}
}

// writeIndex builds a sessions.db with Cline's own column set and the given
// rows. The database is created with a writable connection here; the adapter
// only ever opens it read-only.
func writeIndex(t *testing.T, dbDir string, rows [][6]string) {
	t.Helper()
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, "file:"+filepath.Join(dbDir, indexDBName))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (
		session_id TEXT PRIMARY KEY, source TEXT, pid INTEGER, started_at TEXT,
		status TEXT, provider TEXT, model TEXT, cwd TEXT, workspace_root TEXT,
		parent_session_id TEXT, agent_id TEXT, conversation_id TEXT,
		is_subagent INTEGER NOT NULL DEFAULT 0, metadata_json TEXT,
		messages_path TEXT, updated_at TEXT)`); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO sessions
			(session_id, messages_path, cwd, workspace_root, provider, model, metadata_json)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r[0], r[1], r[2], r[3], r[4], r[5],
			`{"usage":{"inputTokens":999999999,"outputTokens":999999999}}`); err != nil {
			t.Fatalf("insert %s: %v", r[0], err)
		}
	}
}

// copyTree copies a fixture directory into a writable temp tree.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

// TestDiscoveryUsesTheIndexAndTheWalk: the index supplies the cwd and reaches a
// document the walk cannot see, the walk reaches a session the index does not
// name, a stale index row pointing at a deleted document is dropped, and the
// two halves do not report the same file twice.
func TestDiscoveryUsesTheIndexAndTheWalk(t *testing.T) {
	root := t.TempDir()
	copyTree(t, "testdata/traps", root)

	// A document outside the sessions tree: only the index can find it.
	outside := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDoc := filepath.Join(outside, "far.messages.json")
	doc := `{"version":1,"updated_at":"2026-08-16T05:00:00.000Z","agent":"lead",` +
		`"sessionId":"far","messages":[{"id":"f1","role":"assistant","ts":1786855600000,` +
		`"modelInfo":{"id":"m","provider":"p"},` +
		`"metrics":{"inputTokens":5,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}}]}`
	if err := os.WriteFile(outsideDoc, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions := filepath.Join(root, "data", "sessions")
	writeIndex(t, filepath.Join(root, "data", "db"), [][6]string{
		{"trapsess", filepath.Join(sessions, "trapsess", "trapsess.messages.json"),
			"/indexed/cwd", "/indexed/root", "anthropic", "claude-sonnet-4"},
		{"far", outsideDoc, "/indexed/far", "", "anthropic", "claude-sonnet-4"},
		{"gone", filepath.Join(sessions, "gone", "gone.messages.json"), "/x", "", "", ""},
	})

	srcs := discover(t, root)
	byPath := make(map[string]adapter.Source, len(srcs))
	for _, s := range srcs {
		if _, dup := byPath[s.Path]; dup {
			t.Fatalf("source %s discovered twice", s.Path)
		}
		byPath[s.Path] = s
	}
	if len(srcs) != 3 {
		t.Fatalf("discovered %d sources, want 3 (indexed, outside, walked): %v", len(srcs), byPath)
	}
	if _, ok := byPath[outsideDoc]; !ok {
		t.Errorf("the index-only document was not discovered")
	}
	nosidecar := filepath.Join(sessions, "nosidecar", "nosidecar.messages.json")
	if _, ok := byPath[nosidecar]; !ok {
		t.Errorf("the walk-only session was not discovered")
	}
	for p := range byPath {
		if strings.Contains(p, "gone") {
			t.Errorf("a stale index row produced a source for a deleted document: %s", p)
		}
	}

	// The index's cwd wins over the sidecar's, and the accumulator column it
	// sits beside is never read.
	obs := collectAll(t, root)
	if got := eventByKey(t, obs.Events, "cline|trapsess|lead|m1").Project; got != "/indexed/cwd" {
		t.Errorf("indexed project = %q, want /indexed/cwd", got)
	}
	for _, e := range obs.Events {
		if e.InputTokens == 999999999 || e.OutputTokens == 999999999 {
			t.Errorf("%s: sessions.metadata_json reached the ledger", e.DedupKey)
		}
	}
}

// TestIndexReadsUncheckpointedWAL is the read-mode trap on the index database.
//
// Cline holds sessions.db open and writes it under journal_mode=wal, so its
// rows sit in the -wal file until something checkpoints them. immutable=1 makes
// SQLite ignore the WAL entirely and read the main file as of its last
// checkpoint; mode=ro + query_only(1) reads the live database without taking a
// write lock. Measured on this machine's install: the main sessions.db is 4096
// bytes and does not contain the `sessions` table at all, while every row lives
// in a 181KB WAL — an immutable reader there does not lose metadata, it loses
// the whole index. The writer is left open on purpose: closing it checkpoints
// the WAL, after which the assertion would hold either way and prove nothing.
func TestIndexReadsUncheckpointedWAL(t *testing.T) {
	root := t.TempDir()
	sess := filepath.Join(root, "data", "sessions", "walsess")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"version":1,"updated_at":"2026-08-16T05:00:00.000Z","agent":"lead",` +
		`"sessionId":"walsess","messages":[{"id":"w1","role":"assistant","ts":1786855600000,` +
		`"modelInfo":{"id":"m","provider":"p"},` +
		`"metrics":{"inputTokens":11,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}}]}`
	docPath := filepath.Join(sess, "walsess.messages.json")
	if err := os.WriteFile(docPath, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := filepath.Join(root, "data", "db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, indexDBName)
	writer, err := sql.Open(driverName, "file:"+dbPath+"?_pragma=journal_mode(wal)")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	var mode string
	if err := writer.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	if _, err := writer.Exec(`CREATE TABLE sessions (session_id TEXT PRIMARY KEY,
		provider TEXT, model TEXT, cwd TEXT, workspace_root TEXT, messages_path TEXT)`); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if _, err := writer.Exec(`INSERT INTO sessions VALUES
		('walsess','anthropic','claude-sonnet-4','/indexed/wal','/indexed/wal',?)`, docPath); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if fi, err := os.Stat(dbPath + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("no un-checkpointed WAL content (stat err %v); this test cannot detect a stale read", err)
	}

	obs := collectAll(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(obs.Events))
	}
	// Only the index knows this cwd: the document has no sidecar to fall back
	// to, so an index the adapter could not read shows up here as the label.
	if got := obs.Events[0].Project; got != "/indexed/wal" {
		t.Errorf("project = %q, want /indexed/wal — the index's rows are in the WAL "+
			"and an immutable=1 reader cannot see them", got)
	}
}

// TestEnvResolutionFollowsClineOwnChain pins the path chain read out of the
// shipped CLI bundle. The trap it guards is CLINE_SESSION_DATA_DIR: it IS the
// sessions directory, so an adapter that appends "sessions" to it — as the
// harness matrix row spells the path — looks at a directory that never exists
// and silently collects nothing from exactly the machines that set it.
func TestEnvResolutionFollowsClineOwnChain(t *testing.T) {
	abs := func(p string) string {
		v, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	live := abs("testdata/live")

	cases := []struct {
		name string
		env  map[string]string
		home string
		want layout
	}{
		{
			name: "home default",
			home: "/home/u",
			want: layout{sessions: "/home/u/.cline/data/sessions", db: "/home/u/.cline/data/db"},
		},
		{
			name: "CLINE_DIR moves the root",
			env:  map[string]string{DirEnv: "/opt/cline"},
			home: "/home/u",
			want: layout{sessions: "/opt/cline/data/sessions", db: "/opt/cline/data/db"},
		},
		{
			name: "CLINE_DATA_DIR moves the data dir",
			env:  map[string]string{DataDirEnv: "/srv/box"},
			home: "/home/u",
			want: layout{sessions: "/srv/box/sessions", db: "/srv/box/db"},
		},
		{
			name: "CLINE_SESSION_DATA_DIR is the sessions dir itself",
			env:  map[string]string{SessionDataDirEnv: "/srv/sess"},
			home: "/home/u",
			want: layout{sessions: "/srv/sess", db: "/home/u/.cline/data/db"},
		},
		{
			name: "CLINE_DB_DATA_DIR moves the index only",
			env:  map[string]string{DBDataDirEnv: "/srv/idx"},
			home: "/home/u",
			want: layout{sessions: "/home/u/.cline/data/sessions", db: "/srv/idx"},
		},
		{
			// os.UserHomeDir failing costs the DEFAULTS, never the variables:
			// codex and claudecode honour theirs without a home and this one
			// must too, or the machine that most needs the override is the one
			// machine the override does not reach.
			name: "the variables are honoured with no home at all",
			env:  map[string]string{SessionDataDirEnv: "/srv/sess", DBDataDirEnv: "/srv/idx"},
			want: layout{sessions: "/srv/sess", db: "/srv/idx"},
		},
		{
			// A link nothing named stays empty rather than relative: joining ""
			// with "sessions.db" would probe the daemon's working directory.
			name: "an unnamed link stays empty, never relative",
			env:  map[string]string{SessionDataDirEnv: "/srv/sess"},
			want: layout{sessions: "/srv/sess"},
		},
		{
			name: "no home and no variables resolves to nothing",
			want: layout{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := Adapter{}.resolve(adapter.DiscoverConfig{Home: tc.home})
			if got != tc.want {
				t.Errorf("resolve = %+v, want %+v", got, tc.want)
			}
		})
	}

	// End to end: the variable alone finds the documents.
	t.Run("sessions dir env discovers documents", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(SessionDataDirEnv, filepath.Join(live, "data", "sessions"))
		srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(srcs) != 2 {
			t.Fatalf("discovered %d sources, want 2", len(srcs))
		}
	})

	// An aiusage override may name either the Cline root or the data directory
	// inside it, and resolves to the same place.
	t.Run("override normalises root or data dir", func(t *testing.T) {
		clearEnv(t)
		fromRoot := Adapter{}.resolve(adapter.DiscoverConfig{
			Overrides: map[string]string{model.ToolCline: live}})
		fromData := Adapter{}.resolve(adapter.DiscoverConfig{
			Overrides: map[string]string{model.ToolCline: filepath.Join(live, "data")}})
		if fromRoot != fromData {
			t.Errorf("root override %+v != data override %+v", fromRoot, fromData)
		}
	})
}

// TestPrivacyContentReachesNoEmittedField plants one sentinel in every content
// field the surface has — message text, tool-call input, tool-result payload,
// the document's system prompt, the sidecar's prompt and title — and walks every
// string in every emitted record looking for it. The decode is an allow-list, so
// the sentinel has no field to land in and never becomes a value in this
// process; this test is what fails on the day someone widens the allow-list.
func TestPrivacyContentReachesNoEmittedField(t *testing.T) {
	const secret = "S3CRET-CANARY-do-not-store"

	dir := t.TempDir()
	sess := filepath.Join(dir, "data", "sessions", "priv")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{
		"version":1,
		"updated_at":"2026-08-16T05:00:00.000Z",
		"agent":"lead",
		"sessionId":"priv",
		"title":"` + secret + `",
		"system_prompt":"` + secret + `",
		"messages":[
			{"id":"p1","role":"user","content":[{"type":"text","text":"` + secret + `"}],"ts":1786855600000},
			{"id":"p2","role":"assistant","ts":1786855601000,
			 "modelInfo":{"id":"m","provider":"p"},
			 "metrics":{"inputTokens":10,"outputTokens":2,"cacheReadTokens":0,"cacheWriteTokens":0},
			 "content":[
				{"type":"text","text":"` + secret + `"},
				{"type":"thinking","thinking":"` + secret + `"},
				{"type":"tool_use","id":"call_p","name":"run_commands",
				 "input":{"commands":["` + secret + `"],"note":"` + secret + `"}}
			 ]},
			{"id":"p3","role":"user","ts":1786855602000,
			 "content":[{"type":"tool_result","tool_use_id":"call_p","name":"run_commands",
			 "content":[{"query":"` + secret + `","result":"` + secret + `","success":true}]}]}
		]
	}`
	if err := os.WriteFile(filepath.Join(sess, "priv.messages.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"session_id":"priv","cwd":"/workspace/priv","workspace_root":"/workspace/priv",
		"prompt":"` + secret + `",
		"metadata":{"title":"` + secret + `","systemPrompt":"` + secret + `",
		"prompt":"` + secret + `","usage":{"inputTokens":1,"outputTokens":1}}}`
	if err := os.WriteFile(filepath.Join(sess, "priv.json"), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}

	obs := collectAll(t, dir)
	if len(obs.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(obs.Events))
	}
	if len(obs.Activity) != 1 {
		t.Fatalf("got %d activity rows, want 1", len(obs.Activity))
	}
	if obs.Activity[0].Name != "run_commands" {
		t.Errorf("tool name = %q, want run_commands", obs.Activity[0].Name)
	}
	if obs.Events[0].Raw == "" {
		t.Fatal("the audit payload is empty; this test would then prove nothing about it")
	}

	for _, e := range obs.Events {
		assertNoSecret(t, "usage event", e, secret)
	}
	for _, a := range obs.Activity {
		assertNoSecret(t, "activity event", a, secret)
	}
	for _, c := range obs.TurnContexts {
		assertNoSecret(t, "turn context", c, secret)
	}
}

// assertNoSecret walks every string reachable from v and fails on the sentinel.
func assertNoSecret(t *testing.T, what string, v any, secret string) {
	t.Helper()
	rv := reflect.ValueOf(v)
	var walk func(reflect.Value, string)
	walk = func(x reflect.Value, path string) {
		switch x.Kind() {
		case reflect.String:
			if strings.Contains(x.String(), secret) {
				t.Errorf("%s: content reached %s = %q", what, path, x.String())
			}
		case reflect.Pointer, reflect.Interface:
			if !x.IsNil() {
				walk(x.Elem(), path)
			}
		case reflect.Struct:
			for i := 0; i < x.NumField(); i++ {
				walk(x.Field(i), path+"."+x.Type().Field(i).Name)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < x.Len(); i++ {
				walk(x.Index(i), path+"[]")
			}
		case reflect.Map:
			for _, k := range x.MapKeys() {
				walk(k, path+"{key}")
				walk(x.MapIndex(k), path+"{}")
			}
		}
	}
	walk(rv, what)
}

// TestContentDecodeIsAnAllowList asserts the property the privacy test relies
// on directly: a content block is decoded into exactly three fields, so a block
// carrying anything else contributes nothing until this package is taught about
// it on purpose.
func TestContentDecodeIsAnAllowList(t *testing.T) {
	want := map[string]bool{"Type": true, "ID": true, "Name": true}
	rt := reflect.TypeOf(contentBlock{})
	if rt.NumField() != len(want) {
		t.Fatalf("contentBlock has %d fields, want %d: widening it widens what "+
			"this adapter can store", rt.NumField(), len(want))
	}
	for i := 0; i < rt.NumField(); i++ {
		if !want[rt.Field(i).Name] {
			t.Errorf("contentBlock gained field %q", rt.Field(i).Name)
		}
	}

	// metrics is the whole usage object: four counters, no content, no cost
	// string, nothing a provider could hide a prompt in.
	mt := reflect.TypeOf(metrics{})
	for i := 0; i < mt.NumField(); i++ {
		if k := mt.Field(i).Type.Kind(); k != reflect.Int64 {
			t.Errorf("metrics.%s is %s, want a counter", mt.Field(i).Name, k)
		}
	}
}

// TestBrokenSourcesDoNotFailTheCycle: a missing file errors for that source
// alone, a document that is not JSON errors without a checkpoint (so the next
// cycle retries it), and a document whose content is not an array of blocks
// still yields its usage.
func TestBrokenSourcesDoNotFailTheCycle(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "data", "sessions", "broken")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sess, "broken.messages.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	srcs := discover(t, dir)
	if len(srcs) != 1 {
		t.Fatalf("discovered %d sources, want 1", len(srcs))
	}
	obs, err := New().Collect(context.Background(), srcs[0])
	if err == nil {
		t.Error("an unparseable document returned no error")
	}
	if obs.Checkpoint != nil {
		t.Error("an unparseable document advanced the checkpoint; the next cycle would skip it")
	}

	// content as a bare string: the tool calls are lost, the tokens are not.
	doc := `{"version":1,"updated_at":"2026-08-16T05:00:00.000Z","agent":"lead","sessionId":"broken",
		"messages":[{"id":"b1","role":"assistant","ts":1786855600000,"content":"plain string",
		"modelInfo":{"id":"m","provider":"p"},
		"metrics":{"inputTokens":9,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	obs2 := collectAll(t, dir)
	if len(obs2.Events) != 1 || obs2.Events[0].TotalTokens != 10 {
		t.Fatalf("string content cost the document its usage: %+v", obs2.Events)
	}
	if len(obs2.Activity) != 0 {
		t.Errorf("string content produced %d activity rows, want 0", len(obs2.Activity))
	}

	// A source whose file vanished between discovery and collection.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := New().Collect(context.Background(), srcs[0]); err == nil {
		t.Error("a vanished document returned no error")
	}
}

// TestNoDiscoveryRootIsNotAnError: a machine with no Cline install and no home
// yields no sources and no error, so the collector's cycle is unaffected.
func TestNoDiscoveryRootIsNotAnError(t *testing.T) {
	clearEnv(t)
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("Discover with no home: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("got %d sources, want none", len(srcs))
	}

	srcs, err = New().Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatalf("Discover with an empty home: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("got %d sources from an empty home, want none", len(srcs))
	}
}
