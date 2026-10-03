// Package kimicode implements an event-level adapter for Kimi Code
// (MoonshotAI/kimi-code, MIT). Kimi Code keeps one append-only wire log per
// AGENT per session:
//
//	<home>/sessions/<workspace>/<session>/agents/<agent>/wire.jsonl
//
// The log is a mixed record stream (prompts, messages, loop events, config).
// Exactly two record types matter here: `llm.request`, written before every
// provider call, and `usage.record`, written after one returns.
//
// THE MODEL TRAP. `usage.record` carries a `model` field and it is NOT a model
// id: kimi records usage as `usage.record(request.modelAlias, ...)`, so the
// field holds the CONFIG ALIAS the profile was bound under — observed here as
// the literal `__kimi_env_model__`, the synthetic alias for an
// environment-configured profile. The real model id appears only on the
// `llm.request` record that preceded the call, which carries BOTH (`model`:
// `gemma4:31b-cloud`, `modelAlias`: `__kimi_env_model__`). Every usage record
// is therefore resolved against the nearest PRIOR request in the same file,
// and the carry-forward survives a tail read in the checkpoint. Taking the
// record's own field would file every session on this machine under one
// unpriceable pseudo-model.
//
// THE DOUBLE-COUNT TRAP. The same token counts appear TWICE in the file: once
// as `usage.record`, and once inside the `context.append_loop_event` whose
// `event.type` is `step.end` (which also carries the provider `messageId`).
// They are one API response reported by two writers, not two calls. Only
// `usage.record` is read, because it is the complete stream — a compaction or
// title call records usage with `usageScope: "session"` and no loop step at
// all — and because counting both would exactly double every loop turn.
//
// SCOPE IS NOT CUMULATIVE. `usageScope` is `"turn"` or `"session"` and both are
// per-request DELTAS: kimi's own replayer folds every record with
// `addUsage(byScope[scope], rec.usage)` and `addUsage(byModel[model],
// rec.usage)`, i.e. it SUMS them. Every record is counted once, whatever its
// scope; filtering by scope would drop the compaction and summarisation calls
// that only ever arrive as `"session"`.
//
// PRIVACY. The wire log is full of content — system prompts, user input,
// message parts, tool arguments, tool schemas. None of it is decoded: the line
// struct names only counters, model ids, times and scopes, so encoding/json
// discards every other key as it parses and the content never becomes a value
// in this process. No raw audit payload is built either, so `privacy.no_raw` is
// satisfied by construction rather than by a switch.
package kimicode

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
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// HomeEnv names the environment variable that moves the Kimi Code data root,
// and with it every session this adapter reads. Verified against the installed
// CLI (@moonshot-ai/kimi-code 0.36.1): `getDataDir()` returns
// `process.env.KIMI_CODE_HOME` when set and `~/.kimi-code` otherwise, and it is
// the ONLY variable that moves the sessions tree.
//
// Exported for the same reason claudecode.ConfigDirEnv is: what gets collected
// is decided here, not by the defaults, and a systemd unit does not inherit the
// installing shell's environment.
const HomeEnv = "KIMI_CODE_HOME"

// DataDirEnv is a second, lower-precedence root override.
//
// HONEST CAVEAT: kimi-code 0.36.1 does NOT read this variable — the bundle
// carries `KIMI_CODE_DATA_DIR_NAME`, a constant holding the string
// ".kimi-code", and no `process.env.KIMI_CODE_DATA_DIR` lookup anywhere. It is
// accepted here as an explicit operator override for installs that place the
// tree elsewhere, and it is exported so the supervision guard accounts for it
// like any other discovery variable. KIMI_DATA_DIR (the Kimi CLI variable named
// by ccusage) is deliberately NOT read: it belongs to the older, separate
// product and moves a tree this adapter does not parse.
const DataDirEnv = "KIMI_CODE_DATA_DIR"

const (
	dirSessions   = "sessions"
	dirAgents     = "agents"
	fileWire      = "wire.jsonl"
	fileWorkspace = "workspaces.json"

	recUsage   = "usage.record"
	recRequest = "llm.request"
)

// Adapter reads Kimi Code session wire logs. Read-only.
type Adapter struct{}

// New returns a Kimi Code adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolKimiCode }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Kimi Code" }

// Capabilities declares what this project can say about Kimi Code.
//
// Cost is COMPUTED: nothing here calls SetCost. There is NO activity at all —
// this adapter references model.ActivityEvent nowhere, so its surface exposes
// usage and nothing else.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolKimiCode,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(model.ToolKimiCode),
		Tier:      model.TierLive,
	}
}

// roots returns the Kimi Code data roots to scan, in precedence order. Both
// environment variables are honoured when both are set: they name directories,
// and scanning one extra directory is cheaper than silently collecting nothing
// because the operator spelled the override the other way.
func (a Adapter) roots(cfg adapter.DiscoverConfig) []string {
	var out []string
	seen := make(map[string]struct{})
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	add(os.Getenv(HomeEnv))
	add(os.Getenv(DataDirEnv))
	if len(out) > 0 {
		return out
	}
	def := ""
	if cfg.Home != "" {
		def = filepath.Join(cfg.Home, ".kimi-code")
	}
	add(cfg.Root(model.ToolKimiCode, def))
	return out
}

// Discover locates every agent wire log under each root. The tree is
// <root>/sessions/<workspace>/<session>/agents/<agent>/wire.jsonl, and SUBAGENTS
// get their own directory beside `main` — each agent owns its own recorder, so
// a scan restricted to `main` would drop every subagent's tokens.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source

	for _, root := range a.roots(cfg) {
		sessions := filepath.Join(root, dirSessions)
		if !adapter.IsDir(sessions) {
			sessions = root
		}
		if !adapter.IsDir(sessions) {
			continue
		}
		workspaces := readWorkspaces(root)

		_ = filepath.WalkDir(sessions, func(path string, d fs.DirEntry, err error) error {
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
			if d.IsDir() || d.Name() != fileWire {
				return nil
			}
			if !adapter.WalkEntryIsFile(d, path) {
				return nil
			}
			if _, dup := seen[path]; dup {
				return nil
			}
			seen[path] = struct{}{}

			ids := parseIDs(sessions, path)
			label := "kimi-code session " + ids.session
			if ids.agent != "" && ids.agent != "main" {
				label += " (" + ids.agent + ")"
			}
			meta := map[string]string{
				"root":      sessions,
				"session":   ids.session,
				"agent":     ids.agent,
				"workspace": ids.workspace,
			}
			if p := workspaces[ids.workspace]; p != "" {
				meta["project"] = p
			}
			srcs = append(srcs, adapter.Source{
				Tool:  model.ToolKimiCode,
				Class: model.EventLevel,
				Path:  path,
				Label: label,
				Meta:  meta,
			})
			return nil
		})
	}
	return srcs, nil
}

// sourceIDs are the identities carried by a wire log's path.
type sourceIDs struct {
	workspace string // workspace directory (wd_kimi_...)
	session   string // session directory (session_<uuid>) — the session id
	agent     string // agent directory ("main" or a subagent id)
}

// parseIDs recovers the workspace/session/agent identities from a wire log
// path. It anchors on the literal "agents" directory rather than on a fixed
// depth, so a tree that grows another level above the workspace still resolves.
func parseIDs(root, path string) sourceIDs {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	var ids sourceIDs
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != dirAgents {
			continue
		}
		if i+1 < len(parts)-1 {
			ids.agent = parts[i+1]
		}
		if i-1 >= 0 {
			ids.session = parts[i-1]
		}
		if i-2 >= 0 {
			ids.workspace = parts[i-2]
		}
		return ids
	}
	// No "agents" anchor: fall back to the containing directory name, which is
	// the most specific identity the path still offers.
	if len(parts) >= 2 {
		ids.session = parts[len(parts)-2]
	}
	return ids
}

// readWorkspaces maps a workspace directory id to the project root it was
// opened on, from <root>/workspaces.json. A missing or malformed file is not an
// error: the project dimension is then simply unknown for that root.
func readWorkspaces(root string) map[string]string {
	b, err := os.ReadFile(filepath.Join(root, fileWorkspace))
	if err != nil {
		return nil
	}
	var doc struct {
		Workspaces map[string]struct {
			Root string `json:"root"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil
	}
	out := make(map[string]string, len(doc.Workspaces))
	for id, w := range doc.Workspaces {
		if r := strings.TrimSpace(w.Root); r != "" {
			out[id] = r
		}
	}
	return out
}

// tokenUsage is kimi's TokenUsage, verbatim and complete. Verified against the
// installed CLI: `emptyUsage()` and `addUsage()` name these four fields and no
// others, and `inputTotal(u) = inputOther + inputCacheRead + inputCacheCreation`
// — so inputOther EXCLUDES both cache buckets and the components are additive,
// Anthropic-style. There is no reasoning/thinking counter anywhere in the
// shape: kimi streams thinking as `think` content parts, and whatever they cost
// is already inside `output`.
type tokenUsage struct {
	InputOther         int64 `json:"inputOther"`
	Output             int64 `json:"output"`
	InputCacheRead     int64 `json:"inputCacheRead"`
	InputCacheCreation int64 `json:"inputCacheCreation"`
}

func (t tokenUsage) zero() bool {
	return t.InputOther == 0 && t.Output == 0 && t.InputCacheRead == 0 && t.InputCacheCreation == 0
}

// wireLine is the ALLOW-LIST decode of one wire record. Every field here is a
// counter, an identifier, a time or an enum; there is no field that a prompt, a
// message, a tool argument or a tool schema could be decoded into, so
// encoding/json drops all of them while parsing. Adding a field to this struct
// is the only way content could ever reach this package
// (TestWireDecodeIsAnAllowList).
type wireLine struct {
	Type string `json:"type"`
	Time int64  `json:"time"` // unix milliseconds, stamped by the writer

	// Model means DIFFERENT THINGS on the two records this adapter reads: on
	// `llm.request` it is the real provider model id, on `usage.record` it is
	// the config alias. Never read it without checking Type first.
	Model string `json:"model"`

	ModelAlias string `json:"modelAlias"` // llm.request: the alias Model was bound under
	// TurnStep is decoded raw because kimi writes it as a string ("0.1") and a
	// typed decode would drop the whole record — and with it the model
	// carry-forward — if a later version wrote a number instead.
	TurnStep json.RawMessage `json:"turnStep"`

	UsageScope string      `json:"usageScope"` // usage.record: "turn" | "session"
	Usage      *tokenUsage `json:"usage"`      // usage.record: the counters
}

var (
	markerUsage   = []byte(recUsage)
	markerRequest = []byte(recRequest)
)

func lineIsInteresting(raw []byte) bool {
	return bytes.Contains(raw, markerUsage) || bytes.Contains(raw, markerRequest)
}

// ckptState is the per-file parse state persisted in the checkpoint. It exists
// entirely for the model join: a tail read that started after the last
// `llm.request` would otherwise resolve every following usage record against
// nothing, changing both the reported model AND the dedup key of records the
// full read had already keyed differently.
type ckptState struct {
	Model           string `json:"model,omitempty"`            // last llm.request.model (the REAL id)
	Alias           string `json:"alias,omitempty"`            // last llm.request.modelAlias
	RequestKey      string `json:"requestKey,omitempty"`       // that request's identity
	ProtocolVersion string `json:"protocol_version,omitempty"` // "legacy" for headerless records
}

// Collect reads one wire log in full and returns its usage events.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental reads only what is new since cp: an unchanged size+mtime
// skips the file entirely; growth tail-reads from the stored offset with the
// persisted model carry-forward; any shrink or same-size rewrite re-reads from
// zero. Kimi does rewrite a wire log in place — a forked session is truncated
// at a turn, and a wire-protocol migration rewrites the whole file — so the
// shrink path is real, not theoretical; re-derived dedup keys collapse in the
// store. A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	f, err := os.Open(src.Path) // read-only
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("kimicode: open %s: %w", src.Path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("kimicode: stat %s: %w", src.Path, err)
	}
	return a.collectReader(ctx, src, cp, f, fi)
}

func (a Adapter) collectReader(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint, f io.ReadSeeker, fi os.FileInfo) (adapter.Observation, error) {
	size := fi.Size()
	mtimeNS := fi.ModTime().UnixNano()

	var (
		start int64
		state ckptState
	)
	if cp != nil {
		if cp.Size == size && cp.MTimeNS == mtimeNS {
			return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
		}
		if size > cp.Size && cp.Offset >= 0 && cp.Offset <= size {
			start = cp.Offset
			if cp.State != "" {
				if err := json.Unmarshal([]byte(cp.State), &state); err != nil {
					start, state = 0, ckptState{}
				}
			}
			// Older checkpoints did not retain the wire format. Re-read once
			// so a versioned usage tail cannot be mistaken for sparse legacy data.
			if state.ProtocolVersion == "" {
				start, state = 0, ckptState{}
			}
		}
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return adapter.Observation{}, fmt.Errorf("kimicode: seek %s: %w", src.Path, err)
		}
	}

	var (
		events         []model.UsageEvent
		consumed       = start
		mtime          = fi.ModTime().UTC()
		r              = bufio.NewReaderSize(f, 64*1024)
		recognized     = start > 0
		nonempty       bool
		formatErr      error
		parseErr       error
		requestInvalid bool
	)

	for {
		if ctx.Err() != nil {
			return adapter.Observation{Events: events}, ctx.Err()
		}
		raw, rerr := r.ReadBytes('\n')
		terminated := rerr == nil
		if rerr != nil && rerr != io.EOF {
			return adapter.Observation{Events: events}, fmt.Errorf("kimicode: read %s: %w", src.Path, rerr)
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if !terminated && !json.Valid(raw) {
				break // an incomplete final record must be retried after growth
			}
			nonempty = true
			typ, typeErr := recordType(raw)
			if typeErr != nil {
				if json.Valid(raw) {
					formatErr = fmt.Errorf("kimicode: wire %w: incompatible type", adapter.ErrSourceFormat)
					if bytes.Contains(raw, []byte(`"protocol_version"`)) || bytes.Contains(raw, []byte(`"modelAlias"`)) {
						requestInvalid = true
					}
				} else {
					parseErr = errors.Join(parseErr, fmt.Errorf("kimicode: malformed wire record"))
				}
			} else if typ == "metadata" || typ == recUsage || typ == recRequest || lineIsInteresting(raw) ||
				bytes.Contains(raw, []byte(`"usage"`)) || bytes.Contains(raw, []byte(`"protocol_version"`)) ||
				(typ != "profile.bind" && bytes.Contains(raw, []byte(`"modelAlias"`))) {
				ln, known, err := decodeWireRecord(raw, &state)
				if err != nil {
					if errors.Is(err, adapter.ErrSourceFormat) {
						if formatErr == nil {
							formatErr = err
						}
						if typ == "metadata" || typ == recRequest || bytes.Contains(raw, []byte(`"protocol_version"`)) || bytes.Contains(raw, []byte(`"modelAlias"`)) {
							requestInvalid = true
						}
					} else {
						parseErr = errors.Join(parseErr, err)
					}
				} else if known {
					recognized = true
					switch ln.Type {
					case recRequest:
						next := requestState(ln)
						next.ProtocolVersion = state.ProtocolVersion
						state = next
						requestInvalid = false
					case recUsage:
						// A rejected context cannot key dependent usage safely.
						// Repair must replay it once with the original request.
						if requestInvalid {
							break
						}
						if ev, ok := buildEvent(ln, state, src, mtime); ok {
							events = append(events, ev)
						}
					}
				}
			}
		}
		if terminated {
			consumed += int64(len(raw))
			continue
		}
		// Complete-but-unterminated final line: its event was emitted, but the
		// offset stays before it — if it was mid-append, the next cycle re-reads
		// the full line and the store collapses the dedup keys.
		break
	}

	if err := ctx.Err(); err != nil {
		return adapter.Observation{Events: events}, err
	}
	if formatErr != nil {
		return adapter.Observation{Events: events}, errors.Join(parseErr, formatErr)
	}
	if nonempty && !recognized {
		return adapter.Observation{}, fmt.Errorf("kimicode: wire %w: no recognized record anchors", adapter.ErrSourceFormat)
	}
	newState, err := json.Marshal(state)
	if err != nil {
		return adapter.Observation{Events: events}, nil // no checkpoint; next cycle re-reads
	}
	return adapter.Observation{
		Events: events,
		Checkpoint: &model.SourceCheckpoint{
			Tool: model.ToolKimiCode, SourcePath: src.Path,
			Size: size, MTimeNS: mtimeNS, Offset: consumed, State: string(newState),
		},
	}, parseErr
}

// Stop at the top-level discriminator before ordinary message content. Fields
// before type still use the standard decoder, without an ordering assumption.
func recordType(raw []byte) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil {
		return "", err
	}
	if token != json.Delim('{') {
		return "", errors.New("record is not an object")
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return "", err
		}
		if key == "type" {
			var typ string
			err := d.Decode(&typ)
			return typ, err
		}
		var ignored json.RawMessage
		if err := d.Decode(&ignored); err != nil {
			return "", err
		}
	}
	_, err = d.Token()
	return "", err
}

// decodeWireRecord validates only format and accounting fields. Content stays
// outside the decode, including on unrelated record kinds.
func decodeWireRecord(raw []byte, state *ckptState) (wireLine, bool, error) {
	if !json.Valid(raw) {
		return wireLine{}, false, fmt.Errorf("kimicode: malformed wire record")
	}
	bad := func(anchor string) (wireLine, bool, error) {
		return wireLine{}, false, fmt.Errorf("kimicode: wire %w: missing or incompatible %s", adapter.ErrSourceFormat, anchor)
	}
	var discriminator struct {
		Type     *string         `json:"type"`
		Protocol json.RawMessage `json:"protocol_version"`
		Usage    json.RawMessage `json:"usage"`
		Alias    json.RawMessage `json:"modelAlias"`
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return bad("record/type")
	}
	if discriminator.Type == nil {
		if len(discriminator.Protocol)+len(discriminator.Usage)+len(discriminator.Alias) > 0 {
			return bad("type")
		}
		return wireLine{}, false, nil
	}
	if *discriminator.Type == "metadata" {
		var metadata struct {
			Protocol *string `json:"protocol_version"`
			Created  *int64  `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil || metadata.Protocol == nil || strings.TrimSpace(*metadata.Protocol) == "" || *metadata.Protocol == "legacy" || metadata.Created == nil {
			return bad("metadata protocol_version/created_at")
		}
		state.ProtocolVersion = *metadata.Protocol
		return wireLine{Type: "metadata"}, true, nil
	}
	if *discriminator.Type != recRequest && *discriminator.Type != recUsage {
		if len(discriminator.Protocol) > 0 {
			return bad("metadata type")
		}
		// profile.bind also carries modelAlias, without being a request.
		if len(discriminator.Usage) > 0 || (len(discriminator.Alias) > 0 && *discriminator.Type != "profile.bind") {
			return bad("usage/request type")
		}
		return wireLine{}, false, nil
	}
	var ln wireLine
	if err := json.Unmarshal(raw, &ln); err != nil {
		return bad("usage/request fields")
	}
	if ln.Type == recRequest {
		if strings.TrimSpace(ln.Model) == "" {
			return bad("llm.request.model")
		}
	} else {
		if ln.Usage == nil {
			return bad("usage.record.usage")
		}
		var usage struct {
			Input  *int64 `json:"inputOther"`
			Output *int64 `json:"output"`
			Read   *int64 `json:"inputCacheRead"`
			Create *int64 `json:"inputCacheCreation"`
		}
		if err := json.Unmarshal(discriminator.Usage, &usage); err != nil {
			return bad("usage counters")
		}
		if state.ProtocolVersion != "" && state.ProtocolVersion != "legacy" {
			if usage.Input == nil || usage.Output == nil || usage.Read == nil || usage.Create == nil {
				return bad("versioned usage counters")
			}
		} else if usage.Input == nil && usage.Output == nil && usage.Read == nil && usage.Create == nil {
			return bad("legacy usage counter")
		}
	}
	if state.ProtocolVersion == "" {
		state.ProtocolVersion = "legacy"
	}
	return ln, true, nil
}

// requestState turns one `llm.request` record into the carry-forward state that
// the usage records after it resolve against.
func requestState(ln wireLine) ckptState {
	return ckptState{
		Model: strings.TrimSpace(ln.Model),
		Alias: strings.TrimSpace(ln.ModelAlias),
		// The request's own identity: its write time plus the turn.step it
		// belongs to. Both are record CONTENT, so the key it feeds stays stable
		// across a re-read, a tail read and a full rebuild alike.
		RequestKey: strconv.FormatInt(ln.Time, 10) + "|" + strings.TrimSpace(string(ln.TurnStep)),
	}
}

// buildEvent maps one `usage.record` onto a UsageEvent, resolving the model
// against the nearest prior request.
func buildEvent(ln wireLine, state ckptState, src adapter.Source, mtime time.Time) (model.UsageEvent, bool) {
	if ln.Usage == nil || ln.Usage.zero() {
		// kimi records `usage ?? emptyUsage()`, so a provider that returned no
		// usage still writes an all-zero record. It is not a spend.
		return model.UsageEvent{}, false
	}

	input := adapter.NonNeg(ln.Usage.InputOther)
	output := adapter.NonNeg(ln.Usage.Output)
	cacheRead := adapter.NonNeg(ln.Usage.InputCacheRead)
	cacheCreation := adapter.NonNeg(ln.Usage.InputCacheCreation)

	// THE TRAP, resolved: the model comes from the request, never from
	// ln.Model. When no request has been seen the model is UNKNOWN, and it is
	// left empty and reported as such — stamping the alias would put a
	// pseudo-model ("__kimi_env_model__") in the model dimension and claim the
	// source named a model it never named.
	mdl := state.Model

	when := mtime
	if ln.Time > 0 {
		when = time.UnixMilli(ln.Time).UTC()
	}

	ev := model.UsageEvent{
		Tool:      model.ToolKimiCode,
		Model:     mdl,
		SessionID: meta(src, "session"),
		Project:   meta(src, "project"),
		EventTime: when,
		// Provider is left UNKNOWN on purpose. `llm.request.provider` names the
		// wire PROTOCOL the client speaks ("openai" is the OpenAI-legacy chat
		// adapter, used for any OpenAI-compatible endpoint including
		// self-hosted ones), not who bills the tokens. Stamping it would tell
		// the pricing engine to look the model up in the OpenAI namespace and
		// would label a Moonshot — or local — request as OpenAI spend.
		Provider:            "",
		InputTokens:         input,
		OutputTokens:        output,
		CacheCreationTokens: cacheCreation,
		CacheReadTokens:     cacheRead,
		ReasoningTokens:     0, // kimi's TokenUsage has no reasoning counter
		SourcePath:          src.Path,
		Kind:                model.KindUsage,
	}
	// kimi reports no provider total. The four components are additive and
	// disjoint (inputTotal = inputOther + inputCacheRead + inputCacheCreation),
	// so their sum IS the total and ComputedTotal is exactly right.
	ev.TotalTokens = ev.ComputedTotal()
	ev.DedupKey = dedupKey(ln, state, meta(src, "agent"))
	return ev, true
}

// dedupKey builds the stable content key for one usage record.
//
// `usage.record` carries NO message id, no request id and no id of its own, so
// the key is a hash of what the record IS: the agent that wrote it, the
// millisecond the writer stamped on it, its scope, the alias it names, the
// model and request identity it resolved against, and its four counters. Every
// input is file CONTENT — never an offset, a line number or an ordinal — so the
// same record keys identically on a full read, a tail read and a rebuild, and a
// re-read cannot recount it.
//
// SESSION IDENTITY IS DELIBERATELY EXCLUDED, as it is for codex. Kimi's
// `fork()` copies the whole session directory (`cp(sourceDir, targetDir,
// {recursive: true})`, verified in the installed CLI) and then truncates the
// copy at a turn; every record before the fork point survives with its original
// timestamp and counters under a NEW session id, so a key that named the
// session would count one spend once per fork.
// The agent directory stays in the key because a fork preserves it ("main"
// forks to "main") while a subagent running beside the main agent in the same
// session does not.
//
// The residual collision is two records with identical counters written in the
// same millisecond by the same agent under the same request — one API response
// reported twice, or an undercount of a genuine duplicate. It resolves in the
// direction this ledger promises: understate, never overstate.
func dedupKey(ln wireLine, state ckptState, agent string) string {
	tuple := strings.Join([]string{
		agent,
		strconv.FormatInt(ln.Time, 10),
		ln.UsageScope,
		ln.Model, // the alias, verbatim: what the record claimed
		state.Model,
		state.Alias,
		state.RequestKey,
		strconv.FormatInt(ln.Usage.InputOther, 10),
		strconv.FormatInt(ln.Usage.Output, 10),
		strconv.FormatInt(ln.Usage.InputCacheRead, 10),
		strconv.FormatInt(ln.Usage.InputCacheCreation, 10),
	}, "|")
	sum := sha256.Sum256([]byte(tuple))
	return model.ToolKimiCode + "|" + hex.EncodeToString(sum[:])
}

// meta reads one Source.Meta key, tolerating a nil map.
func meta(src adapter.Source, key string) string {
	if src.Meta == nil {
		return ""
	}
	return src.Meta[key]
}
