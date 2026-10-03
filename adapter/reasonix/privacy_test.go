package reasonix

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// sentinel is planted in every content-shaped field of the planted fixture. Any
// appearance of it downstream is a leak.
const sentinel = "S3CRET-DO-NOT-STORE"

// TestPlantedContentReachesNoEmittedField is the privacy proof, and it is a
// whole-struct sweep rather than a list of fields somebody has to remember to
// extend: it walks every string-valued field of model.UsageEvent by reflection,
// so a field added to the shared type is covered the day it appears.
//
// The fixture is a real record with content planted in twenty-odd keys a
// transcript-shaped surface might grow — prompts, results, reasoning content,
// cwd, project, session ids, tool inputs, api keys, host names, even a nested
// cost_quote. None of them is on the decode allow-list, so encoding/json throws
// each one away as it parses and the content never becomes a value in this
// process.
func TestPlantedContentReachesNoEmittedField(t *testing.T) {
	root, _ := newRoot(t, "planted-2026-08-16.jsonl")
	events := collectAll(t, root)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]

	// The record's own counters must still have survived: a privacy test that
	// passes because nothing was parsed proves nothing.
	if ev.InputTokens != 60 || ev.CacheReadTokens != 40 || ev.OutputTokens != 20 ||
		ev.ReasoningTokens != 6 || ev.TotalTokens != 120 {
		t.Fatalf("counters = in %d cache %d out %d reas %d total %d; want 60/40/20/6/120",
			ev.InputTokens, ev.CacheReadTokens, ev.OutputTokens, ev.ReasoningTokens, ev.TotalTokens)
	}

	v := reflect.ValueOf(ev)
	typ := v.Type()
	checked := 0
	for i := range typ.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.String {
			continue
		}
		checked++
		if strings.Contains(f.String(), sentinel) {
			t.Errorf("%s carries planted content: %q", typ.Field(i).Name, f.String())
		}
	}
	if checked < 8 {
		t.Fatalf("swept only %d string fields of model.UsageEvent; the sweep is broken, not the adapter", checked)
	}
}

// TestRawIsAnAllowListNotAStrippedLine: the audit payload is re-marshalled from
// the parsed allow-list, never built by removing known-bad keys from the source
// line. The difference shows up the day the format grows a content field —
// a strip leaks it, an allow-list ignores it.
func TestRawIsAnAllowListNotAStrippedLine(t *testing.T) {
	root, _ := newRoot(t, "planted-2026-08-16.jsonl")
	events := collectAll(t, root)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	raw := events[0].Raw
	if raw == "" {
		t.Fatal("no audit payload")
	}
	if strings.Contains(raw, sentinel) {
		t.Fatalf("audit payload carries planted content: %s", raw)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("audit payload is not JSON: %v", err)
	}
	allowed := map[string]bool{
		"ts": true, "model": true, "source": true,
		"prompt": true, "completion": true, "reasoning": true,
		"cache_hit": true, "cache_miss": true, "total": true,
		"requests": true, "turns": true,
		"usage_source": true, "cost_complete": true, "cost_estimated": true,
		"display_complete": true, "display_status": true, "incomplete_reason": true,
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("audit payload carries %q, which is not on the allow-list", k)
		}
	}
}

// TestDecodeIsAnAllowList proves the mechanism rather than one instance of it:
// the struct the line is decoded into — which is also the struct the audit
// payload is marshalled from — has no field of any composite kind, so a nested
// object of arguments, a message array or a prompt string has nowhere to land.
// A prefix match ("read anything starting with attribution...") would not pass
// this, which is the point.
func TestDecodeIsAnAllowList(t *testing.T) {
	typ := reflect.TypeOf(statsRecord{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		k := f.Type.Kind()
		if k == reflect.Pointer {
			k = f.Type.Elem().Kind()
		}
		switch k {
		case reflect.String, reflect.Int64, reflect.Bool:
		default:
			t.Errorf("statsRecord.%s is a %s; only scalars may be decoded from this surface", f.Name, f.Type)
		}
	}
}

// TestNoActivityOrTurnContextIsInvented: this surface records neither a tool
// call nor what a turn ran under, and an adapter that inferred either from
// adjacency would be making it up. Usage is the floor and this adapter stops
// there.
func TestNoActivityOrTurnContextIsInvented(t *testing.T) {
	root, path := newRoot(t, "planted-2026-08-16.jsonl")
	_ = root
	obs, err := New().(Adapter).CollectIncremental(
		context.Background(), adapter.Source{Tool: model.ToolReasonix, Path: path}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(obs.Activity) != 0 {
		t.Errorf("activity = %d rows; the stats ledger records no calls", len(obs.Activity))
	}
	if len(obs.TurnContexts) != 0 {
		t.Errorf("turn contexts = %d rows; the stats ledger records no attribution", len(obs.TurnContexts))
	}
	if len(obs.Snapshots) != 0 {
		t.Errorf("snapshots = %d; this is an event-level source", len(obs.Snapshots))
	}
	for i, ev := range obs.Events {
		if ev.SessionID != "" || ev.Project != "" {
			t.Errorf("event %d invented identity: session %q project %q", i, ev.SessionID, ev.Project)
		}
	}
}

// TestPlantedFixtureActuallyCarriesTheSecret guards the guard. A fixture that
// quietly lost its planted content would make every test above pass for the
// wrong reason.
func TestPlantedFixtureActuallyCarriesTheSecret(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "planted-2026-08-16.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if n := strings.Count(string(body), sentinel); n < 20 {
		t.Fatalf("fixture plants the sentinel %d times, want at least 20", n)
	}
	var line map[string]json.RawMessage
	if err := json.Unmarshal(body, &line); err != nil {
		t.Fatalf("fixture is not one JSON object: %v", err)
	}
	for _, k := range []string{"prompt_text", "messages", "cwd", "session_id", "tool_input", "api_key"} {
		if _, ok := line[k]; !ok {
			t.Errorf("fixture no longer plants %q", k)
		}
	}
}

// TestModelIsNotTreatedAsContent: the one string that does pass through is the
// model ref, and it must, because it is the pricing key. Stated as a test so a
// future over-zealous scrub cannot quietly drop it.
func TestModelIsNotTreatedAsContent(t *testing.T) {
	root, _ := newRoot(t, "planted-2026-08-16.jsonl")
	events := collectAll(t, root)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Model != "deepseek/deepseek-chat" || events[0].Provider != "deepseek" {
		t.Errorf("model/provider = %q/%q, want deepseek/deepseek-chat and deepseek",
			events[0].Model, events[0].Provider)
	}
	if events[0].Kind != model.KindUsage {
		t.Errorf("kind = %q, want %q", events[0].Kind, model.KindUsage)
	}
}
