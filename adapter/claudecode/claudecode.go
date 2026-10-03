// Package claudecode implements the event-level adapter for the Claude Code
// CLI. It reads append-only JSONL transcripts under each config root's
// projects/ tree, maps Anthropic-style usage (cache tokens additive), and
// deduplicates message replays (including sidechain replays) within a cycle.
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY; nothing under the
// agent's directories is created, locked, or modified.
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// usageMarker fast-skips lines that cannot carry usage data.
const usageMarker = `"usage":{`

// usageMarkerBytes lets the scanner loop probe with bytes.Contains instead of
// copying every line to a string first.
var usageMarkerBytes = []byte(usageMarker)

// hookMarkerBytes fast-skips to the hook records. They are type=="system"
// lines that carry no usage block at all, so the usage marker alone would skip
// every one of them.
var hookMarkerBytes = []byte(hookSummarySubtype)

// hookSummarySubtype is the only hook record Claude Code writes to a
// transcript. Verified over 1264 local transcripts: the type=="system" subtypes
// present are stop_hook_summary, turn_duration, local_command,
// scheduled_task_fire, compact_boundary, bridge_status, informational and the
// model-fallback pair — no PreToolUse/PostToolUse/SessionStart record exists in
// this format at all, so Stop is the only hook event observable here.
const hookSummarySubtype = "stop_hook_summary"

// hookEventName is what a hook row is NAMED. The transcript does not carry a
// hook name: each element of hookInfos identifies its hook only by the raw
// shell COMMAND it ran, which is exactly the tool-input content activity
// collection refuses to store. So the recorded name is the hook EVENT, which is
// the part that is a name rather than content, and the per-hook identity is
// deliberately dropped rather than smuggled in as a command string or a hash of
// one (a hash would be an identifier no human could read and no query could
// group by usefully).
const hookEventName = "Stop"

// syntheticModel is dropped per the parsing spec.
const syntheticModel = "<synthetic>"

// ConfigDirEnv names the environment variable that moves the Claude Code
// configuration root, and with it every transcript this adapter reads. It is
// exported because a caller that copies this process's discovery into another
// one - the systemd units internal/service writes - has to know that the
// environment, not the defaults, decided what gets collected.
const ConfigDirEnv = "CLAUDE_CONFIG_DIR"

// Adapter reads Claude Code usage transcripts.
type Adapter struct{}

// New returns a Claude Code adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolClaudeCode }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Claude Code" }

// Capabilities declares what this project can say about Claude Code.
//
// Cost is COMPUTED: nothing here calls SetCost, so every row is valued from the
// public rate card by collect.stampCost or left unpriced. Activity is an EXACT
// join because a tool_use block and the .message.usage it was billed under sit
// in the SAME message, so mintActivity can name the usage row that paid for
// each call.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolClaudeCode,
		Cost:      model.CostComputed,
		Activity:  model.ActivityExact,
		Reasoning: model.ReasoningReportFor(model.ToolClaudeCode),
		Tier:      model.TierLive,
	}
}

// Discover locates Claude Code config roots that contain a projects/ tree.
//
// Resolution order:
//  1. An explicit override (DiscoverConfig.Overrides[claude-code]), normalised.
//  2. env CLAUDE_CONFIG_DIR (comma list), each entry normalised.
//  3. BOTH <home>/.config/claude and <home>/.claude.
//
// A path ending in /projects is normalised to its parent. A root is accepted
// only if <root>/projects/ exists as a directory. Results are path-deduped.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	var candidates []string

	if cfg.Overrides != nil {
		if v := strings.TrimSpace(cfg.Overrides[model.ToolClaudeCode]); v != "" {
			candidates = append(candidates, splitRoots(v)...)
		}
	}
	if len(candidates) == 0 {
		if env := strings.TrimSpace(os.Getenv(ConfigDirEnv)); env != "" {
			candidates = append(candidates, splitRoots(env)...)
		}
	}
	if len(candidates) == 0 && cfg.Home != "" {
		candidates = append(candidates,
			filepath.Join(cfg.Home, ".config", "claude"),
			filepath.Join(cfg.Home, ".claude"),
		)
	}

	seen := make(map[string]struct{})
	var sources []adapter.Source
	for _, c := range candidates {
		root := normaliseRoot(c)
		if root == "" {
			continue
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		// The no-message-id dedup fallback embeds transcript paths, so resolve
		// symlinks: a re-pointed root would otherwise mint new keys for those
		// lines. A genuinely moved root still re-adds them once — irreducible
		// without state migration.
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		if _, dup := seen[root]; dup {
			continue
		}
		projDir := filepath.Join(root, "projects")
		if fi, err := os.Stat(projDir); err != nil || !fi.IsDir() {
			continue
		}
		seen[root] = struct{}{}
		sources = append(sources, adapter.Source{
			Tool:  model.ToolClaudeCode,
			Class: model.EventLevel,
			Path:  root,
			Label: "Claude Code: " + root,
			Meta:  map[string]string{"projects": projDir},
		})
	}
	return sources, nil
}

// splitRoots splits a comma-separated env/override value into trimmed entries.
func splitRoots(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normaliseRoot collapses a trailing /projects segment to the parent root.
func normaliseRoot(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	if filepath.Base(clean) == "projects" {
		return filepath.Dir(clean)
	}
	return clean
}

// fileStamp is one manifest entry: the size+mtime of a transcript file at the
// last completed read.
type fileStamp struct {
	Size    int64 `json:"size"`
	MTimeNS int64 `json:"mtime"`
}

// Collect walks <root>/projects/**/*.jsonl, parses every usage line, applies
// in-cycle dedup, and returns the surviving events with any per-file errors.
// A bad transcript does not discard observations from readable files.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental gates the whole root on a per-file size+mtime manifest:
// when no transcript under projects/ changed, the walk is stats only and no
// file is opened. Any change re-parses EVERY file — the deduper's sidechain
// consolidation spans all files of a root in one pass, so per-file tail reads
// would break cross-file dedup. Correctness of the full re-parse is carried by
// the persisted dedup keys (INSERT OR IGNORE collapses re-derived events).
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	projDir := filepath.Join(src.Path, "projects")

	type entry struct {
		path    string
		segment string
	}
	var (
		files    []entry
		manifest = make(map[string]fileStamp)
		statErr  bool
	)
	walkErr := filepath.WalkDir(projDir, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees, keep walking
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			return nil
		}
		if !adapter.WalkEntryIsFile(de, path) {
			return nil
		}
		fi, err := de.Info()
		if err != nil {
			// A file we cannot stat cannot be gated; parse unconditionally and
			// withhold the checkpoint so the next cycle re-examines it.
			statErr = true
		} else {
			manifest[path] = fileStamp{Size: fi.Size(), MTimeNS: fi.ModTime().UnixNano()}
		}
		files = append(files, entry{path: path, segment: projectSegment(projDir, path)})
		return nil
	})
	if walkErr != nil {
		return adapter.Observation{}, walkErr // only ctx cancellation escapes the walk
	}

	if cp != nil && !statErr && manifestUnchanged(cp.State, manifest) {
		return adapter.Observation{}, nil // nothing changed: no parse, keep stored checkpoint
	}

	d := newDeduper()
	var parseErr error
	complete := true
	for _, e := range files {
		if ctx.Err() != nil {
			return adapter.Observation{
				Events:       d.events(),
				Activity:     d.activity(),
				TurnContexts: d.turnContexts(),
			}, ctx.Err()
		}
		readComplete, err := parseFile(e.path, e.segment, d)
		// A partial read must not land in the manifest: the unchanged-file
		// gate would skip its unread content on the next poll.
		complete = complete && readComplete
		parseErr = errors.Join(parseErr, err)
	}

	obs := adapter.Observation{
		Events:       d.events(),
		Activity:     d.activity(),
		TurnContexts: d.turnContexts(),
	}
	if statErr || !complete {
		return obs, parseErr
	}
	stateJSON, err := json.Marshal(manifest)
	if err != nil {
		return obs, parseErr
	}
	obs.Checkpoint = &model.SourceCheckpoint{
		Tool: model.ToolClaudeCode, SourcePath: src.Path, State: string(stateJSON),
	}
	return obs, parseErr
}

// manifestUnchanged reports whether the stored manifest JSON matches the
// current file set exactly (same paths, sizes, mtimes — additions, removals
// and edits all break equality).
func manifestUnchanged(stored string, current map[string]fileStamp) bool {
	if stored == "" {
		return false
	}
	var prev map[string]fileStamp
	if err := json.Unmarshal([]byte(stored), &prev); err != nil {
		return false
	}
	if len(prev) != len(current) {
		return false
	}
	for path, st := range current {
		if p, ok := prev[path]; !ok || p != st {
			return false
		}
	}
	return true
}

// projectSegment returns the immediate projects/<segment> directory name used
// as the fallback project when a line carries no cwd.
func projectSegment(projDir, file string) string {
	rel, err := filepath.Rel(projDir, file)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// parseFile reads one JSONL transcript line-by-line, feeding parsed candidates
// into the deduper. The bool reports whether the read completed; incomplete
// reads must not checkpoint the root. Complete malformed usage records are
// reported while retaining the surrounding valid rows.
func parseFile(path, segment string, d *deduper) (bool, error) {
	f, err := os.Open(path) // O_RDONLY
	if err != nil {
		return false, fmt.Errorf("claude-code: open %s: %w", path, err)
	}
	defer f.Close()

	sessionFromName := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate long JSONL lines

	var parseErr error
	complete, recognized, nonempty := true, false, false
	lineNumber := 0
	for sc.Scan() {
		lineNumber++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		nonempty = true
		typ, err := recordType(line)
		if err != nil {
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &typeErr) {
				parseErr = errors.Join(parseErr, fmt.Errorf("claude-code: %s line %d: malformed record", path, lineNumber))
				continue
			}
		}
		switch typ {
		case "assistant", "user", "system", "progress", "summary", "file-history-snapshot", "queue-operation":
			recognized = true
		}
		if bytes.Contains(line, usageMarkerBytes) || typ == "assistant" {
			cand, ok, err := parseLine(line, path, segment, sessionFromName)
			if err != nil {
				parseErr = errors.Join(parseErr, fmt.Errorf("claude-code: %s line %d: %w", path, lineNumber, err))
				complete = complete && !errors.Is(err, adapter.ErrSourceFormat)
			}
			if ok {
				recognized = true
				d.add(cand)
			}
			continue
		}
		// Hook records carry no usage block, so they only reach here.
		if bytes.Contains(line, hookMarkerBytes) {
			d.addHooks(parseHookLine(line, path, segment, sessionFromName))
		}
	}
	if err := sc.Err(); err != nil {
		return false, errors.Join(parseErr, fmt.Errorf("claude-code: read %s: %w", path, err))
	}
	if nonempty && !recognized {
		return false, errors.Join(parseErr, fmt.Errorf("claude-code: %s: %w: no recognized transcript records", path, adapter.ErrSourceFormat))
	}
	return complete, parseErr
}

// recordType stops at the top-level discriminator so ordinary message content
// stays behind the usage/hook marker gate. The standard decoder handles fields
// before type without imposing an ordering or escaping rule on transcripts.
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

// nullable wraps an optional JSON scalar, distinguishing an absent key (zero
// value), an explicit JSON null (Null), and a decoded value — all recorded
// during the single typed decode pass, replacing the former generic re-parse
// of every line just to find guarded nulls. A wrong-typed value fails the
// enclosing Unmarshal, matching the previous pointer-field behaviour.
type nullable[T any] struct {
	Present bool
	Null    bool
	Value   T
}

func (n *nullable[T]) UnmarshalJSON(b []byte) error {
	n.Present = true
	if string(b) == "null" {
		n.Null = true
		return nil
	}
	return json.Unmarshal(b, &n.Value)
}

// rawLine models both serde-untagged shapes: Direct fields at the top level and
// the AgentProgress wrapper under data. Guarded keys use nullable so a JSON
// null is detected in the same pass.
type rawLine struct {
	Type              string            `json:"type"`
	Timestamp         *string           `json:"timestamp"`
	CWD               nullable[string]  `json:"cwd"`
	SessionID         nullable[string]  `json:"sessionId"`
	RequestID         nullable[string]  `json:"requestId"`
	IsSidechain       *bool             `json:"isSidechain"`
	IsAPIErrorMessage nullable[bool]    `json:"isApiErrorMessage"`
	Version           nullable[string]  `json:"version"`
	CostUSD           nullable[float64] `json:"costUSD"`
	// The five turn-attribution fields. Each names what this record was produced
	// UNDER along one axis: the subagent that ran, the skill that was active,
	// the MCP tool and server being served, the plugin the code came from.
	//
	// EVERY ONE IS A PLAIN TOP-LEVEL STRING — not an object, not a list. Verified
	// over the whole local corpus: 99,894 occurrences across the five fields,
	// 100% of them JSON strings. That is what makes per-value cost a sum with no
	// divisor, and it is why the store can key on (usage row, dimension).
	//
	// THIS IS THE ENTIRE ALLOW-LIST, AND THE FIELD TYPES ARE THE ENFORCEMENT.
	// Each is a string, so encoding/json can only ever put a name in it: a
	// nested object of arguments, a prompt, a result has no field to land in and
	// never becomes a value in this process. Adding a sixth attribution axis
	// means adding a line here, which is the point — the shape does not widen on
	// its own the day the transcript format grows a content field.
	//
	// They decode leniently, and that is not cosmetic: this is enrichment, and a
	// field the ledger did without until now must never become a way to LOSE a
	// usage row. A null, a number or an object yields the empty string and the
	// record is stored exactly as it was before these fields existed, with no
	// turn context emitted. They are likewise NOT guarded keys — a record
	// carrying none of them is entirely ordinary (17,684 of 120,571 assistant
	// records locally carry no attribution at all).
	AttributionAgent     lenientString `json:"attributionAgent"`
	AttributionSkill     lenientString `json:"attributionSkill"`
	AttributionMcpTool   lenientString `json:"attributionMcpTool"`
	AttributionMcpServer lenientString `json:"attributionMcpServer"`
	AttributionPlugin    lenientString `json:"attributionPlugin"`
	Message              *message      `json:"message"`
	Data                 *struct {
		Message *message `json:"message"`
	} `json:"data"`
}

// turnContext is one (dimension, value) pair read off a record, before it is
// tied to the turn's surviving usage event.
//
// It rides the candidate rather than becoming a finished model.TurnContext at
// parse time for the same reason a tool call does: THE TURN A CONTEXT BELONGS TO
// IS NOT A FACT ABOUT THE RECORD IT WAS READ IN. Claude Code streams one API
// response across several transcript records sharing a message.id, only one of
// which becomes a usage row, and the winner is chosen on TOKEN metrics that have
// nothing whatever to do with attribution. Taking only the winner's copy is the
// exact shape of the bug that lost 31% of this ledger's tool calls, so the
// deduper unions these across every record of the message — see
// deduper.addContexts.
type turnContext struct {
	dim   model.TurnDimension
	value string
}

// recordContexts reads the five attribution fields off one decoded record, in
// the fixed dimension order, skipping the ones it does not carry. Order is fixed
// rather than map-derived so a turn's rows land in the same sequence on every
// pass and a diff of two collections is empty rather than reshuffled.
func recordContexts(rl *rawLine) []turnContext {
	pairs := [...]struct {
		dim model.TurnDimension
		raw lenientString
	}{
		{model.DimensionAgent, rl.AttributionAgent},
		{model.DimensionSkill, rl.AttributionSkill},
		{model.DimensionMCPTool, rl.AttributionMcpTool},
		{model.DimensionMCPServer, rl.AttributionMcpServer},
		{model.DimensionPlugin, rl.AttributionPlugin},
	}
	var out []turnContext
	for _, p := range pairs {
		if v := strings.TrimSpace(string(p.raw)); v != "" {
			out = append(out, turnContext{dim: p.dim, value: v})
		}
	}
	return out
}

type message struct {
	ID    nullable[string] `json:"id"`
	Model nullable[string] `json:"model"`
	Usage *usage           `json:"usage"`
	// Content carries the assistant's content blocks, decoded ONLY far enough
	// to see which tools were called — see contentBlock.
	Content []contentBlock `json:"content"`
}

// contentBlock is one element of message.content, decoded as an explicit
// ALLOW-LIST of the fields activity collection needs. It is the same discipline
// the audit payload follows, applied to a second fact type.
//
// Input is the part that matters. A tool_use block's input is the tool's
// arguments — the Bash command, the file path, the prompt — and none of it may
// reach the ledger. Decoding it into a struct with a single Skill field means
// encoding/json DISCARDS every other key as it parses: the content is not
// stripped after the fact, it never becomes a value in this process at all. A
// new field appears in a tool's input the day the format grows one, and this
// shape ignores it without being told to.
type contentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input holds the ONE input field that is a name rather than content: which
	// skill a Skill call invoked.
	Input struct {
		Skill string `json:"skill"`
	} `json:"input"`
}

type usage struct {
	InputTokens         *int64           `json:"input_tokens"`
	OutputTokens        *int64           `json:"output_tokens"`
	CacheCreationTokens nullable[int64]  `json:"cache_creation_input_tokens"`
	CacheReadTokens     nullable[int64]  `json:"cache_read_input_tokens"`
	Speed               nullable[string] `json:"speed"`
	// Enrichment-only fields. They are NOT guarded keys: an absent, null or
	// unexpected value must leave the line's stored accounting untouched, so
	// these decode leniently rather than failing the enclosing Unmarshal.
	ServiceTier         lenientString        `json:"service_tier"`
	CacheCreation       *cacheCreation       `json:"cache_creation"`
	OutputTokensDetails *outputTokensDetails `json:"output_tokens_details"`
}

// lenientString decodes a JSON string and treats every other shape (null,
// number, object) as the empty string. Enrichment must never drop a line that
// parsed before it existed.
type lenientString string

func (s *lenientString) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err == nil {
		*s = lenientString(v)
	}
	return nil
}

// cacheCreation is Anthropic's per-TTL split of cache_creation_input_tokens.
// The two lifetimes are priced differently, so the pricing stamp reads the
// split transiently; the ledger keeps only the combined count.
type cacheCreation struct {
	Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
}

// UnmarshalJSON tolerates any shape for the same reason as lenientString.
func (c *cacheCreation) UnmarshalJSON(b []byte) error {
	type plain cacheCreation
	var v plain
	if err := json.Unmarshal(b, &v); err == nil {
		*c = cacheCreation(v)
	}
	return nil
}

// outputTokensDetails records reasoning already included in output_tokens.
// Like the other optional details, an unexpected shape must not discard usage.
type outputTokensDetails struct {
	ThinkingTokens int64 `json:"thinking_tokens"`
}

func (d *outputTokensDetails) UnmarshalJSON(b []byte) error {
	type plain outputTokensDetails
	var v plain
	if err := json.Unmarshal(b, &v); err == nil {
		*d = outputTokensDetails(v)
	}
	return nil
}

// candidate is a parsed usage record awaiting dedup.
type candidate struct {
	event       model.UsageEvent
	messageID   string
	requestID   string
	isSidechain bool
	hasSpeed    bool
	cost        float64
	total       int64
	// calls is the tool/skill calls this record contained, as identities and
	// names rather than finished activity rows. They ride the candidate because
	// the turn a call belongs to is NOT knowable from the record it was read in:
	// see toolCall.
	calls []toolCall
	// contexts is what this record says the turn was running under, at most one
	// value per dimension. Like calls, it rides the candidate because the
	// deduper has to union it across every record of the message before any of
	// it can be attributed — see turnContext.
	contexts []turnContext
}

// parseLine decodes one transcript line into a candidate. It returns ok=false
// to skip the line (malformed JSON, null in a guarded key, missing usage, or an
// empty/synthetic model).
func parseLine(line []byte, path, segment, sessionFromName string) (candidate, bool, error) {
	// A custom field decoder can stop before a later version field is read.
	// Identify the source family independently of field order.
	var source struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(line, &source); err != nil {
		var fieldErr *json.UnmarshalTypeError
		if errors.As(err, &fieldErr) && fieldErr.Field == "version" {
			return candidate{}, false, fmt.Errorf("%w: malformed producer version", adapter.ErrSourceFormat)
		}
	}
	versioned := source.Version != ""
	var rl rawLine
	if err := json.Unmarshal(line, &rl); err != nil {
		var fieldErr *json.UnmarshalTypeError
		if errors.As(err, &fieldErr) && fieldErr.Field == "timestamp" {
			return candidate{}, false, errors.New("usage record has malformed timestamp")
		}
		if versioned {
			return candidate{}, false, fmt.Errorf("%w: malformed versioned assistant record", adapter.ErrSourceFormat)
		}
		return candidate{}, false, nil
	}

	// Flatten AgentProgress (data.message) onto the Direct shape.
	msg := rl.Message
	if msg == nil && rl.Data != nil {
		msg = rl.Data.Message
	}
	// Versioned direct assistant records have stable identities and usage.
	// Older unversioned and AgentProgress records retain their supported fallbacks.
	direct := rl.Data == nil || rl.Data.Message == nil
	if versioned && direct && (rl.Type == "assistant" || (msg != nil && msg.Usage != nil)) {
		if rl.Type != "assistant" {
			return candidate{}, false, fmt.Errorf("%w: assistant usage requires type=assistant", adapter.ErrSourceFormat)
		}
		if msg == nil {
			return candidate{}, false, fmt.Errorf("%w: assistant record missing message", adapter.ErrSourceFormat)
		}
		if strings.TrimSpace(msg.Model.Value) == syntheticModel {
			return candidate{}, false, nil
		}
		if strings.TrimSpace(msg.ID.Value) == "" {
			return candidate{}, false, fmt.Errorf("%w: assistant message missing id", adapter.ErrSourceFormat)
		}
		if strings.TrimSpace(msg.Model.Value) == "" {
			return candidate{}, false, fmt.Errorf("%w: assistant message missing model", adapter.ErrSourceFormat)
		}
		if msg.Usage == nil || msg.Usage.InputTokens == nil || msg.Usage.OutputTokens == nil {
			return candidate{}, false, fmt.Errorf("%w: assistant message missing usage input/output tokens", adapter.ErrSourceFormat)
		}
	}
	if msg == nil || msg.Usage == nil {
		return candidate{}, false, nil
	}
	u := msg.Usage

	// Reject lines where any guarded key is explicitly JSON null, recorded by
	// the nullable fields during the same decode pass.
	if hasGuardedNull(&rl, msg) {
		return candidate{}, false, nil
	}

	// Model: drop <synthetic>; append -fast for fast speed; empty rejects.
	modelID := strings.TrimSpace(msg.Model.Value)
	if modelID == "" || modelID == syntheticModel {
		return candidate{}, false, nil
	}
	if u.Speed.Value == "fast" {
		modelID += "-fast"
	}

	in := deref(u.InputTokens)
	out := deref(u.OutputTokens)
	cacheC := adapter.NonNeg(u.CacheCreationTokens.Value)
	cacheR := adapter.NonNeg(u.CacheReadTokens.Value)
	total := in + out + cacheC + cacheR // reasoning is already in output
	var reasoning int64
	if u.OutputTokensDetails != nil {
		reasoning = adapter.NonNeg(u.OutputTokensDetails.ThinkingTokens)
	}

	project := segment
	if strings.TrimSpace(rl.CWD.Value) != "" {
		project = rl.CWD.Value
	}

	session := sessionFromName
	if strings.TrimSpace(rl.SessionID.Value) != "" {
		session = rl.SessionID.Value
	}

	var eventTime time.Time
	if rl.Timestamp != nil {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*rl.Timestamp)); err == nil {
			eventTime = t.UTC()
		}
	}

	if eventTime.IsZero() {
		return candidate{}, false, errors.New("usage record has missing or malformed timestamp")
	}

	messageID := msg.ID.Value
	requestID := rl.RequestID.Value
	isSidechain := rl.IsSidechain != nil && *rl.IsSidechain

	dedupKey := persistedKey(messageID, path, line)

	// Cache-write TTL split: transient pricing input, never stored. When the
	// usage block omits cache_creation the split stays zero and the pricing
	// engine falls back to the 5m rate for the whole cache write.
	var ttl model.CacheWriteTTL
	if u.CacheCreation != nil {
		ttl.Ephemeral5m = adapter.NonNeg(u.CacheCreation.Ephemeral5m)
		ttl.Ephemeral1h = adapter.NonNeg(u.CacheCreation.Ephemeral1h)
	}

	ev := model.UsageEvent{
		Tool:                model.ToolClaudeCode,
		Model:               modelID,
		Provider:            model.ProviderAnthropic,
		ServiceTier:         strings.TrimSpace(string(u.ServiceTier)),
		SessionID:           session,
		Project:             project,
		EventTime:           eventTime,
		InputTokens:         in,
		OutputTokens:        out,
		CacheCreationTokens: cacheC,
		CacheReadTokens:     cacheR,
		ReasoningTokens:     reasoning,
		TotalTokens:         total,
		CacheTTL:            ttl,
		RequestID:           requestID,
		MessageID:           messageID,
		SourcePath:          path,
		DedupKey:            dedupKey,
		Kind:                model.KindUsage,
		Raw:                 auditPayload(&rl, msg),
	}

	return candidate{
		event:       ev,
		messageID:   messageID,
		requestID:   requestID,
		isSidechain: isSidechain,
		hasSpeed:    u.Speed.Present,
		cost:        rl.CostUSD.Value,
		total:       total,
		calls:       recordCalls(msg.Content, line),
		contexts:    recordContexts(&rl),
	}, true, nil
}

// toolCall is one tool_use block reduced to the identity and the two names the
// activity ledger needs.
//
// A record's blocks are carried as calls rather than as finished activity rows
// because THE TURN A CALL BELONGS TO IS NOT A FACT ABOUT THE RECORD IT WAS READ
// IN. Claude Code streams one API response across SEVERAL transcript records
// that share a single message.id (measured locally: 102,733 assistant records
// with usage collapse to 50,696 message ids, and 7,953 of those ids have more
// than one call-carrying record). Those records are one usage event — that is
// what persistedKey keys on — so their tool_use blocks are calls of ONE turn.
// Counting them per record made every such turn report calls_in_turn=1 for a
// turn that had several, and, worse, the deduper kept only the winning record's
// blocks and dropped the rest: 41,150 of 60,832 calls reached the ledger.
// Settling both the sequence and the divisor therefore has to wait until every
// record of the message has been seen, which is what the deduper does.
type toolCall struct {
	// id is the provider's tool_use id ("toolu_..."). It is globally unique per
	// call — 60,832 blocks locally, 60,832 distinct ids, none repeated — and it
	// is stable across the sidechain replays that repeat a whole record, which
	// is what makes it both the cross-poll dedup key and the identity the union
	// of a message's records is deduplicated on.
	id string
	// fallback identifies a block that carries no id at all (0 of 60,832
	// locally) by the CONTENT of its record plus its position among that
	// record's blocks. It is deliberately not derived from read position: the
	// adapter re-reads every transcript in full on every poll, so a key minted
	// from a file offset or a line number would mint a fresh one each pass and
	// recount the call forever. Two records of one message hash differently, so
	// id-less blocks no longer collide across the message the way a bare
	// within-record index did; a byte-identical replay hashes the same and
	// collapses. The residual: a replay that differs only in a field outside
	// this shape would count an id-less block twice.
	fallback string
	kind     model.ActivityKind
	name     string
}

// identity is what deduplicates a call within one message's union of records.
func (c toolCall) identity() string {
	if c.id != "" {
		return "id:" + c.id
	}
	return "raw:" + c.fallback
}

// dedupKey is the cross-poll key stored in activity_events. The id form is
// unchanged from the first release of this ledger, which is what lets the
// re-read that recovers the dropped calls insert them ALONGSIDE the rows
// already stored instead of duplicating them.
func (c toolCall) dedupKey() string {
	if c.id != "" {
		return model.ToolClaudeCode + "|call|" + c.id
	}
	return model.ToolClaudeCode + "|act|" + c.fallback
}

// recordCalls extracts one record's tool_use blocks. line is hashed only when a
// block turns out to carry no id, so the common path costs nothing.
func recordCalls(blocks []contentBlock, line []byte) []toolCall {
	var (
		out    []toolCall
		digest string
	)
	for i, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		name := strings.TrimSpace(b.Name)
		if name == "" {
			continue
		}
		kind := model.ActivityTool
		// A Skill call names a tool ("Skill") that is never the interesting
		// fact; the skill it invoked is. Record the skill under kind=skill so
		// "which skill" is a group-by rather than a parse at read time. A Skill
		// call with no skill in its input stays a plain tool call rather than
		// becoming a nameless skill row.
		if name == "Skill" {
			if s := strings.TrimSpace(b.Input.Skill); s != "" {
				kind, name = model.ActivitySkill, s
			}
		}
		c := toolCall{id: b.ID, kind: kind, name: name}
		if b.ID == "" {
			if digest == "" {
				digest = fmt.Sprintf("%x", sha1.Sum(line))
			}
			c.fallback = digest + "|" + strconv.Itoa(i)
		}
		out = append(out, c)
	}
	return out
}

// mintActivity turns one turn's whole set of calls into activity rows
// attributed to that turn's usage event.
//
// This is the exact join the whole design rests on: Claude Code puts the
// tool_use blocks and the usage object in the same message, so a call's cost is
// a fact about the response rather than a guess about which nearby turn paid
// for it. UsageDedupKey is that event's dedup key and CallsInTurn is the number
// of calls sharing it, which is what lets the read path divide instead of
// multiply. calls is the union across every record of the message, so the
// divisor equals the number of rows that will exist for the key.
//
// EventTime is copied from the usage event, not from the record each block was
// read in, so both ledgers place the call in the same instant. A streamed
// response spans real time, so this deliberately collapses a turn's calls onto
// the one timestamp its cost is stored under: a windowed comparison of the two
// ledgers is only meaningful if a call and the tokens it is attributed a share
// of land in the same window.
//
// Records this adapter rejects as usage — a guarded JSON null, a <synthetic>
// model — produce no activity either. That is deliberate: a call attributed to
// a usage event that was never stored would be a call whose cost is silently
// unattributable, and the honest place to draw the line is the record the
// ledger accepted.
func mintActivity(calls []toolCall, ev model.UsageEvent, requestID string) []model.ActivityEvent {
	if len(calls) == 0 {
		return nil
	}
	out := make([]model.ActivityEvent, 0, len(calls))
	for i, c := range calls {
		out = append(out, model.ActivityEvent{
			Tool:          model.ToolClaudeCode,
			Kind:          c.kind,
			Name:          c.name,
			SessionID:     ev.SessionID,
			Project:       ev.Project,
			Model:         ev.Model,
			EventTime:     ev.EventTime,
			UsageDedupKey: ev.DedupKey,
			MessageID:     ev.MessageID,
			RequestID:     requestID,
			TurnSeq:       i,
			CallsInTurn:   len(calls),
			SourcePath:    ev.SourcePath,
			DedupKey:      c.dedupKey(),
		})
	}
	return out
}

// hookLine is the ALLOW-LIST decode of a type=="system" hook record.
//
// HookInfos is the point of the shape. Each element identifies its hook only by
// the raw shell command it ran, so it is decoded as a slice of EMPTY structs:
// that yields the NUMBER of hooks that fired while making it impossible for a
// command string to become a value in this process. The count is the fact;
// the command is content.
type hookLine struct {
	Type      string     `json:"type"`
	Subtype   string     `json:"subtype"`
	UUID      string     `json:"uuid"`
	Timestamp string     `json:"timestamp"`
	SessionID string     `json:"sessionId"`
	CWD       string     `json:"cwd"`
	HookCount int        `json:"hookCount"`
	HookInfos []struct{} `json:"hookInfos"`
}

// parseHookLine decodes one hook record into activity rows — one per hook that
// actually fired, so the count reflects hooks rather than summaries.
//
// Hook rows carry no UsageDedupKey: a hook runs outside any assistant turn and
// the record holds no usage block, so there is no cost to attribute and none is
// invented. They are reported as unattributed calls, never as free ones.
func parseHookLine(line []byte, path, segment, sessionFromName string) []model.ActivityEvent {
	var hl hookLine
	if err := json.Unmarshal(line, &hl); err != nil {
		return nil
	}
	if hl.Type != "system" || hl.Subtype != hookSummarySubtype {
		return nil
	}

	n := len(hl.HookInfos)
	if n == 0 {
		n = hl.HookCount
	}
	if n <= 0 {
		return nil // a summary that reports no hook is not a firing
	}

	var eventTime time.Time
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(hl.Timestamp)); err == nil {
		eventTime = t.UTC()
	} else {
		return nil // a row that cannot be windowed is worse than no row
	}

	session := sessionFromName
	if strings.TrimSpace(hl.SessionID) != "" {
		session = hl.SessionID
	}
	project := segment
	if strings.TrimSpace(hl.CWD) != "" {
		project = hl.CWD
	}
	// The record uuid is unique per line, so a re-read of the same transcript
	// re-derives the same keys and inserts nothing.
	base := model.ToolClaudeCode + "|hook|" + hl.UUID
	if hl.UUID == "" {
		sum := sha1.Sum(line)
		base = fmt.Sprintf("%s|hook|%s|%x", model.ToolClaudeCode, path, sum)
	}

	out := make([]model.ActivityEvent, 0, n)
	for i := range n {
		out = append(out, model.ActivityEvent{
			Tool:        model.ToolClaudeCode,
			Kind:        model.ActivityHook,
			Name:        hookEventName,
			SessionID:   session,
			Project:     project,
			EventTime:   eventTime,
			TurnSeq:     i,
			CallsInTurn: n,
			SourcePath:  path,
			DedupKey:    base + "|" + strconv.Itoa(i),
		})
	}
	return out
}

// hasGuardedNull reports whether any guarded key — id, cwd, model, speed,
// costUSD, version, sessionId, requestId, isApiErrorMessage and the cache
// token counts — is explicitly JSON null at its spec position (top level,
// message, usage). Such lines are skipped per the parsing spec. msg and
// msg.Usage are non-nil by the time this runs.
func hasGuardedNull(rl *rawLine, msg *message) bool {
	if rl.CWD.Null || rl.SessionID.Null || rl.RequestID.Null ||
		rl.IsAPIErrorMessage.Null || rl.Version.Null || rl.CostUSD.Null {
		return true
	}
	if msg.ID.Null || msg.Model.Null {
		return true
	}
	u := msg.Usage
	return u.Speed.Null || u.CacheCreationTokens.Null || u.CacheReadTokens.Null
}

// persistedKey returns the stable cross-poll dedup key. With a message id it is
// "claude-code|<id>"; otherwise "claude-code|<sourcePath>|<sha1(line)>" which is
// effectively never deduped (matches ccusage).
func persistedKey(messageID, path string, line []byte) string {
	if messageID != "" {
		return model.ToolClaudeCode + "|" + messageID
	}
	sum := sha1.Sum(line)
	return fmt.Sprintf("%s|%s|%x", model.ToolClaudeCode, path, sum)
}

// auditLine is the ALLOW-LIST of transcript fields persisted in
// UsageEvent.Raw: the token counters an audit of the stored accounting needs,
// plus the identifiers that tie the row back to its provider request. It
// deliberately mirrors the transcript's own nesting so an auditor can compare
// it against the source line field for field.
//
// Everything else the line carries — message content, cwd, tool results, the
// user's prompt — is never read into this shape, so it cannot reach the ledger
// (issue #17). This is an allow-list on purpose: stripping known-bad keys from
// the whole line would silently start leaking the day the transcript format
// grows a new content field.
type auditLine struct {
	Timestamp string       `json:"timestamp,omitempty"`
	RequestID string       `json:"requestId,omitempty"`
	Message   auditMessage `json:"message"`
}

type auditMessage struct {
	ID    string     `json:"id,omitempty"`
	Model string     `json:"model,omitempty"`
	Usage auditUsage `json:"usage"`
}

// auditUsage records the usage block as the transcript reported it: the counts
// are the provider's, not the clamped values stored in the token columns, so a
// mismatch between the two stays visible.
type auditUsage struct {
	InputTokens         *int64               `json:"input_tokens,omitempty"`
	OutputTokens        *int64               `json:"output_tokens,omitempty"`
	CacheCreationTokens *int64               `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     *int64               `json:"cache_read_input_tokens,omitempty"`
	ServiceTier         string               `json:"service_tier,omitempty"`
	Speed               string               `json:"speed,omitempty"`
	CacheCreation       *cacheCreation       `json:"cache_creation,omitempty"`
	OutputTokensDetails *outputTokensDetails `json:"output_tokens_details,omitempty"`
}

// auditPayload builds the stored audit payload from the decoded line. Values
// are copied out of the typed decode, never sliced out of the original bytes.
// Best-effort: an un-marshalable payload yields an empty raw rather than
// failing the parse. msg and msg.Usage are non-nil by the time this runs.
func auditPayload(rl *rawLine, msg *message) string {
	u := msg.Usage
	a := auditLine{
		RequestID: rl.RequestID.Value,
		Message: auditMessage{
			ID:    msg.ID.Value,
			Model: msg.Model.Value,
			Usage: auditUsage{
				InputTokens:         u.InputTokens,
				OutputTokens:        u.OutputTokens,
				ServiceTier:         string(u.ServiceTier),
				Speed:               u.Speed.Value,
				CacheCreation:       u.CacheCreation,
				OutputTokensDetails: u.OutputTokensDetails,
			},
		},
	}
	if rl.Timestamp != nil {
		a.Timestamp = *rl.Timestamp
	}
	if u.CacheCreationTokens.Present {
		v := u.CacheCreationTokens.Value
		a.Message.Usage.CacheCreationTokens = &v
	}
	if u.CacheReadTokens.Present {
		v := u.CacheReadTokens.Value
		a.Message.Usage.CacheReadTokens = &v
	}
	b, err := json.Marshal(a)
	if err != nil {
		return ""
	}
	return string(b)
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return adapter.NonNeg(*p)
}
