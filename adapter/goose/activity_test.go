package goose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// TestActivityFromToolRequests pins every activity row the fixture produces.
// The first is REAL — block shape, tool name and extension exactly as goose
// wrote them while driving a live `shell` call — and the last two are one
// constructed turn holding several calls.
func TestActivityFromToolRequests(t *testing.T) {
	obs := collect(t, fixtureSource(t))

	want := []model.ActivityEvent{
		{
			Name: "developer__shell", SessionID: "20260816_3",
			Project: "/home/dev/projects/demo", MessageID: "chatcmpl-543",
			EventTime: time.Unix(1786854321, 0).UTC(),
			TurnSeq:   0, CallsInTurn: 1,
			DedupKey: "goose|call|20260816_3|9|0",
		},
		{
			Name: "developer__text_editor", SessionID: "sess_priced",
			Project: "/home/dev/projects/priced", MessageID: "chatcmpl-900",
			EventTime: time.Unix(1786860000, 0).UTC(),
			TurnSeq:   0, CallsInTurn: 2,
			DedupKey: "goose|call|sess_priced|12|0",
		},
		{
			// Already prefixed by the provider: kept verbatim, not double-prefixed.
			Name: "todo__todo_write", SessionID: "sess_priced",
			Project: "/home/dev/projects/priced", MessageID: "chatcmpl-900",
			EventTime: time.Unix(1786860000, 0).UTC(),
			TurnSeq:   1, CallsInTurn: 2,
			DedupKey: "goose|call|sess_priced|12|1",
		},
	}

	if len(obs.Activity) != len(want) {
		var names []string
		for _, a := range obs.Activity {
			names = append(names, a.Name)
		}
		t.Fatalf("want %d activity rows, got %d: %v", len(want), len(obs.Activity), names)
	}
	for i, w := range want {
		got := obs.Activity[i]
		if got.Tool != model.ToolGoose || got.Kind != model.ActivityTool {
			t.Errorf("row %d = (%q, %q), want (%q, tool)", i, got.Tool, got.Kind, model.ToolGoose)
		}
		if got.Name != w.Name || got.DedupKey != w.DedupKey {
			t.Errorf("row %d = (%q, %q), want (%q, %q)", i, got.Name, got.DedupKey, w.Name, w.DedupKey)
		}
		if got.SessionID != w.SessionID || got.Project != w.Project || got.MessageID != w.MessageID {
			t.Errorf("row %d identity = (%q,%q,%q), want (%q,%q,%q)", i,
				got.SessionID, got.Project, got.MessageID, w.SessionID, w.Project, w.MessageID)
		}
		if !got.EventTime.Equal(w.EventTime) {
			t.Errorf("row %d EventTime = %v, want %v", i, got.EventTime, w.EventTime)
		}
		if got.TurnSeq != w.TurnSeq || got.CallsInTurn != w.CallsInTurn {
			t.Errorf("row %d turn = (%d of %d), want (%d of %d)",
				i, got.TurnSeq, got.CallsInTurn, w.TurnSeq, w.CallsInTurn)
		}
		if got.TurnSeq >= got.CallsInTurn {
			t.Errorf("row %d violates the activity_events CHECK (turn_seq < calls_in_turn)", i)
		}
	}
}

// TestActivityIsNeverAttributed: usage_ledger carries no message id, and two of
// its rows share one created_timestamp in the fixture (as they do live), so the
// only join on offer is a timestamp guess. An unattributed call is reported as
// unknown cost; an invented one would be reported as fact.
func TestActivityIsNeverAttributed(t *testing.T) {
	obs := collect(t, fixtureSource(t))
	if len(obs.Activity) == 0 {
		t.Fatal("no activity to check")
	}
	for _, a := range obs.Activity {
		if a.UsageDedupKey != "" {
			t.Errorf("%s carries UsageDedupKey %q: goose exposes no join between usage_ledger and messages",
				a.Name, a.UsageDedupKey)
		}
		if a.Model != "" {
			t.Errorf("%s carries Model %q: the ledger and the session name different models and neither is tied to the call",
				a.Name, a.Model)
		}
	}
	// The fixture's own evidence for why: two ledger rows, one second.
	var sameSecond int
	for _, e := range obs.Events {
		if e.SessionID == "20260816_3" && e.EventTime.Unix() == 1786854321 {
			sameSecond++
		}
	}
	if sameSecond < 2 {
		t.Errorf("fixture no longer contains two ledger rows in one second (%d); the reason "+
			"attribution is refused has gone missing from the evidence", sameSecond)
	}
}

// TestToolResponseRowsAreNeverRead: the tool OUTPUT lives on the following user
// message. The adapter reads assistant rows only, so the output is never even
// loaded — a privacy property of the query, not of a filter downstream of it.
func TestToolResponseRowsAreNeverRead(t *testing.T) {
	obs := collect(t, fixtureSource(t))
	for _, a := range obs.Activity {
		if strings.Contains(a.Name, "toolResponse") || a.DedupKey == "goose|call|20260816_3|10|0" {
			t.Errorf("a tool RESPONSE row produced activity: %+v", a)
		}
	}
	if !strings.Contains(activityQuery, "m.role = 'assistant'") {
		t.Error("activityQuery no longer restricts to assistant rows: it would start reading tool output")
	}
}

// TestPrivacyCanariesNeverEscape plants nothing new — the fixture already
// carries SCRUB-CANARY-<n> in every COLUMN of the three tables that can hold
// human or model text (session name, description, recipe json, recipe values,
// extension data, message content and message metadata) and in every
// content-bearing BLOCK TYPE of content_json: user prompts, turn context,
// assistant text, thinking, redacted thinking, image data, tool arguments, a
// tool confirmation prompt, the LLM-written `_meta` title and chain summary, a
// failed call's error string and a tool response's output. None of it may reach
// any emitted field, raw payload included.
//
// The types the decode has no field for are the load-bearing half: they prove
// the allow-list drops content at the PARSE, not in a filter downstream that a
// later edit could bypass.
func TestPrivacyCanariesNeverEscape(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("testdata", "sessions.sql"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	planted := strings.Count(string(script), "SCRUB-CANARY-")
	if planted < 24 {
		t.Fatalf("fixture plants only %d canaries; the test is weaker than it claims", planted)
	}

	obs := collect(t, fixtureSource(t))
	dump := fmt.Sprintf("%+v", obs)
	// Sanity: the dump really does contain what the adapter emits, so a clean
	// result below is evidence rather than an empty string.
	for _, marker := range []string{"gemma4:31b", "developer__shell", `"cost_source"`} {
		if !strings.Contains(dump, marker) {
			t.Fatalf("observation dump is missing %q; the canary scan would pass vacuously", marker)
		}
	}
	if strings.Contains(dump, "SCRUB-CANARY-") {
		for _, line := range strings.Split(dump, "\n") {
			if strings.Contains(line, "SCRUB-CANARY-") {
				t.Errorf("content escaped into an emitted field: %s", line)
			}
		}
	}
}

// TestUsageComesOnlyFromTheLedger: goose records per-message usage in
// messages.metadata_json as well. Counting both surfaces would double every
// turn, so a database whose ledger is empty must report no usage at all — even
// while its messages still yield tool calls.
func TestUsageComesOnlyFromTheLedger(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir)
	writeRows(t, path, `DELETE FROM usage_ledger`)

	obs := collect(t, adapter.Source{Tool: model.ToolGoose, Class: model.EventLevel, Path: path})
	if len(obs.Events) != 0 {
		t.Errorf("empty ledger produced %d events: metadata_json was read as a usage surface", len(obs.Events))
	}
	if len(obs.Activity) == 0 {
		t.Error("activity vanished with the ledger; the two streams are independent")
	}
}

// TestActivityWatermarkIsSeparate: activity advances on the messages table, not
// on the ledger's rowid, so a message written after the last ledger row is still
// collected exactly once.
func TestActivityWatermarkIsSeparate(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir)
	src := adapter.Source{Tool: model.ToolGoose, Class: model.EventLevel, Path: path}
	a := New().(adapter.Incremental)

	first, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Checkpoint == nil {
		t.Fatal("no checkpoint")
	}
	if !strings.Contains(first.Checkpoint.State, `"messages":12`) {
		t.Errorf("checkpoint state = %s, want a messages watermark of 12", first.Checkpoint.State)
	}

	writeRows(t, path, `INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp)
		VALUES (30, 'chatcmpl-931', '20260816_3', 'assistant',
		'[{"type":"toolRequest","id":"call_z","toolCall":{"status":"success","value":{"name":"analyze","arguments":{"path":"SCRUB-CANARY-99"}}}}]',
		1786860600)`)

	second, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Events) != 0 {
		t.Errorf("a new message produced %d usage events", len(second.Events))
	}
	if len(second.Activity) != 1 {
		t.Fatalf("want exactly the new call, got %d", len(second.Activity))
	}
	got := second.Activity[0]
	if got.Name != "analyze" || got.DedupKey != "goose|call|20260816_3|30|0" {
		t.Errorf("new call = (%q, %q)", got.Name, got.DedupKey)
	}
	if dump := fmt.Sprintf("%+v", second); strings.Contains(dump, "SCRUB-CANARY-") {
		t.Errorf("tool arguments escaped: %s", dump)
	}
}

// TestUndatedMessagesProduceNoCalls: activity_events stores event_time_unix NOT
// NULL and is append-only, so a zero time.Time would land as year 1 in a table
// that has no UPDATE and no DELETE to take it back out — a row inside every
// all-time window, forever. An undated message is refused exactly as an undated
// ledger row is, and it costs the pass nothing else.
func TestUndatedMessagesProduceNoCalls(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir)
	writeRows(t, path, `INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp)
		VALUES (60, 'chatcmpl-undated', '20260816_3', 'assistant',
		'[{"type":"toolRequest","id":"call_u","toolCall":{"status":"success","value":{"name":"shell"}}}]', 0)`)

	obs := collect(t, adapter.Source{Tool: model.ToolGoose, Class: model.EventLevel, Path: path})
	for _, a := range obs.Activity {
		if a.EventTime.IsZero() || a.EventTime.Year() < 2000 {
			t.Errorf("undated call emitted with EventTime %v (key %s)", a.EventTime, a.DedupKey)
		}
	}
	if len(obs.Activity) != 3 {
		t.Errorf("want the fixture's 3 dated calls, got %d", len(obs.Activity))
	}
	if len(obs.Events) == 0 {
		t.Error("an undated message cost the source its usage events")
	}
}

// TestCallNameComposition covers the name rule directly: goose splits
// "<extension>__<tool>" across the name and `_meta.goose_extension`, sometimes
// writing the whole thing in the name and sometimes not.
func TestCallNameComposition(t *testing.T) {
	cases := []struct {
		block contentBlock
		want  string
		ok    bool
	}{
		{block: block("toolRequest", "shell", "developer"), want: "developer__shell", ok: true},
		{block: block("toolRequest", "developer__shell", "developer"), want: "developer__shell", ok: true},
		{block: block("toolRequest", "shell", ""), want: "shell", ok: true},
		{block: block("frontendToolRequest", "browser", "web"), want: "web__browser", ok: true},
		{block: block("toolResponse", "shell", "developer")},
		{block: block("text", "", "")},
		{block: contentBlock{Type: "toolRequest"}}, // {"status":"error"}: no name
	}
	for i, c := range cases {
		got, ok := callName(c.block)
		if ok != c.ok || got != c.want {
			t.Errorf("case %d = (%q, %v), want (%q, %v)", i, got, ok, c.want, c.ok)
		}
	}
}

func block(kind, name, ext string) contentBlock {
	b := contentBlock{Type: kind}
	if name != "" {
		b.ToolCall = &toolCall{Status: "success"}
		b.ToolCall.Value = &struct {
			Name string `json:"name"`
		}{Name: name}
	}
	if ext != "" {
		b.Meta = &blockMeta{Extension: ext}
	}
	return b
}

// TestMalformedContentNeverFailsTheSource: a content_json that is not a block
// array costs its calls, never the cycle's usage events.
func TestMalformedContentNeverFailsTheSource(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir)
	writeRows(t, path, `INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp)
		VALUES (40, 'chatcmpl-bad', '20260816_3', 'assistant', 'not json at all', 1786860700)`)

	obs, err := New().Collect(context.Background(), adapter.Source{Tool: model.ToolGoose, Class: model.EventLevel, Path: path})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) == 0 {
		t.Error("a malformed message row cost the source its usage events")
	}
	if obs.Checkpoint == nil {
		t.Error("a malformed message row held the checkpoint back; it is not retryable")
	}
}

// TestOpenReadOnlyRefusesWrites proves the DSN, not just the intent: the
// connection the adapter opens cannot write even when asked directly.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir)
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`DELETE FROM usage_ledger`); err == nil {
		t.Error("the read-only connection executed a DELETE")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_ledger`).Scan(&n); err != nil || n == 0 {
		t.Errorf("read-back after refused write: %d rows, err %v", n, err)
	}
}
