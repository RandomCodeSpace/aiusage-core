// Package crush implements a COST-ONLY adapter for Crush (charmbracelet/crush).
//
// Crush keeps one SQLite database per project at <project>/.crush/crush.db and
// indexes every project it has ever run in at <global-data>/projects.json. The
// adapter reads the index, opens each database read-only (mode=ro), and emits
// ONE usage event per session per growth of that session's accumulated cost.
//
// # Tokens are not available, and the columns that look like tokens are traps
//
// sessions.prompt_tokens / completion_tokens are ASSIGNED, not accumulated.
// Crush's own writer (internal/agent/agent.go, updateSessionTokenCounters) is:
//
//	if usage.OutputTokens != 0 { session.CompletionTokens = usage.OutputTokens }
//	if p := usage.InputTokens + usage.CacheReadTokens; p != 0 { session.PromptTokens = p }
//
// `=`, not `+=`. The column holds the LAST turn's context size — for a long
// session it is roughly the context window, and it is the same number whether
// the session ran two turns or two hundred. Verified on this machine's live
// database: one session of two messages carries prompt_tokens=15290 for a
// prompt of six words. Two further writers make it neither a total nor a last
// value consistently: summarisation sets PromptTokens = 0 outright, and the
// title-generation path goes through UpdateTitleAndUsage, whose SQL is
// `prompt_tokens = prompt_tokens + ?`. The column cannot be read as either
// thing, so this adapter never reads it as usage. It appears in the audit
// payload under a name that says what it is, and nowhere else.
//
// There is no per-message fallback: messages.parts carries Text / ToolCall /
// ToolResult / Finish / ShellCommand parts and Finish holds
// {Reason, Time, Message, Details} — nothing numeric. Confirmed empty of usage
// on the live database.
//
// Only sessions.cost accumulates (`session.Cost += cost`), so cost is the one
// honest figure and this adapter reports cost alone: every event carries zero
// tokens. That is a real measurement of dollars, not an absence of one.
//
// # A cost of zero is unmeasured, not free
//
// Crush zeroes the charge for a step whose usage the provider did not report
// (`estimated`) and for any model configured FlatRate, and it prices from its
// own catalog — a local or unlisted provider therefore accumulates exactly
// 0.0. This adapter emits NOTHING for such a session. A zero-token, zero-cost
// row would assert that a session which really did spend something was free,
// and usage_events is append-only, so that claim could never be withdrawn.
// The live session on this machine is exactly this case: an ollama model,
// 15290 assigned prompt tokens, cost 0.0, and no event.
//
// # Sub-agent cost is rolled into the parent, so children are never counted
//
// runSubAgent finishes with `parentSession.Cost += childSession.Cost` while the
// child row KEEPS its own cost. Summing sessions.cost over every row therefore
// counts every sub-agent's spend twice; Crush's own reporting avoids it with
// `WHERE parent_session_id IS NULL` on every stats query, and so does this
// adapter. The rollup is best-effort in Crush (a failure is logged and the run
// continues), so a lost rollup leaves that child's cost uncounted here —
// understating, which is the only direction this ledger tolerates.
//
// # Growth, watermarks, and why the watermark never moves backwards
//
// The accumulator lives in the source, not in an append-only log, so the
// adapter stores the micro-USD already accounted for per session in its
// checkpoint state and appends only the growth. The watermark is the maximum
// ever observed and is never lowered: Crush's own writes are read-modify-write
// under separate locks (the agent loop saves the whole session row while
// runSubAgent independently adds a child's cost to it), so a lost update can
// make the stored value drop. Re-emitting after a drop would charge the same
// dollars twice; holding the watermark can only under-report.
//
// # Deliberately NOT set: UsageEvent.Model
//
// Crush names a model per MESSAGE, and this adapter does read it — into the
// audit payload — but the ledger's model column is left EMPTY on purpose. The
// collector re-prices every event it stores, and pricing a charge of zero
// tokens against a model the price table knows returns (0, "embedded-...",
// ok=true), which overwrites the harness-reported cost with a stamped 0 —
// verified against pricing. An empty model is not looked up at all
// (pricing.lookupKeys returns nothing for an empty name), so the cost survives.
// Provider is stamped, since a lookup keyed on provider alone never happens.
// When pricing learns to refuse a zero-token charge outright, the model can be
// stamped here and the audit payload keeps it in the meantime.
//
// CRITICAL: strictly read-only. Every database is opened mode=ro with
// query_only, never immutable=1 — Crush writes these files while it runs, and
// SQLite documents wrong results when an immutable-flagged file changes.
//
// Honest caveat, measured rather than assumed: Crush runs journal_mode=WAL, and
// SQLite cannot read a WAL database without its shared-memory index, so a
// mode=ro connection to one whose sidecars are absent CREATES crush.db-shm and
// a zero-length crush.db-wal and leaves them behind. No row changes, the
// database file stays byte-identical with its mtime unmoved, and the WAL holds
// no frames — it is SQLite's coordination state, not a write to the agent's
// data. immutable=1 is the only flag that would suppress it and it is
// forbidden here for the reason above. The same applies to every SQLite source
// in this project. See TestReadingIsObservationalOnAWalDatabase, which measures
// exactly that, and note the operational consequence: on a genuinely read-only
// DIRECTORY the read FAILS (SQLITE_READONLY_DIRECTORY) rather than degrading
// quietly.
package crush

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the pure-Go "sqlite" driver

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

const (
	// GlobalDataEnv names Crush's own global data directory. Crush joins
	// crush.json (and therefore projects.json) DIRECTLY onto it, with no
	// "crush" segment of its own.
	GlobalDataEnv = "CRUSH_GLOBAL_DATA"
	// XDGDataHomeEnv is the second rung of Crush's own resolution order:
	// $XDG_DATA_HOME/crush. It moves what this adapter READS, which is why it
	// is exported and must be registered in cmd.discoveryEnv.
	XDGDataHomeEnv = "XDG_DATA_HOME"

	// PriceSourceReported labels a cost this adapter took from the harness
	// itself rather than from any price table. price_source is an open
	// vocabulary read as an opaque label.
	PriceSourceReported = "crush-session-cost"

	appName      = "crush"
	projectsFile = "projects.json"
	dbName       = "crush.db"
	driverName   = "sqlite"

	// maxParentDepth bounds the walk from a session to its root. Sub-agents can
	// nest, and a corrupt parent link must not spin.
	maxParentDepth = 32

	// msThreshold separates a seconds stamp from a milliseconds one. Crush's
	// schema comments say "Unix timestamp in milliseconds" while every writer
	// is strftime('%s','now') — seconds. The live database is seconds. The
	// source disagrees with itself, so the unit is decided by magnitude:
	// 1e11 seconds is the year 5138, 1e11 milliseconds is 1973.
	msThreshold = 100_000_000_000
)

// sessionsQuery reads every session row. The parent link is read so children
// can be excluded (their cost is already inside the parent's) and so a
// message's model can be folded onto the root that will actually be emitted.
//
// The token columns are read ONLY to record in the audit payload what the
// source claimed; nothing derives usage from them.
const sessionsQuery = `SELECT id, COALESCE(parent_session_id, ''),
	prompt_tokens, completion_tokens, cost, created_at, updated_at
	FROM sessions ORDER BY created_at, id`

// modelsQuery lists the distinct (provider, model) pairs each session's
// messages name. Filtering on a non-empty model rather than on role='assistant'
// keeps it independent of how Crush spells its roles: a message that names a
// model names a model.
const modelsQuery = `SELECT session_id, COALESCE(provider, ''), COALESCE(model, '')
	FROM messages WHERE TRIM(COALESCE(model, '')) != ''
	GROUP BY session_id, provider, model`

// Adapter reads Crush's per-project session databases. Read-only.
type Adapter struct{}

// New returns a Crush adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolCrush }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Crush" }

// Capabilities declares what this project can say about Crush.
//
// Cost is VENDOR-reported and it is the ONLY thing this adapter reports: one
// event per growth of sessions.cost, zero tokens, stamped PriceSourceReported
// so collect.stampCost cannot overwrite it. There is no activity at all — this
// adapter references model.ActivityEvent nowhere.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolCrush,
		Cost:      model.CostVendor,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(model.ToolCrush),
		Tier:      model.TierLive,
	}
}

// globalDir resolves the directory holding projects.json, following Crush's own
// order: CRUSH_GLOBAL_DATA verbatim, else $XDG_DATA_HOME/crush, else
// <home>/.local/share/crush. An explicit aiusage override for this tool
// replaces the derived default but not the harness's own variable, matching the
// other adapters.
func (a Adapter) globalDir(cfg adapter.DiscoverConfig) string {
	if env := strings.TrimSpace(os.Getenv(GlobalDataEnv)); env != "" {
		return env
	}
	def := ""
	// A relative XDG base directory is invalid per the spec and is ignored by
	// internal/config for the same reason; honouring one here would resolve
	// projects.json against the collecting process's working directory, which
	// is the daemon's and not the shell's.
	if xdg := strings.TrimSpace(os.Getenv(XDGDataHomeEnv)); filepath.IsAbs(xdg) {
		def = filepath.Join(xdg, appName)
	} else if cfg.Home != "" {
		def = filepath.Join(cfg.Home, ".local", "share", appName)
	}
	return cfg.Root(model.ToolCrush, def)
}

// project is one entry of Crush's projects.json index.
type project struct {
	Path    string `json:"path"`
	DataDir string `json:"data_dir"`
}

// projectIndex is the file's top-level shape.
type projectIndex struct {
	Projects []project `json:"projects"`
}

// Discover reads the projects index and returns one source per existing
// <data_dir>/crush.db. A missing index is not an error — Crush has simply never
// run on this machine. The index is authoritative: Crush registers every
// working directory it starts in, so nothing is gained by crawling the disk for
// databases it does not know about.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	dir := a.globalDir(cfg)
	if dir == "" {
		return nil, nil
	}
	index := filepath.Join(dir, projectsFile)
	raw, err := os.ReadFile(index)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("crush: read %s: %w", index, err)
	}
	var idx projectIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("crush: parse %s: %w", index, err)
	}

	seen := make(map[string]struct{})
	var srcs []adapter.Source
	for _, p := range idx.Projects {
		if err := ctx.Err(); err != nil {
			return srcs, err
		}
		dataDir := strings.TrimSpace(p.DataDir)
		projPath := strings.TrimSpace(p.Path)
		if dataDir == "" {
			continue
		}
		// Crush absolutises the data directory before registering it. A
		// relative value is resolved against the project it belongs to rather
		// than against this process's working directory, which is unrelated.
		if !filepath.IsAbs(dataDir) {
			if projPath == "" {
				continue
			}
			dataDir = filepath.Join(projPath, dataDir)
		}
		db := filepath.Join(dataDir, dbName)
		if !isFile(db) {
			continue
		}
		if _, dup := seen[db]; dup {
			continue
		}
		seen[db] = struct{}{}
		srcs = append(srcs, adapter.Source{
			Tool:  model.ToolCrush,
			Class: model.EventLevel,
			Path:  db,
			Label: "Crush sessions: " + db,
			Meta:  map[string]string{"project": projPath, "dataDir": dataDir},
		})
	}
	return srcs, nil
}

// ckptState is the incremental gate plus the cost accounting.
//
// Level 1 is the db + WAL file stamps: every cost change is an UPDATE on
// sessions (Crush's own AFTER UPDATE trigger rewrites updated_at with it), so
// equal stamps mean the database cannot hold new cost and is not even opened.
//
// Cost is the per-session watermark in micro-USD: how much of that session's
// accumulator has already been appended to the ledger. It is state, not
// history — losing it costs a re-read, and the dedup key is built from the
// watermark being advanced TO, so a re-read of an unchanged database mints keys
// that already exist and conflict-skips.
type ckptState struct {
	DBSize   int64            `json:"dbSize"`
	DBMTime  int64            `json:"dbMtime"`
	WALSize  int64            `json:"walSize"`
	WALMTime int64            `json:"walMtime"`
	Cost     map[string]int64 `json:"cost,omitempty"`
}

// rawPayload is the audit blob. It is an explicit allow-list of accounting,
// model and identity fields; no session title, no message text, no tool input
// and no file path is read anywhere in this package, so none can reach it.
//
// The two assigned columns are recorded under names that state what they are.
// They exist here to preserve what the source claimed at the moment of the
// charge, NOT as a backfill source: nothing may ever read them as tokens.
type rawPayload struct {
	CostUSD                  float64 `json:"cost_usd"`
	CostMicroUSD             int64   `json:"cost_micro_usd"`
	CostDeltaMicroUSD        int64   `json:"cost_delta_micro_usd"`
	Model                    string  `json:"model,omitempty"`
	Provider                 string  `json:"provider,omitempty"`
	ModelsSeen               int     `json:"models_seen"`
	PromptTokensAssigned     int64   `json:"session_prompt_tokens_assigned"`
	CompletionTokensAssigned int64   `json:"session_completion_tokens_assigned"`
}

// sessionRow is one row of sessionsQuery.
type sessionRow struct {
	id         string
	parent     string
	prompt     int64
	completion int64
	cost       float64
	createdAt  int64
	updatedAt  int64
}

// Collect reads a single Crush database in full.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental applies the file-stamp gate and appends each session's
// cost growth since the stored watermark.
//
// Every failure that stops the read returns NO events and NO checkpoint. That
// is safe in a way it would not be for an append-only source: Crush's cost
// lives in a current value that stays in the database until it is read, so a
// deferred pass loses nothing and the next one re-reads the same accumulator.
// Emitting a partially enriched row instead would put a permanent claim in an
// append-only ledger to avoid a delay that costs nothing.
//
// An individual malformed ROW is the exception: the rows around it are charged
// and the checkpoint lands, with the error reported alongside. The watermark of
// a row that could not be read is carried forward rather than dropped — see
// carryUnreadable, which is what stops a later readable pass from charging that
// session's whole accumulator a second time.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	gate := ckptState{}
	gate.DBSize, gate.DBMTime = fileStamp(src.Path)
	gate.WALSize, gate.WALMTime = fileStamp(src.Path + "-wal")

	var prev ckptState
	if cp != nil && cp.State != "" {
		if err := json.Unmarshal([]byte(cp.State), &prev); err != nil {
			return adapter.Observation{}, fmt.Errorf("crush: decode checkpoint for %s: %w", src.Path, err)
		}
		if prev.DBSize <= 0 {
			return adapter.Observation{}, fmt.Errorf("crush: checkpoint for %s has no database stamp", src.Path)
		}
		if prev.DBSize == gate.DBSize && prev.DBMTime == gate.DBMTime &&
			prev.WALSize == gate.WALSize && prev.WALMTime == gate.WALMTime {
			return adapter.Observation{}, nil // untouched db: skip, keep stored checkpoint
		}
	}

	db, err := openReadOnly(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("crush: open %s: %w", src.Path, err)
	}
	defer db.Close()

	sessions, unreadable, err := readSessions(ctx, db)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("crush: sessions %s: %w", src.Path, err)
	}
	skipped := unreadable
	// The model map is required, not best-effort: a row appended without its
	// provider keeps that gap forever, while a skipped pass keeps nothing.
	models, err := readModels(ctx, db, sessions)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("crush: messages %s: %w", src.Path, err)
	}

	gate.Cost = make(map[string]int64, len(prev.Cost))
	events := make([]model.UsageEvent, 0, 4)
	project := src.Meta["project"]
	for _, s := range sessions {
		// Sub-agent sessions are excluded: runSubAgent already added this
		// session's cost to its parent, and the parent is what gets emitted.
		if s.parent != "" {
			continue
		}
		// The watermark is read before the cost is, and carried forward even
		// when the row is unreadable: dropping it would let a row that becomes
		// readable again re-charge everything already appended for it.
		mark := prev.Cost[s.id]
		if cur, ok := microUSD(s.cost); !ok {
			skipped++
		} else if cur > mark {
			events = append(events, a.event(src, project, s, models[s.id], mark, cur))
			mark = cur
		}
		if mark > 0 {
			gate.Cost[s.id] = mark
		}
	}
	// The loop above garbage collects the watermark of every session that is no
	// longer in the table, which is only sound when the table was read in full.
	if unreadable > 0 {
		carryUnreadable(gate.Cost, prev.Cost, sessions)
	}

	obs := adapter.Observation{Events: events}
	if state, err := json.Marshal(gate); err == nil {
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: model.ToolCrush, SourcePath: src.Path, State: string(state),
		}
	}
	if skipped > 0 {
		return obs, fmt.Errorf("crush: skipped %d malformed session row(s) in %s", skipped, src.Path)
	}
	return obs, nil
}

// event builds the immutable charge for one session's growth from prev to cur
// micro-USD. Every token field stays zero: Crush reports none, and the columns
// that look like tokens are the assigned ones this package refuses to read.
func (a Adapter) event(src adapter.Source, project string, s sessionRow, attr attribution, prev, cur int64) model.UsageEvent {
	ts := eventTime(s)
	ev := model.UsageEvent{
		Tool: model.ToolCrush,
		// Model is deliberately empty. See the package doc: a zero-token charge
		// against a known model prices to 0 and the collector would stamp that
		// over the harness's own figure.
		Model:      "",
		Provider:   attr.provider,
		SessionID:  s.id,
		Project:    project,
		EventTime:  ts,
		SourcePath: src.Path,
		DedupKey:   model.ToolCrush + "|cost|" + s.id + "|" + strconv.FormatInt(cur, 10),
		Kind:       model.KindUsage,
		Raw: rawJSON(rawPayload{
			CostUSD:                  s.cost,
			CostMicroUSD:             cur,
			CostDeltaMicroUSD:        cur - prev,
			Model:                    attr.model,
			Provider:                 attr.provider,
			ModelsSeen:               attr.seen,
			PromptTokensAssigned:     adapter.NonNeg(s.prompt),
			CompletionTokensAssigned: adapter.NonNeg(s.completion),
		}),
	}
	ev.SetCost(cur-prev, PriceSourceReported)
	return ev
}

// carryUnreadable copies forward the watermark of every session this pass did
// not manage to read.
//
// A row that failed to scan is not a row that ceased to exist, and the two are
// indistinguishable from the result set: both are simply absent. The watermark
// map is otherwise rebuilt from the rows actually read, so an unreadable row
// silently loses the record of what has already been charged for it — and the
// next pass that CAN read it charges the whole accumulator again, under a dedup
// key naming a total the ledger has never seen and therefore cannot collapse.
// Measured before this existed: one unreadable pass over the costly fixture,
// followed by growth from 2.5 to 3.0, charged sess-parent 2500000 and then
// 3000000 micro-USD for 3000000 of real spend.
//
// Garbage collection still happens on every pass that read the table in full,
// which is the only situation where a missing id really does mean a session
// Crush deleted.
func carryUnreadable(dst, prev map[string]int64, sessions []sessionRow) {
	if len(prev) == 0 {
		return
	}
	read := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		read[s.id] = struct{}{}
	}
	for id, mark := range prev {
		if _, ok := read[id]; ok || mark <= 0 {
			continue
		}
		dst[id] = mark
	}
}

// readSessions loads every session row, returning the rows and the number of
// permanently unreadable ones.
func readSessions(ctx context.Context, db *sql.DB) ([]sessionRow, int, error) {
	rows, err := db.QueryContext(ctx, sessionsQuery)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []sessionRow
	var skipped int
	for rows.Next() {
		var (
			s                    sessionRow
			prompt, completion   sql.NullInt64
			cost                 sql.NullFloat64
			createdAt, updatedAt sql.NullInt64
		)
		if err := rows.Scan(&s.id, &s.parent, &prompt, &completion, &cost, &createdAt, &updatedAt); err != nil {
			skipped++
			continue
		}
		s.id = strings.TrimSpace(s.id)
		s.parent = strings.TrimSpace(s.parent)
		if s.id == "" {
			skipped++
			continue
		}
		s.prompt, s.completion = prompt.Int64, completion.Int64
		s.cost = cost.Float64
		s.createdAt, s.updatedAt = createdAt.Int64, updatedAt.Int64
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, skipped, err
	}
	return out, skipped, nil
}

// attribution is what a session's messages agree its model and provider were.
// Anything short of unanimity leaves both empty: a session whose messages name
// two providers has no single billing identity, and picking one would invent
// an attribution the source does not support.
type attribution struct {
	model    string
	provider string
	seen     int
}

// readModels folds every message's (provider, model) pair onto the ROOT session
// that will be emitted, so a sub-agent's model counts towards the parent whose
// cost absorbed it.
func readModels(ctx context.Context, db *sql.DB, sessions []sessionRow) (map[string]attribution, error) {
	parents := make(map[string]string, len(sessions))
	for _, s := range sessions {
		parents[s.id] = s.parent
	}

	rows, err := db.QueryContext(ctx, modelsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pairs := make(map[string]map[string]attribution)
	for rows.Next() {
		var sessionID, provider, mdl string
		if err := rows.Scan(&sessionID, &provider, &mdl); err != nil {
			continue
		}
		mdl = strings.TrimSpace(mdl)
		if mdl == "" {
			continue
		}
		root := rootOf(parents, strings.TrimSpace(sessionID))
		if root == "" {
			continue
		}
		provider = strings.TrimSpace(provider)
		set := pairs[root]
		if set == nil {
			set = make(map[string]attribution)
			pairs[root] = set
		}
		set[provider+"\x00"+mdl] = attribution{model: mdl, provider: provider}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]attribution, len(pairs))
	for root, set := range pairs {
		a := attribution{seen: len(set)}
		if len(set) == 1 {
			for _, only := range set {
				a.model, a.provider = only.model, only.provider
			}
		}
		out[root] = a
	}
	return out, nil
}

// rootOf walks a session up to the ancestor with no parent. A missing parent
// row or a cycle stops the walk where it is; the result is only ever used to
// group model names, never to move cost.
func rootOf(parents map[string]string, id string) string {
	cur := id
	for i := 0; i < maxParentDepth; i++ {
		p, known := parents[cur]
		if !known || p == "" || p == cur {
			return cur
		}
		cur = p
	}
	return cur
}

// microUSD converts Crush's REAL dollar accumulator to micro-USD, rounding half
// away from zero. A non-finite or negative value is not a cost and is refused
// (Crush's own CHECK forbids a negative, so this only ever fires on corruption).
func microUSD(cost float64) (int64, bool) {
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return 0, false
	}
	micro := math.Round(cost * 1e6)
	if micro > math.MaxInt64/2 {
		return 0, false
	}
	return int64(micro), true
}

// eventTime places the charge at the session's last write, falling back to its
// creation and then to the collector's own clock. updated_at moves on every
// session UPDATE (Crush installs an AFTER UPDATE trigger that rewrites it), so
// it is the closest stamp to the cost change this event accounts for.
func eventTime(s sessionRow) time.Time {
	for _, raw := range []int64{s.updatedAt, s.createdAt} {
		if t, ok := unixStamp(raw); ok {
			return t
		}
	}
	return time.Now().UTC()
}

// unixStamp reads one of Crush's integer timestamps. Its schema comments claim
// milliseconds and every writer emits seconds; magnitude decides which this
// value is, so a future Crush that finally honours its own comment does not
// scatter events across the year 58000.
func unixStamp(v int64) (time.Time, bool) {
	if v <= 0 {
		return time.Time{}, false
	}
	if v >= msThreshold {
		return time.UnixMilli(v).UTC(), true
	}
	return time.Unix(v, 0).UTC(), true
}

// rawJSON marshals the audit payload, returning "" if it somehow cannot be
// encoded — a missing audit blob is never worth failing a charge over.
func rawJSON(p rawPayload) string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// openReadOnly opens a Crush database strictly read-only. mode=ro prevents
// create/write/lock; query_only additionally refuses any write statement on the
// connection; busy_timeout keeps a transient lock from failing the poll.
// immutable=1 is deliberately NOT used: crush.db is written concurrently by a
// running Crush, and SQLite documents wrong results when an immutable-flagged
// file changes underneath a reader.
func openReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	dsn := u.String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// fileStamp returns (size, mtimeNS) for path, or (-1, 0) when absent — a WAL
// appearing or vanishing must break gate equality.
func fileStamp(path string) (int64, int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return -1, 0
	}
	return fi.Size(), fi.ModTime().UnixNano()
}

// isFile reports whether path exists and is a regular file.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
