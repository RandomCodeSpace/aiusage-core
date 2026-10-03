// Package goose implements the event-level adapter for the Goose CLI.
//
// Goose keeps ONE SQLite database per data root —
// <data-dir>/sessions/sessions.db — and writes a purpose-built usage ledger
// into it. This adapter reads that ledger, `usage_ledger`, and nothing else
// that counts tokens:
//
//	usage_ledger(id, session_id, created_timestamp, model,
//	             input_tokens, output_tokens, total_tokens,
//	             cache_read_tokens, cache_write_tokens,
//	             cost, cost_source, is_compaction)
//
// WHY THE LEDGER AND NOT `sessions`. The `sessions` table carries lifetime
// counters stamped at the session's created_at, and its `total_tokens` column
// is ASSIGNED, not accumulated: goose's own UPDATE sets `total_tokens = ?` from
// the CURRENT turn while incrementing `accumulated_total_tokens` beside it
// (session_manager.rs record_usage_metrics). Reading `total_tokens` as a total
// reports the last turn as though it were the session; reading the ledger AND
// `accumulated_*` double counts, because SUM(usage_ledger) == accumulated_* by
// construction. So this adapter reads the ledger and NEVER selects a token
// column of `sessions` — only its `working_dir` (project) and `provider_name`
// (billing identity), neither of which is a counter. TestQueryReadsNoSessionCounters
// parses the queries and fails on the day one appears.
//
// CARRIED-FORWARD ROWS ARE INCLUDED. A row with cost_source='carried_forward'
// is inserted as MAX(sessions.accumulated_* - SUM(usage_ledger.*), 0) under a
// WHERE that only fires when the accumulator is AHEAD of the ledger: it is the
// GAP, not a duplicate. Filtering it out undercounts. Those rows carry NO model
// (goose's INSERT ... SELECT never binds one), so a model-is-required guard —
// the obvious way to drop junk rows — would silently delete exactly the
// reconciliation this ledger depends on.
//
// TOKEN SPLIT. Goose normalises EVERY provider to a cache-INCLUSIVE input:
// "input_tokens is the total input including cache read/write tokens; the cache
// fields are breakdown subsets of it" (token_usage.rs), and its total is
// input + output with the cache already inside. aiusage's accounting is the
// Anthropic one — input EXCLUSIVE of cache, and pricing charges input, cache
// read and cache write as three separate lines (pricing.Rates.Cost) — so the
// cached tokens are subtracted out of input here. Passing goose's input through
// unchanged would bill every cached token twice: once at the input rate, once
// at the cache rate. TotalTokens stays the provider's own number, which the
// split reconciles to exactly.
//
// COST. `cost` is NULL under every provider goose has no price for (measured on
// this machine: an ollama session, cost and cost_source both NULL), and an
// unpriced event is not a free one — a NULL cost stays nil, never 0. A real
// cost is stamped with the source goose recorded it under
// ("goose-provider_reported" / "goose-estimated" / "goose-carried_forward"), and
// the collector preserves this vendor stamp. Its pricing ladder applies only
// when the source supplies no usable cost.
//
// ACTIVITY. Tool calls come from `messages`, which is a different table with a
// watermark of its own. They are NEVER attributed: usage_ledger rows carry no
// message id, and two ledger rows commonly share one created_timestamp (both of
// the tool-call session's rows landed on the same second locally), so a
// timestamp match would be a positional guess. UsageDedupKey stays empty and
// the store reports the calls as unattributed rather than as free. See
// activity.go.
//
// CRITICAL: strictly read-only. The DSN is mode=ro plus query_only(1) plus
// busy_timeout — never immutable=1, because goose holds this database open in
// WAL mode and writes it live; an immutable reader ignores the WAL and would
// read a database frozen at its last checkpoint.
package goose

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

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0)

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

const (
	// PathRootEnv relocates every Goose directory at once. goose reads it as an
	// absolute path only (`validated_path_root` filters on is_absolute) and
	// derives its data directory as <root>/data, so the database this adapter
	// opens is <GOOSE_PATH_ROOT>/data/sessions/sessions.db — NOT
	// <GOOSE_PATH_ROOT>/sessions/sessions.db.
	PathRootEnv = "GOOSE_PATH_ROOT"
	// DataHomeEnv is the XDG data root goose falls back to when PathRootEnv is
	// unset: <XDG_DATA_HOME>/goose, defaulting to ~/.local/share/goose.
	DataHomeEnv = "XDG_DATA_HOME"

	sessionsDirName = "sessions"
	dbName          = "sessions.db"
	dataSubdir      = "data"
	driverName      = "sqlite"

	// msTimestampThreshold is goose's OWN rule for reading its timestamp
	// columns: a value above it is milliseconds, below it seconds
	// (`MILLISECOND_TIMESTAMP_THRESHOLD`, session_manager.rs). Imported
	// transcripts land millisecond stamps in `messages`; the ledger writes
	// strftime('%s','now'). Applying the rule to both costs nothing and keeps an
	// imported session out of the year 58000.
	msTimestampThreshold = 10_000_000_000
)

// usageQuery reads the ledger rows above the watermark, joined to their session
// for the two NON-COUNTER attributes `sessions` holds that the ledger does not.
// The LEFT is deliberate: goose deletes a session's ledger rows with the session
// (ON DELETE CASCADE), but a row orphaned by anything else is still an observed
// fact and must not be dropped for want of a project name.
//
// No token column of `sessions` appears here, and none may be added: they are
// either the assigned-not-accumulated `total_tokens` family or the
// `accumulated_*` family that equals SUM(usage_ledger) by construction.
const usageQuery = `SELECT l.id, l.session_id, l.created_timestamp, l.model,
	l.input_tokens, l.output_tokens, l.total_tokens,
	l.cache_read_tokens, l.cache_write_tokens,
	l.cost, l.cost_source, l.is_compaction,
	s.working_dir, s.provider_name
	FROM usage_ledger l
	LEFT JOIN sessions s ON s.id = l.session_id
	WHERE l.id > ?
	ORDER BY l.id`

// Adapter reads the Goose session database. Read-only.
type Adapter struct{}

// New returns a Goose adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolGoose }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Goose" }

// Capabilities declares what this project can say about Goose.
//
// Cost is VENDOR-reported: goose.go stamps priceSource from the provider figure
// the usage_ledger carries, and a NULL cost is left unpriced rather than
// stamped 0 — so a goose row is either vendor-valued or unpriced, never an
// estimate. Activity is RECORDED BUT UNATTRIBUTED because usage_ledger rows
// carry no message id and two of them commonly share one second, so a timestamp
// match would be a positional guess.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolGoose,
		Cost:      model.CostVendor,
		Activity:  model.ActivityUnattributed,
		Reasoning: model.ReasoningReportFor(model.ToolGoose),
		Tier:      model.TierLive,
	}
}

// dataDirs returns the Goose data directories to search. GOOSE_PATH_ROOT wins
// when it is ABSOLUTE, exactly as goose itself resolves it — a relative value is
// ignored by the writer, so honouring one here would point the reader at a
// directory the agent never writes.
func (a Adapter) dataDirs(cfg adapter.DiscoverConfig) []string {
	if env := strings.TrimSpace(os.Getenv(PathRootEnv)); env != "" && filepath.IsAbs(env) {
		return []string{filepath.Join(env, dataSubdir)}
	}
	def := ""
	if xdg := strings.TrimSpace(os.Getenv(DataHomeEnv)); xdg != "" && filepath.IsAbs(xdg) {
		def = filepath.Join(xdg, "goose")
	} else if cfg.Home != "" {
		def = filepath.Join(cfg.Home, ".local", "share", "goose")
	}
	return []string{cfg.Root(model.ToolGoose, def)}
}

// Discover locates each <data-dir>/sessions/sessions.db that exists as a
// regular file.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source
	for _, dir := range a.dataDirs(cfg) {
		if ctx.Err() != nil {
			return srcs, ctx.Err()
		}
		if dir == "" {
			continue
		}
		db := filepath.Join(dir, sessionsDirName, dbName)
		if !isFile(db) {
			continue
		}
		if _, dup := seen[db]; dup {
			continue
		}
		seen[db] = struct{}{}
		srcs = append(srcs, adapter.Source{
			Tool:  model.ToolGoose,
			Class: model.EventLevel,
			Path:  db,
			Label: "Goose sessions: " + db,
			Meta:  map[string]string{"dataDir": dir},
		})
	}
	return srcs, nil
}

// ckptState is the adapter-specific half of the checkpoint. The db + WAL file
// stamps are a gate: goose commits in WAL mode, so every write touches one of
// the two, and equal stamps mean the database cannot hold anything new and is
// not even opened. Messages carries the SECOND watermark — activity comes from
// a different table than usage and advances independently — while the ledger
// watermark rides the checkpoint's own Watermark field.
type ckptState struct {
	DBSize   int64 `json:"dbSize"`
	DBMTime  int64 `json:"dbMtime"`
	WALSize  int64 `json:"walSize"`
	WALMTime int64 `json:"walMtime"`
	Messages int64 `json:"messages,omitempty"`
}

// sameFiles reports whether both file stamps match, i.e. nothing has been
// committed to the database since the last completed read.
func (c ckptState) sameFiles(o ckptState) bool {
	return c.DBSize == o.DBSize && c.DBMTime == o.DBMTime &&
		c.WALSize == o.WALSize && c.WALMTime == o.WALMTime
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

// Collect reads a source in full.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental reads only what the checkpoint has not seen: ledger rows
// above the rowid watermark and message rows above their own. Both tables are
// append-only in goose (INTEGER PRIMARY KEY AUTOINCREMENT, no UPDATE anywhere,
// and AUTOINCREMENT never reuses an id after a session delete), which is what
// makes a rowid watermark sound rather than merely convenient.
//
// A nil cp is a full read. The checkpoint is written ONLY when the read
// completed cleanly: advancing the file stamps past an unreadable row would
// gate the whole database shut and never retry it.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	gate := ckptState{}
	gate.DBSize, gate.DBMTime = fileStamp(src.Path)
	gate.WALSize, gate.WALMTime = fileStamp(src.Path + "-wal")

	usageMark := int64(0)
	msgMark := int64(0)
	if cp != nil {
		usageMark = cp.Watermark
		if cp.State != "" {
			var prev ckptState
			if err := json.Unmarshal([]byte(cp.State), &prev); err == nil {
				msgMark = prev.Messages
				if prev.sameFiles(gate) {
					return adapter.Observation{}, nil // untouched db: keep the stored checkpoint
				}
			}
		}
	}

	db, err := openReadOnly(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("goose: open %s: %w", src.Path, err)
	}
	defer db.Close()

	events, usageConsumed, skipped, clean, err := collectUsage(ctx, db, src.Path, usageMark)
	if err != nil {
		return adapter.Observation{Events: events}, err
	}

	obs := adapter.Observation{Events: events}
	msgConsumed := msgMark
	if calls, consumed, ok := collectActivity(ctx, db, src.Path, msgMark); ok {
		obs.Activity = calls
		msgConsumed = consumed
	} else {
		clean = false // activity read broke: do not gate the db shut behind it
	}

	if clean {
		gate.Messages = msgConsumed
		state, merr := json.Marshal(gate)
		if merr == nil {
			obs.Checkpoint = &model.SourceCheckpoint{
				Tool:       model.ToolGoose,
				SourcePath: src.Path,
				Watermark:  usageConsumed,
				State:      string(state),
			}
		}
	}
	if skipped > 0 {
		return obs, fmt.Errorf("goose: skipped %d unusable ledger row(s) in %s", skipped, src.Path)
	}
	return obs, nil
}

// ledgerRow is one usage_ledger row as read, before any interpretation. Every
// nullable column is scanned as a Null* so a NULL can never be mistaken for a
// zero: `cost` NULL is unpriced and `model` NULL is a carried-forward row, and
// both of those distinctions are load-bearing.
type ledgerRow struct {
	id           int64
	sessionID    string
	createdTS    int64
	model        sql.NullString
	input        sql.NullInt64
	output       sql.NullInt64
	total        sql.NullInt64
	cacheRead    sql.NullInt64
	cacheWrite   sql.NullInt64
	cost         sql.NullFloat64
	costSource   sql.NullString
	isCompaction sql.NullInt64
	workingDir   sql.NullString
	provider     sql.NullString
}

// collectUsage reads the ledger above the watermark and maps each row to an
// immutable event. It returns the events, the highest rowid consumed WITHOUT a
// gap, how many rows were unusable, and whether the pass was clean.
func collectUsage(ctx context.Context, db *sql.DB, path string, watermark int64) ([]model.UsageEvent, int64, int, bool, error) {
	rows, err := db.QueryContext(ctx, usageQuery, watermark)
	if err != nil {
		return nil, watermark, 0, false, fmt.Errorf("goose: query %s: %w", path, err)
	}
	defer rows.Close()

	var (
		events   []model.UsageEvent
		consumed = watermark
		skipped  int
		clean    = true
	)
	for rows.Next() {
		if ctx.Err() != nil {
			return events, consumed, skipped, false, ctx.Err()
		}
		var r ledgerRow
		if err := rows.Scan(&r.id, &r.sessionID, &r.createdTS, &r.model,
			&r.input, &r.output, &r.total, &r.cacheRead, &r.cacheWrite,
			&r.cost, &r.costSource, &r.isCompaction,
			&r.workingDir, &r.provider); err != nil {
			clean = false // unreadable row: retry it next cycle
			continue
		}
		ev, ok, usable := buildEvent(r, path)
		if ok {
			events = append(events, ev)
		} else if !usable {
			skipped++
		}
		if clean {
			consumed = r.id
		}
	}
	if rows.Err() != nil {
		clean = false // iteration broke: do not advance past what was read cleanly
	}
	return events, consumed, skipped, clean, nil
}

// buildEvent maps one ledger row to a usage event. The second result reports
// whether an event was produced; the third whether the row was USABLE at all —
// an all-zero row is skipped silently (nothing was spent), an undated one is
// counted as unusable, and the two must not be conflated in the error line.
func buildEvent(r ledgerRow, path string) (model.UsageEvent, bool, bool) {
	cacheRead := adapter.NonNeg(r.cacheRead.Int64)
	cacheWrite := adapter.NonNeg(r.cacheWrite.Int64)
	inputAll := adapter.NonNeg(r.input.Int64)
	output := adapter.NonNeg(r.output.Int64)
	total := adapter.NonNeg(r.total.Int64)

	// Goose reports input INCLUSIVE of the cache buckets and totals it as
	// input + output. aiusage prices input, cache read and cache write as three
	// separate lines, so the cache has to come back out of input; clamping at
	// zero can only ever understate the input line, never invent one.
	input := inputAll - cacheRead - cacheWrite
	if input < 0 {
		input = 0
	}
	if inputAll+output+total == 0 {
		return model.UsageEvent{}, false, true // nothing was spent: not an error
	}
	if total == 0 {
		total = inputAll + output
	}
	ts := r.createdTS
	if ts > msTimestampThreshold {
		ts /= 1000
	}
	if ts <= 0 {
		return model.UsageEvent{}, false, false // undated: no bucket to place it in
	}

	ev := model.UsageEvent{
		Tool: model.ToolGoose,
		// Model is EMPTY on a carried-forward row, and that is correct: the row
		// reconciles whatever ran, and naming the session's current model would
		// attribute one model's gap to another.
		Model:               strings.TrimSpace(r.model.String),
		Provider:            strings.TrimSpace(r.provider.String),
		SessionID:           r.sessionID,
		Project:             strings.TrimSpace(r.workingDir.String),
		EventTime:           time.Unix(ts, 0).UTC(),
		InputTokens:         input,
		OutputTokens:        output,
		CacheCreationTokens: cacheWrite,
		CacheReadTokens:     cacheRead,
		TotalTokens:         total,
		SourcePath:          path,
		DedupKey:            dedupKey(r.sessionID, r.id, ts),
		Kind:                model.KindUsage,
		Raw:                 rawPayload(r),
	}
	// A NULL cost is UNPRICED, never free: leaving CostMicroUSD nil stores SQL
	// NULL, and 0 would assert the request cost nothing in a table that can
	// never be corrected in place.
	if r.cost.Valid && r.cost.Float64 >= 1e-6 {
		micro := math.Round(r.cost.Float64 * 1e6)
		// Comparisons also reject NaN and infinities. The upper bound is
		// exclusive because float64(MaxInt64) rounds up to 1<<63.
		if micro > 0 && micro < float64(math.MaxInt64) {
			ev.SetCost(int64(micro), priceSource(r.costSource.String))
		}
	}
	return ev, true, true
}

// dedupKey is the stable cross-poll identity of a ledger row.
//
// The rowid alone identifies the row within one database file and never repeats
// inside it (AUTOINCREMENT does not reuse ids, and nothing UPDATEs this table),
// which is what the watermark relies on. The session id and the write second
// ride along because a rowid is only unique per FILE: a deleted-and-recreated
// sessions.db restarts at 1, and dropping the new rows as duplicates of the old
// ones would be silent data loss. created_timestamp is written once at INSERT
// and never touched again, so the key is stable across polls.
func dedupKey(sessionID string, rowID, ts int64) string {
	return model.ToolGoose + "|" + sessionID + "|" + strconv.FormatInt(rowID, 10) + "|" + strconv.FormatInt(ts, 10)
}

// priceSource labels a cost with the provenance goose recorded for it
// ('provider_reported', 'estimated', 'carried_forward'). The vocabulary of
// price_source is open and nothing parses it, so the goose- prefix keeps these
// distinguishable from the ladder's own rungs.
func priceSource(costSource string) string {
	if s := strings.TrimSpace(costSource); s != "" {
		return model.ToolGoose + "-" + s
	}
	return model.ToolGoose
}

// rawUsage is the audit payload's shape: an explicit ALLOW-LIST of the usage,
// model and identity fields of one ledger row. It is not built by stripping a
// record down, so a column goose adds later contributes nothing until this
// struct is taught about it. Nullable counters stay pointers so a NULL survives
// as null rather than becoming a fabricated 0.
type rawUsage struct {
	ID           int64    `json:"id"`
	SessionID    string   `json:"session_id"`
	CreatedTS    int64    `json:"created_timestamp"`
	Model        *string  `json:"model"`
	InputTokens  *int64   `json:"input_tokens"`
	OutputTokens *int64   `json:"output_tokens"`
	TotalTokens  *int64   `json:"total_tokens"`
	CacheRead    *int64   `json:"cache_read_tokens"`
	CacheWrite   *int64   `json:"cache_write_tokens"`
	Cost         *float64 `json:"cost"`
	CostSource   *string  `json:"cost_source"`
	IsCompaction bool     `json:"is_compaction"`
	Provider     *string  `json:"provider"`
}

// rawPayload marshals the allow-listed audit blob. A marshal failure yields an
// empty payload rather than a partial one: raw is an audit extra, never a
// reason to lose the event.
func rawPayload(r ledgerRow) string {
	p := rawUsage{
		ID:           r.id,
		SessionID:    r.sessionID,
		CreatedTS:    r.createdTS,
		Model:        nullStr(r.model),
		InputTokens:  nullInt(r.input),
		OutputTokens: nullInt(r.output),
		TotalTokens:  nullInt(r.total),
		CacheRead:    nullInt(r.cacheRead),
		CacheWrite:   nullInt(r.cacheWrite),
		Cost:         nullFloat(r.cost),
		CostSource:   nullStr(r.costSource),
		IsCompaction: r.isCompaction.Valid && r.isCompaction.Int64 != 0,
		Provider:     nullStr(r.provider),
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func nullStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// openReadOnly opens the session database strictly read-only. mode=ro prevents
// create/write/lock, query_only(1) makes the connection refuse a write
// statement outright, and busy_timeout keeps a transient lock from failing a
// poll. immutable=1 is deliberately absent: goose keeps this database open in
// WAL mode with a live writer, and an immutable reader skips locking, skips
// change detection and ignores the WAL entirely — it would read the database as
// of its last checkpoint and is exposed to SQLite's documented wrong-result
// behaviour when the file changes underneath it.
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

// isFile reports whether path exists and is a regular file.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
