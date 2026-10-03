package copilot

// Activity from ~/.copilot/session-state/<id>/events.jsonl: SKILLS AND HOOKS,
// and deliberately nothing else.
//
// This is the surface CLAUDE.md used to say did not exist. It does; it is just
// not the OTEL export. The export names no skill (its
// `github.copilot.context.skills` is what was AVAILABLE, not what ran) and no
// hook at all, while this file names both explicitly: `skill.invoked.data.name`
// and `hook.start.data.hookType`.
//
// TOOL CALLS ARE NOT READ HERE, and that is the whole double-count guard.
// events.jsonl records the same 37 calls the OTEL `execute_tool` spans do, and
// the two surfaces mint different dedup keys for them — the span's
// traceId:spanId against the provider's event id — so emitting both would
// report every Copilot tool call twice. The OTEL key is the one already in the
// ledger and an append-only table cannot be re-keyed, so tool activity stays
// where it is and this file contributes only what OTEL cannot express. The
// consequence is honest and worth stating: an install with the OTEL export
// switched off records skills and hooks and no tool calls, the same way it
// records no tokens.
//
// PRIVACY, AND THIS FILE IS THE REASON THE RULE EXISTS. Unlike the OTEL span
// map, events.jsonl carries prompt and command text in at least six named
// paths: `user.message.data.content`, `assistant.message.data.content`,
// `tool.execution_start.data.arguments.{command,prompt,file_text,query,...}`,
// `permission.requested.data.permissionRequest.toolArgs`,
// `hook.start.data.input.toolCalls[].args` and `skill.invoked.data.content`
// (plus `skill.invoked.data.{path,description}` and
// `subagent.started.data.agentDescription`). None of them has a field to land
// in. The decode is TWO-STAGE and an ALLOW-LIST at both stages: the outer
// struct names only type/id/timestamp and keeps `data` as raw bytes, and the
// data bytes are decoded ONLY for the two record types this reads, into structs
// carrying one string each. So `data.content` is never a value in this process
// for any record, and `data.name` is never one for any record that is not a
// skill invocation. A field this package has not been taught about contributes
// nothing, which is the shape that survives the vendor adding a key — the
// claude-code `contentBlock.Input` pattern.
//
// The cumulative counters are absent for the same reason:
// `session.usage_checkpoint` and `session.shutdown` carry the session's running
// `totalNanoAiu`, and summing them overstates by 6.6x, so this decoder has no
// field for either and cannot be misread into the ledger. See cost.go.
//
// NO COST IS ATTRIBUTED. A hook carries no usage of its own, and a skill
// invocation is one call in a file whose usage rows live on a different
// surface entirely — there is no identity linking an event id to an OTEL span.
// UsageDedupKey stays empty and these rows are reported as unattributed calls,
// which is what they are.
//
// CRITICAL: strictly read-only, O_RDONLY, whole-file re-read gated on
// size+mtime.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// The two record types this reads, and nothing else.
//
// hook.start rather than hook.end: the invocation is the fact, one row per
// firing, and a hook that never returned still fired (locally 21 starts against
// 18 ends). Counting both would double every completed hook.
const (
	typeSkillInvoked = "skill.invoked"
	typeHookStart    = "hook.start"
)

// sessionEventLine is stage one of the decode: the envelope, with `data` left
// as bytes. `parentId` and `agentId` are not named because nothing reads them,
// and every other top-level key the vendor may add is discarded here.
type sessionEventLine struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// skillInvokedData is stage two for a skill invocation: the NAME. The same
// record carries `content`, `path` and `description`; none of them has a field
// here, so encoding/json discards them as it parses.
type skillInvokedData struct {
	Name string `json:"name"`
}

// hookStartData is stage two for a hook firing: the EVENT it fired on
// ("preToolUse"). The same record carries `input`, which holds the tool calls
// and their arguments; it has no field here.
type hookStartData struct {
	HookType string `json:"hookType"`
}

// collectSessionEvents reads one session's events.jsonl and returns its skill
// and hook rows. The file is gated on size+mtime and otherwise re-read whole:
// the dedup keys are the provider's own event ids, so a re-read conflict-skips
// rather than recounting, and a key minted from a byte offset would recount the
// file every time it grew.
func collectSessionEvents(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	fi, err := os.Stat(src.Path)
	if err != nil {
		return adapter.Observation{}, nil // absent/unreadable: written lazily, so normal
	}
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()
	if cp != nil && cp.Size == size && cp.MTimeNS == mtimeNS {
		return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
	}
	newCp := &model.SourceCheckpoint{
		Tool: model.ToolCopilot, SourcePath: src.Path, Size: size, MTimeNS: mtimeNS,
	}

	session := sessionOf(src)
	fallbackTS := fi.ModTime().UTC()

	f, err := os.Open(src.Path) // O_RDONLY
	if err != nil {
		return adapter.Observation{}, nil
	}
	defer f.Close()

	var activity []model.ActivityEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // a record can hold a whole tool result
	for sc.Scan() {
		if ctx.Err() != nil {
			// Partial parse: no checkpoint, so the next cycle re-reads in full.
			return adapter.Observation{Activity: activity}, ctx.Err()
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec sessionEventLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // a malformed line is skipped, never fatal
		}
		if a, ok := toSessionActivity(rec, session, fallbackTS, src.Path); ok {
			activity = append(activity, a)
		}
	}
	if err := sc.Err(); err != nil {
		// The read did not complete: no checkpoint, so the next cycle retries
		// even though size+mtime have not moved.
		return adapter.Observation{}, fmt.Errorf("copilot: read %s: %w", src.Path, err)
	}

	return adapter.Observation{Activity: activity, Checkpoint: newCp}, nil
}

// toSessionActivity converts one envelope into a skill or hook row. Anything
// else — every message, every tool execution, every checkpoint — returns
// ok=false with its `data` bytes never decoded at all.
func toSessionActivity(rec sessionEventLine, session string, fallbackTS time.Time, path string) (model.ActivityEvent, bool) {
	id := strings.TrimSpace(rec.ID)
	if id == "" {
		// The event id is the whole dedup key. A key minted from the read
		// position instead would mint a fresh one on every poll of a file that
		// is re-read whole, i.e. an unbounded recount.
		return model.ActivityEvent{}, false
	}

	var kind model.ActivityKind
	var name string
	switch rec.Type {
	case typeSkillInvoked:
		var d skillInvokedData
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return model.ActivityEvent{}, false
		}
		kind, name = model.ActivitySkill, strings.TrimSpace(d.Name)
	case typeHookStart:
		var d hookStartData
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return model.ActivityEvent{}, false
		}
		kind, name = model.ActivityHook, strings.TrimSpace(d.HookType)
	default:
		return model.ActivityEvent{}, false
	}
	if name == "" {
		return model.ActivityEvent{}, false // an unnamed invocation records nothing
	}

	return model.ActivityEvent{
		Tool:      model.ToolCopilot,
		Kind:      kind,
		Name:      name,
		SessionID: session,
		// Model is left empty: this file names no model for a skill or a hook,
		// and the session's current model is a property of the session rather
		// than of the invocation.
		EventTime:   eventTimestamp(rec.Timestamp, fallbackTS),
		TurnSeq:     0,
		CallsInTurn: 1,
		SourcePath:  path,
		DedupKey:    model.ToolCopilot + "|" + string(kind) + "|" + session + ":" + id,
	}, true
}

// sessionOf resolves the session id: the directory name Discover recorded, else
// the parent directory of the file itself. The directory name IS the session
// UUID the OTEL export reports as gen_ai.conversation.id, so both ledgers place
// a session's calls under the same id.
func sessionOf(src adapter.Source) string {
	if src.Meta != nil {
		if s := strings.TrimSpace(src.Meta[metaSession]); s != "" {
			return s
		}
	}
	if dir := filepath.Base(filepath.Dir(src.Path)); dir != "." && dir != string(filepath.Separator) {
		return dir
	}
	return "unknown-session"
}

// eventTimestamp parses the record's own RFC3339 timestamp, falling back to the
// file mtime. The fallback is harmless here — unlike the usage path — because
// the dedup key is the provider's event id and never the time.
func eventTimestamp(raw string, fallback time.Time) time.Time {
	if s := strings.TrimSpace(raw); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
	}
	return fallback
}
