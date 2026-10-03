package crush

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

// canary is the marker planted in every content field of testdata/secrets.sql.
const canary = "CANARY-a7f3c2e1-SECRET"

// buildDB materialises Crush's real schema plus one data fixture into
// <root>/.crush/crush.db and returns the database path.
//
// WAL is not decoration: Crush opens its databases with journal_mode=WAL
// (internal/db/connect.go), and a WAL database behaves differently under a
// read-only connection than a rollback-journal one — see
// TestReadingIsObservationalOnAWalDatabase.
func buildDB(t *testing.T, root, fixture string) string {
	t.Helper()
	dir := filepath.Join(root, ".crush")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, dbName)
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"schema.sql", fixture} {
		stmt, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(stmt)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return path
}

// writeIndex writes a projects.json naming each project root.
func writeIndex(t *testing.T, globalDir string, roots ...string) {
	t.Helper()
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var idx projectIndex
	for _, r := range roots {
		idx.Projects = append(idx.Projects, project{Path: r, DataDir: filepath.Join(r, ".crush")})
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, projectsFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// collect runs one full read of a fixture database and returns the observation.
func collect(t *testing.T, dbPath, project string) adapter.Observation {
	t.Helper()
	src := adapter.Source{
		Tool: model.ToolCrush, Class: model.EventLevel, Path: dbPath,
		Meta: map[string]string{"project": project},
	}
	obs, err := Adapter{}.Collect(context.Background(), src)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return obs
}

// state decodes the checkpoint an observation carries.
func state(t *testing.T, obs adapter.Observation) ckptState {
	t.Helper()
	if obs.Checkpoint == nil {
		t.Fatal("observation carries no checkpoint")
	}
	var s ckptState
	if err := json.Unmarshal([]byte(obs.Checkpoint.State), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// clearEnv removes both discovery variables so a test controls its own roots.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv(GlobalDataEnv, "")
	t.Setenv(XDGDataHomeEnv, "")
}

// ---------------------------------------------------------------- discovery

func TestDiscoverReadsProjectsIndex(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	globalDir := filepath.Join(base, "share", "crush")
	withDB := filepath.Join(base, "proj-a")
	withoutDB := filepath.Join(base, "proj-b")
	if err := os.MkdirAll(filepath.Join(withoutDB, ".crush"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := buildDB(t, withDB, "live.sql")
	writeIndex(t, globalDir, withDB, withoutDB)
	t.Setenv(GlobalDataEnv, globalDir)

	srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: base})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("want 1 source (the project with a database), got %d: %+v", len(srcs), srcs)
	}
	got := srcs[0]
	want := adapter.Source{
		Tool:  model.ToolCrush,
		Class: model.EventLevel,
		Path:  dbPath,
		Label: "Crush sessions: " + dbPath,
		Meta:  map[string]string{"project": withDB, "dataDir": filepath.Join(withDB, ".crush")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source mismatch\n got %+v\nwant %+v", got, want)
	}
}

// CRUSH_GLOBAL_DATA is joined DIRECTLY: Crush puts crush.json (and so
// projects.json) in that directory with no "crush" segment of its own, while
// XDG_DATA_HOME gets one. Getting this backwards finds nothing at all.
func TestDiscoverEnvPrecedence(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "proj")
	buildDB(t, proj, "live.sql")

	t.Run("XDG_DATA_HOME adds the crush segment", func(t *testing.T) {
		clearEnv(t)
		xdg := filepath.Join(base, "xdg")
		writeIndex(t, filepath.Join(xdg, appName), proj)
		t.Setenv(XDGDataHomeEnv, xdg)
		srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: base})
		if err != nil || len(srcs) != 1 {
			t.Fatalf("want 1 source via XDG_DATA_HOME, got %d (%v)", len(srcs), err)
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		clearEnv(t)
		home := filepath.Join(base, "home")
		writeIndex(t, filepath.Join(home, ".local", "share", appName), proj)
		srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
		if err != nil || len(srcs) != 1 {
			t.Fatalf("want 1 source via the home fallback, got %d (%v)", len(srcs), err)
		}
	})

	// A relative XDG base directory is invalid per the spec, is ignored by
	// internal/config, and must be ignored here too: honouring it would resolve
	// projects.json against the daemon's working directory.
	t.Run("a relative XDG_DATA_HOME is ignored", func(t *testing.T) {
		clearEnv(t)
		home := filepath.Join(base, "home-rel")
		writeIndex(t, filepath.Join(home, ".local", "share", appName), proj)
		t.Setenv(XDGDataHomeEnv, "relative/share")
		srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
		if err != nil || len(srcs) != 1 {
			t.Fatalf("want the home fallback to win over a relative XDG base, got %d (%v)", len(srcs), err)
		}
	})

	t.Run("CRUSH_GLOBAL_DATA beats both", func(t *testing.T) {
		clearEnv(t)
		xdg := filepath.Join(base, "xdg2")
		writeIndex(t, filepath.Join(xdg, appName), proj)
		global := filepath.Join(base, "global")
		writeIndex(t, global) // an EMPTY index at the winning location
		t.Setenv(XDGDataHomeEnv, xdg)
		t.Setenv(GlobalDataEnv, global)
		srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: base})
		if err != nil {
			t.Fatal(err)
		}
		if len(srcs) != 0 {
			t.Fatalf("CRUSH_GLOBAL_DATA did not win: %+v", srcs)
		}
	})
}

func TestDiscoverMissingIndexIsNotAnError(t *testing.T) {
	clearEnv(t)
	t.Setenv(GlobalDataEnv, t.TempDir())
	srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: t.TempDir()})
	if err != nil {
		t.Fatalf("a machine that never ran Crush is not a failure: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want no sources, got %+v", srcs)
	}
}

func TestDiscoverMalformedIndexErrors(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(GlobalDataEnv, dir)
	if _, err := (Adapter{}).Discover(context.Background(), adapter.DiscoverConfig{Home: dir}); err == nil {
		t.Fatal("a corrupt index must be reported, not silently read as empty")
	}
}

// A relative data_dir belongs to its own project, never to the collecting
// process's working directory.
func TestDiscoverResolvesRelativeDataDirAgainstItsProject(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	proj := filepath.Join(base, "proj")
	dbPath := buildDB(t, proj, "live.sql")
	global := filepath.Join(base, "global")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(projectIndex{Projects: []project{{Path: proj, DataDir: ".crush"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, projectsFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(GlobalDataEnv, global)

	srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Path != dbPath {
		t.Fatalf("want %s, got %+v", dbPath, srcs)
	}
}

func TestDiscoverDeduplicatesASharedDataDir(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	proj := filepath.Join(base, "proj")
	buildDB(t, proj, "live.sql")
	global := filepath.Join(base, "global")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(proj, ".crush")
	raw, err := json.Marshal(projectIndex{Projects: []project{
		{Path: proj, DataDir: shared},
		{Path: filepath.Join(base, "other"), DataDir: shared},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, projectsFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(GlobalDataEnv, global)

	srcs, err := Adapter{}.Discover(context.Background(), adapter.DiscoverConfig{Home: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 {
		t.Fatalf("two index entries naming one database must collect once, got %d", len(srcs))
	}
}

// ------------------------------------------------------------ the live trap

// The real capture: 15290 assigned prompt tokens, 8 assigned completion tokens
// and cost 0.0 under an ollama model Crush's catalog cannot price. Every one of
// those numbers is a reason to emit NOTHING.
func TestLiveCaptureWithZeroCostEmitsNothing(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "live.sql")
	obs := collect(t, dbPath, "/home/dev/harness-lab/crush")

	if len(obs.Events) != 0 {
		t.Fatalf("a session Crush could not price is unmeasured, not free; got %d event(s): %+v",
			len(obs.Events), obs.Events)
	}
	if len(obs.Snapshots) != 0 || len(obs.Activity) != 0 || len(obs.TurnContexts) != 0 {
		t.Fatalf("cost-only adapter emitted other streams: %+v", obs)
	}
	if got := state(t, obs).Cost; len(got) != 0 {
		t.Fatalf("no charge means no watermark, got %+v", got)
	}
}

// The named trap, stated as an assertion rather than a comment: 15290 is a
// context size that happens to sit in a column called prompt_tokens, and it
// must never become an input-token count.
func TestAssignedTokenColumnsNeverBecomeUsage(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	obs := collect(t, dbPath, "/proj")
	if len(obs.Events) == 0 {
		t.Fatal("fixture emitted nothing; the assertion below would be vacuous")
	}
	for _, e := range obs.Events {
		if e.InputTokens|e.OutputTokens|e.CacheCreationTokens|e.CacheReadTokens|
			e.ReasoningTokens|e.TotalTokens != 0 {
			t.Fatalf("%s carries tokens Crush never reported: %+v", e.DedupKey, e)
		}
		var raw rawPayload
		if err := json.Unmarshal([]byte(e.Raw), &raw); err != nil {
			t.Fatal(err)
		}
		if raw.PromptTokensAssigned == 0 && raw.CompletionTokensAssigned == 0 {
			continue // sess-nomodel genuinely records zeros
		}
		if raw.PromptTokensAssigned < 0 || raw.CompletionTokensAssigned < 0 {
			t.Fatalf("%s: negative assigned counter: %+v", e.DedupKey, raw)
		}
	}
}

// --------------------------------------------------------- the cost fixture

// expected is one row the costly fixture must produce, spelled out in full.
type expected struct {
	dedup    string
	session  string
	micro    int64
	provider string
	rawModel string
	seen     int
	when     time.Time
}

func costlyExpectations() []expected {
	return []expected{
		{"crush|cost|sess-paid-single|12346", "sess-paid-single", 12346, "anthropic", "claude-sonnet-4-5", 1, time.Unix(1786800600, 0).UTC()},
		{"crush|cost|sess-parent|2500000", "sess-parent", 2500000, "anthropic", "claude-sonnet-4-5", 1, time.Unix(1786810900, 0).UTC()},
		{"crush|cost|sess-multi|1000000", "sess-multi", 1000000, "", "", 2, time.Unix(1786820500, 0).UTC()},
		{"crush|cost|sess-nomodel|250000", "sess-nomodel", 250000, "", "", 0, time.Unix(1786850100, 0).UTC()},
		{"crush|cost|sess-gp|3000000", "sess-gp", 3000000, "", "", 2, time.Unix(1786880900, 0).UTC()},
		{"crush|cost|sess-ms|1000", "sess-ms", 1000, "anthropic", "claude-sonnet-4-5", 1, time.UnixMilli(1786860600000).UTC()},
	}
}

func TestCollectEmitsExactlyTheExpectedCharges(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	obs := collect(t, dbPath, "/proj")
	want := costlyExpectations()

	if len(obs.Events) != len(want) {
		var keys []string
		for _, e := range obs.Events {
			keys = append(keys, e.DedupKey)
		}
		t.Fatalf("want %d events, got %d: %v", len(want), len(obs.Events), keys)
	}
	for i, w := range want {
		e := obs.Events[i]
		if e.DedupKey != w.dedup {
			t.Errorf("event %d: dedup key %q, want %q", i, e.DedupKey, w.dedup)
			continue
		}
		if e.Tool != model.ToolCrush || e.Kind != model.KindUsage {
			t.Errorf("%s: tool/kind %q/%q", w.dedup, e.Tool, e.Kind)
		}
		if e.SessionID != w.session {
			t.Errorf("%s: session %q, want %q", w.dedup, e.SessionID, w.session)
		}
		if e.Project != "/proj" || e.SourcePath != dbPath {
			t.Errorf("%s: project %q source %q", w.dedup, e.Project, e.SourcePath)
		}
		cost, priced := e.Cost()
		if !priced || cost != w.micro {
			t.Errorf("%s: cost %d (priced=%v), want %d", w.dedup, cost, priced, w.micro)
		}
		if e.PriceSource != PriceSourceReported {
			t.Errorf("%s: price source %q, want %q", w.dedup, e.PriceSource, PriceSourceReported)
		}
		if e.Provider != w.provider {
			t.Errorf("%s: provider %q, want %q", w.dedup, e.Provider, w.provider)
		}
		if !e.EventTime.Equal(w.when) {
			t.Errorf("%s: event time %s, want %s", w.dedup, e.EventTime, w.when)
		}
		var raw rawPayload
		if err := json.Unmarshal([]byte(e.Raw), &raw); err != nil {
			t.Fatalf("%s: raw: %v", w.dedup, err)
		}
		if raw.Model != w.rawModel || raw.ModelsSeen != w.seen {
			t.Errorf("%s: raw model %q seen %d, want %q / %d", w.dedup, raw.Model, raw.ModelsSeen, w.rawModel, w.seen)
		}
		if raw.CostMicroUSD != w.micro || raw.CostDeltaMicroUSD != w.micro {
			t.Errorf("%s: raw cost %d delta %d, want %d for a first charge",
				w.dedup, raw.CostMicroUSD, raw.CostDeltaMicroUSD, w.micro)
		}
	}
}

// runSubAgent copies a child's cost into its parent while the child keeps its
// own, so summing sessions.cost over every row double counts. Crush's own stats
// filter parent_session_id IS NULL; so does this adapter.
func TestSubAgentCostIsCountedOnce(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	obs := collect(t, dbPath, "/proj")

	var total int64
	for _, e := range obs.Events {
		if strings.Contains(e.SessionID, "child") || strings.Contains(e.SessionID, "grand") ||
			e.SessionID == "sess-orphan" {
			t.Fatalf("a sub-agent session was charged separately: %s", e.DedupKey)
		}
		c, _ := e.Cost()
		total += c
	}
	// The roots alone: 12346 + 2500000 + 1000000 + 250000 + 3000000 + 1000.
	// Counting sess-child (500000), sess-gp-child (1200000), sess-gp-grand
	// (400000) and sess-orphan (750000) too would report 9613346.
	const wantRootsOnly = 6763346
	if total != wantRootsOnly {
		t.Fatalf("total %d micro-USD, want %d (roots only)", total, wantRootsOnly)
	}
}

// A grandchild's model has to reach the ROOT that absorbed its cost, or the
// root reports a single confident provider it did not spend all its money with.
func TestNestedSubAgentModelsReachTheRoot(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	obs := collect(t, dbPath, "/proj")
	for _, e := range obs.Events {
		if e.SessionID != "sess-gp" {
			continue
		}
		if e.Provider != "" {
			t.Fatalf("sess-gp spent under two providers (its own and its grandchild's); "+
				"reporting %q claims a certainty the source does not support", e.Provider)
		}
		var raw rawPayload
		if err := json.Unmarshal([]byte(e.Raw), &raw); err != nil {
			t.Fatal(err)
		}
		if raw.ModelsSeen != 2 {
			t.Fatalf("sess-gp saw %d model(s), want 2 (its own and the grandchild's)", raw.ModelsSeen)
		}
		return
	}
	t.Fatal("sess-gp produced no event")
}

// ------------------------------------------------------------- incremental

// The file-stamp gate closes on an untouched database — and pinning exactly
// WHEN it closes matters, because the first read of a freshly-checkpointed WAL
// database materialises the sidecars and so moves the very stamps the gate
// compares (see TestReadingIsObservationalOnAWalDatabase). That costs one extra
// full read, once, and never a duplicated charge: the second pass re-reads and
// mints dedup keys that already exist. Normalising the stamps to hide it would
// mean weakening a correctness gate to save a single read.
func TestGateClosesOnAnUntouchedDatabase(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	src := adapter.Source{Tool: model.ToolCrush, Class: model.EventLevel, Path: dbPath,
		Meta: map[string]string{"project": "/proj"}}

	first, err := Adapter{}.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) == 0 {
		t.Fatal("first pass read nothing; the rest of this test is vacuous")
	}

	// Pass two: the sidecars appeared during pass one, so the gate is open and
	// the database is re-read. Nothing new may be charged.
	second, err := Adapter{}.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 0 {
		t.Fatalf("a re-read of an unchanged database charged %d event(s) again", len(second.Events))
	}

	// Pass three: the stamps have settled and the database is not opened.
	third, err := Adapter{}.CollectIncremental(context.Background(), src, second.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Events) != 0 {
		t.Fatalf("unchanged database produced %d event(s)", len(third.Events))
	}
	if third.Checkpoint != nil {
		t.Fatal("a closed gate must leave the stored checkpoint alone, not rewrite it")
	}
}

func TestCostGrowthChargesOnlyTheDelta(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	src := adapter.Source{Tool: model.ToolCrush, Class: model.EventLevel, Path: dbPath,
		Meta: map[string]string{"project": "/proj"}}
	first, err := Adapter{}.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := state(t, first).Cost["sess-paid-single"]; got != 12346 {
		t.Fatalf("watermark %d, want 12346", got)
	}

	bumpCost(t, dbPath, "sess-paid-single", 0.02)

	second, err := Adapter{}.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 {
		t.Fatalf("want exactly the grown session, got %d event(s)", len(second.Events))
	}
	e := second.Events[0]
	if e.DedupKey != "crush|cost|sess-paid-single|20000" {
		t.Fatalf("dedup key %q must name the watermark it advances TO", e.DedupKey)
	}
	if cost, _ := e.Cost(); cost != 20000-12346 {
		t.Fatalf("charged %d micro-USD, want the delta %d", cost, 20000-12346)
	}
	if got := state(t, second).Cost["sess-paid-single"]; got != 20000 {
		t.Fatalf("watermark %d after growth, want 20000", got)
	}
}

// Crush saves whole session rows from two places under separate locks (the
// agent loop and runSubAgent's parent update), so a lost update can lower the
// stored cost. Re-emitting on the way back up would charge the same dollars
// twice; the watermark therefore only ever rises.
func TestCostDropNeverLowersTheWatermark(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	src := adapter.Source{Tool: model.ToolCrush, Class: model.EventLevel, Path: dbPath,
		Meta: map[string]string{"project": "/proj"}}
	first, err := Adapter{}.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}

	bumpCost(t, dbPath, "sess-paid-single", 0.005) // below the 12346 already charged

	second, err := Adapter{}.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range second.Events {
		if e.SessionID == "sess-paid-single" {
			t.Fatalf("a drop re-charged the session: %s", e.DedupKey)
		}
	}
	if got := state(t, second).Cost["sess-paid-single"]; got != 12346 {
		t.Fatalf("watermark %d after a drop, want it held at 12346", got)
	}
}

// A row this pass could not READ is not a row that ceased to exist. Both are
// simply absent from the result set, and rebuilding the watermark map from the
// rows that did read therefore forgets what has already been charged for the
// unreadable one — after which the next readable pass charges its whole
// accumulator again, under a key naming a total the ledger has never seen and
// cannot conflict-skip.
//
// Measured before carryUnreadable existed: sess-parent was charged 2500000 and
// then, after one unreadable pass and growth to 3.0, another 3000000 — 5500000
// micro-USD for 3000000 of real spend.
func TestUnreadableRowKeepsItsWatermark(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	src := adapter.Source{Tool: model.ToolCrush, Class: model.EventLevel, Path: dbPath,
		Meta: map[string]string{"project": "/proj"}}

	first, err := Adapter{}.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := state(t, first).Cost["sess-parent"]; got != 2500000 {
		t.Fatalf("watermark %d after the first charge, want 2500000", got)
	}

	// Corruption a scan cannot survive: SQLite stores what it is given, and the
	// column's CHECK (cost >= 0.0) passes for text, which sorts above numbers.
	corrupt(t, dbPath, `UPDATE sessions SET cost = 'corrupt' WHERE id = 'sess-parent'`)

	second, err := Adapter{}.CollectIncremental(context.Background(), src, first.Checkpoint)
	if err == nil {
		t.Fatal("an unreadable row must be reported, not swallowed")
	}
	if got := state(t, second).Cost["sess-parent"]; got != 2500000 {
		t.Fatalf("watermark %d while the row was unreadable, want it held at 2500000", got)
	}

	// The session spends more, then the row becomes readable again.
	corrupt(t, dbPath, `UPDATE sessions SET cost = 3.0 WHERE id = 'sess-parent'`)
	third, err := Adapter{}.CollectIncremental(context.Background(), src, second.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var charged int64
	for _, e := range third.Events {
		if e.SessionID != "sess-parent" {
			continue
		}
		c, _ := e.Cost()
		charged += c
	}
	if charged != 500000 {
		t.Fatalf("charged %d micro-USD for growth from 2.5 to 3.0, want the delta 500000", charged)
	}
}

// corrupt runs one statement against the fixture with a writable connection.
func corrupt(t *testing.T, dbPath, stmt string) {
	t.Helper()
	db, err := sql.Open(driverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

// The checkpoint is state, not history. Losing it re-reads the database in
// full, and the keys minted are the ones already stored, so the ledger's
// conflict-skip absorbs the repeat.
func TestLostCheckpointMintsTheSameKeys(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	src := adapter.Source{Tool: model.ToolCrush, Class: model.EventLevel, Path: dbPath,
		Meta: map[string]string{"project": "/proj"}}
	first, err := Adapter{}.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Adapter{}.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != len(again.Events) {
		t.Fatalf("re-read produced %d events, first pass %d", len(again.Events), len(first.Events))
	}
	for i := range first.Events {
		if first.Events[i].DedupKey != again.Events[i].DedupKey {
			t.Fatalf("event %d: key %q on re-read, %q originally",
				i, again.Events[i].DedupKey, first.Events[i].DedupKey)
		}
	}
}

// bumpCost writes a new cost for one session, the way a running Crush would.
func bumpCost(t *testing.T, dbPath, session string, cost float64) {
	t.Helper()
	db, err := sql.Open(driverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE sessions SET cost = ? WHERE id = ?`, cost, session); err != nil {
		t.Fatal(err)
	}
}

// ----------------------------------------------------------------- privacy

// Every content field of the fixture carries the canary. None of them may reach
// any field of any emitted event, the checkpoint included.
func TestNoContentReachesAnEmittedField(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "secrets.sql")
	obs := collect(t, dbPath, "/proj")
	if len(obs.Events) != 1 {
		t.Fatalf("want the one paid session, got %d", len(obs.Events))
	}

	blob, err := json.Marshal(struct {
		Events     []model.UsageEvent
		Raw        []string
		Checkpoint *model.SourceCheckpoint
	}{
		Events:     obs.Events,
		Raw:        []string{obs.Events[0].Raw}, // UsageEvent.Raw is json:"-"
		Checkpoint: obs.Checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), canary) {
		t.Fatalf("content leaked into the observation: %s", blob)
	}

	// Field by field, so a future field that is not marshalled still fails.
	e := obs.Events[0]
	for name, v := range map[string]string{
		"Model": e.Model, "Provider": e.Provider, "SessionID": e.SessionID,
		"Project": e.Project, "RequestID": e.RequestID, "MessageID": e.MessageID,
		"SourcePath": e.SourcePath, "DedupKey": e.DedupKey, "Raw": e.Raw,
		"PriceSource": e.PriceSource, "ServiceTier": e.ServiceTier,
	} {
		if strings.Contains(v, canary) {
			t.Errorf("%s carries content: %q", name, v)
		}
	}
}

// The title is generated from the user's prompt, so it is content and this
// package must not even select it. Reading the source is the only check that
// fails on the day someone adds it to a query.
func TestNoQuerySelectsAContentColumn(t *testing.T) {
	src, err := os.ReadFile("crush.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{sessionsQuery, modelsQuery} {
		lower := strings.ToLower(q)
		for _, col := range []string{"title", "todos", "parts", "content", "path", "summary_message_id"} {
			if strings.Contains(lower, col) {
				t.Errorf("query selects the content column %q:\n%s", col, q)
			}
		}
	}
	// The whole package, not just the two constants above.
	for _, col := range []string{"FROM files", "FROM read_files", "sessions.title", "m.parts"} {
		if strings.Contains(string(src), col) {
			t.Errorf("package reads %q", col)
		}
	}
}

// -------------------------------------------------------------- read-only

// The adapter must be incapable of writing, not merely disinclined.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`UPDATE sessions SET cost = 99.0 WHERE id = 'sess-parent'`,
		`DELETE FROM sessions`,
		`INSERT INTO sessions VALUES ('x',NULL,'t',0,0,0,1.0,1,1,NULL,NULL)`,
		`CREATE TABLE nope (a TEXT)`,
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("a mode=ro connection accepted: %s", stmt)
		}
	}
}

// What "strictly observational" actually costs on a WAL database, measured
// rather than assumed.
//
// Crush runs journal_mode=WAL, and SQLite CANNOT read a WAL database without
// its shared-memory index: a read-only connection to one whose sidecars are
// absent creates crush.db-shm and a zero-length crush.db-wal, and leaves them
// there. That is SQLite's coordination state, not a change to any row — the
// database file itself is byte-identical, its mtime unmoved, and the WAL holds
// no frames. It is also unavoidable at this layer: immutable=1 is the only flag
// that suppresses it and it is forbidden here, because Crush writes these files
// while it runs and SQLite documents wrong results for an immutable-flagged
// file that changes.
//
// The consequence worth knowing operationally: a genuinely read-only DIRECTORY
// makes the read FAIL (SQLITE_READONLY_DIRECTORY), it does not make it silent.
func TestReadingIsObservationalOnAWalDatabase(t *testing.T) {
	root := t.TempDir()
	dbPath := buildDB(t, root, "costly.sql")
	for _, s := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + s); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := (Adapter{}).Collect(context.Background(), adapter.Source{
		Tool: model.ToolCrush, Path: dbPath, Meta: map[string]string{"project": "/proj"},
	}); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("collection touched the database file: %d/%s -> %d/%s",
			before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Error("collection changed the database file's bytes")
	}
	// A rollback journal would mean a transaction was started for writing.
	if _, err := os.Stat(dbPath + "-journal"); err == nil {
		t.Error("collection created a rollback journal")
	}
	// The WAL may be materialised, but it must be empty: no frames means no
	// change was ever staged.
	if fi, err := os.Stat(dbPath + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("collection left %d bytes in the write-ahead log", fi.Size())
	}
}

// Project directories have spaces in them. The DSN is a file: URI, so this is
// not obviously safe and is worth pinning.
func TestCollectHandlesAPathWithSpaces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "my crush project")
	dbPath := buildDB(t, root, "costly.sql")
	obs := collect(t, dbPath, root)
	if len(obs.Events) != len(costlyExpectations()) {
		t.Fatalf("want %d events from a path with spaces, got %d",
			len(costlyExpectations()), len(obs.Events))
	}
}

// ------------------------------------------------------------------ units

// Stamping a model would let the collector re-price the event: a zero-token
// charge against a model the price table knows resolves to 0 with ok=true, and
// the stamped 0 replaces the harness's own figure in an append-only row.
// Verified against pricing; see the package doc.
func TestModelIsNeverStamped(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	for _, e := range collect(t, dbPath, "/proj").Events {
		if e.Model != "" {
			t.Fatalf("%s stamped model %q; a zero-token charge against a priced model "+
				"is re-stamped at 0 and the reported cost is lost", e.DedupKey, e.Model)
		}
	}
}

func TestMicroUSD(t *testing.T) {
	cases := []struct {
		in    float64
		want  int64
		valid bool
	}{
		{0, 0, true},
		{0.0123456, 12346, true}, // rounds up from 12345.6
		{0.0000004, 0, true},     // below one micro-USD: a real cost that cannot be charged
		{0.0000005, 1, true},     // exactly half rounds away from zero
		{2.5, 2500000, true},
		{0.001, 1000, true},
		{-1, 0, false},
	}
	for _, c := range cases {
		got, ok := microUSD(c.in)
		if ok != c.valid || got != c.want {
			t.Errorf("microUSD(%v) = %d,%v; want %d,%v", c.in, got, ok, c.want, c.valid)
		}
	}
}

// Crush's schema comments say milliseconds; every writer emits seconds. The
// unit is decided by magnitude so a future Crush that honours its own comment
// does not scatter events across the year 58000.
func TestUnixStampResolvesTheUnitAmbiguity(t *testing.T) {
	sec, ok := unixStamp(1786853158)
	if !ok || sec.Year() != 2026 {
		t.Fatalf("seconds stamp read as %s (ok=%v)", sec, ok)
	}
	ms, ok := unixStamp(1786853158000)
	if !ok || !ms.Equal(sec) {
		t.Fatalf("millisecond stamp read as %s, want %s", ms, sec)
	}
	if _, ok := unixStamp(0); ok {
		t.Fatal("zero is not a timestamp")
	}
	if _, ok := unixStamp(-5); ok {
		t.Fatal("a negative is not a timestamp")
	}
}

func TestRootOfStopsOnCyclesAndMissingParents(t *testing.T) {
	parents := map[string]string{
		"a": "", "b": "a", "c": "b",
		"loop1": "loop2", "loop2": "loop1",
		"orphan": "gone",
	}
	if got := rootOf(parents, "c"); got != "a" {
		t.Errorf("rootOf(c) = %q, want a", got)
	}
	if got := rootOf(parents, "orphan"); got != "gone" {
		t.Errorf("rootOf(orphan) = %q, want gone (the walk stops where the map does)", got)
	}
	got := rootOf(parents, "loop1")
	if got != "loop1" && got != "loop2" {
		t.Errorf("rootOf(loop1) = %q; a cycle must terminate somewhere in it", got)
	}
}

func TestCollectOnAMissingDatabaseFailsWithoutEmitting(t *testing.T) {
	obs, err := Adapter{}.Collect(context.Background(), adapter.Source{
		Tool: model.ToolCrush, Path: filepath.Join(t.TempDir(), "absent.db"),
	})
	if err == nil {
		t.Fatal("want an error for an unreadable source")
	}
	if len(obs.Events) != 0 || obs.Checkpoint != nil {
		t.Fatalf("a failed read must advance nothing: %+v", obs)
	}
}

// A schema without messages must not silently drop provider attribution: the
// pass fails, nothing is appended, and the accumulator is still in the source
// for the next one. Nothing is ever lost by deferring a current value.
func TestMissingMessagesTableDefersRatherThanDegrades(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "costly.sql")
	db, err := sql.Open(driverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE messages`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	obs, err := Adapter{}.Collect(context.Background(), adapter.Source{
		Tool: model.ToolCrush, Path: dbPath, Meta: map[string]string{"project": "/proj"},
	})
	if err == nil {
		t.Fatal("want an error, not a pass of rows with attribution silently missing")
	}
	if len(obs.Events) != 0 || obs.Checkpoint != nil {
		t.Fatalf("a deferred pass must append nothing: %+v", obs)
	}
}

func TestAdapterIdentity(t *testing.T) {
	a := New()
	if a.ID() != model.ToolCrush || a.DisplayName() != "Crush" {
		t.Fatalf("identity %q / %q", a.ID(), a.DisplayName())
	}
	if _, ok := a.(adapter.Incremental); !ok {
		t.Fatal("adapter must implement adapter.Incremental")
	}
}

// A guard for the fixture itself: if the schema stops matching what Crush
// writes, every assertion above is testing a fiction.
func TestFixtureSchemaCarriesTheColumnsTheAdapterReads(t *testing.T) {
	dbPath := buildDB(t, t.TempDir(), "live.sql")
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{sessionsQuery, modelsQuery} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("query failed against the fixture schema: %v\n%s", err, q)
		}
		rows.Close()
	}
	var prompt int64
	if err := db.QueryRow(`SELECT prompt_tokens FROM sessions WHERE id = ?`,
		"9dcd37b6-c9cb-4c49-a571-9cbc4caca957").Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if prompt != 15290 {
		t.Fatalf("the captured assigned prompt_tokens is %d, want the recorded 15290", prompt)
	}
}
