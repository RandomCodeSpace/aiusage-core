// Package reasonix implements an EVENT-LEVEL adapter for Reasonix
// (esengine/DeepSeek-Reasonix, MIT).
//
// SURFACE. Reasonix ships a purpose-built usage ledger and this adapter reads
// that and nothing else:
//
//	${REASONIX_STATE_HOME:-${REASONIX_HOME:-~/.reasonix}}/stats/YYYY-MM-DD.jsonl
//
// One append-only JSONL file per writer-local day, one record per completed
// request. The session transcripts under <root>/projects/**/sessions/ are
// deliberately NOT read: they carry prompts, reasoning and tool arguments, and
// they lack the exact counters these records carry. Reasonix's own registry
// entry says the same thing in the other direction — the transcript overlaps
// these records and is excluded.
//
// CRITICAL: strictly observational. Files are opened O_RDONLY, never written,
// never locked, never rotated. Reasonix guards its own appends with
// <root>/stats/.append.lock; this adapter never touches it.
//
// # Records are EVENTS, not cumulative counters
//
// This is the bug class that eats OTEL-shaped and session-state surfaces, so it
// is settled by evidence rather than by shape. Two proofs, both from this
// machine:
//
//  1. Reasonix indexes its own stats files into a SQLite catalog whose record
//     table is keyed PRIMARY KEY(file_path, byte_offset) with columns
//     source/model_ref/provider/prompt/completion/reasoning/cache_hit/
//     cache_miss/total/requests/turns, and whose day rollup is built by SUMMING
//     those columns. A cumulative re-export summed that way would be nonsense,
//     so the vendor's own reader treats every line as a delta.
//  2. Two consecutive live records on this machine read prompt=6207 then
//     prompt=5718. A running total does not go down.
//
// So the collector adds these up; it never diffs them and never takes a max.
//
// # Identity is the LINE's CONTENT; the byte offset is only a gate
//
// The dedup key is a hash of the record's own bytes. It carries no path and no
// read position, so a re-read from zero — a lost checkpoint, a moved
// REASONIX_HOME, a symlinked root re-pointed — re-derives exactly the same keys
// and collapses in the store. A key minted from a byte offset would recount
// every line after any rewrite, and a position is not an identity.
//
// Collision safety comes from the record itself: `ts` is RFC3339 with
// NANOSECOND precision and sits beside the full counter tuple, so two distinct
// requests cannot produce identical bytes. The only line that can collide with
// an already-stored line is a genuine duplicate write, which is exactly what
// deduplication is for. The trade is deliberate and in the conservative
// direction the ledger always takes: this can only ever UNDER-count, and an
// append-only ledger can never take an over-count back.
//
// The byte offset survives as the incremental CHECKPOINT, where being wrong
// costs a re-read rather than a fact. Offsets are safe on THIS surface — daily
// files are append-only and every record is newline-terminated (the writer
// guards record boundaries) — unlike the re-read-in-full transcript surfaces,
// where a poll re-walks the whole tree and a position-derived key would recount
// on every pass. The checkpoint is trusted only on pure growth; a shrink or a
// same-size rewrite re-reads from zero.
//
// # Cost: the captured rows are unpriced
//
// The captured Ollama rows carry cost_complete=false and
// incomplete_reason="no_price", with no amount. This adapter stamps no cost:
// CostMicroUSD remains nil, and the pricing ladder can value the counters.
// The exact v1.25.3 writer can also emit optional quoted cost amounts and
// currencies. They are not mapped here; unknown currencies must never be
// interpreted as USD. See testdata/live-1.25.3.provenance.md for the versioned
// writer evidence and the captured surface's limits.
//
// # Names and counters ONLY
//
// The record shape is content-free by construction — no prompt, no result, no
// tool input, no cwd, not even a session id — and the decode is an ALLOW-LIST
// of the seventeen fields below, so a content field added upstream contributes
// nothing until this package is taught about it on purpose. Every string field
// read here is an enum or an identifier: a model ref, a surface name ("cli"),
// a status ("unavailable"), a reason ("no_price"). privacy.no_raw is satisfied
// by construction, not by a switch.
package reasonix

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// StateHomeEnv and HomeEnv name the environment variables that move the
// Reasonix state root, and with it every stats file this adapter reads.
// StateHomeEnv is checked FIRST; HomeEnv is the fallback; ~/.reasonix is the
// default. They are exported because a caller that copies this process's
// discovery into another one — the systemd units internal/service writes — has
// to know that the environment, not the defaults, decided what gets collected.
const (
	StateHomeEnv = "REASONIX_STATE_HOME"
	HomeEnv      = "REASONIX_HOME"
)

// statsDirName is the ledger directory under the state root.
const statsDirName = "stats"

// defaultHomeDir is the state root when neither environment variable names one.
const defaultHomeDir = ".reasonix"

// maxExpandDepth bounds ${VAR:-${VAR:-...}} recursion. A cyclic or absurdly
// nested value stops expanding rather than spinning; what is left is treated as
// a literal path, which then simply fails to exist.
const maxExpandDepth = 8

// envValue reads one environment variable whose NAME is not known until a
// user-authored ${VAR:-default} reference has been parsed out of one of the two
// variables above. It is deliberately the ONLY indirect lookup here, and the two
// variables that actually move discovery are read by constant in stateRoot so
// internal/cmd's discoveryEnv guard can see them and fail until they are
// registered.
//
// It is a package variable holding os.LookupEnv rather than a direct call
// because that guard requires every os.Getenv/os.LookupEnv argument in an
// adapter to be a package constant it can check against the install-suppression
// list, and a name that comes out of the user's own value can never be one. The
// guard's purpose is still served: what suppresses the automatic install is
// REASONIX_STATE_HOME or REASONIX_HOME being SET, and a variable those two
// merely point AT cannot move discovery unless one of them is set first.
var envValue = os.LookupEnv

// Adapter reads Reasonix daily usage ledgers. Read-only.
type Adapter struct{}

// New returns a Reasonix adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolReasonix }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Reasonix" }

// Capabilities declares what this project can say about Reasonix.
//
// Cost is COMPUTED: nothing here calls SetCost, so a row is valued from the
// public rate card or left unpriced. There is NO activity at all — this adapter
// references model.ActivityEvent nowhere.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolReasonix,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(model.ToolReasonix),
		Tier:      model.TierLive,
	}
}

// StatsDir resolves the stats directory for one discovery config, applying the
// full three-step root resolution. Exported for the CLI's `sources`/`doctor`
// surfaces, which report where an adapter is looking without collecting.
func StatsDir(cfg adapter.DiscoverConfig) string {
	root := stateRoot(cfg)
	if root == "" {
		return ""
	}
	return filepath.Join(root, statsDirName)
}

// stateRoot resolves the Reasonix state root, in the vendor's own order:
//
//	explicit override > REASONIX_STATE_HOME > REASONIX_HOME > <home>/.reasonix
//
// Every candidate then goes through the same THREE steps, which is the part
// that is easy to get wrong: ${VAR:-default} expansion, then tilde expansion,
// then resolution of a still-relative path against the working directory. An
// adapter that skips a step reads a different directory than the harness wrote
// to and reports zero usage while looking healthy.
func stateRoot(cfg adapter.DiscoverConfig) string {
	if cfg.Overrides != nil {
		if v := strings.TrimSpace(cfg.Overrides[model.ToolReasonix]); v != "" {
			return resolveRoot(v, cfg.Home)
		}
	}
	// Read by constant, not through envValue: these two are what suppress the
	// automatic supervision install, and internal/cmd's discoveryEnv guard finds
	// them by parsing exactly this call shape.
	//
	// A variable set to whitespace is unset: trimming to empty falls through to
	// the next rung rather than resolving "" against the working directory,
	// which would collect from the repository the CLI happens to be run in.
	if v, ok := os.LookupEnv(StateHomeEnv); ok {
		if v = strings.TrimSpace(v); v != "" {
			return resolveRoot(v, cfg.Home)
		}
	}
	if v, ok := os.LookupEnv(HomeEnv); ok {
		if v = strings.TrimSpace(v); v != "" {
			return resolveRoot(v, cfg.Home)
		}
	}
	if cfg.Home == "" {
		return ""
	}
	return filepath.Join(cfg.Home, defaultHomeDir)
}

// resolveRoot runs the three steps in order on one candidate value.
func resolveRoot(v, home string) string {
	v = strings.TrimSpace(expandVars(v, 0))
	if v == "" {
		return ""
	}
	v = expandTilde(v, home)
	if !filepath.IsAbs(v) {
		if abs, err := filepath.Abs(v); err == nil {
			v = abs
		}
	}
	return filepath.Clean(v)
}

// expandVars performs shell-style ${VAR}, ${VAR:-default} and $VAR expansion.
//
// It is written out rather than delegated to os.ExpandEnv because os.ExpandEnv
// does not implement the ":-" default form, and dropping to the bare ${VAR}
// behaviour would expand `${XDG_STATE_HOME:-~/.local/state}/reasonix` to the
// empty string on a machine that sets no XDG_STATE_HOME — silently collecting
// from "/reasonix".
func expandVars(s string, depth int) string {
	if depth > maxExpandDepth || !strings.ContainsRune(s, '$') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' || i+1 >= len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		if s[i+1] == '{' {
			end, ok := matchBrace(s, i+1)
			if !ok {
				// Unbalanced "${": literal, not an expansion. Anything else
				// would silently swallow the rest of the path.
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteString(expandBraced(s[i+2:end], depth))
			i = end + 1
			continue
		}
		name, n := scanName(s[i+1:])
		if n == 0 {
			b.WriteByte(c)
			i++
			continue
		}
		v, _ := envValue(name)
		b.WriteString(v)
		i += 1 + n
	}
	return b.String()
}

// expandBraced resolves the inside of one ${...} reference.
func expandBraced(inner string, depth int) string {
	name, def, hasDefault := strings.Cut(inner, ":-")
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if v, ok := envValue(name); ok && v != "" {
		return v
	}
	if hasDefault {
		// The default may itself be a reference, and on this surface commonly
		// is: the documented shape is a chain of them.
		return expandVars(def, depth+1)
	}
	return ""
}

// matchBrace returns the index of the '}' closing the '{' at open, counting
// nested braces so ${A:-${B:-x}} resolves as one reference rather than two.
func matchBrace(s string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// scanName reads a bare $NAME identifier from the head of s.
func scanName(s string) (string, int) {
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		alpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if alpha || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		break
	}
	return s[:i], i
}

// expandTilde replaces a LEADING ~ or ~/ with the discovery home. A "~user"
// form is left alone: resolving another account's home is not this adapter's
// business, and guessing would point collection at a directory the user never
// named.
func expandTilde(v, home string) string {
	if home == "" || !strings.HasPrefix(v, "~") {
		return v
	}
	switch {
	case v == "~":
		return home
	case strings.HasPrefix(v, "~/"):
		return filepath.Join(home, v[2:])
	}
	return v
}

// Discover lists the daily ledger files under the resolved stats directory. A
// missing root is not an error: Reasonix simply is not installed here.
//
// The root is NOT symlink-resolved, and does not need to be: dedup keys are
// hashes of a record's own bytes, so a re-pointed root cannot mint a new
// identity for a line already stored.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	dir := StatsDir(cfg)
	if dir == "" || !adapter.IsDir(dir) {
		return nil, nil
	}
	entries, err := os.ReadDir(dir) // sorted by name: stable source order
	if err != nil {
		return nil, fmt.Errorf("reasonix: read %s: %w", dir, err)
	}
	var srcs []adapter.Source
	for _, d := range entries {
		if ctx.Err() != nil {
			return srcs, ctx.Err()
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, d.Name())
		if !adapter.WalkEntryIsFile(d, path) {
			continue
		}
		srcs = append(srcs, adapter.Source{
			Tool:  model.ToolReasonix,
			Class: model.EventLevel,
			Path:  path,
			Label: "Reasonix usage: " + path,
			Meta:  map[string]string{"root": dir, "day": adapter.FileStem(path)},
		})
	}
	return srcs, nil
}

// Collect reads one daily ledger in full.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental tail-reads what is new since cp: an unchanged size+mtime
// opens nothing; pure growth seeks to the stored offset; a shrink or a
// same-size rewrite re-reads from zero, which is free of consequence because
// the keys are content hashes and re-derive identically.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	f, err := os.Open(src.Path) // read-only
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("reasonix: open %s: %w", src.Path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("reasonix: stat %s: %w", src.Path, err)
	}
	return a.collectReader(ctx, src, cp, f, fi)
}

func (a Adapter) collectReader(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint, f io.ReadSeeker, fi os.FileInfo) (adapter.Observation, error) {
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()

	var start int64
	if cp != nil {
		if cp.Size == size && cp.MTimeNS == mtimeNS {
			return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
		}
		if size > cp.Size && cp.Offset >= 0 && cp.Offset <= size {
			start = cp.Offset
		}
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return adapter.Observation{}, fmt.Errorf("reasonix: seek %s: %w", src.Path, err)
		}
	}

	// A record with no parseable ts falls back to the file's mtime. That cannot
	// cause a recount: the dedup key is the line's content, which the mtime is
	// not part of. Every live record carries a nanosecond ts, so this is a
	// tolerance rather than a path.
	mtime := fi.ModTime().UTC()

	var (
		events    []model.UsageEvent
		schemaErr error
		skipped   int
		consumed  = start
		r         = bufio.NewReaderSize(f, 64*1024)
	)
	for {
		if ctx.Err() != nil {
			return adapter.Observation{Events: events}, ctx.Err()
		}
		raw, rerr := r.ReadBytes('\n')
		terminated := rerr == nil
		if rerr != nil && rerr != io.EOF {
			return adapter.Observation{Events: events}, fmt.Errorf("reasonix: read %s: %w", src.Path, rerr)
		}
		// A blank line is padding, not corruption; a decoded line that carries
		// no tokens is bookkeeping, not corruption either. Only a line that
		// fails to decode counts against the tally, so a healthy file never
		// reports an error every cycle.
		if line := trimLine(raw); len(line) > 0 {
			ev, ok, derr := parseRecord(line, mtime, src.Path)
			switch {
			case derr != nil:
				skipped++
				if errors.Is(derr, adapter.ErrSourceFormat) {
					schemaErr = derr
				}
			case ok:
				events = append(events, ev)
			}
		}
		if terminated {
			consumed += int64(len(raw))
			continue
		}
		// A complete-but-unterminated final line is emitted, but the offset
		// stays BEFORE it. If it was a write in progress the next cycle re-reads
		// the whole line and the identical key collapses; a genuinely truncated
		// JSON object does not parse and contributes nothing either way.
		break
	}

	if err := ctx.Err(); err != nil {
		return adapter.Observation{Events: events}, err
	}
	obs := adapter.Observation{
		Events: events,
		Checkpoint: &model.SourceCheckpoint{
			Tool: model.ToolReasonix, SourcePath: src.Path,
			Size: size, MTimeNS: mtimeNS, Offset: consumed,
		},
	}
	if schemaErr != nil {
		// Keep the old checkpoint so fixing a recognized current record does
		// not require an unrelated append to recover it. Valid events still
		// reach the ledger and replay under their existing dedup keys.
		obs.Checkpoint = nil
		return obs, fmt.Errorf("reasonix: %s: %w", src.Path, schemaErr)
	}
	if skipped > 0 {
		return obs, fmt.Errorf("reasonix: skipped %d unparseable record(s) in %s", skipped, src.Path)
	}
	return obs, nil
}

// statsRecord is the ALLOW-LIST of stats fields this adapter reads. Everything
// else a line carries — now or after some future release — is discarded by
// encoding/json as it parses and never becomes a value in this process.
//
// Every field is a number, a bool or a closed-vocabulary string. There is no
// field a prompt, a tool argument, a file path or a result could land in, which
// is what makes the privacy guarantee structural instead of a rule someone has
// to keep remembering.
//
// It is also the audit payload: usage_events.raw is this same shape marshalled
// back out (auditJSON), which is why the tags carry omitempty. One list, used in
// both directions — a field this package has not been taught about is neither
// read nor stored, and the two can never drift apart the way a hand-kept second
// copy would. The audit payload is therefore built from the allow-list, never
// by stripping known-bad keys out of the source line, which would start leaking
// the day the format grows a content field.
type statsRecord struct {
	TS     string `json:"ts,omitempty"`     // RFC3339 nanosecond, UTC
	Model  string `json:"model,omitempty"`  // "<provider>/<model>" ref
	Source string `json:"source,omitempty"` // surface that spent it: cli, web, acp, ...

	Prompt     int64 `json:"prompt"`               // total input, cache_hit included
	Completion int64 `json:"completion"`           // total output
	Reasoning  int64 `json:"reasoning,omitempty"`  // subset of completion
	CacheHit   int64 `json:"cache_hit,omitempty"`  // cached portion of prompt
	CacheMiss  int64 `json:"cache_miss,omitempty"` // uncached portion of prompt
	Total      int64 `json:"total"`                // provider-authoritative total
	Requests   int64 `json:"requests,omitempty"`
	Turns      int64 `json:"turns,omitempty"`

	// Cost HONESTY, never a cost amount. These drive no arithmetic; they are
	// the record's own statement of what it does and does not know, kept in the
	// audit payload so an unpriced row can say why.
	UsageSource      string `json:"usage_source,omitempty"`
	CostComplete     *bool  `json:"cost_complete,omitempty"`
	CostEstimated    *bool  `json:"cost_estimated,omitempty"`
	DisplayComplete  *bool  `json:"display_complete,omitempty"`
	DisplayStatus    string `json:"display_status,omitempty"`
	IncompleteReason string `json:"incomplete_reason,omitempty"`
}

var errCurrentTimestamp = fmt.Errorf("%w: current stats record requires a valid ts timestamp", adapter.ErrSourceFormat)

// parseRecord maps one JSONL line onto a usage event. The two failure modes are
// kept apart: a non-nil error means malformed JSON or a missing current
// timestamp; ok=false with a nil error means a decoded record has no tokens.
// Zero-usage bookkeeping is not corruption.
func parseRecord(line []byte, mtime time.Time, path string) (model.UsageEvent, bool, error) {
	var rec statsRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		if json.Valid(line) {
			return model.UsageEvent{}, false, fmt.Errorf("%w: decode stats record: %v", adapter.ErrSourceFormat, err)
		}
		return model.UsageEvent{}, false, err
	}

	// The current writer always emits ts. Its usage/cost markers distinguish
	// these records from older tolerated shapes that use the file mtime.
	// Counter/model/source omissions are valid: the writer uses omitempty.
	if rec.UsageSource != "" || rec.CostComplete != nil || rec.DisplayComplete != nil {
		if _, err := time.Parse(time.RFC3339Nano, rec.TS); err != nil {
			return model.UsageEvent{}, false, errCurrentTimestamp
		}
	}

	prompt := adapter.NonNeg(rec.Prompt)
	completion := adapter.NonNeg(rec.Completion)
	reasoning := adapter.NonNeg(rec.Reasoning)
	cacheHit := adapter.NonNeg(rec.CacheHit)
	if cacheHit > prompt {
		cacheHit = prompt // cached is a SUBSET of prompt; never bill it twice
	}
	// The input column is derived from `prompt`, not read from `cache_miss`.
	// Both are written, and on every live record here they agree
	// (cache_miss == prompt - cache_hit) — but `total` is built from `prompt`,
	// so deriving keeps the stored components summing to the authoritative
	// total even on the day the two disagree. cache_miss stays in the audit
	// payload as the source's own claim.
	input := prompt - cacheHit

	total := adapter.NonNeg(rec.Total)
	if total == 0 {
		total = prompt + completion
	}
	if input == 0 && cacheHit == 0 && completion == 0 && reasoning == 0 && total == 0 {
		// Distinguish legitimate zero/turn/request records from a complete
		// unrelated object. Only this zero path needs presence information;
		// decoded counters alone cannot distinguish absent from zero.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			return model.UsageEvent{}, false, fmt.Errorf("%w: expected stats object", adapter.ErrSourceFormat)
		}
		for _, name := range []string{"ts", "model", "source", "prompt", "completion", "reasoning", "cache_hit", "cache_miss", "total", "requests", "turn", "turns", "usage_source", "cost_complete", "cost_estimated", "display_complete", "display_status", "incomplete_reason"} {
			if _, present := fields[name]; present {
				return model.UsageEvent{}, false, nil
			}
		}
		return model.UsageEvent{}, false, fmt.Errorf("%w: no recognized stats fields", adapter.ErrSourceFormat)
	}

	ev := model.UsageEvent{
		Tool:     model.ToolReasonix,
		Model:    rec.Model,
		Provider: providerOf(rec.Model),
		// The stats ledger is deliberately content-free: it names no session
		// and no workspace, so both stay empty — unknown, which is what the
		// source says, rather than a constant standing in for one.
		SessionID:           "",
		Project:             "",
		EventTime:           eventTime(rec.TS, mtime),
		InputTokens:         input,
		OutputTokens:        completion,
		CacheCreationTokens: 0, // reasonix reports hit/miss only; no explicit write
		CacheReadTokens:     cacheHit,
		ReasoningTokens:     reasoning,
		TotalTokens:         total,
		SourcePath:          path,
		Kind:                model.KindUsage,
		Raw:                 auditJSON(rec),
	}
	// No cost is stamped: this surface carries the honesty flags and no amount.
	// CostMicroUSD stays nil — unpriced, which is not $0 — and the pricing
	// ladder gets its turn.
	ev.DedupKey = dedupKey(line)
	return ev, true, nil
}

// dedupKey is the stable cross-poll identity: reasonix|<sha256 of the record's
// own bytes>. No path, no offset, no read position — see the package doc.
func dedupKey(line []byte) string {
	sum := sha256.Sum256(line)
	return model.ToolReasonix + "|" + hex.EncodeToString(sum[:16])
}

// providerOf returns the billing identity named by a "<provider>/<model>" ref,
// which is how Reasonix itself derives it. A ref with no slash names no
// provider, and empty means unknown rather than a guess.
func providerOf(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.IndexByte(ref, '/'); i > 0 {
		return ref[:i]
	}
	return ""
}

// eventTime parses the record's own timestamp. The bucket comes from THIS, never
// from the file name: the daily files are cut on the WRITER's local calendar, so
// a record in 2026-08-16.jsonl can legitimately carry a UTC timestamp on the
// 15th (any zone east of UTC crosses midnight early), and dating it by its file
// would move usage between days for every user not on UTC.
func eventTime(ts string, fallback time.Time) time.Time {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return fallback
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC()
		}
	}
	return fallback
}

// auditJSON re-marshals the allow-listed fields into usage_events.raw: the
// counters an audit of the stored accounting needs, plus the cost-honesty flags
// that explain an unpriced row. It re-emits the source's own field names so an
// auditor can compare the payload against the line field for field — including
// cache_miss, which the ledger's columns do not carry because the input count is
// derived from prompt instead.
//
// A marshal failure yields no payload rather than a partial one: raw is an audit
// record, and half of one is worse than none.
func auditJSON(rec statsRecord) string {
	b, err := json.Marshal(rec)
	if err != nil {
		return ""
	}
	return string(b)
}

// trimLine strips the trailing newline and any surrounding whitespace, leaving
// the exact bytes the dedup key hashes. Trimming matters: a writer that later
// switches to CRLF must not re-key every record it has already written.
func trimLine(raw []byte) []byte { return bytes.TrimSpace(raw) }
