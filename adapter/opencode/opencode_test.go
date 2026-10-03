package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// writeDB creates a temp opencode.db with a `message(id, session_id, data)`
// table and inserts the given rows. The DB is created with a normal (writable)
// connection in the test; the adapter only ever opens it read-only.
func writeDB(t *testing.T, dir string, rows [][3]string) string {
	t.Helper()
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE message (id TEXT, session_id TEXT, data TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO message (id, session_id, data) VALUES (?, ?, ?)`, r[0], r[1], r[2]); err != nil {
			t.Fatalf("insert row: %v", err)
		}
	}
	return path
}

func discover(t *testing.T, dir string) []adapter.Source {
	t.Helper()
	// Ensure no ambient env override leaks into discovery.
	t.Setenv(DataDirEnv, dir)
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return srcs
}

func sourceByKind(srcs []adapter.Source, kind string) (adapter.Source, bool) {
	for _, s := range srcs {
		if s.Meta != nil && s.Meta["kind"] == kind {
			return s, true
		}
	}
	return adapter.Source{}, false
}

func TestDBMappingAndProject(t *testing.T) {
	dir := t.TempDir()
	// cache.write -> CacheCreation, cache.read -> CacheRead, additive total.
	// Reasoning is additive too, matching every observed opencode row:
	// input(100)+output(50)+reasoning(7)+write(20)+read(30)=207 == total, so
	// nothing is redistributed.
	data := `{
		"id":"msg_1","sessionID":"sess_json","providerID":"anthropic","modelID":"claude-sonnet-4",
		"time":{"created":1730000000000},
		"tokens":{"input":100,"output":50,"reasoning":7,"cache":{"read":30,"write":20},"total":207},
		"cost":0.01,"path":{"cwd":"/home/dev/projects/myapp","root":"/home/dev"}
	}`
	writeDB(t, dir, [][3]string{{"msg_1", "sess_db", data}})

	srcs := discover(t, dir)
	dbSrc, ok := sourceByKind(srcs, kindDB)
	if !ok {
		t.Fatalf("no db source discovered; got %d sources", len(srcs))
	}

	obs, err := New().Collect(context.Background(), dbSrc)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(obs.Events))
	}
	ev := obs.Events[0]

	if ev.Tool != model.ToolOpenCode {
		t.Errorf("Tool = %q, want %q", ev.Tool, model.ToolOpenCode)
	}
	if ev.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q, want claude-sonnet-4", ev.Model)
	}
	// DB session_id column wins over the JSON sessionID.
	if ev.SessionID != "sess_db" {
		t.Errorf("SessionID = %q, want sess_db (DB column wins)", ev.SessionID)
	}
	// project = path.cwd (real path, not a constant).
	if ev.Project != "/home/dev/projects/myapp" {
		t.Errorf("Project = %q, want /home/dev/projects/myapp", ev.Project)
	}
	if ev.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", ev.InputTokens)
	}
	if ev.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", ev.OutputTokens)
	}
	if ev.CacheCreationTokens != 20 {
		t.Errorf("CacheCreationTokens = %d, want 20 (cache.write)", ev.CacheCreationTokens)
	}
	if ev.CacheReadTokens != 30 {
		t.Errorf("CacheReadTokens = %d, want 30 (cache.read)", ev.CacheReadTokens)
	}
	if ev.ReasoningTokens != 7 {
		t.Errorf("ReasoningTokens = %d, want 7", ev.ReasoningTokens)
	}
	if ev.TotalTokens != 207 {
		t.Errorf("TotalTokens = %d, want 207", ev.TotalTokens)
	}
	if ev.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic (providerID)", ev.Provider)
	}
	if ev.DedupKey != "opencode|msg_1" {
		t.Errorf("DedupKey = %q, want opencode|msg_1", ev.DedupKey)
	}
	if ev.MessageID != "msg_1" {
		t.Errorf("MessageID = %q, want msg_1", ev.MessageID)
	}
	wantTS := time.UnixMilli(1730000000000).UTC()
	if !ev.EventTime.Equal(wantTS) {
		t.Errorf("EventTime = %v, want %v", ev.EventTime, wantTS)
	}
	if ev.Kind != model.KindUsage {
		t.Errorf("Kind = %q, want %q", ev.Kind, model.KindUsage)
	}
}

func TestTotalFallbackFillsOutput(t *testing.T) {
	dir := t.TempDir()
	// output absent, total exceeds known components -> fallback fills output.
	// known = input(100)+output(0)+write(0)+read(0)+extra(0)=100; total=180
	// -> output=80, stored total stays 180.
	data := `{
		"id":"m2","sessionID":"s2","providerID":"openai","modelID":"gpt-5",
		"time":{"created":1730000001000},
		"tokens":{"input":100,"output":0,"reasoning":0,"cache":{"read":0,"write":0},"total":180},
		"path":{"cwd":"/work"}
	}`
	writeDB(t, dir, [][3]string{{"m2", "s2", data}})

	srcs := discover(t, dir)
	dbSrc, _ := sourceByKind(srcs, kindDB)
	obs, err := New().Collect(context.Background(), dbSrc)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(obs.Events))
	}
	ev := obs.Events[0]
	if ev.OutputTokens != 80 {
		t.Errorf("OutputTokens = %d, want 80 (fallback fill)", ev.OutputTokens)
	}
	if ev.TotalTokens != 180 {
		t.Errorf("TotalTokens = %d, want 180", ev.TotalTokens)
	}
	// Components must sum to the authoritative total.
	if got := ev.InputTokens + ev.OutputTokens + ev.CacheCreationTokens + ev.CacheReadTokens; got != ev.TotalTokens {
		t.Errorf("component sum = %d, want %d (== TotalTokens)", got, ev.TotalTokens)
	}
}

// TestReasoningIsAdditive pins the accounting rule verified against real
// opencode data: total = input + output + reasoning + cache.read + cache.write.
// The row with output == 0 and reasoning > 0 is the regression guard — with
// reasoning left out of the fallback's known sum the gap fill would copy the
// reasoning count into OutputTokens while ReasoningTokens still carried it,
// billing those tokens twice.
func TestReasoningIsAdditive(t *testing.T) {
	dir := t.TempDir()
	rows := [][3]string{
		// output == 0, reasoning > 0, total fully explained: no gap to fill.
		{"r_gap", "s", `{"id":"r_gap","time":{"created":1730000000000},"sessionID":"s","providerID":"anthropic","modelID":"claude-sonnet-4",
			"tokens":{"input":1000,"output":0,"reasoning":400,"cache":{"read":200,"write":100},"total":1700},
			"path":{"cwd":"/w"}}`},
		// reasoning > output: impossible if reasoning were a subset of output.
		{"r_big", "s", `{"id":"r_big","time":{"created":1730000000000},"sessionID":"s","providerID":"openai","modelID":"gpt-5",
			"tokens":{"input":50,"output":10,"reasoning":900,"cache":{"read":0,"write":0},"total":960},
			"path":{"cwd":"/w"}}`},
		// output == 0, reasoning > 0 AND a genuine remainder: the fill takes the
		// remainder only (200 - 100 - 40), never the reasoning count.
		{"r_fill", "s", `{"id":"r_fill","time":{"created":1730000000000},"sessionID":"s","providerID":"openai","modelID":"gpt-5",
			"tokens":{"input":100,"output":0,"reasoning":40,"cache":{"read":0,"write":0},"total":200},
			"path":{"cwd":"/w"}}`},
	}
	writeDB(t, dir, rows)

	srcs := discover(t, dir)
	dbSrc, _ := sourceByKind(srcs, kindDB)
	obs, err := New().Collect(context.Background(), dbSrc)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 3 {
		t.Fatalf("want 3 events, got %d", len(obs.Events))
	}

	byKey := map[string]model.UsageEvent{}
	for _, ev := range obs.Events {
		byKey[ev.DedupKey] = ev
	}

	cases := []struct {
		key                                     string
		input, output, reasoning, total, cacheR int64
		cacheC                                  int64
	}{
		{"opencode|r_gap", 1000, 0, 400, 1700, 200, 100},
		{"opencode|r_big", 50, 10, 900, 960, 0, 0},
		{"opencode|r_fill", 100, 60, 40, 200, 0, 0},
	}
	for _, c := range cases {
		ev, ok := byKey[c.key]
		if !ok {
			t.Fatalf("missing event %s", c.key)
		}
		if ev.InputTokens != c.input || ev.OutputTokens != c.output ||
			ev.ReasoningTokens != c.reasoning || ev.TotalTokens != c.total ||
			ev.CacheReadTokens != c.cacheR || ev.CacheCreationTokens != c.cacheC {
			t.Errorf("%s: in=%d out=%d reason=%d cacheR=%d cacheC=%d total=%d; want %d/%d/%d/%d/%d/%d",
				c.key, ev.InputTokens, ev.OutputTokens, ev.ReasoningTokens,
				ev.CacheReadTokens, ev.CacheCreationTokens, ev.TotalTokens,
				c.input, c.output, c.reasoning, c.cacheR, c.cacheC, c.total)
		}
		// Additive identity: reasoning is part of the authoritative total.
		sum := ev.InputTokens + ev.OutputTokens + ev.ReasoningTokens +
			ev.CacheReadTokens + ev.CacheCreationTokens
		if sum != ev.TotalTokens {
			t.Errorf("%s: additive component sum = %d, want %d", c.key, sum, ev.TotalTokens)
		}
	}
}

// TestReasoningModeIsAdditive keeps the adapter's accounting and the mode the
// pricing engine reads from drifting apart.
func TestReasoningModeIsAdditive(t *testing.T) {
	if got := model.ReasoningModeFor(model.ToolOpenCode); got != model.ReasoningAdditive {
		t.Errorf("ReasoningModeFor(opencode) = %q, want %q", got, model.ReasoningAdditive)
	}
}

func TestDropsEmptyModelAndAllZero(t *testing.T) {
	dir := t.TempDir()
	rows := [][3]string{
		// empty modelID -> dropped
		{"a", "s", `{"id":"a","sessionID":"s","modelID":"","tokens":{"input":10,"output":5,"total":15}}`},
		// missing modelID -> dropped
		{"b", "s", `{"id":"b","sessionID":"s","tokens":{"input":10,"output":5,"total":15}}`},
		// all-zero tokens with a model -> dropped
		{"c", "s", `{"id":"c","sessionID":"s","modelID":"gpt-5","tokens":{"input":0,"output":0,"total":0,"cache":{"read":0,"write":0},"reasoning":0}}`},
		// malformed JSON -> dropped, must not fail the source
		{"d", "s", `{not json`},
		// valid -> kept
		{"e", "s", `{"id":"e","time":{"created":1730000000000},"sessionID":"s","modelID":"gpt-5","tokens":{"input":1,"output":1,"total":2},"path":{"cwd":"/x"}}`},
	}
	writeDB(t, dir, rows)

	srcs := discover(t, dir)
	dbSrc, _ := sourceByKind(srcs, kindDB)
	obs, err := New().Collect(context.Background(), dbSrc)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 kept event, got %d", len(obs.Events))
	}
	if obs.Events[0].DedupKey != "opencode|e" {
		t.Errorf("kept wrong event: %q", obs.Events[0].DedupKey)
	}
}

func TestJSONTreeAndDBSameDedupKey(t *testing.T) {
	dir := t.TempDir()
	const sharedData = `{
		"id":"shared","sessionID":"sx","providerID":"anthropic","modelID":"claude-sonnet-4",
		"time":{"created":1730000002000},
		"tokens":{"input":10,"output":20,"cache":{"read":1,"write":2},"reasoning":0,"total":33},
		"path":{"cwd":"/repo/proj"}
	}`
	// DB row.
	writeDB(t, dir, [][3]string{{"shared", "sx", sharedData}})
	// JSON copy under storage/message/<session>/<message>.json
	msgDir := filepath.Join(dir, storageDirName, messageDirName, "sx")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(msgDir, "shared.json"), []byte(sharedData), 0o644); err != nil {
		t.Fatalf("write json: %v", err)
	}

	srcs := discover(t, dir)
	dbSrc, okDB := sourceByKind(srcs, kindDB)
	jsonSrc, okJSON := sourceByKind(srcs, kindJSON)
	if !okDB || !okJSON {
		t.Fatalf("want both db and json sources; got %d", len(srcs))
	}
	// DB must be discovered before JSON so it wins on INSERT OR IGNORE.
	if srcs[0].Meta["kind"] != kindDB {
		t.Errorf("first source kind = %q, want db (DB wins ordering)", srcs[0].Meta["kind"])
	}

	a := New()
	dbObs, err := a.Collect(context.Background(), dbSrc)
	if err != nil {
		t.Fatalf("Collect db: %v", err)
	}
	jsonObs, err := a.Collect(context.Background(), jsonSrc)
	if err != nil {
		t.Fatalf("Collect json: %v", err)
	}
	if len(dbObs.Events) != 1 || len(jsonObs.Events) != 1 {
		t.Fatalf("want 1 event each; db=%d json=%d", len(dbObs.Events), len(jsonObs.Events))
	}
	dk, jk := dbObs.Events[0].DedupKey, jsonObs.Events[0].DedupKey
	if dk != jk {
		t.Errorf("dedup keys differ: db=%q json=%q (must collapse)", dk, jk)
	}
	if dk != "opencode|shared" {
		t.Errorf("DedupKey = %q, want opencode|shared", dk)
	}
	// JSON path: no DB columns, so id/session come from JSON itself.
	if jsonObs.Events[0].SessionID != "sx" {
		t.Errorf("json SessionID = %q, want sx", jsonObs.Events[0].SessionID)
	}
	if jsonObs.Events[0].Project != "/repo/proj" {
		t.Errorf("json Project = %q, want /repo/proj", jsonObs.Events[0].Project)
	}
}

func TestProjectFallbackWhenNoCwd(t *testing.T) {
	dir := t.TempDir()
	data := `{"id":"nz","time":{"created":1730000000000},"sessionID":"s","modelID":"gpt-5","tokens":{"input":5,"output":5,"total":10}}`
	writeDB(t, dir, [][3]string{{"nz", "s", data}})

	srcs := discover(t, dir)
	dbSrc, _ := sourceByKind(srcs, kindDB)
	obs, err := New().Collect(context.Background(), dbSrc)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(obs.Events))
	}
	if obs.Events[0].Project != "opencode" {
		t.Errorf("Project = %q, want opencode (no cwd fallback)", obs.Events[0].Project)
	}
}

func TestFindDBPrefixVariant(t *testing.T) {
	dir := t.TempDir()
	// No opencode.db; only opencode-<token>.db present.
	path := filepath.Join(dir, "opencode-abc123.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE message (id TEXT, session_id TEXT, data TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _ = db.Exec(`INSERT INTO message VALUES (?,?,?)`, "pfx",
		"s", `{"id":"pfx","time":{"created":1730000000000},"sessionID":"s","modelID":"gpt-5","tokens":{"input":1,"output":1,"total":2}}`)
	db.Close()

	got := findDB(dir)
	if got != path {
		t.Fatalf("findDB = %q, want %q", got, path)
	}

	srcs := discover(t, dir)
	dbSrc, ok := sourceByKind(srcs, kindDB)
	if !ok {
		t.Fatalf("prefixed db not discovered")
	}
	obs, err := New().Collect(context.Background(), dbSrc)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 1 || obs.Events[0].DedupKey != "opencode|pfx" {
		t.Fatalf("unexpected events: %+v", obs.Events)
	}
}

func TestDiscoverNoDirIsClean(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	srcs := discover(t, dir)
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources for missing dir, got %d", len(srcs))
	}
}

// TestReadsUncheckpointedWAL is the regression for issue #18. opencode keeps
// its database open and writes it live under journal_mode=wal, so committed
// rows can sit in the -wal file indefinitely (74.65 MiB of them on the machine
// that motivated the fix). immutable=1 makes SQLite ignore the WAL and read the
// main file as of its last checkpoint, so those rows silently vanish; the
// mode=ro + query_only DSN reads them. The writer stays open on purpose:
// closing it would checkpoint the WAL and the assertion would hold either way.
func TestReadsUncheckpointedWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")

	writer, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(wal)")
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
	if _, err := writer.Exec(`CREATE TABLE message (id TEXT, session_id TEXT, data TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	data := `{"id":"wal1","time":{"created":1730000000000},"sessionID":"s","modelID":"gpt-5","tokens":{"input":5,"output":6,"total":11}}`
	if _, err := writer.Exec(`INSERT INTO message VALUES ('wal1','s',?)`, data); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The row must still live in the WAL, not the main file, or this test
	// proves nothing.
	fi, err := os.Stat(path + "-wal")
	if err != nil || fi.Size() == 0 {
		t.Fatalf("no un-checkpointed WAL content (stat err %v); test cannot detect a stale read", err)
	}

	src := adapter.Source{
		Tool: model.ToolOpenCode, Class: model.EventLevel,
		Path: path, Meta: map[string]string{"kind": kindDB},
	}
	obs, err := Adapter{}.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("CollectIncremental: %v", err)
	}
	if len(obs.Events) != 1 {
		t.Fatalf("read %d events, want 1: rows committed to the WAL are invisible", len(obs.Events))
	}
	if obs.Events[0].DedupKey != "opencode|wal1" {
		t.Fatalf("DedupKey = %q, want opencode|wal1", obs.Events[0].DedupKey)
	}
	if obs.Events[0].TotalTokens != 11 {
		t.Fatalf("TotalTokens = %d, want 11", obs.Events[0].TotalTokens)
	}
}

// TestIncrementalWatermark: only message rows above the stored rowid watermark
// are read on re-collect; an unchanged table yields no events and no new
// checkpoint.
func TestIncrementalWatermark(t *testing.T) {
	dir := t.TempDir()
	data1 := `{"id":"w1","time":{"created":1730000000000},"sessionID":"s","modelID":"gpt-5","tokens":{"input":1,"output":1,"total":2}}`
	dbPath := writeDB(t, dir, [][3]string{{"w1", "s", data1}})

	a := Adapter{}
	src := adapter.Source{
		Tool: model.ToolOpenCode, Class: model.EventLevel,
		Path: dbPath, Meta: map[string]string{"kind": kindDB},
	}

	obs1, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(obs1.Events) != 1 || obs1.Checkpoint == nil {
		t.Fatalf("full read: err=%v events=%d cp=%v", err, len(obs1.Events), obs1.Checkpoint)
	}
	if obs1.Checkpoint.Watermark != 1 {
		t.Fatalf("watermark = %d want 1", obs1.Checkpoint.Watermark)
	}

	// Unchanged: the watermark query returns nothing.
	obs2, err := a.CollectIncremental(context.Background(), src, obs1.Checkpoint)
	if err != nil || len(obs2.Events) != 0 || obs2.Checkpoint != nil {
		t.Fatalf("unchanged: err=%v events=%d cp=%+v", err, len(obs2.Events), obs2.Checkpoint)
	}

	// New row: only it is read.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	data2 := `{"id":"w2","time":{"created":1730000000000},"sessionID":"s","modelID":"gpt-5","tokens":{"input":3,"output":4,"total":7}}`
	if _, err := db.Exec(`INSERT INTO message VALUES ('w2','s',?)`, data2); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	obs3, err := a.CollectIncremental(context.Background(), src, obs1.Checkpoint)
	if err != nil || len(obs3.Events) != 1 {
		t.Fatalf("delta read: err=%v events=%d want 1", err, len(obs3.Events))
	}
	if obs3.Events[0].DedupKey != "opencode|w2" {
		t.Fatalf("delta event = %q want opencode|w2", obs3.Events[0].DedupKey)
	}
	if obs3.Checkpoint == nil || obs3.Checkpoint.Watermark != 2 {
		t.Fatalf("advanced checkpoint = %+v want watermark 2", obs3.Checkpoint)
	}
}
