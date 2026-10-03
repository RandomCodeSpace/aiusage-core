package goose

import (
	"context"
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// buildDB materialises testdata/sessions.sql into <dir>/sessions/sessions.db.
// The fixture is loaded through a normal writable connection here; the adapter
// only ever opens the result read-only.
func buildDB(t *testing.T, dir string) string {
	t.Helper()
	return buildDBFrom(t, dir, filepath.Join("testdata", "sessions.sql"))
}

func buildDBFrom(t *testing.T, dir, sqlPath string) string {
	t.Helper()
	script, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sessDir := filepath.Join(dir, sessionsDirName)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(sessDir, dbName)
	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(string(script)); err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return path
}

// fixtureSource builds the fixture database and discovers it the way the daemon
// would, through GOOSE_PATH_ROOT (whose data directory is <root>/data).
func fixtureSource(t *testing.T) adapter.Source {
	t.Helper()
	root := t.TempDir()
	buildDB(t, filepath.Join(root, dataSubdir))
	t.Setenv(PathRootEnv, root)
	t.Setenv(DataHomeEnv, "")
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("want 1 source, got %d", len(srcs))
	}
	return srcs[0]
}

func collect(t *testing.T, src adapter.Source) adapter.Observation {
	t.Helper()
	obs, err := New().Collect(context.Background(), src)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return obs
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

// TestLedgerMappingIsExact pins every field of every event the fixture produces,
// dedup keys included.
func TestLedgerMappingIsExact(t *testing.T) {
	obs := collect(t, fixtureSource(t))

	want := []model.UsageEvent{
		{
			Model: "gemma4:31b", Provider: "ollama", SessionID: "20260816_1",
			Project: "/home/dev/projects/demo", EventTime: time.Unix(1786853116, 0).UTC(),
			InputTokens: 7447, OutputTokens: 8, TotalTokens: 7455,
			DedupKey: "goose|20260816_1|1|1786853116",
		},
		{
			Model: "gemma4:31b", Provider: "ollama", SessionID: "20260816_3",
			Project: "/home/dev/projects/demo", EventTime: time.Unix(1786854321, 0).UTC(),
			InputTokens: 7466, OutputTokens: 15, TotalTokens: 7481,
			DedupKey: "goose|20260816_3|3|1786854321",
		},
		{
			Model: "gemma4:31b", Provider: "ollama", SessionID: "20260816_3",
			Project: "/home/dev/projects/demo", EventTime: time.Unix(1786854321, 0).UTC(),
			InputTokens: 7491, OutputTokens: 12, TotalTokens: 7503,
			DedupKey: "goose|20260816_3|4|1786854321",
		},
		{
			Model: "claude-sonnet-4-5", Provider: "anthropic", SessionID: "sess_priced",
			Project: "/home/dev/projects/priced", EventTime: time.Unix(1786860000, 0).UTC(),
			InputTokens: 1500, OutputTokens: 400, CacheReadTokens: 9000,
			CacheCreationTokens: 1500, TotalTokens: 12400,
			DedupKey: "goose|sess_priced|5|1786860000",
		},
		{
			Model: "claude-sonnet-4-5", Provider: "anthropic", SessionID: "sess_priced",
			Project: "/home/dev/projects/priced", EventTime: time.Unix(1786860100, 0).UTC(),
			InputTokens: 900, OutputTokens: 120, TotalTokens: 1020,
			DedupKey: "goose|sess_priced|6|1786860100",
		},
		{
			// carried_forward: no model, and it is still an event.
			Model: "", Provider: "anthropic", SessionID: "sess_priced",
			Project: "/home/dev/projects/priced", EventTime: time.Unix(1786860200, 0).UTC(),
			InputTokens: 20, OutputTokens: 0, TotalTokens: 20,
			DedupKey: "goose|sess_priced|7|1786860200",
		},
		{
			// orphan: the session row is gone, the usage is not.
			Model: "gpt-5", Provider: "", SessionID: "gone_session",
			Project: "", EventTime: time.Unix(1786860300, 0).UTC(),
			InputTokens: 300, OutputTokens: 40, TotalTokens: 340,
			DedupKey: "goose|gone_session|8|1786860300",
		},
	}

	if len(obs.Events) != len(want) {
		var keys []string
		for _, e := range obs.Events {
			keys = append(keys, e.DedupKey)
		}
		t.Fatalf("want %d events, got %d: %v", len(want), len(obs.Events), keys)
	}
	for i, w := range want {
		got := obs.Events[i]
		if got.Tool != model.ToolGoose {
			t.Errorf("event %d Tool = %q, want %q", i, got.Tool, model.ToolGoose)
		}
		if got.Kind != model.KindUsage {
			t.Errorf("event %d Kind = %q, want usage", i, got.Kind)
		}
		if got.DedupKey != w.DedupKey {
			t.Errorf("event %d DedupKey = %q, want %q", i, got.DedupKey, w.DedupKey)
		}
		if got.Model != w.Model || got.Provider != w.Provider ||
			got.SessionID != w.SessionID || got.Project != w.Project {
			t.Errorf("event %d identity = (%q,%q,%q,%q), want (%q,%q,%q,%q)",
				i, got.Model, got.Provider, got.SessionID, got.Project,
				w.Model, w.Provider, w.SessionID, w.Project)
		}
		if !got.EventTime.Equal(w.EventTime) {
			t.Errorf("event %d EventTime = %v, want %v", i, got.EventTime, w.EventTime)
		}
		if got.InputTokens != w.InputTokens || got.OutputTokens != w.OutputTokens ||
			got.CacheReadTokens != w.CacheReadTokens || got.CacheCreationTokens != w.CacheCreationTokens ||
			got.TotalTokens != w.TotalTokens {
			t.Errorf("event %d tokens = (in %d, out %d, read %d, write %d, total %d), want (%d, %d, %d, %d, %d)",
				i, got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheCreationTokens, got.TotalTokens,
				w.InputTokens, w.OutputTokens, w.CacheReadTokens, w.CacheCreationTokens, w.TotalTokens)
		}
		if got.ReasoningTokens != 0 {
			t.Errorf("event %d ReasoningTokens = %d, want 0: usage_ledger has no reasoning column",
				i, got.ReasoningTokens)
		}
	}
}

// TestLive146FixtureExactEvent replays the usage ledger written by a bounded
// Goose 1.46.0 run in an isolated XDG data root. Human/model text, message
// identifiers, and the workspace path were sanitized; schema version 16,
// timestamps, model/provider identity, token counters, and NULL cost remain as
// written.
func TestLive146FixtureExactEvent(t *testing.T) {
	dir := t.TempDir()
	path := buildDBFrom(t, dir, filepath.Join("testdata", "live-1.46.0.sql"))
	obs := collect(t, adapter.Source{Tool: model.ToolGoose, Class: model.EventLevel, Path: path})
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(obs.Events), obs.Events)
	}
	e := obs.Events[0]
	if e.Model != "gemma4:31b" || e.Provider != "ollama" ||
		e.SessionID != "20260831_1" || e.Project != "/workspace/capture" {
		t.Errorf("identity = model %q provider %q session %q project %q",
			e.Model, e.Provider, e.SessionID, e.Project)
	}
	if !e.EventTime.Equal(time.Unix(1788189025, 0).UTC()) {
		t.Errorf("event time = %v, want %v", e.EventTime, time.Unix(1788189025, 0).UTC())
	}
	if e.InputTokens != 359 || e.OutputTokens != 2 || e.TotalTokens != 361 ||
		e.CacheReadTokens != 0 || e.CacheCreationTokens != 0 {
		t.Errorf("usage = in %d out %d read %d write %d total %d",
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheCreationTokens, e.TotalTokens)
	}
	if e.DedupKey != "goose|20260831_1|1|1788189025" {
		t.Errorf("dedup key = %q", e.DedupKey)
	}
	if _, known := e.Cost(); known || e.PriceSource != "" {
		t.Errorf("cost = %v source %q, want unpriced", e.CostMicroUSD, e.PriceSource)
	}
	if len(obs.Activity) != 0 || len(obs.TurnContexts) != 0 {
		t.Fatalf("activity/turn context = %d/%d, want 0/0", len(obs.Activity), len(obs.TurnContexts))
	}
}

// TestCacheTokensComeOutOfInput is the accounting trap goose's own source
// documents: "input_tokens is the total input including cache read/write
// tokens; the cache fields are breakdown subsets of it" (token_usage.rs). The
// pricing engine charges input, cache read and cache write as three separate
// lines, so passing goose's input through would bill every cached token twice.
func TestCacheTokensComeOutOfInput(t *testing.T) {
	obs := collect(t, fixtureSource(t))
	ev := eventByKey(t, obs.Events, "goose|sess_priced|5|1786860000")

	// The fixture row reads input 12000, cache_read 9000, cache_write 1500.
	if ev.InputTokens != 1500 {
		t.Errorf("InputTokens = %d, want 1500 (12000 - 9000 read - 1500 write)", ev.InputTokens)
	}
	if ev.CacheReadTokens != 9000 || ev.CacheCreationTokens != 1500 {
		t.Errorf("cache = (read %d, write %d), want (9000, 1500)", ev.CacheReadTokens, ev.CacheCreationTokens)
	}
	// The split reconciles exactly to goose's own total: nothing is invented and
	// nothing is lost.
	if sum := ev.ComputedTotal(); sum != ev.TotalTokens {
		t.Errorf("components sum to %d but TotalTokens = %d: the split must be lossless", sum, ev.TotalTokens)
	}
	if ev.TotalTokens != 12400 {
		t.Errorf("TotalTokens = %d, want the provider's own 12400", ev.TotalTokens)
	}
}

// TestNullCostIsUnpricedNeverFree covers the first trap: goose leaves cost and
// cost_source NULL under every provider it has no price for (measured live: an
// ollama session, both NULL). Unpriced is not $0.
func TestNullCostIsUnpricedNeverFree(t *testing.T) {
	obs := collect(t, fixtureSource(t))

	for _, key := range []string{
		"goose|20260816_1|1|1786853116",
		"goose|20260816_3|3|1786854321",
		"goose|sess_priced|7|1786860200", // carried_forward, cost NULL
		"goose|gone_session|8|1786860300",
	} {
		ev := eventByKey(t, obs.Events, key)
		if ev.CostMicroUSD != nil {
			t.Errorf("%s: CostMicroUSD = %d, want nil (unpriced, not free)", key, *ev.CostMicroUSD)
		}
		if _, known := ev.Cost(); known {
			t.Errorf("%s: Cost() reports a known cost for a NULL cost column", key)
		}
		if ev.PriceSource != "" {
			t.Errorf("%s: PriceSource = %q, want empty", key, ev.PriceSource)
		}
	}

	// A cost goose DID record is carried, labelled with the provenance it wrote.
	priced := eventByKey(t, obs.Events, "goose|sess_priced|5|1786860000")
	micro, known := priced.Cost()
	if !known || micro != 36500 {
		t.Errorf("priced event cost = (%d, %v), want (36500, true) for $0.0365", micro, known)
	}
	if priced.PriceSource != "goose-provider_reported" {
		t.Errorf("PriceSource = %q, want goose-provider_reported", priced.PriceSource)
	}
	est := eventByKey(t, obs.Events, "goose|sess_priced|6|1786860100")
	if est.PriceSource != "goose-estimated" {
		t.Errorf("estimated PriceSource = %q, want goose-estimated", est.PriceSource)
	}
}

// TestCarriedForwardRowsAreIncluded covers the second trap. A
// cost_source='carried_forward' row is computed as
// MAX(sessions.accumulated_* - SUM(usage_ledger.*), 0): it is the GAP, so
// filtering it out undercounts. It also carries NO model, which is how a
// model-is-required guard would delete it by accident.
func TestCarriedForwardRowsAreIncluded(t *testing.T) {
	obs := collect(t, fixtureSource(t))
	ev := eventByKey(t, obs.Events, "goose|sess_priced|7|1786860200")

	if ev.Model != "" {
		t.Errorf("Model = %q, want empty: goose binds no model on a carried-forward row", ev.Model)
	}
	if ev.TotalTokens != 20 {
		t.Errorf("TotalTokens = %d, want 20", ev.TotalTokens)
	}

	// The whole point: the session's ledger rows reproduce its accumulator.
	var total int64
	for _, e := range obs.Events {
		if e.SessionID == "sess_priced" {
			total += e.TotalTokens
		}
	}
	if total != 13440 {
		t.Errorf("sess_priced totals %d, want 13440 (= accumulated_total_tokens): "+
			"dropping the carried-forward row undercounts", total)
	}
}

// TestSessionAccumulatorsAreNeverSummed covers the third trap from the numeric
// side: sessions.total_tokens holds only the LAST turn (7503) while the ledger
// holds both (14984), and the accumulator holds the same 14984 that the ledger
// sums to — reading either column alongside the ledger is the double count.
func TestSessionAccumulatorsAreNeverSummed(t *testing.T) {
	obs := collect(t, fixtureSource(t))

	var total int64
	for _, e := range obs.Events {
		if e.SessionID == "20260816_3" {
			total += e.TotalTokens
		}
	}
	if total != 14984 {
		t.Errorf("session 20260816_3 totals %d, want 14984 (7481 + 7503)", total)
	}
	if total == 7503 {
		t.Error("session total equals sessions.total_tokens: the assigned column was read as a total")
	}
	if total == 2*14984 {
		t.Error("session total is double the accumulator: the ledger and accumulated_* were both counted")
	}
}

// TestQueriesReadNoSessionCounters is the same trap from the source side. The
// numeric test above only proves today's fixture adds up; this one fails the
// day a token column of `sessions` is added to a query, which is the actual
// mistake being guarded against.
func TestQueriesReadNoSessionCounters(t *testing.T) {
	forbidden := []string{
		"accumulated_total_tokens", "accumulated_input_tokens", "accumulated_output_tokens",
		"accumulated_cache_read_tokens", "accumulated_cache_write_tokens", "accumulated_cost",
		"s.total_tokens", "s.input_tokens", "s.output_tokens",
		"s.cache_read_tokens", "s.cache_write_tokens",
		"m.tokens", "metadata_json",
	}
	queries := map[string]string{"usageQuery": usageQuery, "activityQuery": activityQuery}
	for name, q := range queries {
		lower := strings.ToLower(q)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("%s selects %s: session counters are either assigned-not-accumulated "+
					"or equal to SUM(usage_ledger) by construction, and messages carry a second "+
					"copy of the usage", name, bad)
			}
		}
	}
}

// TestRawIsAnAllowList checks the audit payload carries usage/model/identity
// fields and nothing else, and that a NULL stays null rather than becoming a
// fabricated zero.
func TestRawIsAnAllowList(t *testing.T) {
	obs := collect(t, fixtureSource(t))
	ev := eventByKey(t, obs.Events, "goose|20260816_1|1|1786853116")

	var raw map[string]any
	if err := json.Unmarshal([]byte(ev.Raw), &raw); err != nil {
		t.Fatalf("raw is not JSON: %v (%q)", err, ev.Raw)
	}
	wantKeys := map[string]bool{
		"id": true, "session_id": true, "created_timestamp": true, "model": true,
		"input_tokens": true, "output_tokens": true, "total_tokens": true,
		"cache_read_tokens": true, "cache_write_tokens": true,
		"cost": true, "cost_source": true, "is_compaction": true, "provider": true,
	}
	for k := range raw {
		if !wantKeys[k] {
			t.Errorf("raw carries unexpected key %q", k)
		}
	}
	for k := range wantKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("raw is missing key %q", k)
		}
	}
	if v, ok := raw["cost"]; !ok || v != nil {
		t.Errorf("raw cost = %v, want null: a NULL cost must not become a number", v)
	}
	if v, ok := raw["cache_read_tokens"]; !ok || v != nil {
		t.Errorf("raw cache_read_tokens = %v, want null", v)
	}
}

// TestMillisecondTimestampsAreNormalised applies goose's own rule
// (MILLISECOND_TIMESTAMP_THRESHOLD): a stamp above 10^10 is milliseconds.
// Imported sessions carry those, and reading one as seconds lands the event in
// the year 58000.
func TestMillisecondTimestampsAreNormalised(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir)
	writeRows(t, path, `INSERT INTO usage_ledger (id, session_id, created_timestamp, model,
		input_tokens, output_tokens, total_tokens, cost, cost_source, is_compaction)
		VALUES (50, '20260816_1', 1786853116000, 'gemma4:31b', 10, 2, 12, NULL, NULL, 0)`)

	obs := collect(t, adapter.Source{Tool: model.ToolGoose, Class: model.EventLevel, Path: path})
	ev := eventByKey(t, obs.Events, "goose|20260816_1|50|1786853116")
	if want := time.Unix(1786853116, 0).UTC(); !ev.EventTime.Equal(want) {
		t.Errorf("EventTime = %v, want %v", ev.EventTime, want)
	}
}

// TestIncrementalWatermarkAndGate: the second pass over an untouched database
// opens nothing and returns nothing; a new ledger row is picked up on its own,
// without re-emitting what was already consumed.
func TestIncrementalWatermarkAndGate(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir)
	src := adapter.Source{Tool: model.ToolGoose, Class: model.EventLevel, Path: path}
	a := New().(adapter.Incremental)

	first, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Events) == 0 || first.Checkpoint == nil {
		t.Fatalf("first pass: %d events, checkpoint %v", len(first.Events), first.Checkpoint)
	}
	if first.Checkpoint.Watermark != 9 {
		t.Errorf("watermark = %d, want 9 (the highest ledger rowid, including the skipped all-zero row)",
			first.Checkpoint.Watermark)
	}

	second, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Events) != 0 || len(second.Activity) != 0 {
		t.Errorf("unchanged database re-emitted %d events and %d calls",
			len(second.Events), len(second.Activity))
	}
	if second.Checkpoint != nil {
		t.Error("unchanged database returned a checkpoint; the stored one must be kept")
	}

	writeRows(t, path, `INSERT INTO usage_ledger (id, session_id, created_timestamp, model,
		input_tokens, output_tokens, total_tokens, cost, cost_source, is_compaction)
		VALUES (20, '20260816_3', 1786860500, 'gemma4:31b', 100, 5, 105, NULL, NULL, 0)`)

	third, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if len(third.Events) != 1 {
		t.Fatalf("want exactly the new row, got %d events", len(third.Events))
	}
	if key := third.Events[0].DedupKey; key != "goose|20260816_3|20|1786860500" {
		t.Errorf("new event key = %q", key)
	}
	if third.Checkpoint == nil || third.Checkpoint.Watermark != 20 {
		t.Errorf("checkpoint after the new row = %v", third.Checkpoint)
	}
}

// writeRows mutates the fixture database through a separate writable
// connection, standing in for goose itself. The adapter never opens it this way.
func writeRows(t *testing.T, path, stmt string) {
	t.Helper()
	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("write row: %v", err)
	}
}

// TestCollectDoesNotDisturbTheSource: an adapter is strictly observational, so
// a poll must leave the database file exactly as it found it — no journal, no
// WAL, no mtime bump.
func TestCollectDoesNotDisturbTheSource(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	collect(t, adapter.Source{Tool: model.ToolGoose, Class: model.EventLevel, Path: path})

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("source changed: %d/%v -> %d/%v",
			before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("poll created %s", filepath.Base(path)+suffix)
		}
	}
}

// TestDiscoverPathRootUsesDataSubdir: goose resolves GOOSE_PATH_ROOT to
// <root>/data (config/paths.rs Paths::get_dir), so the database is at
// <root>/data/sessions/sessions.db. Looking under <root>/sessions finds nothing.
func TestDiscoverPathRootUsesDataSubdir(t *testing.T) {
	root := t.TempDir()
	want := buildDB(t, filepath.Join(root, dataSubdir))
	buildDB(t, root) // a decoy at the path the env var appears to name

	t.Setenv(PathRootEnv, root)
	t.Setenv(DataHomeEnv, "")
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 || srcs[0].Path != want {
		t.Fatalf("discovered %v, want [%s]", srcs, want)
	}
	if srcs[0].Tool != model.ToolGoose || srcs[0].Class != model.EventLevel {
		t.Errorf("source = (%q, %q), want (%q, event)", srcs[0].Tool, srcs[0].Class, model.ToolGoose)
	}
}

// TestDiscoverIgnoresRelativePathRoot: goose's own validated_path_root filters
// on is_absolute, so a relative value moves nothing for the writer and must move
// nothing for the reader either.
func TestDiscoverIgnoresRelativePathRoot(t *testing.T) {
	home := t.TempDir()
	want := buildDB(t, filepath.Join(home, ".local", "share", "goose"))

	t.Setenv(PathRootEnv, "relative/path")
	t.Setenv(DataHomeEnv, "")
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 || srcs[0].Path != want {
		t.Fatalf("discovered %v, want [%s]", srcs, want)
	}
}

// TestDiscoverXDGDataHome: with no GOOSE_PATH_ROOT the data dir is
// <XDG_DATA_HOME>/goose, and only ~/.local/share/goose when that is unset.
func TestDiscoverXDGDataHome(t *testing.T) {
	xdg := t.TempDir()
	home := t.TempDir()
	want := buildDB(t, filepath.Join(xdg, "goose"))
	buildDB(t, filepath.Join(home, ".local", "share", "goose")) // must lose

	t.Setenv(PathRootEnv, "")
	t.Setenv(DataHomeEnv, xdg)
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 || srcs[0].Path != want {
		t.Fatalf("discovered %v, want [%s]", srcs, want)
	}
}

// TestDiscoverMissingDatabaseIsClean: no goose install is not an error.
func TestDiscoverMissingDatabaseIsClean(t *testing.T) {
	t.Setenv(PathRootEnv, "")
	t.Setenv(DataHomeEnv, "")
	srcs, err := New().Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want no sources, got %v", srcs)
	}
}

// TestEnvConstantsAreExported guards the contract internal/cmd's discoveryEnv
// test enforces: every environment variable that moves this adapter's surface
// must be an exported constant of this package, so the guard can find it by
// parsing the source.
func TestEnvConstantsAreExported(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Getenv" {
				return true
			}
			id, ok := call.Args[0].(*ast.Ident)
			if !ok {
				t.Errorf("os.Getenv is called with a non-constant argument at %v; the discoveryEnv "+
					"guard resolves package constants only", fset.Position(call.Pos()))
				return true
			}
			found[id.Name] = true
			return true
		})
	}
	for _, name := range []string{"PathRootEnv", "DataHomeEnv"} {
		if !found[name] {
			t.Errorf("%s is not read through os.Getenv; the guard would not see it", name)
		}
	}
	if PathRootEnv != "GOOSE_PATH_ROOT" || DataHomeEnv != "XDG_DATA_HOME" {
		t.Errorf("env constants = (%q, %q)", PathRootEnv, DataHomeEnv)
	}
}
