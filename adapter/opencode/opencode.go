// Package opencode implements the event-level adapter for the opencode CLI.
//
// opencode stores per-message usage both in a SQLite database and as JSON
// files. Under each data directory we read BOTH:
//
//   - SQLite "opencode.db" (or the first "opencode-<token>.db") — table
//     `message(id, session_id, data)` where `data` is the message JSON.
//   - JSON files under "storage/message/**/*.json" (the same shape).
//
// Both carry the same per-message `data` payload:
//
//	{id, sessionID, providerID, modelID, time:{created:<ms>},
//	 tokens:{input, output, reasoning, cache:{read, write}, total},
//	 cost, path:{cwd, root}}
//
// Token mapping (opencode reports cache read/write as separate buckets, like
// Anthropic): Input=tokens.input, Output=tokens.output,
// CacheCreation=tokens.cache.write, CacheRead=tokens.cache.read,
// Reasoning=tokens.reasoning, and Total is reconciled against tokens.total via
// tokenutil.ApplyTotalFallback.
//
// Reasoning is normalized to ADDITIVE output. Current writers subtract it from
// output; older writers retained it inside output. A positive total that exactly
// accounts for input/output/cache identifies that overlap (see buildEvent).
//
// The persisted dedup key is "opencode|<message id>", so the SQLite row and the
// JSON file for the same message collapse to one stored event (DB is discovered
// first, so it wins on INSERT OR IGNORE).
//
// CRITICAL: strictly read-only. JSON files are opened O_RDONLY; the database is
// opened with a read-only DSN (mode=ro plus query_only(1)) — never immutable=1,
// because opencode writes this database live and keeps a large WAL an immutable
// reader cannot see (see collectDB). Nothing under the agent's directories is
// created, locked, or modified.
package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0)

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/internal/tokenutil"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

const (
	// DataDirEnv names the environment variable that moves the opencode data
	// directory, and with it every database this adapter reads.
	DataDirEnv     = "OPENCODE_DATA_DIR"
	primaryDBName  = "opencode.db"
	dbPrefix       = "opencode-"
	dbSuffix       = ".db"
	messageDirName = "message"
	storageDirName = "storage"

	// Source kinds carried in Source.Meta["kind"].
	kindDB   = "db"
	kindJSON = "json"
)

// Adapter reads opencode CLI message usage. Read-only.
type Adapter struct{}

// New returns an opencode adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolOpenCode }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "opencode" }

// Capabilities declares what this project can say about opencode.
//
// Cost is COMPUTED: nothing here calls SetCost. Activity is an EXACT join —
// collectActivity joins part.message_id to message.id, the very id the usage
// dedup key is already built from.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolOpenCode,
		Cost:      model.CostComputed,
		Activity:  model.ActivityExact,
		Reasoning: model.ReasoningReportFor(model.ToolOpenCode),
		Tier:      model.TierLive,
	}
}

// dataDirs returns the configured opencode data directories. OPENCODE_DATA_DIR
// may be a comma-separated list that fully REPLACES the default; otherwise the
// discovery root (override or ~/.local/share/opencode) is used.
func (a Adapter) dataDirs(cfg adapter.DiscoverConfig) []string {
	if env := strings.TrimSpace(os.Getenv(DataDirEnv)); env != "" {
		var out []string
		for _, p := range strings.Split(env, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	def := ""
	if cfg.Home != "" {
		def = filepath.Join(cfg.Home, ".local", "share", "opencode")
	}
	return []string{cfg.Root(model.ToolOpenCode, def)}
}

// Discover locates, per data dir, the SQLite database (if any) and the JSON
// message tree (if any). The database is discovered FIRST so that, on a dedup
// collision with the JSON copy, the DB row wins (INSERT OR IGNORE).
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source

	for _, dir := range a.dataDirs(cfg) {
		if ctx.Err() != nil {
			return srcs, ctx.Err()
		}
		if dir == "" || !adapter.IsDir(dir) {
			continue
		}

		if dbPath := findDB(dir); dbPath != "" {
			if _, dup := seen[dbPath]; !dup {
				seen[dbPath] = struct{}{}
				srcs = append(srcs, adapter.Source{
					Tool:  model.ToolOpenCode,
					Class: model.EventLevel,
					Path:  dbPath,
					Label: "opencode db " + filepath.Base(dbPath),
					Meta:  map[string]string{"kind": kindDB},
				})
			}
		}

		msgDir := filepath.Join(dir, storageDirName, messageDirName)
		if adapter.IsDir(msgDir) {
			if _, dup := seen[msgDir]; !dup {
				seen[msgDir] = struct{}{}
				srcs = append(srcs, adapter.Source{
					Tool:  model.ToolOpenCode,
					Class: model.EventLevel,
					Path:  msgDir,
					Label: "opencode messages " + dir,
					Meta:  map[string]string{"kind": kindJSON},
				})
			}
		}
	}
	return srcs, nil
}

// findDB returns the primary opencode.db if present, else the first
// opencode-<token>.db (lexically ordered for determinism), else "".
func findDB(dir string) string {
	primary := filepath.Join(dir, primaryDBName)
	if isFile(primary) {
		return primary
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries { // ReadDir returns entries sorted by name
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, dbPrefix) && strings.HasSuffix(name, dbSuffix) {
			return filepath.Join(dir, name)
		}
	}
	return ""
}

// Collect reads one discovered source (DB or JSON tree) read-only.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental scans new database rows and retries pending modern messages.
// Legacy databases retain their rowid cursor; JSON trees are always read in full.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	switch kindOf(src) {
	case kindDB:
		return collectDB(ctx, src, cp)
	case kindJSON:
		return collectJSON(ctx, src)
	default:
		return adapter.Observation{}, fmt.Errorf("opencode: unknown source kind for %s", src.Path)
	}
}

func kindOf(src adapter.Source) string {
	if src.Meta != nil {
		if k := src.Meta["kind"]; k != "" {
			return k
		}
	}
	return ""
}

var errInvalidTimestamp = errors.New("missing or invalid creation timestamp")
var errInvalidMessage = errors.New("invalid message JSON")

// dbState is private retry and file state. Missing or legacy state replays a
// modern schema once, using the same persisted event identities.
type dbState struct {
	Version int      `json:"version"`
	Pending []int64  `json:"pending,omitempty"`
	Legacy  bool     `json:"legacy,omitempty"`
	Files   *dbFiles `json:"files,omitempty"`
}

type fileStamp struct {
	Present bool  `json:"present"`
	Size    int64 `json:"size"`
	MTime   int64 `json:"mtime"`
}

type dbFiles struct {
	DB  fileStamp `json:"db"`
	WAL fileStamp `json:"wal"`
}

func statDBFiles(path string) (dbFiles, error) {
	var files dbFiles
	for _, file := range []struct {
		path  string
		stamp *fileStamp
	}{{path, &files.DB}, {path + "-wal", &files.WAL}} {
		info, err := os.Stat(file.path)
		if errors.Is(err, fs.ErrNotExist) && file.stamp == &files.WAL {
			continue
		}
		if err != nil {
			return dbFiles{}, err
		}
		if !info.Mode().IsRegular() {
			return dbFiles{}, fmt.Errorf("not a regular file: %s", file.path)
		}
		*file.stamp = fileStamp{Present: true, Size: info.Size(), MTime: info.ModTime().UnixNano()}
	}
	return files, nil
}

func validDBState(state dbState, watermark int64) bool {
	if state.Version != 1 || watermark < 0 || (state.Legacy && len(state.Pending) != 0) {
		return false
	}
	for i, rowid := range state.Pending {
		if rowid <= 0 || rowid > watermark || (i > 0 && rowid <= state.Pending[i-1]) {
			return false
		}
	}
	return true
}

// collectDB observes message completion and parts in one read-only snapshot.
// Never use immutable=1: the producer's latest messages can live in its WAL.
func collectDB(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	if err := ctx.Err(); err != nil {
		return adapter.Observation{}, err
	}
	before, err := statDBFiles(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: stat db %s: %w", src.Path, err)
	}
	var state dbState
	stateValid := cp != nil && cp.State != "" && json.Unmarshal([]byte(cp.State), &state) == nil && validDBState(state, cp.Watermark)
	if stateValid && cp.Tool == model.ToolOpenCode && cp.SourcePath == src.Path && state.Files != nil && *state.Files == before {
		return adapter.Observation{}, nil
	}
	absolute, err := filepath.Abs(src.Path)
	if err != nil {
		return adapter.Observation{}, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: open db %s: %w", src.Path, err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: snapshot %s: %w", src.Path, err)
	}
	defer tx.Rollback()

	// Both timestamps identify the supported mutable family. A partial modern
	// schema must not silently fall back to legacy finality assumptions.
	var anchors, sessionAnchors int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM pragma_table_info('message') WHERE name IN ('time_created','time_updated')),
		(SELECT COUNT(*) FROM pragma_table_info('session') WHERE name IN ('id','directory'))`).Scan(&anchors, &sessionAnchors); err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: schema %s: %w", src.Path, err)
	}
	if anchors == 1 {
		return adapter.Observation{}, fmt.Errorf("%w: opencode incomplete modern message schema in %s", adapter.ErrSourceFormat, src.Path)
	}
	modern := anchors == 2
	watermark := int64(0)
	if cp != nil {
		watermark = cp.Watermark
	}
	if modern {
		if cp == nil || cp.State == "" || (stateValid && state.Legacy) {
			watermark = 0
			state = dbState{}
		} else {
			if !stateValid {
				return adapter.Observation{}, fmt.Errorf("%w: opencode invalid checkpoint state for %s", adapter.ErrSourceFormat, src.Path)
			}
		}
	} else {
		state = dbState{Legacy: true}
	}
	pendingJSON, _ := json.Marshal(state.Pending)
	payload := "data"
	if modern {
		// User summaries contain patches. Usage needs only their role to
		// advance the rowid cursor; count collection selects scalars below.
		payload = `CASE WHEN CASE WHEN json_valid(data) THEN json_extract(data, '$.role') END = 'user'
			THEN '{"role":"user"}' ELSE data END`
	}
	rows, err := tx.QueryContext(ctx, `SELECT rowid, id, session_id, `+payload+` FROM message
 WHERE rowid > ? OR rowid IN (SELECT value FROM json_each(?)) ORDER BY rowid`, watermark, string(pendingJSON))
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: query %s: %w", src.Path, err)
	}
	var obs adapter.Observation
	var completed, pending []int64
	consumed := watermark
	var diagnostics error
	for rows.Next() {
		var rowid int64
		var id, sessionID, data sql.NullString
		if err := rows.Scan(&rowid, &id, &sessionID, &data); err != nil {
			rows.Close()
			return obs, fmt.Errorf("opencode: scan %s: %w", src.Path, err)
		}
		if rowid > consumed {
			consumed = rowid
		}
		raw := []byte(data.String)
		if modern {
			var header struct {
				Role string `json:"role"`
				Time struct {
					Completed int64 `json:"completed"`
				} `json:"time"`
			}
			if err := json.Unmarshal(raw, &header); err != nil || (header.Role != "assistant" && header.Role != "user") || strings.TrimSpace(id.String) == "" {
				rows.Close()
				return obs, fmt.Errorf("%w: opencode unclassified message row %d in %s", adapter.ErrSourceFormat, rowid, src.Path)
			}
			if header.Role == "user" {
				continue
			}
			if header.Time.Completed <= 0 {
				pending = append(pending, rowid)
				continue
			}
		}
		ev, ok, err := buildEvent(raw, id.String, sessionID.String, src.Path)
		if err != nil && (modern || !errors.Is(err, errInvalidMessage)) {
			rowErr := fmt.Errorf("opencode message row %d in %s: %w", rowid, src.Path, err)
			if !errors.Is(err, errInvalidTimestamp) {
				rowErr = fmt.Errorf("%w: %v", adapter.ErrSourceFormat, rowErr)
			}
			diagnostics = errors.Join(diagnostics, rowErr)
			// Modern malformed payloads may still be repaired in place. Hold the
			// checkpoint rather than consume an unclassified record.
			if modern && !errors.Is(err, errInvalidTimestamp) {
				rows.Close()
				return obs, diagnostics
			}
		}
		if ok {
			obs.Events = append(obs.Events, ev)
		}
		completed = append(completed, rowid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return obs, fmt.Errorf("opencode: read %s: %w", src.Path, err)
	}
	if len(completed) > 0 {
		obs.Activity, err = collectActivity(ctx, tx, src, obs.Events, completed, modern)
		if err != nil {
			return obs, errors.Join(diagnostics, fmt.Errorf("opencode: parts %s: %w", src.Path, err))
		}
	}
	if modern && sessionAnchors == 2 {
		// Summaries update existing user messages after assistant completion.
		// Read their scalar counts independently of the usage rowid cursor, also
		// recovering historical counts without replaying token history.
		obs.CodeChanges, err = collectCodeChanges(ctx, tx)
		diagnostics = errors.Join(diagnostics, err)
	}
	if err := tx.Commit(); err != nil {
		return obs, errors.Join(diagnostics, fmt.Errorf("opencode: snapshot %s: %w", src.Path, err))
	}
	if err := db.Close(); err != nil {
		return obs, errors.Join(diagnostics, fmt.Errorf("opencode: close db %s: %w", src.Path, err))
	}
	next := &model.SourceCheckpoint{Tool: model.ToolOpenCode, SourcePath: src.Path, Watermark: consumed}
	state.Version = 1
	state.Pending = pending
	state.Files = nil
	// A changed source may contain writes outside our snapshot. Leave the file
	// gate unset so the next pass observes them, including summary-only updates.
	if after, err := statDBFiles(src.Path); diagnostics == nil && err == nil && before == after {
		state.Files = &after
	}
	encoded, _ := json.Marshal(state)
	next.State = string(encoded)
	if cp == nil || cp.Watermark != next.Watermark || cp.State != next.State {
		obs.Checkpoint = next
	}
	return obs, diagnostics
}

// toolPart is one tool-call row of the `part` table, reduced to the three
// fields activity needs. The tool INPUT (state.input) and its output
// (state.output) are never selected: activity is names and counts only.
type toolPart struct {
	messageID string
	partID    string
	createdMS int64
	name      string
}

// collectActivity reads parts of the exact completed message set. The optional
// legacy part table may be absent; every other read failure holds the cursor.
func collectActivity(ctx context.Context, db *sql.Tx, src adapter.Source, events []model.UsageEvent, completed []int64, modern bool) ([]model.ActivityEvent, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='part' AND type='table')`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		if modern {
			return nil, fmt.Errorf("%w: modern database has no part table", adapter.ErrSourceFormat)
		}
		return nil, nil
	}
	ids, _ := json.Marshal(completed)
	rows, err := db.QueryContext(ctx, `
		SELECT p.message_id, p.id, p.time_created, json_extract(p.data,'$.tool')
		FROM part p
		JOIN message m ON m.id = p.message_id
		WHERE m.rowid IN (SELECT value FROM json_each(?))
		  AND json_extract(p.data,'$.type') = 'tool'
		ORDER BY p.message_id, p.id`, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		parts   []toolPart
		byMsg   = map[string]int{}
		scanErr error
	)
	for rows.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var (
			msgID, partID sql.NullString
			created       sql.NullInt64
			name          sql.NullString
		)
		if err := rows.Scan(&msgID, &partID, &created, &name); err != nil {
			scanErr = err
			continue
		}
		n := strings.TrimSpace(name.String)
		if partID.String == "" || n == "" {
			continue // no stable identity or no name: nothing worth recording
		}
		parts = append(parts, toolPart{
			messageID: msgID.String, partID: partID.String,
			createdMS: created.Int64, name: n,
		})
		byMsg[msgID.String]++
	}
	if err := errors.Join(rows.Err(), scanErr); err != nil {
		return nil, err
	}

	// Attributes come from the message's own usage event, so an activity row and
	// the row it is attributed to always agree on time, session, project and
	// model. Agreeing on TIME is what makes a windowed comparison of the two
	// ledgers meaningful at all.
	usage := make(map[string]*model.UsageEvent, len(events))
	for i := range events {
		usage[events[i].MessageID] = &events[i]
	}

	seq := map[string]int{}
	out := make([]model.ActivityEvent, 0, len(parts))
	for _, p := range parts {
		a := model.ActivityEvent{
			Tool:        model.ToolOpenCode,
			Kind:        model.ActivityTool,
			Name:        p.name,
			MessageID:   p.messageID,
			TurnSeq:     seq[p.messageID],
			CallsInTurn: byMsg[p.messageID],
			SourcePath:  src.Path,
			DedupKey:    "opencode|part|" + p.partID,
		}
		seq[p.messageID]++
		if u, ok := usage[p.messageID]; ok {
			a.UsageDedupKey = u.DedupKey
			a.SessionID = u.SessionID
			a.Project = u.Project
			a.Model = u.Model
			a.EventTime = u.EventTime
		} else if p.createdMS > 0 {
			// No usage event for this message (zero tokens, unparseable model).
			// The call still happened; it simply has no cost to be attributed.
			a.EventTime = time.UnixMilli(p.createdMS).UTC()
		}
		if a.EventTime.IsZero() {
			continue // no defensible timestamp: a row that cannot be windowed
		}
		out = append(out, a)
	}
	return out, nil
}

// collectJSON walks storage/message/**/*.json read-only, parsing each as a
// message `data` payload. The DB columns are unavailable here, so id/session
// come from the JSON itself.
func collectJSON(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	var events []model.UsageEvent
	var diagnostics error
	walkErr := filepath.WalkDir(src.Path, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !adapter.WalkEntryIsFile(d, path) || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		raw, rerr := os.ReadFile(path) // read-only
		if rerr != nil {
			return nil // skip unreadable file
		}
		ev, ok, parseErr := buildEvent(raw, "", "", path)
		if errors.Is(parseErr, errInvalidTimestamp) {
			diagnostics = errors.Join(diagnostics, fmt.Errorf("opencode message %s: %w", path, parseErr))
		}
		if !ok {
			return nil
		}
		events = append(events, ev)
		return nil
	})
	return adapter.Observation{Events: events}, errors.Join(diagnostics, walkErr)
}

// message is the per-message `data` JSON payload (DB column or JSON file).
type message struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Time       struct {
		Created int64 `json:"created"` // unix milliseconds
	} `json:"time"`
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
		Total int64 `json:"total"`
	} `json:"tokens"`
	Path struct {
		Cwd  string `json:"cwd"`
		Root string `json:"root"`
	} `json:"path"`
}

// buildEvent parses a message payload and maps it onto a UsageEvent.
//
// dbID/dbSession override the JSON id/sessionID when non-empty (DB columns are
// authoritative). Returns ok=false when the payload is unparseable, the model
// id is empty, or every token component is zero. Nonzero usage also requires
// a positive creation timestamp; malformed fields return a diagnostic.
func buildEvent(raw []byte, dbID, dbSession, srcPath string) (model.UsageEvent, bool, error) {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		var fieldErr *json.UnmarshalTypeError
		if errors.As(err, &fieldErr) && (fieldErr.Field == "time.created" || fieldErr.Field == "time") {
			return model.UsageEvent{}, false, errInvalidTimestamp
		}
		return model.UsageEvent{}, false, errInvalidMessage
	}

	mdl := strings.TrimSpace(m.ModelID)
	if mdl == "" {
		return model.UsageEvent{}, false, nil // require non-empty modelID
	}

	id := strings.TrimSpace(m.ID)
	if dbID != "" {
		id = strings.TrimSpace(dbID)
	}
	if id == "" {
		return model.UsageEvent{}, false, nil // need a stable dedup key
	}

	session := strings.TrimSpace(m.SessionID)
	if dbSession != "" {
		session = strings.TrimSpace(dbSession)
	}
	if session == "" {
		session = "unknown"
	}

	input := adapter.NonNeg(m.Tokens.Input)
	output := adapter.NonNeg(m.Tokens.Output)
	cacheCreation := adapter.NonNeg(m.Tokens.Cache.Write)
	cacheRead := adapter.NonNeg(m.Tokens.Cache.Read)
	reasoning := adapter.NonNeg(m.Tokens.Reasoning)
	total := adapter.NonNeg(m.Tokens.Total)

	// The 1.1.65 writer retained reasoning inside output; newer writers subtract
	// it. Normalize only when the positive provider total proves that overlap.
	// Subtract bounded components from total so the identity cannot overflow.
	if reasoning > 0 && output >= reasoning && total > 0 && input <= total &&
		cacheCreation <= total-input && cacheRead <= total-input-cacheCreation &&
		output == total-input-cacheCreation-cacheRead {
		output -= reasoning
	}

	// Reconcile against the provider total. opencode cache buckets are additive
	// (Anthropic-style) and so is reasoning, so all of them participate in the
	// known sum and only a genuinely unexplained remainder is redistributed.
	// Passing reasoning as the extra bucket is also what stops the gap-fill
	// branch from billing it twice: were reasoning left out of the known sum,
	// a row with output == 0 and reasoning > 0 would have the fallback copy the
	// reasoning count into OutputTokens while ReasoningTokens kept it as well.
	output, extra := tokenutil.ApplyTotalFallback(input, output, cacheCreation, cacheRead, reasoning, total)

	// Authoritative stored total: prefer the provider total, else the sum of the
	// additive components (extra already carries reasoning plus any overflow).
	storedTotal := total
	if sum := input + output + cacheCreation + cacheRead + extra; storedTotal < sum {
		storedTotal = sum
	}

	if input == 0 && output == 0 && cacheCreation == 0 && cacheRead == 0 &&
		reasoning == 0 && extra == 0 && storedTotal == 0 {
		return model.UsageEvent{}, false, nil // drop all-zero records
	}

	project := strings.TrimSpace(m.Path.Cwd)
	if project == "" {
		project = "opencode"
	}

	if m.Time.Created <= 0 {
		return model.UsageEvent{}, false, errInvalidTimestamp
	}
	when := time.UnixMilli(m.Time.Created).UTC()

	ev := model.UsageEvent{
		Tool:                model.ToolOpenCode,
		Model:               mdl,
		Provider:            strings.TrimSpace(m.ProviderID),
		SessionID:           session,
		Project:             project,
		EventTime:           when,
		InputTokens:         input,
		OutputTokens:        output,
		CacheCreationTokens: cacheCreation,
		CacheReadTokens:     cacheRead,
		ReasoningTokens:     reasoning,
		TotalTokens:         storedTotal,
		MessageID:           id,
		SourcePath:          srcPath,
		DedupKey:            "opencode|" + id,
		Kind:                model.KindUsage,
	}
	return ev, true, nil
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
