// Package qwencode implements an event-level adapter for Qwen Code.
//
// SURFACE. Qwen Code keeps its OWN append-only usage ledger, one JSON record
// per API response, at
//
//	${QWEN_RUNTIME_DIR:-${QWEN_HOME:-~/.qwen}}/usage/token-usage-<localMonth>.jsonl
//
// That file is the authoritative surface and this adapter reads nothing else.
// The session transcripts under projects/ are what the third-party parsers read
// instead, and they carry no request identity, so they force a content-tuple
// dedup key while the ledger next door hands out a per-record randomUUID. The
// ledger record carries exactly: schemaVersion, id, timestamp, localDate,
// localMonth, sessionId, model, authType, source, inputTokens, outputTokens,
// cachedTokens, thoughtsTokens, totalTokens, apiDurationMs. Every one of those
// is a counter or an identity; the surface has no content field at all.
//
// ABSENCE IS NOT PROOF OF NON-USE. The write is gated on
// `privacy.usageStatisticsEnabled` (default true, overridable per run by
// QWEN_USAGE_STATISTICS_ENABLED), so a missing usage/ directory means EITHER
// the harness was never run OR the user opted out. Discovery therefore returns
// no sources and NO error when the directory is absent: an error would report a
// deliberate privacy choice as a fault, and inventing a fallback surface would
// collect what the user turned off.
//
// PRESENCE IS NOT PROOF OF EVERYTHING EITHER. The same call site skips the
// write for an INTERNAL prompt id, so the harness's own utility calls (next
// speaker checks, summarisation and the like) spend tokens that never reach
// this ledger. The rows that are here are exact; the ledger is a floor, not a
// bill, and it will read low against a provider console.
//
// THE BUCKET COMES FROM `timestamp`, NEVER FROM THE FILE NAME. Two fields of
// this surface are writer-local by the harness's own documentation: the record's
// `localDate`/`localMonth`, and the FILE NAME, which is built from
// `record.localMonth`. A ledger copied from a machine in another zone keeps its
// original buckets, so a reader that trusts either would place events in the
// writing machine's calendar. `timestamp` is an ISO 8601 instant and is the only
// field read for time here — which is also the project rule: grouping keys are
// derived on read, from UTC seconds.
//
// TOKEN SEMANTICS ARE GEMINI'S SHAPE, FILLED BY WHICHEVER WIRE ANSWERED. Qwen
// Code is a Gemini CLI fork and the record is built straight off
// `GenerateContentResponseUsageMetadata` (ApiResponseEvent copies
// promptTokenCount / candidatesTokenCount / cachedContentTokenCount /
// thoughtsTokenCount / totalTokenCount into the ledger's five counters) — but
// only authType gemini and vertex-ai fill that struct from a Gemini response.
// openai and qwen-oauth fill it in `convertOpenAIResponseToGemini`, and
// anthropic in a converter of its own. The SHAPE is shared; the SEMANTICS are
// not, and the ledger records no separate hint about which applied.
//
// `cachedTokens` is a SUBSET of the prompt count on every one of those wires
// (`cachedContentTokenCount` inside `promptTokenCount`;
// `prompt_tokens_details.cached_tokens` inside `prompt_tokens`;
// `cache_read_input_tokens` folded into the anthropic converter's own prompt
// total), so it is mapped to CacheRead and SUBTRACTED from Input rather than
// added beside it. `calculateInputTokens` also falls back to the cached count
// when the prompt count is absent, so `inputTokens == cachedTokens` is a real
// record shape and Input is then legitimately zero.
//
// `thoughtsTokens` is NOT additive here, which is the one place this surface
// contradicts its Gemini ancestry. On the OpenAI-compatible wires — authType
// openai, and qwen-oauth whose QwenContentGenerator EXTENDS
// OpenAIContentGenerator — the converter writes `candidatesTokenCount =
// usage.completion_tokens` beside `thoughtsTokenCount =
// usage.completion_tokens_details.reasoning_tokens`, and reasoning_tokens is a
// COMPONENT of completion_tokens (the converter's own estimation fallback
// clamps the count with `Math.min(estimated, completionTokens)`, which only
// holds if it sits inside). The anthropic converter writes no thoughts count at
// all. Only the native Gemini wire reports thoughts BESIDE candidates.
//
// A reasoning mode is a property of the TOOL in this project
// (model.ReasoningModeFor), not of a row, so one rule has to cover a ledger
// that mixes wires: SUBSET. It is exact for openai, qwen-oauth and anthropic,
// and on the Gemini wire it can only UNDER-bill, which is the direction this
// project takes when it cannot know. See ReasoningMode below.
//
// Reasoning is therefore not part of the total floor either. The provider's own
// `totalTokens` stays authoritative and is raised only when it falls below
// `input + cached + output` (issue #49). Adding reasoning to that floor would
// raise the stored total by the reasoning count of every OpenAI-wire record —
// an OVERSTATEMENT appended to an immutable ledger, on the wire most Qwen Code
// installs use. Cache read stays outside the floor for the reason it always
// does: it is already inside the prompt count.
//
// The ledger reports no cache-creation count and no service tier, and it names
// no project or working directory, so those fields stay empty rather than
// carrying a guess.
//
// PROVIDER IS LEFT UNKNOWN ON PURPOSE. `authType` is one of openai, qwen-oauth,
// gemini, vertex-ai, anthropic — the credential/wire kind, not the biller. Every
// one of them can be pointed at a third-party or local endpoint through a base
// URL, and since the free Qwen OAuth tier was discontinued the common
// configuration is exactly that: this machine's own records read
// authType=openai against a `gemma4:31b` served from localhost. Stamping
// ProviderOpenAI there would attribute local inference to OpenAI's bill.
// model.UsageEvent treats an empty provider as unknown and renders it as such,
// which is the true answer; the value is kept verbatim in the audit payload.
// Pricing is unaffected either way: the provider only adds namespaced lookup
// keys that are tried BEFORE the bare model id, never instead of it.
//
// TURN CONTEXT. `source` is `subagent_name || "main"`, i.e. the name of the
// subagent that issued the request, so a record whose source is not the "main"
// sentinel is a subagent turn and produces one TurnContext on the agent
// dimension. The sentinel produces none: "main" means no subagent ran, and
// storing it would both invent an agent that does not exist and be
// indistinguishable from a real subagent named "main".
//
// NO ACTIVITY, AND THE SECOND SURFACE IS REFUSED. This ledger records no tool
// call, skill invocation or hook, so the Activity stream stays empty. Qwen does
// write tool and skill counters — `tools.byName` / `skills.byName` inside
// `${QWEN_HOME:-~/.qwen}/usage_record.jsonl` — and they are deliberately not
// read: they are per-session COUNTS with no per-invocation identity, no
// timestamp and nothing to deduplicate on, so they cannot become append-only
// invocation rows; the same file also re-reports the session's whole token
// total, which the ledger already carries event by event; and the harness writes
// a session's summary from several independent paths (session end, transcript
// deletion salvage, history rebuild) whose own reader resolves the collision
// last-wins. A last-wins summary is not a source for an append-only ledger.
//
// THE THREE NAMED BUG CLASSES, checked:
//   - Split-identity records: does not occur. One API response is one record
//     with its own randomUUID; nothing is streamed across records and no
//     identity spans two lines. There is consequently no divisor to get wrong.
//   - Cumulative-vs-event counting: does not occur on this surface. Each record
//     holds THAT response's counts, never a running total, so a tail read needs
//     no baseline and re-reading a file cannot re-add a total.
//   - Assigned-not-accumulated columns: does not occur. `totalTokens` is the
//     provider's figure for the one response the record describes.
//
// CRITICAL: strictly observational. The ledger is opened O_RDONLY and never
// written, locked or rotated.
package qwencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// ReasoningMode is the reasoning-billing rule this surface reports, so the fact
// travels with the adapter that measured it: SUBSET. thoughtsTokens is INSIDE
// outputTokens on the OpenAI-compatible wires (authType openai and qwen-oauth —
// the wire this machine's own records took), and is never written at all on the
// anthropic one; only the native Gemini wire reports it beside output. One tool
// id cannot carry two rules, so the mode is the one that is exact on three
// wires and can only under-bill on the fourth. Billing this ADDITIVE would
// charge the reasoning tokens of every OpenAI-wire turn twice, since the output
// count it sits inside is already being charged.
//
// It is the same value model.ReasoningModeFor returns for a tool it does not
// know, so nothing is mis-billed while the adapter is unregistered. Registering
// it in model.reasoningModes is still part of wiring the adapter up: the
// fallback is a default, and this is a measurement.
const ReasoningMode = model.ReasoningSubset

// RuntimeDirEnv and HomeEnv name the environment variables that move the Qwen
// ledger, and with it everything this adapter reads. They are exported because
// a caller that copies this process's discovery into another one — the systemd
// units internal/service writes — has to know that the environment, not the
// defaults, decided what gets collected.
//
// The precedence is the harness's own: QWEN_RUNTIME_DIR wins, then QWEN_HOME,
// then ~/.qwen. QWEN_DATA_DIR is deliberately absent — no such variable exists
// in the harness; a third-party parser invented it.
//
// The harness has one rung BETWEEN those two that no environment variable
// exposes: `advanced.runtimeOutputDir` in settings.json (Storage's own order is
// QWEN_RUNTIME_DIR > runtimeOutputDir > QWEN_HOME > ~/.qwen). It is not read
// here, and it cannot be read reliably: settings merge across scopes and a
// relative value resolves against the WORKSPACE the qwen process was started
// in, which a polling daemon has no way to enumerate. An install that sets it
// therefore collects nothing rather than something wrong — the same silence as
// an opted-out install, and the reason absence is never reported as a fault.
const (
	RuntimeDirEnv = "QWEN_RUNTIME_DIR"
	HomeEnv       = "QWEN_HOME"
)

const (
	// usageDirName, filePrefix and fileExt mirror the harness's own constants.
	usageDirName = "usage"
	filePrefix   = "token-usage-"
	fileExt      = ".jsonl"
	// defaultDirName is the home-relative default runtime base directory.
	defaultDirName = ".qwen"
	// mainSource is the sentinel `source` value meaning "no subagent issued
	// this request". Anything else is a subagent name.
	mainSource = "main"
)

// Adapter reads the Qwen Code usage ledger. Read-only.
type Adapter struct{}

// New returns a Qwen Code adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolQwenCode }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Qwen Code" }

// Capabilities declares what this project can say about Qwen Code.
//
// Cost is COMPUTED: nothing here calls SetCost. There is NO activity at all —
// this adapter references model.ActivityEvent nowhere; the harness's own usage
// ledger records turns, not calls.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolQwenCode,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(model.ToolQwenCode),
		Tier:      model.TierLive,
	}
}

// root resolves the runtime base directory whose usage/ subdirectory holds the
// ledger, in the harness's own precedence order:
//
//  1. An explicit override (DiscoverConfig.Overrides[qwen-code]).
//  2. env QWEN_RUNTIME_DIR.
//  3. env QWEN_HOME.
//  4. <home>/.qwen.
//
// Each value is taken as ONE path, not a comma list: the harness reads these
// variables with a bare process.env lookup, so a comma would be part of a
// directory name there and splitting on it here would look somewhere the
// harness never writes.
//
// A leading ~ is expanded everywhere, because the harness expands it too. An
// ENVIRONMENT value that is still RELATIVE afterwards is dropped rather than
// resolved: the harness resolves it against the qwen process's own working
// directory, which a daemon polling later cannot know, so resolving it against
// this process's directory would confidently read the wrong tree. An explicit
// override is aiusage's own configuration and is taken as given.
func (a Adapter) root(cfg adapter.DiscoverConfig) string {
	if cfg.Overrides != nil {
		if v := expandTilde(strings.TrimSpace(cfg.Overrides[model.ToolQwenCode]), cfg.Home); v != "" {
			return filepath.Clean(v)
		}
	}
	// Spelled out one call per variable, not looped over a slice: the guard test
	// in internal/cmd parses these sources to check every discovery variable is
	// registered, and it can only resolve a constant named at the call site.
	for _, v := range []string{os.Getenv(RuntimeDirEnv), os.Getenv(HomeEnv)} {
		p := expandTilde(strings.TrimSpace(v), cfg.Home)
		if p != "" && filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
	}
	if cfg.Home == "" {
		return ""
	}
	return filepath.Join(cfg.Home, defaultDirName)
}

// expandTilde expands a leading ~ against home. An empty result means "no usable
// value here, try the next candidate".
func expandTilde(p, home string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home == "" {
			return ""
		}
		rest := strings.TrimLeft(p[1:], `/\`)
		return filepath.Join(home, filepath.FromSlash(rest))
	}
	return p
}

// Discover lists the monthly ledger files under <root>/usage.
//
// A missing usage/ directory yields no sources and no error: the harness only
// writes it while privacy.usageStatisticsEnabled is on, so its absence means
// "never ran OR opted out" and neither is a fault to report. Files are listed
// non-recursively and matched on the harness's own name shape
// (token-usage-*.jsonl) — the month in that name is writer-local metadata and
// is never parsed, here or anywhere else in this package.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	root := a.root(cfg)
	if root == "" {
		return nil, nil
	}
	dir := filepath.Join(root, usageDirName)
	if !adapter.IsDir(dir) {
		return nil, nil
	}
	// os.ReadDir sorts by file name, so sources arrive in a stable order and a
	// collection pass reads the months in the same sequence every time.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // unreadable ledger directory is not a collection failure
	}
	var srcs []adapter.Source
	for _, d := range entries {
		if ctx.Err() != nil {
			return srcs, ctx.Err()
		}
		name := d.Name()
		if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileExt) {
			continue
		}
		path := filepath.Join(dir, name)
		if !adapter.WalkEntryIsFile(d, path) {
			continue
		}
		srcs = append(srcs, adapter.Source{
			Tool:  model.ToolQwenCode,
			Class: model.EventLevel,
			Path:  path,
			Label: "qwen usage ledger " + name,
			Meta:  map[string]string{"root": root},
		})
	}
	return srcs, nil
}

// Collect reads one ledger file in full and returns its usage events.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental reads only what is new since cp: an unchanged size+mtime
// skips the file entirely; growth tail-reads from the stored offset; any shrink
// or same-size rewrite re-reads from zero. A nil cp is a full read.
//
// The tail read needs NO carried state, and that is a property of the surface
// rather than an omission: every record holds its own response's counts and its
// own UUID, so a record read from byte 40,000 means exactly what it would have
// meant read from byte 0. Re-reading is therefore always safe — the re-derived
// dedup keys collapse in the store — which is why every uncertain case above
// restarts from zero instead of guessing.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	f, err := os.Open(src.Path) // read-only
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("qwencode: open %s: %w", src.Path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("qwencode: stat %s: %w", src.Path, err)
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
			return adapter.Observation{}, fmt.Errorf("qwencode: seek %s: %w", src.Path, err)
		}
	}

	var (
		obs          adapter.Observation
		mtime        = fi.ModTime().UTC()
		consumed     = start
		skipped      int
		complete     bool
		recognized   = start > 0
		nonempty     bool
		formatDetail string
		r            = bufio.NewReaderSize(f, 64*1024)
	)
	for {
		if ctx.Err() != nil {
			return obs, ctx.Err()
		}
		raw, rerr := r.ReadBytes('\n')
		terminated := rerr == nil
		if rerr != nil && rerr != io.EOF {
			// Unreadable remainder. Keep what was parsed but WITHHOLD the
			// checkpoint: stamping the file's current size and mtime beside a
			// partial offset would make the next poll's unchanged-file check
			// skip the file outright, and the unread tail would wait for the
			// next append that may never come.
			return obs, fmt.Errorf("qwencode: read %s: %w", src.Path, rerr)
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if !terminated && !json.Valid(raw) {
				complete = true
				break // keep a partial append before its incomplete final record
			}
			nonempty = true
			if !json.Valid(raw) {
				skipped++ // complete poison is consumed; format anchors are checked separately
				consumed += int64(len(raw))
				continue
			}
			known, detail := ledgerRecordShape(raw)
			var rec record
			if detail != "" {
				skipped++
				if formatDetail == "" {
					formatDetail = detail
				}
			} else if known {
				if err := json.Unmarshal(raw, &rec); err != nil {
					skipped++
					if formatDetail == "" {
						formatDetail = "record fields"
					}
				} else {
					recognized = true
					if ev, ok := rec.event(src.Path, mtime); ok {
						obs.Events = append(obs.Events, ev)
						if tc, ok := rec.turnContext(ev); ok {
							obs.TurnContexts = append(obs.TurnContexts, tc)
						}
					}
				}
			}
		}
		if terminated {
			consumed += int64(len(raw))
			continue
		}
		// Clean EOF, or a complete-but-unterminated final line: its event was
		// emitted, but the offset stays before it — if it was mid-append, the
		// next cycle re-reads the whole line and the store collapses the dedup
		// key.
		complete = true
		break
	}

	if err := ctx.Err(); err != nil {
		return obs, err
	}
	if formatDetail != "" {
		return obs, fmt.Errorf("qwencode: ledger %w: skipped %d unusable record(s) in %s; missing or incompatible %s", adapter.ErrSourceFormat, skipped, src.Path, formatDetail)
	}
	if nonempty && !recognized {
		return obs, fmt.Errorf("qwencode: ledger %w: no recognized record anchors", adapter.ErrSourceFormat)
	}
	if complete {
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: model.ToolQwenCode, SourcePath: src.Path,
			Size: size, MTimeNS: mtimeNS, Offset: consumed,
		}
	}
	if skipped > 0 {
		return obs, fmt.Errorf("qwencode: skipped %d unusable record(s) in %s: malformed JSON", skipped, src.Path)
	}
	return obs, nil
}

// ledgerRecordShape keeps absent legacy/zero buckets distinct from a renamed
// current accounting shape. Unknown additions never enter the audit payload.
func ledgerRecordShape(raw []byte) (bool, string) {
	var shape struct {
		Version  json.RawMessage `json:"schemaVersion"`
		ID       *string         `json:"id"`
		Input    *int64          `json:"inputTokens"`
		Output   *int64          `json:"outputTokens"`
		Total    *int64          `json:"totalTokens"`
		Cached   json.RawMessage `json:"cachedTokens"`
		Thoughts json.RawMessage `json:"thoughtsTokens"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return false, "record/id/counter fields"
	}
	if shape.ID == nil && len(shape.Version) == 0 && shape.Input == nil && shape.Output == nil && shape.Total == nil && len(shape.Cached)+len(shape.Thoughts) == 0 {
		return false, "" // an unrelated record kind; a whole unknown file fails below
	}
	if shape.ID == nil || strings.TrimSpace(*shape.ID) == "" {
		return false, "id"
	}
	for _, field := range []json.RawMessage{shape.Cached, shape.Thoughts} {
		if len(field) == 0 {
			continue
		}
		var count int64
		if bytes.Equal(bytes.TrimSpace(field), []byte("null")) || json.Unmarshal(field, &count) != nil {
			return false, "cached/thoughts counter"
		}
	}
	if len(shape.Version) > 0 {
		var version int
		if json.Unmarshal(shape.Version, &version) != nil || version <= 0 {
			return false, "schemaVersion"
		}
		if shape.Input == nil || shape.Output == nil || shape.Total == nil {
			return false, "versioned inputTokens/outputTokens/totalTokens"
		}
	} else if shape.Input == nil && shape.Output == nil && shape.Total == nil && len(shape.Cached)+len(shape.Thoughts) == 0 {
		return false, "legacy token counter"
	}
	return true, ""
}

// record is the ALLOW-LIST of ledger fields this adapter reads. It is a typed
// struct on purpose: encoding/json discards every key that is not named here as
// it parses, so a field the harness adds later — a response excerpt, a prompt, a
// tool argument — never becomes a value in this process, let alone in the
// ledger. A map[string]any decode, or re-emitting the source bytes as the audit
// payload, would start leaking on the day that field ships.
//
// The counters are int64 rather than pointers because the harness writes every
// one of them unconditionally (each passes through toNonNegativeInteger), so an
// absent field and a zero field mean the same thing here: no tokens.
type record struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	SessionID     string `json:"sessionId"`
	Model         string `json:"model"`
	AuthType      string `json:"authType"`
	Source        string `json:"source"`
	InputTokens   int64  `json:"inputTokens"`
	OutputTokens  int64  `json:"outputTokens"`
	CachedTokens  int64  `json:"cachedTokens"`
	ThoughtsToken int64  `json:"thoughtsTokens"`
	TotalTokens   int64  `json:"totalTokens"`
	APIDurationMs int64  `json:"apiDurationMs"`

	// localDate and localMonth are READ so the audit payload can record what the
	// writer claimed, and are used for NOTHING else. They are the writer's local
	// calendar and the file name is built from localMonth; bucketing from either
	// would bake the writing machine's timezone into stored data.
	LocalDate  string `json:"localDate"`
	LocalMonth string `json:"localMonth"`
}

// event maps one ledger record onto a usage event.
//
// A record with no `id` is refused. The harness mints a randomUUID for every
// record it writes and its own reader requires the field, so a record without
// one was not written by this surface; the alternatives are both worse than
// dropping it. A content hash silently MERGES two identical requests made in
// the same second — an undercount that looks like a quieter day — and a key
// minted from a read position recounts the record the first time a full re-read
// replaces a tail read. Refusing keeps the ledger honest and the skip is
// reported.
//
// Version numbers are not an upper bound: a newer writer retaining these
// accounting anchors stays readable. Collection diagnoses incompatible shapes
// before calling event; a renamed counter must not become a clean zero.
func (r record) event(path string, mtime time.Time) (model.UsageEvent, bool) {
	if r.ID == "" {
		return model.UsageEvent{}, false
	}

	// Gemini accounting: cachedTokens is the slice of the prompt count served
	// from cache, so it is subtracted out of Input and reported separately
	// rather than added on top. The clamp matters for real records: when the API
	// omits prompt tokens the harness falls back to the cached count, so
	// inputTokens == cachedTokens and Input is legitimately zero.
	in := adapter.NonNeg(r.InputTokens)
	cached := adapter.NonNeg(r.CachedTokens)
	if cached > in {
		cached = in
	}
	input := in - cached
	output := adapter.NonNeg(r.OutputTokens)
	reasoning := adapter.NonNeg(r.ThoughtsToken)

	// The provider's own total stays authoritative wherever it covers what is
	// stored beside it, and is raised when it does not (issue #49): a row must
	// never report a total below its own components. Reasoning is NOT one of
	// them, for the same reason cache read is not: on the OpenAI-compatible
	// wires it is already inside the output count, so adding it here would
	// raise the stored total by the reasoning count of every reasoning turn —
	// see the package comment.
	total := adapter.NonNeg(r.TotalTokens)
	if sum := input + cached + output; total < sum {
		total = sum
	}
	if total == 0 {
		return model.UsageEvent{}, false // no usage in this record
	}

	return model.UsageEvent{
		Tool:  model.ToolQwenCode,
		Model: r.Model,
		// Provider is deliberately empty: authType names the credential kind,
		// not the biller. See the package comment.
		Provider:            "",
		ServiceTier:         "",
		SessionID:           r.SessionID,
		Project:             "", // the ledger records no cwd
		EventTime:           r.eventTime(mtime),
		InputTokens:         input,
		OutputTokens:        output,
		CacheCreationTokens: 0, // the surface reports none
		CacheReadTokens:     cached,
		ReasoningTokens:     reasoning,
		TotalTokens:         total,
		MessageID:           r.ID,
		SourcePath:          path,
		// The provider's own UUID, and nothing else: no path and no session, so
		// a ledger copied to another machine or another directory counts once.
		DedupKey: model.ToolQwenCode + "|" + r.ID,
		Kind:     model.KindUsage,
		Raw:      r.auditPayload(),
	}, true
}

// eventTime parses the record's ISO 8601 instant. `timestamp` is the ONLY time
// field read: localDate and localMonth are the writer's calendar, and the file
// name is built from localMonth.
//
// An unparseable timestamp falls back to the file's mtime. That is safe here in
// a way it is not for every adapter: the dedup key is the provider's UUID and
// does not contain the time, so a fallback that moves between polls can never
// re-count the record — it only misplaces it, by the age of the file at worst.
func (r record) eventTime(mtime time.Time) time.Time {
	if s := strings.TrimSpace(r.Timestamp); s != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	return mtime
}

// turnContext records the subagent a turn ran as, on the agent dimension.
//
// `source` is the harness's `subagent_name || "main"`. The "main" sentinel means
// no subagent issued the request and produces NO row: an absent context is the
// honest record of "the main conversation ran this", while a stored "main" would
// both invent an agent and collide with a subagent that really is called main.
//
// One record is one API response, so there is at most one value here and no
// union across records is needed — this surface does not stream one response
// across several lines. The row names the turn's FULL cost, as every turn
// context does; it is one of the six partitions and is never summed with
// another.
func (r record) turnContext(ev model.UsageEvent) (model.TurnContext, bool) {
	name := strings.TrimSpace(r.Source)
	if name == "" || name == mainSource {
		return model.TurnContext{}, false
	}
	return model.TurnContext{
		UsageDedupKey: ev.DedupKey,
		Tool:          model.ToolQwenCode,
		Dimension:     model.DimensionAgent,
		Value:         name,
		SessionID:     ev.SessionID,
		Project:       ev.Project,
		Model:         ev.Model,
		EventTime:     ev.EventTime,
		SourcePath:    ev.SourcePath,
	}, true
}

// auditRecord is what UsageEvent.Raw stores: the ledger record's counters and
// identities, copied field by field out of the typed decode above and
// re-marshalled. It is an allow-list twice over — the decode names the fields it
// reads, and this names the fields it keeps — so neither a new harness field nor
// a hand-edited line can reach the ledger through the audit payload.
//
// It happens to cover the whole record today, because the whole record is
// counters and identities. That is a fact about this version of the surface, not
// a licence to store the source line: the line is re-emitted from the decode
// precisely so the payload stops growing the moment the record does.
type auditRecord struct {
	SchemaVersion int    `json:"schemaVersion,omitempty"`
	ID            string `json:"id,omitempty"`
	Timestamp     string `json:"timestamp,omitempty"`
	LocalDate     string `json:"localDate,omitempty"`
	LocalMonth    string `json:"localMonth,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	Model         string `json:"model,omitempty"`
	AuthType      string `json:"authType,omitempty"`
	Source        string `json:"source,omitempty"`
	InputTokens   int64  `json:"inputTokens"`
	OutputTokens  int64  `json:"outputTokens"`
	CachedTokens  int64  `json:"cachedTokens"`
	ThoughtsToken int64  `json:"thoughtsTokens"`
	TotalTokens   int64  `json:"totalTokens"`
	APIDurationMs int64  `json:"apiDurationMs,omitempty"`
}

// auditPayload builds the stored audit payload. Best-effort: an un-marshalable
// payload yields an empty raw rather than failing the parse.
func (r record) auditPayload() string {
	b, err := json.Marshal(auditRecord{
		SchemaVersion: r.SchemaVersion,
		ID:            r.ID,
		Timestamp:     r.Timestamp,
		LocalDate:     r.LocalDate,
		LocalMonth:    r.LocalMonth,
		SessionID:     r.SessionID,
		Model:         r.Model,
		AuthType:      r.AuthType,
		Source:        r.Source,
		InputTokens:   r.InputTokens,
		OutputTokens:  r.OutputTokens,
		CachedTokens:  r.CachedTokens,
		ThoughtsToken: r.ThoughtsToken,
		TotalTokens:   r.TotalTokens,
		APIDurationMs: r.APIDurationMs,
	})
	if err != nil {
		return ""
	}
	return string(b)
}
