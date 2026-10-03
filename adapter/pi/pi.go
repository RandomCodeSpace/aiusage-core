// Package pi implements event-level adapters for TWO harnesses that share ONE
// session format: Pi (earendil-works/pi, `@earendil-works/pi-coding-agent`) and
// OpenClaw (openclaw/openclaw), which ships pi's own session manager. The
// on-disk record shapes are identical — verified byte for byte against live
// sessions from both CLIs on 2026-08-16 — so one parser serves both and the
// only differences are where the files live and what tool id the rows carry.
//
// SURFACE. An append-only JSONL tree, one file per session:
//
//	Pi:       ${PI_CODING_AGENT_DIR:-~/.pi/agent}/sessions/<encoded-cwd>/<ts>_<uuid>.jsonl
//	          (or ${PI_CODING_AGENT_SESSION_DIR} used directly as the sessions dir)
//	OpenClaw: ${OPENCLAW_STATE_DIR:-${OPENCLAW_HOME:-~}/.openclaw}/agents/<id>/sessions/<uuid>.jsonl
//
// Line 1 is the session header (`{"type":"session","id":<uuid>,"cwd":...}`).
// Every later line is an entry with its own `id` and a `parentId`, forming a
// tree; branching appends rather than rewrites. Token usage lives on three
// entry kinds: `message` entries whose `.message.role == "assistant"` carry
// `.message.usage`, and `compaction` / `branch_summary` entries carry the usage
// of the LLM call that produced the summary.
//
// TOKEN SEMANTICS, read off pi-ai's own normalisers rather than guessed:
// `input`, `output`, `cacheRead` and `cacheWrite` are DISJOINT (the OpenAI path
// computes `input = prompt_tokens - cached - cache_write`, and calculateCost
// tiers on `input + cacheRead + cacheWrite`), `totalTokens` is their sum, and
// `reasoning` is a SUBSET of `output` — adding it to a total would bill the same
// token twice. `cost` is pi's own per-call computation from its model catalogue,
// in USD.
//
// WHAT IS CAPTURED: usage and activity. A `toolCall` content block sits in the
// SAME record as the usage object it cost, so tool calls attribute exactly, with
// the divisor counted from that one record. There is NO turn context: the entry
// shape is `{type,id,parentId,timestamp}` and carries no attribution field of
// any kind, and a subagent gets its own session FILE rather than a marker on the
// parent's turn, so there is nothing to record and nothing to infer.
package pi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// Environment variables that move what these adapters READ. Every one of them
// is exported because a supervised install must know that the collected surface
// was relocated by the environment: a unit does not inherit the installing
// shell, so an install made under one of these would collect somewhere else
// than the shell that installed it (see cmd.discoveryEnv).
//
// Every lookup spells its variable with one of these constants AT the os.Getenv
// call, never through a helper's parameter: cmd.TestDiscoveryEnvCoversEveryAdapterVariable
// parses this file and resolves the ARGUMENT of each lookup through the
// package's own constants, so a name it cannot resolve is a variable the
// automatic install cannot suppress — and a unit installed under one of those
// would collect from the default location while the shell that installed it
// read somewhere else.
const (
	// AgentDirEnv moves Pi's agent directory, and with it the sessions tree
	// beneath it. It is `<APP_NAME>_CODING_AGENT_DIR` in pi's config.js, where
	// APP_NAME comes from the package's own piConfig — "PI" for
	// @earendil-works/pi-coding-agent. OpenClaw reads this variable too, as the
	// documented fallback for OpenClawAgentDirEnv.
	//
	// NOT `PI_AGENT_DIR`: that name appears in third-party tooling but pi itself
	// never reads it (verified against pi 0.84.2's dist/config.js), so honouring
	// it would point this adapter at a directory the harness does not use.
	AgentDirEnv = "PI_CODING_AGENT_DIR"
	// SessionDirEnv points Pi at a sessions directory directly, bypassing the
	// <agent dir>/sessions/<encoded-cwd> layout. It is the environment form of
	// --session-dir and wins over AgentDirEnv for session lookup.
	SessionDirEnv = "PI_CODING_AGENT_SESSION_DIR"

	// OpenClawStateDirEnv is OpenClaw's state root; everything else hangs off it.
	OpenClawStateDirEnv = "OPENCLAW_STATE_DIR"
	// OpenClawHomeEnv replaces the home directory OpenClaw derives `.openclaw`
	// from, ahead of HOME/USERPROFILE.
	OpenClawHomeEnv = "OPENCLAW_HOME"
	// OpenClawAgentDirEnv overrides the per-agent directory. The sessions tree is
	// its sibling, so an override is scanned via its parent.
	OpenClawAgentDirEnv = "OPENCLAW_AGENT_DIR"
	// OpenClawConfigPathEnv names the config file, which may itself relocate an
	// agent's directory (`agents.list[].agentDir`). This adapter does not parse
	// the config — it is declared because it can move the surface, and a run
	// under it must not be treated as the default install.
	OpenClawConfigPathEnv = "OPENCLAW_CONFIG_PATH"
	// OpenClawProfileEnv (and its `--profile` flag) relocates the whole state
	// root to `~/.openclaw-<name>`; only the "default" profile keeps
	// `~/.openclaw`. The CLI resolves it into OpenClawStateDirEnv inside its own
	// process, which this adapter never sees, so discovery ALSO globs sibling
	// `.openclaw-*` roots. That is best-effort by construction: a profile whose
	// state dir was set explicitly to somewhere else is only found through
	// OpenClawStateDirEnv.
	OpenClawProfileEnv = "OPENCLAW_PROFILE"
)

const (
	dirSessions     = "sessions"
	dirAgents       = "agents"
	dirCrestodian   = "crestodian"
	openClawRootDir = ".openclaw"
	// trajectorySuffix names OpenClaw's per-run trace sidecar, which sits in the
	// sessions directory under the session's own name and ends in `.jsonl` —
	// i.e. it is caught by every glob written for the session files themselves.
	// It must never be read: see the doc comment on excluded().
	trajectorySuffix = ".trajectory.jsonl"
	jsonlSuffix      = ".jsonl"
)

// Adapter reads one harness's pi-format session transcripts. Read-only: it
// opens files for reading, takes no lock, and writes nothing.
type Adapter struct {
	tool  string
	label string
}

// NewPi returns the adapter for Pi.
func NewPi() adapter.Adapter { return Adapter{tool: model.ToolPi, label: "Pi"} }

// NewOpenClaw returns the adapter for OpenClaw.
func NewOpenClaw() adapter.Adapter { return Adapter{tool: model.ToolOpenClaw, label: "OpenClaw"} }

// ID returns the stable tool identifier.
func (a Adapter) ID() string { return a.tool }

// DisplayName returns the human-friendly name.
func (a Adapter) DisplayName() string { return a.label }

// Capabilities declares what this project can say about this adapter's harness.
//
// Both harnesses declare the SAME values because they are one package over one
// byte-identical session format — what the code can observe does not depend on
// which of the two wrote the file. They remain two declarations under two tool
// ids because they are two tools, and their rows are never summed.
//
// Cost is VENDOR-reported: pi.go stamps a.tool+"-reported" from the figure the
// harness itself recorded, which collect.stampCost is forbidden to overwrite.
// Activity is an EXACT join because a toolCall block sits in the SAME record as
// the usage object it cost.
func (a Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      a.tool,
		Cost:      model.CostVendor,
		Activity:  model.ActivityExact,
		Reasoning: model.ReasoningReportFor(a.tool),
		Tier:      model.TierLive,
	}
}

// Discover locates the session transcripts of this adapter's harness.
//
// Both harnesses are scanned by the same rule — every `*.jsonl` under a
// discovered sessions root — and both exclude the same sidecars. Results are
// sorted by path so a fork and its source are always visited in the same order,
// which makes which of the two duplicates lands in the ledger deterministic
// across passes (their dedup keys are equal, so only the first is stored).
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source
	for _, root := range a.sessionRoots(cfg) {
		a.scan(ctx, root, seen, &srcs)
	}
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].Path < srcs[j].Path })
	return srcs, nil
}

// sessionRoots resolves the directories to scan for session files.
func (a Adapter) sessionRoots(cfg adapter.DiscoverConfig) []string {
	if a.tool == model.ToolPi {
		return a.piRoots(cfg)
	}
	return a.openClawRoots(cfg)
}

// piRoots resolves Pi's sessions directory. PI_CODING_AGENT_SESSION_DIR names a
// sessions directory outright (the environment form of --session-dir, which pi
// uses as the directory itself rather than as a parent); otherwise it is
// <agent dir>/sessions, with the agent dir from PI_CODING_AGENT_DIR, the
// per-tool override, or ~/.pi/agent.
func (a Adapter) piRoots(cfg adapter.DiscoverConfig) []string {
	if dir := strings.TrimSpace(os.Getenv(SessionDirEnv)); dir != "" {
		return []string{dir}
	}
	agentDir := strings.TrimSpace(os.Getenv(AgentDirEnv))
	if agentDir == "" {
		def := ""
		if cfg.Home != "" {
			def = filepath.Join(cfg.Home, ".pi", "agent")
		}
		agentDir = cfg.Root(model.ToolPi, def)
	}
	if agentDir == "" {
		return nil
	}
	return []string{filepath.Join(agentDir, dirSessions)}
}

// openClawRoots resolves the sessions directories of every OpenClaw agent.
//
// The state root is OPENCLAW_STATE_DIR, else <OPENCLAW_HOME or home>/.openclaw,
// plus every sibling `.openclaw-<profile>` directory: `--profile x` /
// OPENCLAW_PROFILE=x relocates the whole root that way and resolves it into
// OPENCLAW_STATE_DIR only inside the CLI's own process, so a profile that is not
// currently exported is findable by name alone. Under each root the sessions
// live at agents/<id>/sessions; the legacy pre-migration `sessions/` and the
// chat engine's `crestodian/sessions/` are scanned too, since both still hold
// files in this exact format.
func (a Adapter) openClawRoots(cfg adapter.DiscoverConfig) []string {
	var roots []string
	add := func(p string) {
		if p != "" {
			roots = append(roots, p)
		}
	}

	if state := strings.TrimSpace(os.Getenv(OpenClawStateDirEnv)); state != "" {
		add(state)
	} else {
		home := strings.TrimSpace(os.Getenv(OpenClawHomeEnv))
		if home == "" {
			home = cfg.Home
		}
		base := cfg.Root(model.ToolOpenClaw, "")
		if base != "" && base != cfg.Home {
			// An explicit per-tool override names the state root itself.
			add(base)
		} else if home != "" {
			add(filepath.Join(home, openClawRootDir))
			// Profile roots: ~/.openclaw-<name>. Globbing them is the only way to
			// see a profile the environment is not currently pointing at.
			matches, _ := filepath.Glob(filepath.Join(home, openClawRootDir+"-*"))
			sort.Strings(matches)
			for _, m := range matches {
				if adapter.IsDir(m) {
					add(m)
				}
			}
		}
	}

	var out []string
	for _, root := range roots {
		out = append(out,
			filepath.Join(root, dirAgents),
			filepath.Join(root, dirSessions),
			filepath.Join(root, dirCrestodian),
		)
	}
	// An agent-dir override moves <root>/agents/<id>/agent; its sessions sibling
	// hangs off the same parent, so the parent is what gets scanned. OpenClaw
	// falls back to Pi's variable for this override, and so does this.
	agentDir := strings.TrimSpace(os.Getenv(OpenClawAgentDirEnv))
	if agentDir == "" {
		agentDir = strings.TrimSpace(os.Getenv(AgentDirEnv))
	}
	if agentDir != "" {
		out = append(out, filepath.Dir(agentDir), agentDir)
	}
	return out
}

// scan walks one root for session transcripts.
func (a Adapter) scan(ctx context.Context, root string, seen map[string]struct{}, srcs *[]adapter.Source) {
	if !adapter.IsDir(root) {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if !strings.HasSuffix(name, jsonlSuffix) || excluded(name) {
			return nil
		}
		if !adapter.WalkEntryIsFile(d, path) {
			return nil
		}
		abs := path
		if p, err := filepath.Abs(path); err == nil {
			abs = p
		}
		if _, dup := seen[abs]; dup {
			return nil
		}
		seen[abs] = struct{}{}
		*srcs = append(*srcs, adapter.Source{
			Tool:  a.tool,
			Class: model.EventLevel,
			Path:  path,
			Label: a.tool + " session " + adapter.FileStem(path),
		})
		return nil
	})
}

// excluded rejects the sidecars that share the sessions directory and the
// `.jsonl` extension with the transcripts.
//
// OpenClaw writes `<session>.trajectory.jsonl` next to `<session>.jsonl`, and it
// is a TRAP, not merely noise. Measured on one live two-call run: the trajectory
// repeats that run's tokens FOUR times — `model.completed.data.usage` and
// `trace.artifacts.data.usage` both carry the run-cumulative
// {input 41329, output 20, total 41349}, `messagesSnapshot` re-embeds both
// assistant messages with their own per-call usage objects (20668 + 20681 =
// 41349 again), and `promptCache.lastCallUsage` carries the LAST call's 20681
// twice more. A parser that took every usage object it found would report
// 206,758 tokens for a run that cost 41,349 — 5x — and it would do it while
// reading a file that also contains the system prompt, the user prompt, every
// assistant text and the tool catalogue. The session transcript alone already
// sums to exactly 41,349, so the sidecar adds no fact and every risk.
//
// The name filter is the cheap gate; readSession's header check is the one that
// actually decides, since it refuses any file whose first record is not a
// `session` header regardless of what it is called.
func excluded(lowerName string) bool {
	return strings.HasSuffix(lowerName, trajectorySuffix)
}

// ckptState is the per-file parse state persisted in the checkpoint. A tail read
// resumes past the session header and past every model_change, so the facts
// those records establish have to survive the gap: the session id and cwd for
// every row's identity, and the provider/model carry-forward that
// compaction/branch_summary usage depends on (those entries name no model of
// their own). Rejected is retained for old checkpoints: refused sources now
// return a format error without a checkpoint, and old refusals never tail-read.
type ckptState struct {
	SessionID string `json:"session,omitempty"`
	Project   string `json:"project,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Rejected  bool   `json:"rejected,omitempty"`
}

// Collect reads one session transcript in full.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental reads only what is new since cp: an unchanged size+mtime
// skips the file; pure growth tail-reads from the stored offset with the
// persisted header/model state; anything else (a shrink, a same-size rewrite —
// pi rewrites a whole file when it migrates its session version) re-reads from
// zero, which is harmless because every dedup key is derived from content.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	f, err := os.Open(src.Path) // read-only; no lock, no write, no rotation
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("%s: open %s: %w", a.tool, src.Path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("%s: stat %s: %w", a.tool, src.Path, err)
	}
	return a.collectReader(ctx, src, cp, f, fi)
}

func (a Adapter) collectReader(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint, f io.ReadSeeker, fi os.FileInfo) (adapter.Observation, error) {
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()

	var (
		start int64
		state ckptState
	)
	if cp != nil {
		if cp.Size == size && cp.MTimeNS == mtimeNS {
			return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
		}
		if size > cp.Size && cp.Offset > 0 && cp.Offset <= size && cp.State != "" {
			var st ckptState
			if err := json.Unmarshal([]byte(cp.State), &st); err == nil && !st.Rejected {
				start, state = cp.Offset, st
			}
		}
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return adapter.Observation{}, fmt.Errorf("%s: seek %s: %w", a.tool, src.Path, err)
		}
	}

	obs, newState, consumed, checkpointOK, err := a.readSession(ctx, f, src.Path, start, state)
	if !checkpointOK {
		return obs, err
	}
	stateJSON, marshalErr := json.Marshal(newState)
	if marshalErr != nil {
		return obs, err // no checkpoint: the next cycle re-reads and re-derives
	}
	obs.Checkpoint = &model.SourceCheckpoint{
		Tool: a.tool, SourcePath: src.Path,
		Size: size, MTimeNS: mtimeNS, Offset: consumed, State: string(stateJSON),
	}
	return obs, err
}

// readSession parses the transcript from the reader's current position.
func (a Adapter) readSession(ctx context.Context, r io.Reader, path string, start int64, state ckptState) (adapter.Observation, ckptState, int64, bool, error) {
	var (
		obs         adapter.Observation
		consumed    = start
		br          = bufio.NewReaderSize(r, 64*1024)
		first       = start == 0
		diagnostics error
	)
	for {
		if ctx.Err() != nil {
			return obs, state, consumed, false, ctx.Err()
		}
		raw, rerr := br.ReadBytes('\n')
		terminated := rerr == nil
		if rerr != nil && rerr != io.EOF {
			return obs, state, consumed, false, fmt.Errorf("%s: read %s: %w", a.tool, path, rerr)
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 {
			var e entry
			decodeErr := json.Unmarshal(trimmed, &e)
			if decodeErr != nil {
				// The writer may still be completing an unterminated record. A complete
				// malformed record is visible but does not prevent later valid rows.
				if !terminated {
					break
				}
				var typeErr *json.UnmarshalTypeError
				if errors.As(decodeErr, &typeErr) && (first || typeErr.Field == "type" || e.Type == "session" || e.Type == "message" || e.Usage != nil) {
					return obs, state, consumed, false, fmt.Errorf("%s: %s at byte %d: %w: incompatible %s", a.tool, path, consumed, adapter.ErrSourceFormat, typeErr.Field)
				}
				diagnostics = errors.Join(diagnostics, fmt.Errorf("%s: %s at byte %d: malformed record: %w", a.tool, path, consumed, decodeErr))
			} else {
				if anchor := missingAnchor(e, first); anchor != "" {
					if !terminated {
						break
					}
					return obs, state, consumed, false, errors.Join(diagnostics, fmt.Errorf("%s: %s at byte %d: %w: missing or incompatible %s", a.tool, path, consumed, adapter.ErrSourceFormat, anchor))
				}
				first = false
				a.apply(&obs, &state, e, path)
			}
		}
		if terminated {
			consumed += int64(len(raw))
			continue
		}
		// Even valid unterminated JSON is replayed after the writer adds its newline.
		break
	}
	if ctx.Err() != nil {
		return obs, state, consumed, false, ctx.Err()
	}
	if first && consumed > 0 {
		return obs, state, consumed, false, errors.Join(diagnostics, fmt.Errorf("%s: %s: %w: missing session header", a.tool, path, adapter.ErrSourceFormat))
	}
	return obs, state, consumed, true, diagnostics
}

// Required facts are checked on the existing allow-listed decode. Content is
// not decoded again just to validate a transcript envelope.
func missingAnchor(e entry, first bool) string {
	if first && e.Type != "session" {
		return "type=session"
	}
	if e.Type == "session" {
		if e.ID == "" {
			return "session.id"
		}
		if parseTime(e.Timestamp).IsZero() {
			return "session.timestamp"
		}
		if e.CWD == "" {
			return "session.cwd"
		}
	}
	if e.Type == "message" {
		if e.Message == nil || e.Message.Role == "" {
			return "message.role"
		}
		if e.Message.Role == "assistant" && e.Message.Usage == nil {
			return "message.usage"
		}
	}
	if e.Message != nil && e.Message.Role == "assistant" && e.Type != "message" {
		return "type=message"
	}
	var u *usage
	if e.Type == "message" && e.Message != nil && e.Message.Role == "assistant" {
		u = e.Message.Usage
	}
	if e.Type == "compaction" || e.Type == "branch_summary" {
		u = e.Usage
	}
	if u != nil {
		if e.ID == "" {
			return "usage entry.id"
		}
		if parseTime(e.Timestamp).IsZero() {
			return "usage entry.timestamp"
		}
		if u.Input == nil || u.Output == nil {
			return "usage.input/output"
		}
	}
	return ""
}

// apply folds one decoded entry into the observation and the carry-forward state.
func (a Adapter) apply(obs *adapter.Observation, state *ckptState, e entry, path string) {
	switch e.Type {
	case "session":
		state.SessionID = e.ID
		state.Project = e.CWD
		return
	case "model_change":
		// The only carry-forward pi needs. Assistant messages name their own
		// model and provider; compaction and branch_summary name neither.
		if e.ModelID != "" {
			state.Model = e.ModelID
		}
		if e.Provider != "" {
			state.Provider = e.Provider
		}
		return
	case "message":
		if e.Message == nil || e.Message.Role != "assistant" || e.Message.Usage == nil {
			// user, toolResult and usage-less assistant records carry no tokens.
			return
		}
		mdl, provider := e.Message.Model, e.Message.Provider
		if mdl == "" {
			mdl = state.Model
		}
		if provider == "" {
			provider = state.Provider
		}
		ev, ok := a.event(e, *e.Message.Usage, mdl, provider, e.Message.API, e.Message.ResponseModel,
			e.Message.ResponseID, e.Message.StopReason, *state, path)
		if !ok {
			return
		}
		obs.Events = append(obs.Events, ev)
		obs.Activity = append(obs.Activity, a.calls(e, ev, mdl, *state, path)...)
		return
	case "compaction", "branch_summary":
		// Real spend: the LLM call that wrote the summary. It names no model, so
		// the carry-forward answers for it.
		if e.Usage == nil {
			return
		}
		if ev, ok := a.event(e, *e.Usage, state.Model, state.Provider, "", "", "", "", *state, path); ok {
			obs.Events = append(obs.Events, ev)
		}
		return
	}
}

// event builds one usage row from a usage object.
//
// THE DEDUP KEY EXCLUDES THE SESSION ID, and that is the single most important
// decision in this adapter. `pi --fork <session>` (and the interactive/RPC fork
// commands, and resuming a session found in another project) writes a NEW file
// with a NEW session uuid and COPIES every entry of the source into it verbatim
// — same entry id, same timestamp, same usage object. Verified live: forking a
// 2-call session reproduced entry `c9f6aa91` with its 1234 tokens and its
// `call_7py5mn1h` tool call under a different session id. A key of
// session+entry would count that spend once per fork; a key of the entry id
// alone would be an 8-hex-character namespace (pi mints ids as
// `randomUUID().slice(0,8)`, unique only WITHIN one file), where a birthday
// collision across a real corpus would silently DROP a turn. So the key is the
// entry id plus a hash of the turn's own facts: identical copies collapse,
// and two different turns that happen to share an id do not.
func (a Adapter) event(e entry, u usage, mdl, provider, api, responseModel, responseID, stopReason string,
	state ckptState, path string) (model.UsageEvent, bool) {

	in := adapter.NonNeg(*u.Input)
	out := adapter.NonNeg(*u.Output)
	cr := adapter.NonNeg(u.CacheRead)
	cw := adapter.NonNeg(u.CacheWrite)
	// reasoning is a SUBSET of output (pi-ai types.d.ts: "output already includes
	// these tokens"), so it is reported but never added to anything.
	reasoning := int64(0)
	if u.Reasoning != nil {
		reasoning = adapter.NonNeg(*u.Reasoning)
	}
	keyReasoning := min(reasoning, out)
	if u.Reasoning == nil && a.tool == model.ToolOpenClaw && u.ReasoningTokens != nil {
		reasoning = adapter.NonNeg(*u.ReasoningTokens)
	}
	if reasoning > out {
		reasoning = out
	}
	// cacheWrite1h is a SUBSET of cacheWrite, so it is a SPLIT of a number
	// already counted, never an addition to it. It is transient pricing
	// enrichment (model.CacheWriteTTL): Anthropic charges a 1h write at 2x the
	// base input rate where a 5m write goes at the cacheWrite rate, and the
	// ledger stores only the combined count. A split that does not add up is
	// discarded by pricing.ChargeFor in favour of "all 5m", so a source
	// reporting more 1h than it wrote is clamped here rather than silently
	// throwing the whole split away.
	cw1h := adapter.NonNeg(u.CacheWrite1h)
	if cw1h > cw {
		cw1h = cw
	}
	total := adapter.NonNeg(u.TotalTokens)
	if total == 0 {
		total = in + out + cr + cw
	}
	if in == 0 && out == 0 && cr == 0 && cw == 0 && total == 0 {
		// A failed call records a zeroed usage object (a retired model answering
		// 410 does exactly this). Nothing was billed; nothing is a usage event.
		return model.UsageEvent{}, false
	}

	when := parseTime(e.Timestamp)
	ev := model.UsageEvent{
		Tool:                a.tool,
		Model:               mdl,
		Provider:            provider,
		SessionID:           state.SessionID,
		Project:             state.Project,
		EventTime:           when,
		InputTokens:         in,
		OutputTokens:        out,
		CacheCreationTokens: cw,
		CacheReadTokens:     cr,
		ReasoningTokens:     reasoning,
		TotalTokens:         total,
		CacheTTL:            model.CacheWriteTTL{Ephemeral5m: cw - cw1h, Ephemeral1h: cw1h},
		MessageID:           responseID,
		SourcePath:          path,
		Kind:                model.KindUsage,
	}
	// The established key predates OpenClaw's reasoningTokens spelling.
	// Keep its reasoning component so replaying an existing source after this
	// correction cannot insert the same billed tokens under a new identity.
	ev.DedupKey = a.dedupKey(e, when, mdl, provider, api, in, out, cr, cw, keyReasoning, total, u.Cost.Total)
	// The source's own cost, in USD, computed by pi from its model catalogue.
	// Stamped only when positive: a stamped zero would assert the request was
	// free, and an append-only ledger cannot take that back. Zero here means
	// "pi has no price for this model" far more often than it means free.
	if micro := microUSD(u.Cost.Total); micro > 0 {
		ev.SetCost(micro, a.tool+"-reported")
	}
	ev.Raw = auditJSON(auditPayload{
		Entry: e.ID, Type: e.Type, Timestamp: e.Timestamp,
		Session: state.SessionID, Provider: provider, Model: mdl, API: api,
		ResponseModel: responseModel, ResponseID: responseID, StopReason: stopReason,
		Input: in, Output: out, CacheRead: cr, CacheWrite: cw, CacheWrite1h: cw1h,
		Reasoning: reasoning, TotalTokens: total, CostUSD: u.Cost.Total,
	})
	return ev, true
}

// dedupKey hashes the turn's identifying facts under the entry id. Content is
// not in the tuple and could not be: the tuple is ids, a timestamp, names and
// counters.
func (a Adapter) dedupKey(e entry, when time.Time, mdl, provider, api string,
	in, out, cr, cw, reasoning, total int64, cost float64) string {

	tuple := strings.Join([]string{
		e.Type, e.ID, when.UTC().Format(time.RFC3339Nano), provider, mdl, api,
		fmt.Sprint(in), fmt.Sprint(out), fmt.Sprint(cr), fmt.Sprint(cw),
		fmt.Sprint(reasoning), fmt.Sprint(total), fmt.Sprint(microUSD(cost)),
	}, "|")
	sum := sha256.Sum256([]byte(tuple))
	return fmt.Sprintf("%s|%s|%x", a.tool, e.ID, sum[:12])
}

// calls turns one assistant record's `toolCall` blocks into activity rows.
//
// The join is EXACT and needs no guesswork: the blocks and the usage object are
// fields of the SAME record, so every call is attributed to the usage row built
// from that record, and the divisor is the number of blocks counted right here
// rather than a number any part of the source claims. One record is one API
// response — pi appends the assistant message once, on completion, never in
// streaming fragments — so there is no split identity to union across.
//
// PRIVACY: `arguments` has no field in this package's decode, so encoding/json
// discards a command string, a file path or a patch body as it parses. Only the
// tool NAME survives, which is the fact being recorded.
func (a Adapter) calls(e entry, ev model.UsageEvent, mdl string, state ckptState, path string) []model.ActivityEvent {
	var blocks []contentBlock
	for _, b := range e.Message.Blocks() {
		if b.Type == "toolCall" && b.Name != "" {
			blocks = append(blocks, b)
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	when := parseTime(e.Timestamp)
	out := make([]model.ActivityEvent, 0, len(blocks))
	for i, b := range blocks {
		name := b.Name
		// A namespace qualifies the name for dynamically loaded and MCP tools;
		// dropping it would merge two different tools that share a bare name.
		if b.Namespace != "" {
			name = b.Namespace + "/" + name
		}
		key := b.ID
		if key == "" {
			// No provider id: hash the usage key (which already excludes the
			// session, so a fork still collapses) with the block's position among
			// THIS RECORD's blocks and its name. Never a file offset or line
			// number — the transcript is re-read in full whenever it changes, and
			// a key minted from a read position would recount the call every pass.
			sum := sha256.Sum256([]byte(ev.DedupKey + "|" + fmt.Sprint(i) + "|" + name))
			key = fmt.Sprintf("%x", sum[:12])
		}
		out = append(out, model.ActivityEvent{
			Tool:      a.tool,
			Kind:      model.ActivityTool,
			Name:      name,
			SessionID: state.SessionID,
			Project:   state.Project,
			Model:     mdl,
			EventTime: when,
			// The turn's own usage row. Both keys exclude the session id, so a
			// forked copy of this record joins to the same ledger row it always
			// did instead of to a second copy of it.
			UsageDedupKey: ev.DedupKey,
			MessageID:     ev.MessageID,
			TurnSeq:       i,
			CallsInTurn:   len(blocks),
			SourcePath:    path,
			DedupKey:      a.tool + "|call|" + key,
		})
	}
	return out
}

// microUSD converts a USD amount to millionths, rounding to nearest.
func microUSD(usd float64) int64 {
	if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0
	}
	return int64(math.Round(usd * 1e6))
}

// parseTime reads an entry's ISO-8601 timestamp. A missing or unparseable one
// yields the zero time rather than a file mtime: an mtime moves every time the
// agent appends to the session, which would change a content-derived dedup key
// and recount the record.
func parseTime(s string) time.Time {
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
