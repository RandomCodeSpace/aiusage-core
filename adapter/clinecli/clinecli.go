// Package clinecli implements the event-level adapter for the Cline CLI
// (`cline`, npm package "cline", Apache-2.0). The VS Code extension writes a
// different, id-less surface and is deliberately NOT read here.
//
// # Two surfaces, each authoritative for one thing
//
// USAGE comes from the per-session message document
// <sessions>/<session id>/<session id>.messages.json. Every assistant message
// carries its own `metrics` block — inputTokens, outputTokens, cacheReadTokens,
// cacheWriteTokens — a stable `id` (nanoid, e.g. "msg_BZEHy5S0"), a `ts` in unix
// milliseconds and a `modelInfo{id, provider}`. One assistant message is one API
// response, so one message with metrics is one usage event.
//
// DISCOVERY comes from <db>/sessions.db, opened read-only. Its `sessions` table
// names each session's `messages_path` outright, along with the `cwd` that
// becomes the event's project. It is an INDEX, never a usage source: the table
// has no token columns at all. A directory walk runs after it and picks up any
// message document the index does not name, so a pruned or missing index costs
// discovery speed and metadata, never events.
//
// # Path resolution follows Cline's own, not the third-party parsers
//
// Read out of the shipped CLI bundle (cline 3.0.55), the chain is:
//
//	root     = $CLINE_DIR              or <home>/.cline
//	data     = $CLINE_DATA_DIR         or <root>/data
//	sessions = $CLINE_SESSION_DATA_DIR or <data>/sessions
//	db       = $CLINE_DB_DATA_DIR      or <data>/db
//
// Two traps are baked into that. The `data/` level is real — documentation and
// third-party parsers that spell the path ~/.cline/sessions are describing a
// layout this CLI does not write. And CLINE_SESSION_DATA_DIR IS the sessions
// directory, not a parent containing one: appending "sessions" to it (as the
// harness matrix row does) looks at a directory that never exists, so an
// adapter that "supports" the variable that way silently collects nothing from
// exactly the machines that set it.
//
// # Traps this adapter is built around
//
// CUMULATIVE-VS-EVENT. The session sidecar <session id>.json carries
// `metadata.usage` and `metadata.aggregateUsage`, and both are running totals of
// the very message metrics read here — measured on a live two-turn session,
// metadata.usage was 9592/37 against per-message metrics of 4770/20 and 4822/17.
// The same object is copied into sessions.metadata_json in the index database.
// Reading either alongside the messages would count every token twice, and
// aggregateUsage additionally folds in subagent sessions that carry their own
// message documents. Neither is read: the per-message metrics are the events,
// and the accumulators are ignored everywhere.
//
// CACHE TOKENS ARE NOT UNIFORMLY ADDITIVE. Cline's own cost function prices a
// message as (inputTokens - cacheReadTokens - cacheWriteTokens) at the input
// rate plus the two cache buckets at their own rates, i.e. it treats the cache
// counts as a SUBSET of inputTokens. But its usage normaliser copies each
// upstream provider's field verbatim — OpenAI's `prompt_tokens` includes cached
// tokens, Anthropic's `input_tokens` excludes them — so the relationship is a
// property of the provider behind the request, not of Cline. The subset test is
// therefore made per event, on the only evidence available: when
// cacheRead+cacheWrite fits inside inputTokens they are treated as a subset and
// subtracted out of the input component; when it does not, they cannot be one
// and are treated as additive. Either way the four components sum to exactly the
// stored total, so no token is counted twice and none is discarded. The residual
// error is bounded and one-directional: an Anthropic-backed request whose cache
// write happens to fit inside its input count is UNDERSTATED by that write,
// never overstated.
//
// SPLIT IDENTITY, INVERTED. The message document is not append-only — Cline
// rewrites the whole JSON on every save — so a byte-offset tail read is
// meaningless and every read is a full parse of the document. The identities
// survive it: message ids are persisted in the file and reappear unchanged on
// every rewrite, so the dedup keys collapse re-reads. The gate is the file's
// size and mtime.
//
// Message ids are minted locally (Io("msg") in the bundle), not handed down by
// the provider, so they are only unique within their own message stream. The
// dedup key is scoped accordingly: cline|<session id>|<agent>|<message id>,
// where the agent is the document's own `agent` field ("lead" for the top-level
// stream). A collision across two sessions is unrepresentable rather than
// merely improbable.
//
// # Activity
//
// A tool call lives in the SAME assistant message as the metrics it was billed
// under — one `{"type":"tool_use","id":"call_...","name":"run_commands"}` block
// in that message's content — so the join is exact and the divisor is the number
// of tool_use blocks in that one message. Nothing is inferred from adjacency. A
// call in a message with no metrics is emitted with an empty UsageDedupKey: the
// call is an observed fact whose cost is unknown, never free.
//
// Only kind=tool is emitted. The CLI's hook log is a separate file
// ($CLINE_HOOKS_LOG_PATH, default <data>/logs/hooks.jsonl) with no usage join,
// and a skill invocation is not distinguishable from a tool call in this
// surface, so neither is invented here.
//
// PRIVACY: names and counts only, by construction. Content blocks are decoded
// through a three-field allow-list — type, id, name — so a block's `text`, a
// tool call's `input` and a tool result's `content` have no field to land in and
// never become values in this process. The document's `system_prompt` and the
// sidecar's `prompt` are likewise never decoded. The audit payload is
// re-marshalled from an allow-list struct rather than kept as source bytes.
//
// # Not built (yet), and why
//
// The index carries `is_subagent`, `parent_session_id`, `parent_agent_id`,
// `agent_id` and `conversation_id`, and each message document names its own
// `agent`. Together those are the handle a future DimensionAgent turn context
// would hang on — a subagent session's every turn ran under its parent. It is
// deliberately not built here: no subagent session exists on the verification
// machine, and attribution guessed from an unexercised schema is exactly the
// kind of claim this ledger must not make.
//
// CRITICAL: strictly read-only. Message documents are opened O_RDONLY; the index
// database uses a read-only DSN (mode=ro plus query_only(1)) and never
// immutable=1, because Cline holds sessions.db open and writes it live with a
// WAL an immutable reader would not see. Nothing under the agent's directories
// is created, locked or modified.
package clinecli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0)

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

const (
	// DirEnv moves the Cline root directory (default <home>/.cline), and with it
	// every path below.
	DirEnv = "CLINE_DIR"
	// DataDirEnv moves the Cline data directory (default <root>/data), and with
	// it both the sessions tree and the index database.
	DataDirEnv = "CLINE_DATA_DIR"
	// SessionDataDirEnv moves the sessions directory itself (default
	// <data>/sessions) — the directory that holds <session id>/ subdirectories,
	// NOT a parent containing one.
	SessionDataDirEnv = "CLINE_SESSION_DATA_DIR"
	// DBDataDirEnv moves the database directory (default <data>/db) that holds
	// the sessions.db discovery index.
	DBDataDirEnv = "CLINE_DB_DATA_DIR"

	// messagesSuffix is the message document's compound extension.
	messagesSuffix = ".messages.json"
	// indexDBName is the discovery index inside the database directory.
	indexDBName = "sessions.db"
	// driverName is the modernc.org/sqlite database/sql driver name.
	driverName = "sqlite"

	// defaultAgent labels a document that names no agent stream.
	defaultAgent = "lead"
	// defaultProject labels a session whose cwd is unknown.
	defaultProject = "cline"

	// roleAssistant is the only role that carries usage or tool calls.
	roleAssistant = "assistant"
	// blockToolUse is the content-block type of a tool call.
	blockToolUse = "tool_use"

	// Source.Meta keys carrying the index metadata Collect reuses.
	metaSession  = "session"
	metaCwd      = "cwd"
	metaModel    = "model"
	metaProvider = "provider"
)

// indexQuery lists every session the index names a message document for. The
// token-bearing `metadata_json` column is deliberately NOT selected: it holds
// the running usage accumulator, and this adapter's whole accounting depends on
// that number never being added to the per-message metrics it summarises.
const indexQuery = `SELECT session_id, messages_path, cwd, workspace_root, provider, model
	FROM sessions
	WHERE messages_path IS NOT NULL AND TRIM(messages_path) != ''`

// Adapter reads Cline CLI session message documents. Read-only.
type Adapter struct{}

// New returns a Cline CLI adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolCline }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Cline" }

// Capabilities declares what this project can say about Cline.
//
// Cost is COMPUTED: nothing here calls SetCost. Activity is an EXACT join —
// buildActivity names the usage row that paid for each call, because the
// tool_use block sits in the SAME message as the metrics it was billed under.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolCline,
		Cost:      model.CostComputed,
		Activity:  model.ActivityExact,
		Reasoning: model.ReasoningReportFor(model.ToolCline),
		Tier:      model.TierLive,
	}
}

// layout is one resolved Cline installation: where the message documents live
// and where the discovery index lives.
type layout struct {
	sessions string
	db       string
}

// resolve reproduces Cline's own path chain (see the package comment). An
// explicit aiusage override replaces the whole chain and is normalised, since a
// user pointing at a Cline installation may reasonably name either the root or
// the data directory inside it; the environment variables are honoured exactly
// as the CLI defines them, with no such guessing.
func (a Adapter) resolve(cfg adapter.DiscoverConfig) layout {
	if cfg.Overrides != nil {
		if v := strings.TrimSpace(cfg.Overrides[model.ToolCline]); v != "" {
			data := normaliseDataDir(v)
			return layout{
				sessions: filepath.Join(data, "sessions"),
				db:       filepath.Join(data, "db"),
			}
		}
	}

	// Each variable is read through its own package constant at its own call
	// site: internal/cmd's discovery-environment guard parses these sources and
	// resolves the argument statically, so a shared getenv helper would leave it
	// unable to tell which variable is being read.
	//
	// Each link is derived from the one above it only when it was not named
	// outright. An unresolvable home therefore costs the DEFAULTS, never the
	// variables: a machine where os.UserHomeDir fails still collects from the
	// directory CLINE_SESSION_DATA_DIR points at, the same way codex honours
	// CODEX_HOME and claudecode CLAUDE_CONFIG_DIR without one. A link nothing
	// named stays EMPTY rather than becoming a relative path, so a blank
	// database directory can never turn into a `sessions.db` in whatever
	// directory the daemon happens to have been started from.
	root := strings.TrimSpace(os.Getenv(DirEnv))
	if root == "" && cfg.Home != "" {
		root = filepath.Join(cfg.Home, ".cline")
	}
	data := strings.TrimSpace(os.Getenv(DataDirEnv))
	if data == "" && root != "" {
		data = filepath.Join(root, "data")
	}
	sessions := strings.TrimSpace(os.Getenv(SessionDataDirEnv))
	if sessions == "" && data != "" {
		sessions = filepath.Join(data, "sessions")
	}
	db := strings.TrimSpace(os.Getenv(DBDataDirEnv))
	if db == "" && data != "" {
		db = filepath.Join(data, "db")
	}
	return layout{sessions: sessions, db: db}
}

// normaliseDataDir accepts either a Cline root or the data directory inside it
// and returns the data directory. A path that already holds sessions/ or db/ is
// taken as-is; one that holds data/ is descended into; anything else is taken at
// face value so a not-yet-populated directory still resolves.
func normaliseDataDir(p string) string {
	clean := filepath.Clean(p)
	if adapter.IsDir(filepath.Join(clean, "sessions")) || adapter.IsDir(filepath.Join(clean, "db")) {
		return clean
	}
	if adapter.IsDir(filepath.Join(clean, "data")) {
		return filepath.Join(clean, "data")
	}
	return clean
}

// Discover lists one source per session message document: first every document
// the index database names, then every document under the sessions tree the
// index did not. Both halves are needed. The index alone would lose a session
// whose row was pruned or whose database is on a machine that never ran the
// migration; the walk alone would lose the cwd that becomes the project, and
// with it a message document the index points at from outside the tree.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	l := a.resolve(cfg)
	if l.sessions == "" && l.db == "" {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var srcs []adapter.Source

	for _, row := range l.indexRows(ctx) {
		if ctx.Err() != nil {
			return srcs, ctx.Err()
		}
		path := absClean(row.messagesPath)
		if path == "" || !isFile(path) {
			continue // stale index row: the document is gone
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		srcs = append(srcs, newSource(path, row.sessionID, row.project(), row.model, row.provider))
	}

	for _, path := range walkMessageDocs(ctx, l.sessions) {
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		srcs = append(srcs, newSource(path, sessionIDFromPath(path), "", "", ""))
	}
	return srcs, ctx.Err()
}

// newSource builds one message-document source. Empty metadata is omitted rather
// than stored blank, so Collect can tell "the index said nothing" from "the
// index said empty" and fall back to the session sidecar.
func newSource(path, sessionID, project, mdl, provider string) adapter.Source {
	meta := map[string]string{}
	for k, v := range map[string]string{
		metaSession:  sessionID,
		metaCwd:      project,
		metaModel:    mdl,
		metaProvider: provider,
	} {
		if v = strings.TrimSpace(v); v != "" {
			meta[k] = v
		}
	}
	label := "cline session " + sessionID
	if sessionID == "" {
		label = "cline session " + filepath.Base(path)
	}
	return adapter.Source{
		Tool:  model.ToolCline,
		Class: model.EventLevel,
		Path:  path,
		Label: label,
		Meta:  meta,
	}
}

// indexRow is one row of the discovery index.
type indexRow struct {
	sessionID     string
	messagesPath  string
	cwd           string
	workspaceRoot string
	provider      string
	model         string
}

// project returns the row's project attribution: the working directory the
// session ran in, falling back to the workspace root.
func (r indexRow) project() string {
	if v := strings.TrimSpace(r.cwd); v != "" {
		return v
	}
	return strings.TrimSpace(r.workspaceRoot)
}

// indexRows reads this layout's discovery index, if it named a directory to
// hold one. An unnamed database directory yields no rows rather than a relative
// probe for a `sessions.db` in the process's working directory.
func (l layout) indexRows(ctx context.Context) []indexRow {
	if l.db == "" {
		return nil
	}
	return indexRows(ctx, filepath.Join(l.db, indexDBName))
}

// indexRows reads the discovery index read-only. Every failure is swallowed and
// answered with no rows: the index is an accelerator, and the directory walk
// behind it finds the same documents without it. A missing database, an older
// schema without the `sessions` table and a database locked mid-write all land
// here and none of them may cost the cycle its usage.
func indexRows(ctx context.Context, path string) []indexRow {
	if !isFile(path) {
		return nil
	}
	// mode=ro cannot create/write/lock; query_only(1) refuses a write statement
	// on the connection; busy_timeout survives a transient lock. immutable=1 is
	// deliberately absent — Cline writes this database live and keeps a WAL an
	// immutable reader ignores entirely, which is how a reader ends up quietly
	// describing the database as of its last checkpoint.
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	dsn := u.String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	rows, err := db.QueryContext(ctx, indexQuery)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []indexRow
	for rows.Next() {
		var (
			id, msgs                      sql.NullString
			cwd, workspace, provider, mdl sql.NullString
		)
		if err := rows.Scan(&id, &msgs, &cwd, &workspace, &provider, &mdl); err != nil {
			continue
		}
		out = append(out, indexRow{
			sessionID:     strings.TrimSpace(id.String),
			messagesPath:  strings.TrimSpace(msgs.String),
			cwd:           cwd.String,
			workspaceRoot: workspace.String,
			provider:      strings.TrimSpace(provider.String),
			model:         strings.TrimSpace(mdl.String),
		})
	}
	if rows.Err() != nil {
		return out // partial index is still a superset of nothing
	}
	return out
}

// walkMessageDocs lists <sessions>/*/*.messages.json. The tree is exactly two
// levels deep, so os.ReadDir twice is cheaper and more predictable than a
// recursive walk, and it cannot wander into a session's checkpoint or log data.
func walkMessageDocs(ctx context.Context, sessionsDir string) []string {
	if sessionsDir == "" || !adapter.IsDir(sessionsDir) {
		return nil
	}
	dirs, err := os.ReadDir(sessionsDir) // sorted by name: deterministic order
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range dirs {
		if ctx.Err() != nil {
			return out
		}
		sub := filepath.Join(sessionsDir, d.Name())
		if !adapter.IsDir(sub) {
			continue
		}
		entries, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), messagesSuffix) {
				continue
			}
			path := filepath.Join(sub, e.Name())
			if !adapter.WalkEntryIsFile(e, path) {
				continue
			}
			out = append(out, absClean(path))
		}
	}
	return out
}

// sessionIDFromPath recovers the session id from <id>/<id>.messages.json,
// preferring the file name over the directory name (the file is what Cline
// names after the session).
func sessionIDFromPath(path string) string {
	base := filepath.Base(path)
	if id := strings.TrimSuffix(base, messagesSuffix); id != base {
		return id
	}
	return filepath.Base(filepath.Dir(path))
}

// Collect reads one session message document read-only.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental gates the document on its size and mtime and otherwise
// re-parses it in full. There is no tail read to be had: Cline rewrites the
// whole JSON document on every save, so a byte offset into the previous version
// points into the middle of a different file. The full re-parse is safe because
// the message ids it re-derives are the persisted dedup keys, which conflict-skip
// on insert.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	fi, err := os.Stat(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("cline: stat %s: %w", src.Path, err)
	}
	size, mtime := fi.Size(), fi.ModTime().UnixNano()
	if cp != nil && cp.Size == size && cp.MTimeNS == mtime {
		return adapter.Observation{}, nil // untouched document: keep the stored checkpoint
	}

	raw, err := os.ReadFile(src.Path) // read-only
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("cline: read %s: %w", src.Path, err)
	}
	if ctx.Err() != nil {
		return adapter.Observation{}, ctx.Err()
	}

	var d document
	// A type mismatch on one field still populates the rest, so the parse is
	// only fatal when it produced no messages at all.
	if err := json.Unmarshal(raw, &d); err != nil && len(d.Messages) == 0 {
		return adapter.Observation{}, fmt.Errorf("cline: parse %s: %w", src.Path, err)
	}

	obs := a.build(d, src)
	obs.Checkpoint = &model.SourceCheckpoint{
		Tool: model.ToolCline, SourcePath: src.Path, Size: size, MTimeNS: mtime,
	}
	return obs, nil
}

// build turns one parsed document into its events and activity.
func (a Adapter) build(d document, src adapter.Source) adapter.Observation {
	sessionID := strings.TrimSpace(d.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(src.Meta[metaSession])
	}
	if sessionID == "" {
		sessionID = sessionIDFromPath(src.Path)
	}
	agent := strings.TrimSpace(d.Agent)
	if agent == "" {
		agent = defaultAgent
	}
	project := a.project(src)
	fallbackTime := parseISO(d.UpdatedAt)

	var (
		events   []model.UsageEvent
		activity []model.ActivityEvent
	)
	for _, m := range d.Messages {
		if !strings.EqualFold(strings.TrimSpace(m.Role), roleAssistant) {
			continue // only an assistant message carries usage or tool calls
		}
		msgID := strings.TrimSpace(m.ID)
		if msgID == "" {
			continue // no stable identity: neither a key nor a divisor is honest
		}
		key := dedupKey(sessionID, agent, msgID)
		when := m.eventTime(fallbackTime)
		if when.IsZero() {
			continue // a record that cannot be placed in time cannot be reported
		}

		mdl := m.modelID(src.Meta[metaModel])
		usageKey := ""
		if ev, ok := buildEvent(m, key, sessionID, project, mdl,
			m.providerID(src.Meta[metaProvider]), when, src.Path); ok {
			usageKey = ev.DedupKey
			events = append(events, ev)
		}
		activity = append(activity, buildActivity(m, key, usageKey, sessionID, project, mdl, when, src.Path)...)
	}
	return adapter.Observation{Events: events, Activity: activity}
}

// project resolves the workspace path an event is attributed to: the index's
// cwd when discovery had one, else the session sidecar written next to the
// message document, else a constant label. The sidecar is only opened on the
// fallback path, and only for a document that changed, so the common case costs
// no extra read.
func (a Adapter) project(src adapter.Source) string {
	if v := strings.TrimSpace(src.Meta[metaCwd]); v != "" {
		return v
	}
	if v := sidecarProject(src.Path); v != "" {
		return v
	}
	return defaultProject
}

// sidecarProject reads <session id>.json beside the message document for its
// cwd. The sidecar also holds the user's prompt, the system prompt and the
// running usage accumulator; none of the three has a field in `sessionFile` to
// decode into, so none of them becomes a value here.
func sidecarProject(messagesPath string) string {
	base := filepath.Base(messagesPath)
	id := strings.TrimSuffix(base, messagesSuffix)
	if id == base {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(messagesPath), id+".json"))
	if err != nil {
		return ""
	}
	var s sessionFile
	if err := json.Unmarshal(raw, &s); err != nil && s.Cwd == "" {
		return ""
	}
	if v := strings.TrimSpace(s.Cwd); v != "" {
		return v
	}
	return strings.TrimSpace(s.WorkspaceRoot)
}

// dedupKey is the persisted usage key. Message ids are minted by the CLI rather
// than the provider, so they are scoped by the session and the agent stream they
// belong to; the session id is a millisecond stamp plus a random suffix and is
// globally unique.
func dedupKey(sessionID, agent, messageID string) string {
	return "cline|" + sessionID + "|" + agent + "|" + messageID
}

// buildEvent maps one assistant message's metrics onto a usage event. Returns
// ok=false when the message reports no metrics or accounts for no tokens at all.
func buildEvent(m message, key, sessionID, project, mdl, provider string, when time.Time, srcPath string) (model.UsageEvent, bool) {
	if m.Metrics == nil {
		return model.UsageEvent{}, false
	}
	input, output, cacheRead, cacheCreate, total := mapTokens(*m.Metrics)
	if total == 0 {
		return model.UsageEvent{}, false
	}
	return model.UsageEvent{
		Tool:                model.ToolCline,
		Model:               mdl,
		Provider:            provider,
		SessionID:           sessionID,
		Project:             project,
		EventTime:           when,
		InputTokens:         input,
		OutputTokens:        output,
		CacheCreationTokens: cacheCreate,
		CacheReadTokens:     cacheRead,
		TotalTokens:         total,
		MessageID:           strings.TrimSpace(m.ID),
		SourcePath:          srcPath,
		DedupKey:            key,
		Kind:                model.KindUsage,
		Raw:                 m.auditJSON(),
	}, true
}

// mapTokens splits one metrics block into the ledger's components so that they
// sum to exactly the returned total.
//
// The cache buckets are tested against the input count per event rather than
// assumed one way (see the package comment): Cline copies each upstream
// provider's usage fields verbatim, and whether a cached token is already inside
// the input count is that provider's convention, not Cline's. When the two cache
// buckets fit inside inputTokens they are treated as the subset Cline's own
// pricing assumes them to be and are subtracted out of the input component; when
// they do not fit they cannot be a subset and are added on top. Both branches
// keep every reported count and double-count none of them.
func mapTokens(m metrics) (input, output, cacheRead, cacheCreate, total int64) {
	reported := adapter.NonNeg(m.InputTokens)
	output = adapter.NonNeg(m.OutputTokens)
	cacheRead = adapter.NonNeg(m.CacheReadTokens)
	cacheCreate = adapter.NonNeg(m.CacheWriteTokens)

	input = reported
	if cacheRead+cacheCreate <= reported {
		input = reported - cacheRead - cacheCreate
	}
	return input, output, cacheRead, cacheCreate, input + output + cacheRead + cacheCreate
}

// buildActivity emits one row per tool_use block in the message. The blocks and
// the metrics are in the SAME message, so CallsInTurn is the exact number of
// calls billed against that one usage row and usageKey is empty only when the
// message reported no usage at all — an observed call whose cost is unknown,
// never one that was free.
func buildActivity(m message, msgKey, usageKey, sessionID, project, mdl string, when time.Time, srcPath string) []model.ActivityEvent {
	calls := m.toolCalls()
	if len(calls) == 0 {
		return nil
	}
	out := make([]model.ActivityEvent, 0, len(calls))
	for i, c := range calls {
		id := strings.TrimSpace(c.ID)
		if id == "" {
			// An id-less block is identified by its position among the tool calls
			// of this message, which is a property of the document's content
			// rather than of a read: a rewrite reproduces the same blocks in the
			// same order, so the key is stable across polls. A read position — a
			// byte offset, a line number — would not be: this document is
			// rewritten whole on every save.
			id = fmt.Sprintf("idx%d", i)
		}
		out = append(out, model.ActivityEvent{
			Tool:          model.ToolCline,
			Kind:          model.ActivityTool,
			Name:          strings.TrimSpace(c.Name),
			SessionID:     sessionID,
			Project:       project,
			Model:         mdl,
			EventTime:     when,
			UsageDedupKey: usageKey,
			MessageID:     strings.TrimSpace(m.ID),
			TurnSeq:       i,
			CallsInTurn:   len(calls),
			SourcePath:    srcPath,
			DedupKey:      msgKey + "|call|" + id,
		})
	}
	return out
}

// document is the messages.json envelope, decoded through an ALLOW-LIST. The
// document's `system_prompt` has no field here and never becomes a value.
type document struct {
	SessionID string    `json:"sessionId"`
	Agent     string    `json:"agent"`
	UpdatedAt string    `json:"updated_at"`
	Messages  []message `json:"messages"`
}

// message is one conversation message. `content` is held as raw bytes and
// decoded separately through contentBlock's three-field allow-list, so a
// provider that spells content as a bare string costs this adapter its tool
// calls and never its usage.
type message struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	TS        int64           `json:"ts"`
	ModelInfo *modelInfo      `json:"modelInfo"`
	Metrics   *metrics        `json:"metrics"`
	Content   json.RawMessage `json:"content"`
}

// modelInfo names the model that answered the message.
type modelInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// metrics is the per-message usage block: the four counters Cline records for
// one API response. There is no reasoning counter in this shape — the CLI's
// message serialiser writes exactly these four plus an optional cost — so none
// is invented.
type metrics struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

// contentBlock is one content block reduced to the three fields activity needs.
// A tool call's `input`, a text block's `text` and a tool result's `content`
// have NO field here: encoding/json discards them as it parses, so the content
// never becomes a value in this process.
type contentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// sessionFile is the per-session sidecar reduced to the two path fields the
// project attribution needs. Its `prompt`, `metadata.systemPrompt` and
// `metadata.usage` accumulator have no field here.
type sessionFile struct {
	Cwd           string `json:"cwd"`
	WorkspaceRoot string `json:"workspace_root"`
}

// toolCalls returns the message's tool_use blocks, in document order. A block
// with no name records nothing worth grouping by and is dropped.
func (m message) toolCalls() []contentBlock {
	if len(m.Content) == 0 {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	var out []contentBlock
	for _, b := range blocks {
		if b.Type == blockToolUse && strings.TrimSpace(b.Name) != "" {
			out = append(out, b)
		}
	}
	return out
}

// eventTime returns the message's own timestamp, falling back to the document's
// last-write stamp.
func (m message) eventTime(fallback time.Time) time.Time {
	if m.TS > 0 {
		return time.UnixMilli(m.TS).UTC()
	}
	return fallback
}

// modelID returns the message's model, falling back to the session's.
func (m message) modelID(sessionModel string) string {
	if m.ModelInfo != nil {
		if v := strings.TrimSpace(m.ModelInfo.ID); v != "" {
			return v
		}
	}
	return strings.TrimSpace(sessionModel)
}

// providerID returns the message's billing provider, falling back to the
// session's. Cline names it verbatim ("anthropic", "openai-compatible", ...).
func (m message) providerID(sessionProvider string) string {
	if m.ModelInfo != nil {
		if v := strings.TrimSpace(m.ModelInfo.Provider); v != "" {
			return v
		}
	}
	return strings.TrimSpace(sessionProvider)
}

// auditRecord is the allow-list persisted as UsageEvent.Raw. It is re-marshalled
// from decoded fields rather than carried as source bytes, so nothing the
// message holds outside this shape can reach the ledger.
type auditRecord struct {
	ID        string     `json:"id"`
	Role      string     `json:"role"`
	TS        int64      `json:"ts"`
	ModelInfo *modelInfo `json:"modelInfo,omitempty"`
	Metrics   *metrics   `json:"metrics,omitempty"`
}

// auditJSON renders the message's allow-listed fields as the stored audit
// payload. Best-effort: an un-marshalable record yields an empty raw rather than
// dropping the event.
func (m message) auditJSON() string {
	b, err := json.Marshal(auditRecord{
		ID: m.ID, Role: m.Role, TS: m.TS, ModelInfo: m.ModelInfo, Metrics: m.Metrics,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// parseISO parses the document's ISO-8601 write stamp, returning the zero time
// when it is absent or unparseable.
func parseISO(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// absClean normalises a path so the index and the directory walk agree on which
// documents are the same file.
func absClean(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// isFile reports whether path exists and is a regular file.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
