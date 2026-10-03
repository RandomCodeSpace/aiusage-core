package collect

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/agy"
	"github.com/RandomCodeSpace/aiusage-core/adapter/claudecode"
	"github.com/RandomCodeSpace/aiusage-core/adapter/codex"
	"github.com/RandomCodeSpace/aiusage-core/adapter/copilot"
	"github.com/RandomCodeSpace/aiusage-core/adapter/hermes"
	"github.com/RandomCodeSpace/aiusage-core/adapter/opencode"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// incrementalFixture holds one populated multi-adapter home with a real SQLite
// store, so tests and benchmarks drive the exact production read path.
type incrementalFixture struct {
	reg *adapter.Registry
	st  *store.Ledger
	dc  adapter.DiscoverConfig

	codexSession string
	ccTranscript string
	copilotOTEL  string
	opencodeDB   string
	hermesDB     string
	agyTurns     string
}

func mkdirAll(t testing.TB, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeLines(t testing.TB, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendLines(t testing.TB, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

func execSQL(t testing.TB, dbPath string, stmts []string, args ...[]any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	for i, stmt := range stmts {
		var a []any
		if i < len(args) {
			a = args[i]
		}
		if _, err := db.Exec(stmt, a...); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// setupIncrementalFixture pins every adapter's discovery to temp fixtures (env
// and overrides), seeds initial data for each, and opens a real store.
func setupIncrementalFixture(t testing.TB) *incrementalFixture {
	home := t.TempDir()
	fx := &incrementalFixture{}

	// codex: two cumulative token_count records -> events of 1100 + 650.
	codexHome := filepath.Join(home, "codex")
	fx.codexSession = filepath.Join(codexHome, "sessions", "s1.jsonl")
	writeLines(t, fx.codexSession,
		`{"type":"turn_context","payload":{"model":"gpt-5-codex"}}`+"\n"+
			`{"type":"event_msg","timestamp":"2026-05-29T10:00:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"output_tokens":100,"total_tokens":1100}}}}`+"\n"+
			`{"type":"event_msg","timestamp":"2026-05-29T10:01:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1500,"output_tokens":250,"total_tokens":1750}}}}`+"\n")
	t.Setenv("CODEX_HOME", codexHome)

	// claude-code: one transcript line -> event of 1000.
	ccRoot := filepath.Join(home, "claude")
	fx.ccTranscript = filepath.Join(ccRoot, "projects", "proj", "sess.jsonl")
	writeLines(t, fx.ccTranscript,
		`{"timestamp":"2026-05-29T10:00:00Z","sessionId":"s","requestId":"r1","message":{"id":"m1","model":"claude-opus","usage":{"input_tokens":400,"output_tokens":600}}}`+"\n")
	t.Setenv("CLAUDE_CONFIG_DIR", ccRoot)

	// copilot: one chat span -> event of 150.
	copRoot := filepath.Join(home, "cop")
	fx.copilotOTEL = filepath.Join(copRoot, ".copilot", "otel", "f.jsonl")
	writeLines(t, fx.copilotOTEL,
		`{"type":"span","traceId":"t1","spanId":"s1","name":"chat m","endTime":[1775934264,0],"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m","gen_ai.conversation.id":"c1","gen_ai.usage.input_tokens":100,"gen_ai.usage.output_tokens":50}}`+"\n")
	t.Setenv("COPILOT_OTEL_FILE_EXPORTER_PATH", "")

	// opencode: one message row -> event of 30.
	ocDir := filepath.Join(home, "oc")
	mkdirAll(t, ocDir)
	fx.opencodeDB = filepath.Join(ocDir, "opencode.db")
	execSQL(t, fx.opencodeDB, []string{
		`CREATE TABLE message (id TEXT, session_id TEXT, data TEXT)`,
		`INSERT INTO message VALUES ('m1','s1','{"id":"m1","sessionID":"s1","modelID":"gpt-5","time":{"created":1748512800000},"tokens":{"input":10,"output":20,"total":30}}')`,
	})
	t.Setenv("OPENCODE_DATA_DIR", ocDir)

	// hermes: one growing session row -> delta of 300 (100+200).
	hermesHome := filepath.Join(home, "hermes")
	mkdirAll(t, hermesHome)
	fx.hermesDB = filepath.Join(hermesHome, "state.db")
	execSQL(t, fx.hermesDB, []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, model TEXT, billing_provider TEXT,
			started_at TEXT, ended_at TEXT, input_tokens INTEGER, output_tokens INTEGER,
			cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER)`,
		`INSERT INTO sessions VALUES ('A','claude-opus','anthropic','2026-05-29T10:00:00Z','',100,200,0,0,0)`,
	})
	t.Setenv("HERMES_HOME", hermesHome)

	// agy: a content-only blob -> sources but zero usage (checkpoint-only path),
	// plus one cumulative turn -> delta of 60. Both files exercise the shared
	// gemini-shape parser, which is why the cumulative-turn case lives here.
	agyDir := filepath.Join(home, "agy")
	writeLines(t, filepath.Join(agyDir, "conv.json"), `{"id":"c1","messages":[{"role":"user","content":"hi"}]}`)
	fx.agyTurns = filepath.Join(agyDir, "turns.jsonl")
	writeLines(t, fx.agyTurns,
		`{"id":"t1","model":"antigravity","sessionId":"gs","timestamp":"2026-05-29T10:00:00Z","tokens":{"input":50,"output":10,"total":60}}`+"\n")

	fx.dc = adapter.DiscoverConfig{
		Home: home,
		Overrides: map[string]string{
			model.ToolCopilot: copRoot,
			model.ToolAgy:     agyDir,
		},
	}
	fx.reg = adapter.NewRegistry(
		claudecode.New(), codex.New(), copilot.New(), opencode.New(),
		hermes.New(), agy.New(),
	)

	st, err := store.Open(filepath.Join(home, "store", "usage.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fx.st = st
	return fx
}

func storedTotal(t testing.TB, st *store.Ledger) int64 {
	t.Helper()
	sum, err := st.Summarize(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	return sum.Totals.Total
}

// TestIncrementalCycleExactDelta is the acceptance test for incremental
// collection: a full cycle, an append to every source, a re-collect that must
// land EXACTLY the appended delta (no double count, no gap), and an idle cycle
// that must see no data at all (near-zero parse work).
//
// The codex append is the binding whole-file-invariant case: its records are
// cumulative, so a naive tail-read would count the appended running total
// (2400) in full instead of the 650 delta — only the persisted per-file
// baseline makes the tail read exact.
func TestIncrementalCycleExactDelta(t *testing.T) {
	fx := setupIncrementalFixture(t)
	ctx := context.Background()

	// Cycle 1: everything lands in full.
	s1, err := RunOnce(ctx, fx.reg, fx.st, fx.dc)
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if len(s1.Errors) != 0 {
		t.Fatalf("cycle 1 errors: %v", s1.Errors)
	}
	// codex 1100+650, claude-code 1000, copilot 150, opencode 30,
	// hermes 300, agy 60.
	const base = 1100 + 650 + 1000 + 150 + 30 + 300 + 60
	if got := storedTotal(t, fx.st); got != base {
		t.Fatalf("cycle 1 total=%d want %d", got, base)
	}

	// Append new data to every source.
	appendLines(t, fx.codexSession,
		`{"type":"event_msg","timestamp":"2026-05-29T10:05:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":2000,"output_tokens":400,"total_tokens":2400}}}}`+"\n")
	appendLines(t, fx.ccTranscript,
		`{"timestamp":"2026-05-29T10:06:00Z","sessionId":"s","requestId":"r2","message":{"id":"m2","model":"claude-opus","usage":{"input_tokens":200,"output_tokens":300}}}`+"\n")
	appendLines(t, fx.copilotOTEL,
		`{"type":"span","traceId":"t2","spanId":"s2","name":"chat m","endTime":[1775934300,0],"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m","gen_ai.conversation.id":"c2","gen_ai.usage.input_tokens":50,"gen_ai.usage.output_tokens":25}}`+"\n")
	execSQL(t, fx.opencodeDB, []string{
		`INSERT INTO message VALUES ('m2','s1','{"id":"m2","sessionID":"s1","modelID":"gpt-5","time":{"created":1748512900000},"tokens":{"input":15,"output":25,"total":40}}')`,
	})
	execSQL(t, fx.hermesDB, []string{
		`UPDATE sessions SET input_tokens=150, output_tokens=250 WHERE id='A'`,
		`INSERT INTO sessions VALUES ('B','claude-opus','anthropic','2026-05-29T10:07:00Z','',5,5,0,0,0)`,
	})
	appendLines(t, fx.agyTurns,
		`{"id":"t2","model":"antigravity","sessionId":"gs","timestamp":"2026-05-29T10:08:00Z","tokens":{"input":20,"output":5,"total":25}}`+"\n")

	// Cycle 2: exactly the delta, nothing more, nothing missing.
	s2, err := RunOnce(ctx, fx.reg, fx.st, fx.dc)
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if len(s2.Errors) != 0 {
		t.Fatalf("cycle 2 errors: %v", s2.Errors)
	}
	// codex 650 (2400-1750, NOT 2400), claude-code 500, copilot 75,
	// opencode 40, hermes 100 (A) + 10 (B), agy 25.
	const delta = 650 + 500 + 75 + 40 + 100 + 10 + 25
	if got := storedTotal(t, fx.st); got != base+delta {
		t.Fatalf("cycle 2 total=%d want %d (base %d + delta %d)", got, base+delta, base, delta)
	}
	if s2.EventsInserted != 7 {
		t.Fatalf("cycle 2 inserted=%d want 7 (codex, cc, copilot, oc, hermes A+B, agy)", s2.EventsInserted)
	}
	// The codex tail read must have produced exactly the one delta event.
	if s2.EventsSeen > 6 {
		t.Fatalf("cycle 2 saw %d events; incremental reads should re-emit at most the changed files (codex 1 + cc 2 + copilot 2 + oc 1)", s2.EventsSeen)
	}

	// Cycle 3: nothing changed anywhere -> no events seen, no snapshots, no
	// inserts. This is the near-zero-work steady state.
	s3, err := RunOnce(ctx, fx.reg, fx.st, fx.dc)
	if err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if len(s3.Errors) != 0 {
		t.Fatalf("cycle 3 errors: %v", s3.Errors)
	}
	if s3.EventsSeen != 0 || s3.Snapshots != 0 || s3.EventsInserted != 0 {
		t.Fatalf("idle cycle did work: seen=%d snapshots=%d inserted=%d want 0,0,0",
			s3.EventsSeen, s3.Snapshots, s3.EventsInserted)
	}
	if got := storedTotal(t, fx.st); got != base+delta {
		t.Fatalf("idle cycle changed totals: %d want %d", got, base+delta)
	}
}
