package opencode

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// opencodeSecret is planted in every tool part's input and output. No activity
// row may carry it.
const opencodeSecret = "cat ~/.ssh/id_ed25519 && rm -rf /home/dev"

// msgData builds a message payload with the token shape the adapter reads.
func msgData(id, sessionID string, total int64) string {
	return `{"id":"` + id + `","sessionID":"` + sessionID + `","providerID":"anthropic",` +
		`"modelID":"claude-sonnet-4","time":{"created":1730000000000},` +
		`"tokens":{"input":100,"output":50,"reasoning":0,"cache":{"read":0,"write":0},"total":` +
		itoa(total) + `},"path":{"cwd":"/home/dev/projects/myapp","root":"/home/dev"}}`
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// toolPartData builds a `part` payload of type "tool", complete with the input
// and output blobs that must never escape into an activity row.
func toolPartData(tool string) string {
	return `{"type":"tool","tool":"` + tool + `","callID":"call_x",` +
		`"state":{"status":"completed","input":{"command":"` + opencodeSecret + `"},` +
		`"output":"` + opencodeSecret + `","time":{"start":1,"end":2}}}`
}

// writeDBWithParts creates a message table and a part table and fills both.
// messages is (id, session_id, data); parts is (id, message_id, data).
func writeDBWithParts(t *testing.T, dir string, messages [][3]string, parts [][3]string) string {
	t.Helper()
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE message (id TEXT, session_id TEXT, data TEXT)`); err != nil {
		t.Fatalf("create message: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE part (id TEXT, message_id TEXT, session_id TEXT,
		time_created INTEGER, time_updated INTEGER, data TEXT)`); err != nil {
		t.Fatalf("create part: %v", err)
	}
	for _, m := range messages {
		if _, err := db.Exec(`INSERT INTO message (id, session_id, data) VALUES (?,?,?)`,
			m[0], m[1], m[2]); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	for _, p := range parts {
		if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
			VALUES (?,?,?,?,?,?)`, p[0], p[1], "sess_db", 1730000000000, 1730000000000, p[2]); err != nil {
			t.Fatalf("insert part: %v", err)
		}
	}
	return path
}

func collectDBObs(t *testing.T, dir string) adapter.Observation {
	t.Helper()
	srcs := discover(t, dir)
	dbSrc, ok := sourceByKind(srcs, kindDB)
	if !ok {
		t.Fatalf("no db source discovered; got %d sources", len(srcs))
	}
	obs, err := New().Collect(context.Background(), dbSrc)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return obs
}

// TestOpencodeToolPartsJoinTheirMessage covers the join: opencode's part rows
// carry the message id that IS the usage event's identity, so attribution is
// exact rather than positional.
func TestOpencodeToolPartsJoinTheirMessage(t *testing.T) {
	dir := t.TempDir()
	writeDBWithParts(t,
		dir,
		[][3]string{{"msg_1", "sess_db", msgData("msg_1", "sess_json", 150)}},
		[][3]string{
			{"prt_1", "msg_1", toolPartData("bash")},
			{"prt_2", "msg_1", toolPartData("read")},
			// A non-tool part must not become a call.
			{"prt_3", "msg_1", `{"type":"text","text":"` + opencodeSecret + `"}`},
		})

	obs := collectDBObs(t, dir)
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 usage event, got %d", len(obs.Events))
	}
	if len(obs.Activity) != 2 {
		t.Fatalf("want 2 tool rows (text is not a call), got %d: %+v", len(obs.Activity), obs.Activity)
	}

	names := map[string]bool{}
	for _, a := range obs.Activity {
		names[a.Name] = true
		if a.UsageDedupKey != obs.Events[0].DedupKey {
			t.Errorf("%s names usage key %q, want %q", a.Name, a.UsageDedupKey, obs.Events[0].DedupKey)
		}
		if a.CallsInTurn != 2 {
			t.Errorf("%s: calls_in_turn = %d, want 2 — the divisor must equal the rows emitted",
				a.Name, a.CallsInTurn)
		}
		if !a.EventTime.Equal(obs.Events[0].EventTime) {
			t.Errorf("%s: time %s != usage time %s", a.Name, a.EventTime, obs.Events[0].EventTime)
		}
		if a.Kind != model.ActivityTool {
			t.Errorf("%s: kind = %s, want tool", a.Name, a.Kind)
		}
	}
	if !names["bash"] || !names["read"] {
		t.Errorf("tool names = %v, want bash and read", names)
	}
}

// TestOpencodeActivityCarriesNoToolIO is the privacy invariant: `part.data`
// embeds the full tool input AND output, and none of it may reach a row.
func TestOpencodeActivityCarriesNoToolIO(t *testing.T) {
	dir := t.TempDir()
	writeDBWithParts(t,
		dir,
		[][3]string{{"msg_1", "sess_db", msgData("msg_1", "sess_json", 150)}},
		[][3]string{{"prt_1", "msg_1", toolPartData("bash")}})

	obs := collectDBObs(t, dir)
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	a := obs.Activity[0]
	for field, v := range map[string]string{
		"Name": a.Name, "SessionID": a.SessionID, "Project": a.Project, "Model": a.Model,
		"DedupKey": a.DedupKey, "UsageDedupKey": a.UsageDedupKey, "SourcePath": a.SourcePath,
	} {
		if strings.Contains(v, "id_ed25519") || strings.Contains(v, "rm -rf") {
			t.Fatalf("activity field %s leaked tool I/O: %q", field, v)
		}
	}
}

// TestOpencodeActivityRespectsTheWatermark: a message already consumed must not
// have its calls re-emitted, and a new message's calls must be picked up.
func TestOpencodeActivityRespectsTheWatermark(t *testing.T) {
	dir := t.TempDir()
	writeDBWithParts(t,
		dir,
		[][3]string{{"msg_1", "sess_db", msgData("msg_1", "sess_json", 150)}},
		[][3]string{{"prt_1", "msg_1", toolPartData("bash")}})

	srcs := discover(t, dir)
	dbSrc, ok := sourceByKind(srcs, kindDB)
	if !ok {
		t.Fatal("no db source")
	}
	a := New().(adapter.Incremental)

	first, err := a.CollectIncremental(context.Background(), dbSrc, nil)
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if len(first.Activity) != 1 {
		t.Fatalf("first pass activity = %d, want 1", len(first.Activity))
	}
	if first.Checkpoint == nil {
		t.Fatal("first pass produced no checkpoint")
	}

	second, err := a.CollectIncremental(context.Background(), dbSrc, first.Checkpoint)
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if len(second.Activity) != 0 {
		t.Fatalf("consumed messages re-emitted %d activity rows; the watermark is not held",
			len(second.Activity))
	}
}

// TestOpencodeCallsWithoutAMessageStillCount: a message whose usage event was
// dropped (all-zero tokens) still had its tools called. The calls are kept and
// left unattributed rather than discarded.
func TestOpencodeCallsWithoutAMessageStillCount(t *testing.T) {
	dir := t.TempDir()
	zero := `{"id":"msg_z","sessionID":"sess_json","providerID":"anthropic","modelID":"claude-sonnet-4",` +
		`"time":{"created":1730000000000},` +
		`"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0},"total":0}}`
	writeDBWithParts(t,
		dir,
		[][3]string{{"msg_z", "sess_db", zero}},
		[][3]string{{"prt_1", "msg_z", toolPartData("glob")}})

	obs := collectDBObs(t, dir)
	if len(obs.Events) != 0 {
		t.Fatalf("an all-zero message produced %d usage events, want 0", len(obs.Events))
	}
	if len(obs.Activity) != 1 {
		t.Fatalf("want the call kept, got %d rows", len(obs.Activity))
	}
	if obs.Activity[0].UsageDedupKey != "" {
		t.Errorf("call claims usage key %q with no stored usage row", obs.Activity[0].UsageDedupKey)
	}
	if obs.Activity[0].EventTime.IsZero() {
		t.Error("call has no time; it could not be windowed")
	}
}

// TestOpencodeMissingPartTableIsNotFatal: an older opencode database has no
// `part` table at all, and usage collection must not care.
func TestOpencodeMissingPartTableIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir, [][3]string{{"msg_1", "sess_db", msgData("msg_1", "sess_json", 150)}})

	obs := collectDBObs(t, dir)
	if len(obs.Events) != 1 {
		t.Fatalf("want the usage event, got %d", len(obs.Events))
	}
	if len(obs.Activity) != 0 {
		t.Fatalf("want no activity without a part table, got %d", len(obs.Activity))
	}
}
