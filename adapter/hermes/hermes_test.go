package hermes

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// TestMain keeps an ambient Hermes installation from replacing the temporary
// databases built by this package. The env-precedence test sets HERMES_HOME
// explicitly after this reset.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(HomeEnv)
	os.Exit(m.Run())
}

// makeStateDB creates a Hermes-shaped state.db with a sessions table and runs
// the supplied seed callback to insert rows. It returns the db path.
func makeStateDB(t *testing.T, home string, seed func(*sql.DB)) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	dbPath := filepath.Join(home, dbName)
	db, err := sql.Open(driverName, "file:"+dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		model TEXT,
		billing_provider TEXT,
		started_at TEXT,
		ended_at TEXT,
		input_tokens INTEGER,
		output_tokens INTEGER,
		cache_read_tokens INTEGER,
		cache_write_tokens INTEGER,
		reasoning_tokens INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if seed != nil {
		seed(db)
	}
	return dbPath
}

func insertSession(t *testing.T, db *sql.DB, id, mdl, provider, startedAt string, in, out, cRead, cWrite, reason int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO sessions
		(id, model, billing_provider, started_at, ended_at,
		 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, mdl, provider, startedAt, "", in, out, cRead, cWrite, reason)
	if err != nil {
		t.Fatalf("insert session %s: %v", id, err)
	}
}

func TestCollectSingleSession(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".hermes")
	dbPath := makeStateDB(t, home, func(db *sql.DB) {
		// in=100 out=200 cacheRead=30 cacheWrite=40 reasoning=15
		insertSession(t, db, "sess-1", "claude-3-5-sonnet", "anthropic", "2026-05-29T10:00:00Z",
			100, 200, 30, 40, 15)
	})

	a := New()
	if a.ID() != model.ToolHermes {
		t.Fatalf("ID = %q, want %q", a.ID(), model.ToolHermes)
	}

	cfg := adapter.DiscoverConfig{Home: dir}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("Discover returned %d sources, want 1", len(srcs))
	}
	src := srcs[0]
	if src.Class != model.Aggregate {
		t.Fatalf("Source.Class = %q, want %q", src.Class, model.Aggregate)
	}
	if src.Path != dbPath {
		t.Fatalf("Source.Path = %q, want %q", src.Path, dbPath)
	}

	obs, err := a.Collect(context.Background(), src)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 0 {
		t.Fatalf("Collect produced %d events, want 0 (aggregate uses Snapshots)", len(obs.Events))
	}
	if len(obs.Snapshots) != 1 {
		t.Fatalf("Collect produced %d snapshots, want 1", len(obs.Snapshots))
	}

	s := obs.Snapshots[0]
	if s.Key != "sess-1" {
		t.Errorf("Key = %q, want sess-1", s.Key)
	}
	if s.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", s.SessionID)
	}
	if s.Tool != model.ToolHermes {
		t.Errorf("Tool = %q, want %q", s.Tool, model.ToolHermes)
	}
	if s.Model != "claude-3-5-sonnet" {
		t.Errorf("Model = %q, want claude-3-5-sonnet", s.Model)
	}
	if s.Project != metaProject {
		t.Errorf("Project = %q, want %q", s.Project, metaProject)
	}
	// Provider comes from the row's billing_provider, not a static constant.
	if s.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic (billing_provider)", s.Provider)
	}
	if s.SourcePath != dbPath {
		t.Errorf("SourcePath = %q, want %q", s.SourcePath, dbPath)
	}
	if s.InputTokens != 100 || s.OutputTokens != 200 {
		t.Errorf("in/out = %d/%d, want 100/200", s.InputTokens, s.OutputTokens)
	}
	// cache_write_tokens -> CacheCreation; cache_read_tokens -> CacheRead.
	if s.CacheCreationTokens != 40 {
		t.Errorf("CacheCreation = %d, want 40 (from cache_write_tokens)", s.CacheCreationTokens)
	}
	if s.CacheReadTokens != 30 {
		t.Errorf("CacheRead = %d, want 30 (from cache_read_tokens)", s.CacheReadTokens)
	}
	if s.ReasoningTokens != 15 {
		t.Errorf("Reasoning = %d, want 15", s.ReasoningTokens)
	}
	// Total = in + out + cacheCreation + cacheRead = 100+200+40+30 = 370.
	if s.TotalTokens != 370 {
		t.Errorf("Total = %d, want 370 (in+out+cacheC+cacheR; reasoning excluded)", s.TotalTokens)
	}
	// No ended_at in the fixture, so the record's started_at is the timestamp.
	if want := time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC); !s.ObservedTime.Equal(want) {
		t.Errorf("ObservedTime = %v, want started_at %v", s.ObservedTime, want)
	}
	if !strings.Contains(s.Raw, `"billing_provider":"anthropic"`) {
		t.Errorf("Raw missing billing_provider: %q", s.Raw)
	}
	if !strings.Contains(s.Raw, `"started_at":"2026-05-29T10:00:00Z"`) {
		t.Errorf("Raw missing started_at: %q", s.Raw)
	}
}

func TestCollectFiltersBlankModelAndCountsRows(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".hermes")
	makeStateDB(t, home, func(db *sql.DB) {
		insertSession(t, db, "ok-1", "gpt-5", "openai", "", 10, 20, 0, 0, 0)
		insertSession(t, db, "ok-2", "gpt-5", "openai", "", 5, 5, 0, 0, 0)
		// model is whitespace-only -> excluded by query's TRIM(model) != ''.
		insertSession(t, db, "blank", "   ", "openai", "", 999, 999, 999, 999, 999)
		// model is NULL -> excluded.
		_, err := db.Exec(`INSERT INTO sessions (id, model) VALUES ('nullmodel', NULL)`)
		if err != nil {
			t.Fatalf("insert null model: %v", err)
		}
	})

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: dir})
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover: err=%v sources=%d", err, len(srcs))
	}
	obs, err := a.Collect(context.Background(), srcs[0])
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Snapshots) != 2 {
		t.Fatalf("got %d snapshots, want 2 (blank/null model excluded)", len(obs.Snapshots))
	}
	keys := map[string]bool{}
	for _, s := range obs.Snapshots {
		keys[s.Key] = true
	}
	if !keys["ok-1"] || !keys["ok-2"] {
		t.Errorf("expected keys ok-1 and ok-2, got %v", keys)
	}
}

// TestCollectUsesRecordTimestamps: ObservedTime must carry the row's real
// timestamp — ended_at first, started_at as fallback, poll time only when the
// row has neither — so a downtime gap's delta lands in the session's window,
// not as a spike at the restart second. SQLite's zoneless datetime() layout
// must parse too (taken as UTC).
func TestCollectUsesRecordTimestamps(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".hermes")
	makeStateDB(t, home, func(db *sql.DB) {
		insert := func(id, startedAt, endedAt string) {
			_, err := db.Exec(`INSERT INTO sessions
				(id, model, billing_provider, started_at, ended_at,
				 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens)
				VALUES (?,?,?,?,?,1,1,0,0,0)`, id, "model-t", "prov", startedAt, endedAt)
			if err != nil {
				t.Fatalf("insert session %s: %v", id, err)
			}
		}
		insert("ended", "2026-05-29T10:00:00Z", "2026-05-29T12:34:56Z")
		insert("ended-sqlite", "", "2026-05-29 12:34:56")
		insert("live", "2026-05-29T10:00:00Z", "")
		insert("bare", "", "")
	})

	before := time.Now().UTC().Add(-time.Second)
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: dir})
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover: err=%v sources=%d", err, len(srcs))
	}
	obs, err := a.Collect(context.Background(), srcs[0])
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	byKey := map[string]model.AggregateSnapshot{}
	for _, s := range obs.Snapshots {
		byKey[s.Key] = s
	}
	if len(byKey) != 4 {
		t.Fatalf("got %d snapshots, want 4", len(byKey))
	}

	endedAt := time.Date(2026, 5, 29, 12, 34, 56, 0, time.UTC)
	if got := byKey["ended"].ObservedTime; !got.Equal(endedAt) {
		t.Errorf("ended ObservedTime = %v, want ended_at %v", got, endedAt)
	}
	if got := byKey["ended-sqlite"].ObservedTime; !got.Equal(endedAt) {
		t.Errorf("ended-sqlite ObservedTime = %v, want ended_at %v (SQLite layout, UTC)", got, endedAt)
	}
	startedAt := time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC)
	if got := byKey["live"].ObservedTime; !got.Equal(startedAt) {
		t.Errorf("live ObservedTime = %v, want started_at %v", got, startedAt)
	}
	if got := byKey["bare"].ObservedTime; got.Before(before) {
		t.Errorf("bare ObservedTime = %v, want poll-time fallback >= %v", got, before)
	}
}

func TestDiscoverHonorsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	envHome := filepath.Join(dir, "custom-hermes")
	dbPath := makeStateDB(t, envHome, func(db *sql.DB) {
		insertSession(t, db, "e-1", "model-x", "prov", "", 1, 1, 0, 0, 0)
	})

	t.Setenv(HomeEnv, envHome)
	a := New()
	// Home points elsewhere; HERMES_HOME must win.
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: filepath.Join(dir, "ignored")})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 || srcs[0].Path != dbPath {
		t.Fatalf("env override not honored: %+v", srcs)
	}
}

func TestDiscoverNoDBReturnsEmpty(t *testing.T) {
	dir := t.TempDir() // no .hermes/state.db
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: dir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("got %d sources, want 0 when no state.db", len(srcs))
	}
}

func TestCollectReadOnlyDoesNotCreateDB(t *testing.T) {
	// Collect against a non-existent path must NOT create it (mode=ro).
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope", "state.db")
	a := New()
	src := adapter.Source{Tool: model.ToolHermes, Class: model.Aggregate, Path: missing}
	if _, err := a.Collect(context.Background(), src); err == nil {
		t.Fatalf("Collect on missing db should error under mode=ro")
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatalf("read-only Collect created the db file at %s", missing)
	}
}

// TestCollectWALModeLiveDB: hermes keeps state.db open in WAL mode while we
// poll. The read-only open must work against the live WAL file — immutable=1
// gives documented wrong results on a concurrently-written DB and was dropped.
func TestCollectWALModeLiveDB(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".hermes")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	dbPath := filepath.Join(home, dbName)
	writer, err := sql.Open(driverName, "file:"+dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, model TEXT, billing_provider TEXT,
		started_at TEXT, ended_at TEXT,
		input_tokens INTEGER, output_tokens INTEGER,
		cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	insertSession(t, writer, "live-1", "model-y", "prov", "", 7, 3, 0, 0, 0)

	a := New()
	src := adapter.Source{Tool: model.ToolHermes, Class: model.Aggregate, Path: dbPath}
	obs, err := a.Collect(context.Background(), src)
	if err != nil {
		t.Fatalf("Collect against live WAL db: %v", err)
	}
	if len(obs.Snapshots) != 1 || obs.Snapshots[0].Key != "live-1" {
		t.Fatalf("snapshots = %+v, want one for live-1", obs.Snapshots)
	}
}

// TestIncrementalGates covers both checkpoint levels: an untouched database
// is skipped without opening; when it changes, only sessions whose row
// content changed are re-emitted (rows grow in place, so a rowid watermark
// cannot work here).
func TestIncrementalGates(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".hermes")
	dbPath := makeStateDB(t, home, func(db *sql.DB) {
		insertSession(t, db, "A", "claude-opus", "anthropic", "2026-05-29T10:00:00Z", 100, 200, 0, 0, 0)
		insertSession(t, db, "B", "claude-opus", "anthropic", "2026-05-29T10:00:00Z", 10, 10, 0, 0, 0)
	})

	a := Adapter{}
	src := adapter.Source{Tool: model.ToolHermes, Class: model.Aggregate, Path: dbPath}

	obs1, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(obs1.Snapshots) != 2 || obs1.Checkpoint == nil {
		t.Fatalf("full read: err=%v snaps=%d cp=%v", err, len(obs1.Snapshots), obs1.Checkpoint)
	}

	// Level 1: untouched db -> no open, no snapshots, no new checkpoint.
	obs2, err := a.CollectIncremental(context.Background(), src, obs1.Checkpoint)
	if err != nil || len(obs2.Snapshots) != 0 || obs2.Checkpoint != nil {
		t.Fatalf("unchanged skip: err=%v snaps=%d cp=%+v", err, len(obs2.Snapshots), obs2.Checkpoint)
	}

	// Level 2: grow only session A -> only A re-emits.
	writer, err := sql.Open(driverName, "file:"+dbPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := writer.Exec(`UPDATE sessions SET input_tokens=150 WHERE id='A'`); err != nil {
		t.Fatalf("grow A: %v", err)
	}
	writer.Close()

	obs3, err := a.CollectIncremental(context.Background(), src, obs1.Checkpoint)
	if err != nil {
		t.Fatalf("changed read: %v", err)
	}
	if len(obs3.Snapshots) != 1 || obs3.Snapshots[0].Key != "A" {
		t.Fatalf("changed read snapshots = %+v want only session A", obs3.Snapshots)
	}
	if obs3.Snapshots[0].InputTokens != 150 {
		t.Fatalf("session A input = %d want 150", obs3.Snapshots[0].InputTokens)
	}
	if obs3.Checkpoint == nil {
		t.Fatalf("changed read returned no checkpoint")
	}

	// The refreshed checkpoint closes the gate again.
	obs4, err := a.CollectIncremental(context.Background(), src, obs3.Checkpoint)
	if err != nil || len(obs4.Snapshots) != 0 {
		t.Fatalf("re-skip: err=%v snaps=%d", err, len(obs4.Snapshots))
	}
}
