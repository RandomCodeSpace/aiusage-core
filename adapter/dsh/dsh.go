// Package dsh implements an event-level adapter for DSH, the DeepSeek Harness.
//
// SURFACE. DSH keeps one append-only session log per session at
// ${DSH_HOME:-~/.dsh}/sessions/<project-dir>/<session-dir>/session.jsonl.zstd.
// The default physical encoding is a concatenation of independent Zstandard
// frames — one checksummed frame for the header line, then one per durable
// append batch — so the file is JSONL only after decompression. A backend
// configured `compression: none` writes the same logical lines as plain
// session.jsonl instead, and discovery accepts both spellings. This is the
// second zstd surface in the harness matrix (codex is the other) and, unlike
// codex's, it is compressed BY DEFAULT: a reader without a zstd decoder sees
// nothing at all here rather than losing only cold sessions.
//
// WHAT IS READ. Accounting and identity records, by name:
//
//   - `session`          — the immutable header line: session id, cwd, agent preset.
//   - `assistant/message` — one completed model call: `data.usage` plus the
//     provider/model that served it. This is the ONLY usage record.
//   - `tool/call`         — one tool invocation the model requested, named and
//     correlated to its step.
//   - `request/context`   — provider/model fallback for a later completed call.
//   - `assistant/chunk`   — usage-chunk sequence numbers only, to detect a
//     completed message that lost its reported accounting.
//
// Everything else — user prompts, tool results, packed chunk rows, request
// headers with their system prompt and tool schemas — is ignored by TYPE, so
// content never enters a decode in the first place.
//
// SPLIT-IDENTITY TRAP (confirmed live). The same model call reports its usage
// TWICE: once as an `assistant/chunk` whose `chunk.type` is "usage", and once
// as the step's `assistant/message` `data.usage`. The numbers are identical —
// DSH's own token meter says so ("a final assistant-message usage for the same
// (turn, step) replaces that sample instead of double-counting it"). This
// adapter counts ONLY `assistant/message`. The message's `sourceEventSeqs` names
// every chunk seq it collapses, including that usage chunk, and is kept in the
// audit payload as the evidence that the collapse happened.
//
// TOKEN ACCOUNTING. DSH's TokenUsage counts are DISJOINT: `inputTokens` is
// UNCACHED input, with cached input reported separately as `cacheReadTokens`
// and `cacheWriteTokens` (billed input is the sum of the three), and
// `reasoningTokens` is a subdivision of `outputTokens` that is never added
// again. So the components map straight onto the Anthropic-style ledger columns
// and the total is their sum; the source reports no total of its own.
//
// FORK SEEDS. A forked session's log opens with its parent's leading events
// copied VERBATIM under a new session id, and `seedLength` counts them. Usage
// and activity keys are minted from the message identity, so those copies
// COLLAPSE onto the originals. Turn context cannot ride that: its value comes
// from the session HEADER, and the two headers can disagree about the very
// records they share, so the seeded prefix records no context here and leaves
// those turns to the log that owns them. See header.seeded.
//
// COST ATTRIBUTION. A DSH step is, by the harness's own definition, "one model
// call plus the tool executions it requested", and every `tool/call` carries the
// (turn, step) it belongs to. So a call joins its usage row EXACTLY, with no
// timestamp guessing: the usage row is the `assistant/message` of the same
// (turn, step). When a step somehow carries no assistant message, or more than
// one, the calls are emitted UNATTRIBUTED rather than attached to a guess.
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY, never written,
// locked, rotated or repaired — DSH's own loader repairs a torn tail, and this
// adapter must never be the thing that does it.
package dsh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// HomeEnv names the environment variable that moves the DSH home, and with it
// every session log this adapter reads. Exported for the same reason
// claudecode.ConfigDirEnv and codex.HomeEnv are: what gets collected is decided
// here, not by the defaults, and a supervised install must account for that.
//
// NOTE FOR THE INTEGRATOR: add dsh.HomeEnv to cmd.discoveryEnv(). Until then
// TestDiscoveryEnvCoversEveryAdapterVariable fails by design — it reads the
// adapter sources precisely so a new discovery variable cannot be forgotten.
const HomeEnv = "DSH_HOME"

const (
	// defaultHomeDir is the DSH home relative to the user's home directory.
	defaultHomeDir = ".dsh"
	// dirSessions is the sessions root inside the DSH home.
	dirSessions = "sessions"
	// transcriptPlain and transcriptZstd are the only two file names a session
	// directory may hold a transcript under. DSH's own discovery reads the fixed
	// transcript name and rejects flat <project>/<id>.jsonl artifacts rather than
	// ignoring them; matching that keeps a stray file from becoming a source.
	transcriptPlain = "session.jsonl"
	transcriptZstd  = "session.jsonl.zstd"
)

// maxDecoded bounds the bytes read out of one decompressed transcript. A zstd
// frame can expand by orders of magnitude, and a truncated or malformed file is
// exactly the case this adapter is required to tolerate rather than error on —
// so the read is bounded instead of trusted. One gigabyte of JSONL is far past
// any real session and small enough that hitting it is a fault, not a workload.
const maxDecoded = 1 << 30

// Record types this adapter reads. The set is an ALLOW-LIST: an unknown type is
// skipped before its `data` is decoded, so a record carrying prompt text, a
// tool result or a system prompt never becomes a value in this process.
const (
	typeSession   = "session"
	typeAssistant = "assistant/message"
	typeToolCall  = "tool/call"
	typeReqCtx    = "request/context"
)

// Adapter reads DSH session logs. Read-only.
type Adapter struct{}

// New returns a DSH adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolDSH }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "DSH" }

// Capabilities declares what this project can say about DSH.
//
// Cost is COMPUTED: nothing here calls SetCost. Activity is an EXACT join —
// the call and the usage object it was billed under come from the same record,
// so every activity row names the usage row that paid for it.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolDSH,
		Cost:      model.CostComputed,
		Activity:  model.ActivityExact,
		Reasoning: model.ReasoningReportFor(model.ToolDSH),
		Tier:      model.TierLive,
	}
}

// homes returns the configured DSH home directories. DSH_HOME may be a
// comma-separated list; otherwise the discovery root (override or ~/.dsh).
func (a Adapter) homes(cfg adapter.DiscoverConfig) []string {
	if env := strings.TrimSpace(os.Getenv(HomeEnv)); env != "" {
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
		def = filepath.Join(cfg.Home, defaultHomeDir)
	}
	return []string{cfg.Root(model.ToolDSH, def)}
}

// Discover locates every session transcript under <home>/sessions.
//
// The layout is <sessions>/<project-dir>/<session-dir>/session.jsonl[.zstd].
// The project directory encodes the session's cwd lossily (separators replaced,
// truncated to a filesystem component limit), so it is NOT parsed for the
// project: the header line inside the transcript carries the absolute cwd
// verbatim and is the only honest source for it. The walk is depth-agnostic and
// matches on the fixed transcript file names alone, which keeps a project
// directory rename or a deeper layout from silently emptying discovery.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source

	for _, home := range a.homes(cfg) {
		if home == "" {
			continue
		}
		root := filepath.Join(home, dirSessions)
		if !adapter.IsDir(root) {
			// A root configured directly at the sessions directory is still a
			// sessions directory; anything else has nothing to walk.
			root = home
		}
		if !adapter.IsDir(root) {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				// Skip unreadable entries; never fail the whole walk.
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if name := d.Name(); name != transcriptPlain && name != transcriptZstd {
				return nil
			}
			if !adapter.WalkEntryIsFile(d, path) {
				return nil
			}
			if _, dup := seen[path]; dup {
				return nil
			}
			seen[path] = struct{}{}
			srcs = append(srcs, adapter.Source{
				Tool:  model.ToolDSH,
				Class: model.EventLevel,
				Path:  path,
				Label: "dsh session " + filepath.Base(filepath.Dir(path)),
				Meta:  map[string]string{"root": root},
			})
			return nil
		})
	}
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].Path < srcs[j].Path })
	return srcs, nil
}

// Collect reads one session transcript in full.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental gates the transcript on size+mtime: an unchanged file is
// not opened at all. Any change re-reads the WHOLE file.
//
// There is deliberately no tail read. The default artifact is a stream of
// independent zstd frames whose boundaries are not derivable from a byte offset
// without parsing every block header, and DSH's crash recovery may TRUNCATE and
// RE-ENCODE the tail in place, which would leave a stored offset pointing into
// the middle of a rewritten frame. Re-reading is correct in both cases and the
// store collapses the re-derived dedup keys; the cost is one small file per
// changed session per cycle. A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	fi, err := os.Stat(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("dsh: stat %s: %w", src.Path, err)
	}
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()
	if cp != nil && cp.Size == size && cp.MTimeNS == mtimeNS {
		return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
	}

	res, err := readTranscript(ctx, src.Path, fi.ModTime().UTC())
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("dsh: read %s: %w", src.Path, err)
	}

	obs := adapter.Observation{
		Events:       res.events,
		Activity:     res.activity,
		TurnContexts: res.contexts,
	}
	if res.scanErr == nil && res.formatErr == nil {
		// The read reached the end of the stream. Skipped lines are permanently
		// unparseable rather than unread, so the checkpoint may advance; a scan
		// that aborted withholds it, and the unread remainder is picked up next
		// cycle.
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: model.ToolDSH, SourcePath: src.Path, Size: size, MTimeNS: mtimeNS,
		}
	}
	if res.formatErr != nil {
		return obs, errors.Join(fmt.Errorf("dsh: %s: %w", src.Path, res.formatErr), res.scanErr)
	}
	switch {
	case res.scanErr != nil && res.skipped > 0:
		return obs, fmt.Errorf("dsh: partial read of %s (%d unparseable record(s) skipped): %w", src.Path, res.skipped, res.scanErr)
	case res.scanErr != nil:
		return obs, fmt.Errorf("dsh: partial read of %s: %w", src.Path, res.scanErr)
	case res.skipped > 0:
		return obs, fmt.Errorf("dsh: skipped %d unparseable record(s) in %s", res.skipped, src.Path)
	}
	return obs, nil
}

// --- decode shapes -------------------------------------------------------
//
// PRIVACY BY SHAPE. These structs ARE the privacy boundary. encoding/json
// discards every key they do not name, so a field that does not exist here
// cannot become a value anywhere in this process. Note what is deliberately
// absent: toolCall has no `arguments`, assistantData.message has no `content`,
// and there is no shape at all for user/message, tool/result or request/header.

// header is the transcript's first logical line: the immutable SessionHeader.
type header struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	CreatedAt   int64  `json:"createdAt"`
	CWD         string `json:"cwd"`
	AgentPreset string `json:"agentPreset"`
	// SeedLength is how many LEADING EVENTS of this log were copied verbatim
	// from the parent named by parentSession. It is what separates the replayed
	// ancestor prefix from the session's own suffix; see header.seeded.
	SeedLength int64 `json:"seedLength"`
}

// seeded reports whether a record at this seq is part of the fork seed — the
// parent's leading events, copied verbatim into this log — rather than one of
// this session's own.
//
// This matters for exactly one fact: agentPreset. It is a HEADER field, so
// every record in the file would otherwise inherit it, and the seeded prefix's
// records did not run under it. DSH sets the child's header from the parent's
// LIVE scope chain rather than the parent's header, precisely because a parent
// that recomposed while blank runs on a newer preset than its header names — so
// the two headers CAN disagree about turns that appear in both files. Since a
// replayed record keeps the parent's message id, both files derive the same
// usage dedup key, and usage_turn_context is keyed (usage_dedup_key, dimension)
// with ON CONFLICT DO NOTHING: whichever transcript the walk reaches first wins
// silently, which is the ancestor's turns labelled with the fork's composition
// about half the time. Withholding the seeded prefix's context leaves those
// turns to the session that actually owns them.
//
// The predicate is DSH's own — `seq >= header.seedLength` is what its subagent
// projection uses to prove a record "comes from the child's OWN log suffix ...
// and not from a fork seed's replayed ancestor descriptor", and seedLength is
// an index into the event array (`events[i].seq === i`, seqs contiguous).
// A record with NO seq in a seeded log cannot be shown to be its own, so it is
// treated as replayed: a missing context is recoverable from the parent, a
// wrong one is not.
func (h header) seeded(seq *int64) bool {
	return h.SeedLength > 0 && (seq == nil || *seq < h.SeedLength)
}

// envelope is the common part of every storage record. `data` is left raw so it
// is only decoded once the type is known to be one this adapter reads.
type envelope struct {
	Type string          `json:"type"`
	Seq  *int64          `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
	// SourceEventSeqs names the earlier event seqs this record collapses. On an
	// assistant/message those are the assistant/chunk seqs — INCLUDING the usage
	// chunk that reports the same numbers — which makes it the evidence that the
	// split identity was collapsed rather than counted twice. Integers only.
	SourceEventSeqs []int64 `json:"sourceEventSeqs"`
}

// tokenUsage is DSH's TokenUsage. The counts are disjoint: InputTokens is
// UNCACHED input, CacheRead/CacheWrite are the rest of billed input, and
// ReasoningTokens is a subdivision of OutputTokens.
type tokenUsage struct {
	InputTokens     *int64 `json:"inputTokens"`
	OutputTokens    *int64 `json:"outputTokens"`
	CacheReadTokens *int64 `json:"cacheReadTokens"`
	CacheWriteTok   *int64 `json:"cacheWriteTokens"`
	ReasoningTokens *int64 `json:"reasoningTokens"`
}

func (u tokenUsage) allZero() bool {
	return deref(u.InputTokens) == 0 && deref(u.OutputTokens) == 0 &&
		deref(u.CacheReadTokens) == 0 && deref(u.CacheWriteTok) == 0 &&
		deref(u.ReasoningTokens) == 0
}

// messageSource is the assistant message's provenance. `model` is the provider
// model id that produced the message; `replayState.responseModel` is what the
// provider said it actually served, which can differ from the routed id and is
// kept for audit only.
type messageSource struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	ReplayState struct {
		ResponseModel string `json:"responseModel"`
		ResponseID    string `json:"responseId"`
	} `json:"replayState"`
}

// assistantData is the `assistant/message` payload. `message` deliberately
// exposes only its identity and provenance: there is no `content` field, so the
// model's text and its tool-call arguments are discarded at the decode.
type assistantData struct {
	Turn    int64 `json:"turn"`
	Step    int64 `json:"step"`
	Message struct {
		ID     string        `json:"id"`
		Source messageSource `json:"source"`
	} `json:"message"`
	Usage *tokenUsage `json:"usage"`
}

// toolCallData is the `tool/call` payload. It has NO `arguments` field, and
// that is the whole privacy story for activity: the command string, the file
// path and the prompt the model passed have nowhere to land. `name` is a name
// (`bash`, `glob`, `mcp__github__create_issue`) and is kept verbatim.
type toolCallData struct {
	Turn   int64  `json:"turn"`
	Step   int64  `json:"step"`
	CallID string `json:"callId"`
	Name   string `json:"name"`
}

// requestCtxData is the `request/context` payload: route metadata only, with no
// content field of any kind. It is the fallback when an assistant message names
// no model of its own.
type requestCtxData struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// --- audit payload -------------------------------------------------------

// auditRecord is the ALLOW-LIST of transcript fields persisted in
// UsageEvent.Raw: the token counters an audit of the stored accounting needs,
// the identifiers that tie the row back to its provider request, and the
// collapsed chunk seqs that prove the duplicate usage chunk was not counted.
//
// Everything else the record carries is never read into this shape, so it
// cannot reach the ledger. This is an allow-list on purpose: stripping known-bad
// keys out of a whole record would start leaking the day DSH grows a new one.
type auditRecord struct {
	Seq             int64      `json:"seq"`
	Time            int64      `json:"time,omitempty"`
	Turn            int64      `json:"turn,omitempty"`
	Step            int64      `json:"step,omitempty"`
	MessageID       string     `json:"messageId,omitempty"`
	Provider        string     `json:"provider,omitempty"`
	Model           string     `json:"model,omitempty"`
	ResponseModel   string     `json:"responseModel,omitempty"`
	ResponseID      string     `json:"responseId,omitempty"`
	Usage           auditUsage `json:"usage"`
	SourceEventSeqs []int64    `json:"sourceEventSeqs,omitempty"`
}

// auditUsage records the usage block as the transcript reported it: the counts
// are the provider's, not the values stored in the token columns, so a mismatch
// between the two stays visible.
type auditUsage struct {
	InputTokens     *int64 `json:"inputTokens,omitempty"`
	OutputTokens    *int64 `json:"outputTokens,omitempty"`
	CacheReadTokens *int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTok   *int64 `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens *int64 `json:"reasoningTokens,omitempty"`
}

// --- reading -------------------------------------------------------------

// result is one transcript read.
type result struct {
	events   []model.UsageEvent
	activity []model.ActivityEvent
	contexts []model.TurnContext
	skipped  int
	// scanErr is set when the stream ended early — a torn zstd tail, an
	// unreadable remainder, the decoded-size bound. It withholds the checkpoint.
	scanErr    error
	formatErr  error
	recognized bool
}

func (r *result) badFormat(reason string) {
	r.formatErr = errors.Join(r.formatErr, fmt.Errorf("%w: %s", adapter.ErrSourceFormat, reason))
}

// step identifies one model call within a session.
type step struct{ turn, step int64 }

// stepAnchor is what a step's assistant message contributes to the calls made
// in that step: the identity to key them on and, when the message carried
// usage, the ledger row to attribute them to.
type stepAnchor struct {
	messageID string
	usageKey  string
	model     string
	count     int // assistant/message records seen for this step
}

// pendingCall is a tool/call held until the whole transcript is read, so its
// step's assistant message can be resolved regardless of record order.
type pendingCall struct {
	seq  int64
	when time.Time
	at   step
	name string
	id   string
}

// openTranscript opens the transcript read-only and returns a reader over its
// logical JSONL lines, plus a closer for whatever it wrapped.
//
// A `.zstd` transcript is a concatenation of independent frames; the decoder
// consumes them as one stream, which is exactly how the file is meant to be
// read back. The decode is bounded by maxDecoded.
func openTranscript(path string) (io.Reader, func(), error) {
	f, err := os.Open(path) // read-only; never O_RDWR, never locked
	if err != nil {
		return nil, nil, err
	}
	if !strings.HasSuffix(path, ".zstd") {
		return f, func() { _ = f.Close() }, nil
	}
	// Concurrency 1: a decoder that spawns goroutines per frame would make a
	// per-cycle scan of many sessions unbounded in threads for no gain here.
	z, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return &boundedReader{r: z, left: maxDecoded}, func() { z.Close(); _ = f.Close() }, nil
}

// errTooLarge is returned once a decompressed transcript passes maxDecoded.
var errTooLarge = fmt.Errorf("decompressed transcript exceeds %d bytes", int64(maxDecoded))

// boundedReader stops a decompressed stream at maxDecoded with an ERROR rather
// than an EOF. An io.LimitReader would end the scan silently, which reads
// exactly like a complete file and would advance the checkpoint over the
// remainder — the one direction a checkpoint must never move.
type boundedReader struct {
	r    io.Reader
	left int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// readTranscript decodes one session log into usage events, activity and turn
// context. Malformed lines are skipped and counted; a stream that ends early
// keeps everything decoded so far and reports scanErr so the checkpoint is
// withheld.
func readTranscript(ctx context.Context, path string, mtime time.Time) (result, error) {
	r, closeFn, err := openTranscript(path)
	if err != nil {
		return result{}, err
	}
	defer closeFn()

	var (
		res       result
		hdr       header
		anchors   = map[step]*stepAnchor{}
		usageSeqs = map[int64]bool{}
		calls     []pendingCall
		curModel  string
		curProv   string
		// usage rows in log order, paired with the step they belong to so the
		// anchors can be resolved after the whole file is read.
		br = bufio.NewReaderSize(r, 64*1024)
	)

	// header is the FIRST NON-EMPTY line, not physical line 0. A blank line
	// ahead of it — one empty append frame, a re-encoded tail — would otherwise
	// shift the header into the record path, where its type is unknown and it is
	// discarded in silence: no session id, no cwd and no agentPreset for the
	// whole transcript, with no skipped count and no error to show for it.
	seenFirst := false

	for {
		if ctx.Err() != nil {
			res.scanErr = ctx.Err()
			break
		}
		raw, rerr := br.ReadBytes('\n')
		if len(raw) == 0 && rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				res.scanErr = rerr
			}
			break
		}
		trimmed := trimSpaceBytes(raw)
		if errors.Is(rerr, io.EOF) && len(trimmed) > 0 && !json.Valid(trimmed) {
			res.scanErr = io.ErrUnexpectedEOF
			break
		}
		if len(trimmed) > 0 {
			if !seenFirst {
				seenFirst = true
				// The first logical line is the header. It is decoded as a
				// header and NOT as an envelope: it carries no seq and no data.
				if err := json.Unmarshal(trimmed, &hdr); err != nil || hdr.Type != typeSession {
					res.skipped++
					hdr = header{}
				} else {
					res.recognized = true
				}
			} else if !decodeRecord(trimmed, mtime, hdr, path, &res, anchors, usageSeqs, &calls, &curModel, &curProv) {
				res.skipped++
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				// A torn tail frame is normal on a live session: keep what
				// decoded and withhold the checkpoint so the rest is re-read.
				res.scanErr = rerr
			}
			break
		}
	}
	if seenFirst && !res.recognized && res.scanErr == nil {
		res.badFormat("no recognized session records")
	}

	res.activity = resolveCalls(calls, anchors, hdr, path)
	return res, nil
}

// decodeRecord handles one non-header line. It returns false only when the line
// could not be decoded at all, which is what the skipped counter reports; a
// well-formed record of an uninteresting type is a success that emits nothing.
func decodeRecord(raw []byte, mtime time.Time, hdr header, path string, res *result,
	anchors map[step]*stepAnchor, usageSeqs map[int64]bool, calls *[]pendingCall, curModel, curProv *string) bool {

	var env envelope
	decodeErr := json.Unmarshal(raw, &env)
	expectedUsage := false
	for _, seq := range env.SourceEventSeqs {
		expectedUsage = expectedUsage || usageSeqs[seq]
	}
	// Tool results also carry collapse provenance. A referenced usage chunk
	// identifies an assistant message even if its type field was renamed.
	collapsed := env.SourceEventSeqs != nil && (env.Type == typeAssistant || expectedUsage)
	if decodeErr != nil {
		if collapsed {
			res.badAssistant(env, anchors, "malformed collapsed assistant record")
		}
		return false
	}
	// The current writer attaches this collapse provenance to assistant/message.
	// Legacy records without it retain their stable identity/time fallbacks.
	if collapsed && (env.Type != typeAssistant || env.Seq == nil || *env.Seq < 0 || env.Time <= 0) {
		res.badAssistant(env, anchors, "collapsed assistant record missing type, seq or time")
		return true
	}
	switch env.Type {
	case "assistant/chunk":
		// Only remember accounting-chunk identities. No content is decoded or
		// emitted, and the completed message remains the only usage row.
		var d struct {
			Chunk struct {
				Type  string      `json:"type"`
				Usage *tokenUsage `json:"usage"`
			} `json:"chunk"`
		}
		if env.Seq != nil && json.Unmarshal(env.Data, &d) == nil && d.Chunk.Type == "usage" && d.Chunk.Usage != nil {
			usageSeqs[*env.Seq] = true
		}
		return true
	case typeReqCtx:
		res.recognized = true
		var d requestCtxData
		if json.Unmarshal(env.Data, &d) == nil {
			if d.Model != "" {
				*curModel = d.Model
			}
			if d.Provider != "" {
				*curProv = d.Provider
			}
		}
		return true

	case typeToolCall:
		res.recognized = true
		var d toolCallData
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return false
		}
		if d.Name == "" {
			return true // a call with no name is not a name we can record
		}
		*calls = append(*calls, pendingCall{
			seq:  derefSeq(env.Seq),
			when: recordTime(env.Time, hdr, mtime),
			at:   step{turn: d.Turn, step: d.Step},
			name: d.Name,
			id:   d.CallID,
		})
		return true

	case typeAssistant:
		res.recognized = true
		var d assistantData
		if err := json.Unmarshal(env.Data, &d); err != nil {
			if collapsed {
				res.badAssistant(env, anchors, "malformed collapsed assistant data")
			}
			return false
		}
		if collapsed {
			if strings.TrimSpace(d.Message.ID) == "" {
				res.badAssistant(env, anchors, "collapsed assistant message missing id")
				return true
			}
			// The writer may omit usage for a no-accounting response, but not
			// after it received an explicitly referenced accounting chunk.
			if (expectedUsage && d.Usage == nil) || (d.Usage != nil && (d.Usage.InputTokens == nil || d.Usage.OutputTokens == nil)) {
				res.badAssistant(env, anchors, "collapsed assistant message missing reported input/output usage")
				return true
			}
		}
		at := step{turn: d.Turn, step: d.Step}
		a := anchors[at]
		if a == nil {
			a = &stepAnchor{}
			anchors[at] = a
		}
		a.count++
		if a.count == 1 {
			a.messageID = d.Message.ID
		}

		mdl := d.Message.Source.Model
		if mdl == "" {
			mdl = *curModel
		}
		prov := d.Message.Source.Provider
		if prov == "" {
			prov = *curProv
		}
		if a.count == 1 {
			a.model = mdl
		}
		if d.Usage == nil || d.Usage.allZero() {
			// A step whose provider reported no accounting (or an empty stream)
			// is a real step with no cost to record. It still anchors its calls.
			return true
		}

		seq := derefSeq(env.Seq)
		when := recordTime(env.Time, hdr, mtime)
		ev := buildEvent(*d.Usage, d, hdr, mdl, prov, seq, when, path)
		ev.Raw = auditPayload(env, d, seq)
		res.events = append(res.events, ev)
		if a.count == 1 {
			a.usageKey = ev.DedupKey
		} else {
			// Two usage-bearing assistant messages for one step: the join stops
			// being 1:1, so the step attributes nothing rather than half of it.
			a.usageKey = ""
		}
		if hdr.AgentPreset != "" && !hdr.seeded(env.Seq) {
			res.contexts = append(res.contexts, model.TurnContext{
				UsageDedupKey: ev.DedupKey,
				Tool:          model.ToolDSH,
				Dimension:     model.DimensionAgent,
				Value:         hdr.AgentPreset,
				SessionID:     ev.SessionID,
				Project:       ev.Project,
				Model:         ev.Model,
				EventTime:     ev.EventTime,
				SourcePath:    path,
			})
		}
		return true
	}
	// Every other record type — user/message, tool/result, request/header, the
	// packed text-chunks / reasoning-chunks / tool-call-chunks rows — is ignored
	// by name, undecoded.
	return true
}

func (r *result) badAssistant(env envelope, anchors map[step]*stepAnchor, reason string) {
	r.badFormat(reason)
	// Rejecting one message must not make another message in its step an exact
	// call join. Preserve a known message identity so accounting recovery cannot
	// insert its calls again under different keys, but withhold usage attribution.
	var d struct {
		Turn    int64           `json:"turn"`
		Step    int64           `json:"step"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(env.Data, &d) == nil && d.Turn > 0 && d.Step > 0 {
		at := step{turn: d.Turn, step: d.Step}
		a := anchors[at]
		if a == nil {
			a = &stepAnchor{}
			anchors[at] = a
		}
		a.count++
		a.usageKey = ""
		if a.count == 1 {
			var message struct {
				ID     string        `json:"id"`
				Source messageSource `json:"source"`
			}
			if json.Unmarshal(d.Message, &message) == nil && strings.TrimSpace(message.ID) != "" {
				a.messageID = message.ID
				a.model = message.Source.Model
			}
		}
	}
}

// buildEvent maps one DSH TokenUsage onto a ledger row.
//
// The components are already disjoint at the source, so they map one-to-one and
// the total is their sum: DSH reports no total of its own, and computing it as
// input+output+cacheRead+cacheWrite is exactly the harness's own arithmetic
// ("billed input = sum of the three", reasoning "not added again").
func buildEvent(u tokenUsage, d assistantData, hdr header, mdl, prov string,
	seq int64, when time.Time, path string) model.UsageEvent {

	in := deref(u.InputTokens)
	out := deref(u.OutputTokens)
	cr := deref(u.CacheReadTokens)
	cw := deref(u.CacheWriteTok)
	reason := deref(u.ReasoningTokens)
	if reason > out {
		reason = out // reasoning is a subdivision of output, never more than it
	}

	ev := model.UsageEvent{
		Tool:                model.ToolDSH,
		Model:               mdl,
		Provider:            prov,
		SessionID:           hdr.ID,
		Project:             hdr.CWD,
		EventTime:           when,
		InputTokens:         in,
		OutputTokens:        out,
		CacheCreationTokens: cw,
		CacheReadTokens:     cr,
		ReasoningTokens:     reason,
		TotalTokens:         in + out + cr + cw,
		MessageID:           d.Message.ID,
		RequestID:           d.Message.Source.ReplayState.ResponseID,
		SourcePath:          path,
		Kind:                model.KindUsage,
	}
	ev.DedupKey = usageKey(d.Message.ID, hdr.ID, seq)
	return ev
}

// usageKey is the assistant message's own identity.
//
// The message id is a randomUUID minted per message, and it is preferred over
// the session-scoped seq for a reason the seq cannot serve: a DSH session may
// be FORKED, and a forked child's log opens with its parent's leading events
// copied VERBATIM (SessionHeader.seedLength records how many). Keyed on the
// message id, those copies collapse onto the originals; keyed on session+seq
// they would be new rows, and the parent's history would be counted once per
// fork taken from it.
//
// The fallback exists only for a record with no message id — which the harness
// itself never writes, since createAssistantMessage always mints one — and is
// scoped to the session so two sessions cannot collide on a bare seq. Such a
// record duplicates across a fork; that is the narrower of the two failures and
// it is confined to logs a foreign writer produced.
func usageKey(messageID, sessionID string, seq int64) string {
	if messageID != "" {
		return model.ToolDSH + "|msg|" + messageID
	}
	return model.ToolDSH + "|" + sessionID + "|seq|" + strconv.FormatInt(seq, 10)
}

// resolveCalls turns the buffered tool/call records into activity, attributing
// each to the usage row of its own (turn, step).
//
// A DSH step is "one model call plus the tool executions it requested", so this
// join is the source's own structure rather than a proximity guess. CallsInTurn
// is the number of calls sharing that step, and TurnSeq their 0-based order.
// Both are what this pass observed: the store derives the cost divisor by
// COUNTING the stored rows, so a step read while it was still being written
// corrects itself on the next pass rather than freezing a low denominator.
func resolveCalls(calls []pendingCall, anchors map[step]*stepAnchor, hdr header,
	path string) []model.ActivityEvent {

	if len(calls) == 0 {
		return nil
	}
	total := map[step]int{}
	for _, c := range calls {
		total[c.at]++
	}
	seen := map[step]int{}

	out := make([]model.ActivityEvent, 0, len(calls))
	for _, c := range calls {
		idx := seen[c.at]
		seen[c.at]++

		var (
			usageKey  string
			anchorID  string
			turnModel string
		)
		if a := anchors[c.at]; a != nil && a.count == 1 {
			// Exactly one assistant message for this step: the join is 1:1.
			usageKey, anchorID, turnModel = a.usageKey, a.messageID, a.model
		}

		out = append(out, model.ActivityEvent{
			Tool:          model.ToolDSH,
			Kind:          model.ActivityTool,
			Name:          c.name,
			SessionID:     hdr.ID,
			Project:       hdr.CWD,
			Model:         turnModel,
			EventTime:     c.when,
			UsageDedupKey: usageKey,
			MessageID:     anchorID,
			TurnSeq:       idx,
			CallsInTurn:   total[c.at],
			SourcePath:    path,
			DedupKey:      activityKey(anchorID, hdr.ID, c),
		})
	}
	return out
}

// activityKey binds a call to the assistant message that issued it, so a forked
// session's replayed calls collapse onto the originals exactly as their usage
// rows do. A provider call id is unique within its request but not across
// sessions or providers, which is why it is never the whole key.
//
// Without a resolvable anchor the key falls back to the session and the call
// record's own seq — stable across polls (the log is append-only and seqs are
// contiguous and immutable) but session-scoped, so such a call duplicates
// across a fork. It carries no cost attribution either way.
func activityKey(anchorID, sessionID string, c pendingCall) string {
	if anchorID != "" {
		id := c.id
		if id == "" {
			id = "#" + strconv.FormatInt(c.seq, 10)
		}
		return model.ToolDSH + "|call|" + anchorID + "|" + id
	}
	return model.ToolDSH + "|call|" + sessionID + "|seq|" + strconv.FormatInt(c.seq, 10)
}

// auditPayload builds the stored audit payload from the decoded record. Values
// are copied out of the typed decode, never sliced out of the original bytes.
// Best-effort: an un-marshalable payload yields an empty raw rather than
// failing the parse.
func auditPayload(env envelope, d assistantData, seq int64) string {
	a := auditRecord{
		Seq:           seq,
		Time:          env.Time,
		Turn:          d.Turn,
		Step:          d.Step,
		MessageID:     d.Message.ID,
		Provider:      d.Message.Source.Provider,
		Model:         d.Message.Source.Model,
		ResponseModel: d.Message.Source.ReplayState.ResponseModel,
		ResponseID:    d.Message.Source.ReplayState.ResponseID,
		Usage: auditUsage{
			InputTokens:     d.Usage.InputTokens,
			OutputTokens:    d.Usage.OutputTokens,
			CacheReadTokens: d.Usage.CacheReadTokens,
			CacheWriteTok:   d.Usage.CacheWriteTok,
			ReasoningTokens: d.Usage.ReasoningTokens,
		},
		SourceEventSeqs: env.SourceEventSeqs,
	}
	b, err := json.Marshal(a)
	if err != nil {
		return ""
	}
	return string(b)
}

// recordTime converts a record's epoch-millisecond stamp to UTC, falling back
// to the session's creation stamp and then the file mtime.
func recordTime(ms int64, hdr header, mtime time.Time) time.Time {
	if ms > 0 {
		return time.UnixMilli(ms).UTC()
	}
	if hdr.CreatedAt > 0 {
		return time.UnixMilli(hdr.CreatedAt).UTC()
	}
	return mtime
}

func derefSeq(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return adapter.NonNeg(*p)
}

// trimSpaceBytes trims ASCII whitespace without allocating.
func trimSpaceBytes(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
