package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// secret is planted in EVERY field of the session format that can hold content.
// A single occurrence anywhere in the adapter's output is a leak.
const secret = "CANARY-9c1f2d7e-do-not-emit"

// contentFixture is the real record layout with the secret in EVERY place the
// harness can put content, enumerated from pi-ai's own types rather than from
// what the fixtures on disk happen to contain: prompts and responses in both
// content shapes (array and bare string), thinking blocks and their signatures,
// image blocks (base64 `data`), tool arguments and thought signatures, tool
// results with their details and added tool names, a `bashExecution` message's
// command and whole output, an assistant message's `diagnostics` (whose
// `error.stack` carries file paths and argument values), `errorMessage`,
// `rawStopReason`, compaction and branch summaries, extension custom entries and
// custom messages, a label, a session name, and the header's `parentSession`
// path. The counters and the identity fields are the only things the adapter is
// allowed to keep.
func contentFixture() []string {
	s := secret
	return []string{
		`{"type":"session","version":3,"id":"SESS-1","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/proj","parentSession":"/somewhere/` + s + `.jsonl"}`,
		`{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-08-16T00:00:00.100Z","provider":"anthropic","modelId":"claude-x"}`,
		`{"type":"thinking_level_change","id":"m2","parentId":"m1","timestamp":"2026-08-16T00:00:00.100Z","thinkingLevel":"off"}`,
		`{"type":"message","id":"u1","parentId":"m2","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"` + s + `"}],"timestamp":1}}`,
		// OpenClaw writes a user message's content as a plain string.
		`{"type":"message","id":"u2","parentId":"u1","timestamp":"2026-08-16T00:00:01.500Z","message":{"role":"user","content":"` + s + `","timestamp":1}}`,
		// The `!` command records a shell run as its own message role, with the
		// command line and its whole output as plain top-level strings.
		`{"type":"message","id":"sh1","parentId":"u2","timestamp":"2026-08-16T00:00:01.700Z","message":{"role":"bashExecution","command":"` + s + `",` +
			`"output":"` + s + `","exitCode":0,"cancelled":false,"truncated":false,"fullOutputPath":"/x/` + s + `","timestamp":1}}`,
		`{"type":"message","id":"a1","parentId":"sh1","timestamp":"2026-08-16T00:00:02.000Z","message":{"role":"assistant",` +
			`"content":[{"type":"thinking","thinking":"` + s + `","thinkingSignature":"` + s + `","redacted":true},` +
			`{"type":"text","text":"` + s + `","textSignature":"` + s + `"},` +
			`{"type":"image","data":"` + s + `","mimeType":"image/png"},` +
			`{"type":"toolCall","id":"call_1","name":"exec","namespace":null,"thoughtSignature":"` + s + `",` +
			`"arguments":{"command":"` + s + `","cwd":"/x/` + s + `","body":{"nested":["` + s + `"]}}}],` +
			`"api":"anthropic-messages","provider":"anthropic","model":"claude-x","responseModel":"claude-x-2","responseId":"req_1","stopReason":"toolUse",` +
			`"diagnostics":[{"type":"stream-error","timestamp":1,"error":{"name":"Error","message":"` + s + `","stack":"` + s + `","code":"` + s + `"},` +
			`"details":{"request":"` + s + `"}}],` +
			`"usage":{"input":100,"output":20,"cacheRead":5,"cacheWrite":3,"reasoning":4,"totalTokens":128,"cost":{"input":0.001,"output":0.002,"cacheRead":0,"cacheWrite":0,"total":0.003}},` +
			`"errorMessage":"` + s + `","rawStopReason":"` + s + `"}}`,
		// A toolResult carries its own optional usage object in the same
		// `.message.usage` position an assistant turn's does. It is content AND a
		// counting trap: it must reach neither the output nor the ledger.
		`{"type":"message","id":"t1","parentId":"a1","timestamp":"2026-08-16T00:00:03.000Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"exec",` +
			`"content":[{"type":"text","text":"` + s + `"},{"type":"image","data":"` + s + `","mimeType":"image/png"}],` +
			`"details":{"aggregated":"` + s + `","cwd":"/x/` + s + `"},"addedToolNames":["` + s + `"],` +
			`"usage":{"input":7777,"output":7777,"cacheRead":0,"cacheWrite":0,"totalTokens":15554,"cost":{"total":9.5}},` +
			`"isError":false,"timestamp":1}}`,
		`{"type":"compaction","id":"c1","parentId":"t1","timestamp":"2026-08-16T00:00:04.000Z","summary":"` + s + `","firstKeptEntryId":"u1","tokensBefore":9999,` +
			`"details":{"notes":"` + s + `"},"usage":{"input":9,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":10,"cost":{"total":0}}}`,
		`{"type":"branch_summary","id":"b1","parentId":"c1","timestamp":"2026-08-16T00:00:05.000Z","fromId":"u1","summary":"` + s + `",` +
			`"usage":{"input":2,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":3,"cost":{"total":0}}}`,
		`{"type":"custom_message","id":"x1","parentId":"b1","timestamp":"2026-08-16T00:00:06.000Z","customType":"ext","content":"` + s + `","display":true}`,
		// The same entry with content as an ARRAY of blocks: both shapes are legal
		// (`string | (TextContent | ImageContent)[]`).
		`{"type":"custom_message","id":"x1b","parentId":"x1","timestamp":"2026-08-16T00:00:06.500Z","customType":"ext",` +
			`"content":[{"type":"text","text":"` + s + `"},{"type":"image","data":"` + s + `","mimeType":"image/png"}],` +
			`"details":{"note":"` + s + `"},"display":true}`,
		`{"type":"custom","id":"x2","parentId":"x1","timestamp":"2026-08-16T00:00:07.000Z","customType":"ext","data":{"blob":"` + s + `"}}`,
		`{"type":"label","id":"l1","parentId":"x2","timestamp":"2026-08-16T00:00:08.000Z","targetId":"a1","label":"` + s + `"}`,
		`{"type":"session_info","id":"n1","parentId":"l1","timestamp":"2026-08-16T00:00:09.000Z","name":"` + s + `"}`,
	}
}

// TestNoContentReachesAnyEmittedField plants the canary in every content field
// of a full-shape session and asserts it reaches nothing the adapter produces —
// not an event, not an activity row, not the audit payload, not the checkpoint.
//
// The guarantee is structural rather than a filter: the decode structs in
// json.go name only ids, timestamps, names and counters, so encoding/json
// discards a prompt, a tool argument and a summary as it parses. This test is
// what keeps that true when someone adds a field.
func TestNoContentReachesAnyEmittedField(t *testing.T) {
	for _, a := range []adapter.Adapter{NewPi(), NewOpenClaw()} {
		t.Run(a.ID(), func(t *testing.T) {
			dir := t.TempDir()
			writeSession(t, dir, "s.jsonl", contentFixture())
			path := filepath.Join(dir, "s.jsonl")

			// The canary really is in the source, or the test proves nothing.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// One count per content-bearing field of the real surface. The
			// number is a floor, not a target: it fails when a field is dropped
			// from the fixture, which is how the sweep stays a proof rather than
			// a sample.
			if n := strings.Count(string(raw), secret); n < 34 {
				t.Fatalf("fixture holds the canary %d times, want the full content surface", n)
			}

			obs, err := a.Collect(context.Background(), adapter.Source{
				Tool: a.ID(), Class: model.EventLevel, Path: path,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(obs.Events) != 3 {
				t.Fatalf("events = %d, want 3 (assistant + compaction + branch_summary)", len(obs.Events))
			}
			if len(obs.Activity) != 1 {
				t.Fatalf("activity = %d, want 1", len(obs.Activity))
			}

			// Whole-output sweep: marshal everything the adapter emits, including
			// the fields that are excluded from the export shape, and search it.
			for _, blob := range emitted(t, obs) {
				if strings.Contains(blob, secret) {
					t.Errorf("canary reached an emitted value: %s", blob)
				}
			}

			// The facts that must survive, so the sweep is not passing by
			// emitting nothing at all.
			ev := obs.Events[0]
			if ev.Model != "claude-x" || ev.Provider != "anthropic" || ev.MessageID != "req_1" {
				t.Errorf("identity fields lost: %+v", ev)
			}
			if ev.InputTokens != 100 || ev.OutputTokens != 20 || ev.CacheReadTokens != 5 ||
				ev.CacheCreationTokens != 3 || ev.ReasoningTokens != 4 || ev.TotalTokens != 128 {
				t.Errorf("counters lost: %+v", ev)
			}
			if obs.Activity[0].Name != "exec" {
				t.Errorf("tool name lost: %q", obs.Activity[0].Name)
			}
		})
	}
}

// TestRawIsAnAllowListNotAStrippedRecord: the audit payload is BUILT from named
// fields, so a key the harness starts writing tomorrow cannot ride into the
// ledger. A raw built by removing known-bad keys would carry it on day one.
func TestRawIsAnAllowListNotAStrippedRecord(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl", []string{
		`{"type":"session","id":"S","timestamp":"2026-08-16T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","timestamp":"2026-08-16T00:00:01.000Z","message":{"role":"assistant","provider":"p","model":"m",` +
			`"futureFieldTheHarnessAdded":"` + secret + `","content":[{"type":"text","text":"` + secret + `"}],` +
			`"usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"unknownCounter":42,"cost":{"total":0}}}}`,
	})
	obs := collectFile(t, NewPi(), filepath.Join(dir, "s.jsonl"))
	if len(obs.Events) != 1 {
		t.Fatalf("events = %d", len(obs.Events))
	}
	raw := obs.Events[0].Raw
	if strings.Contains(raw, secret) || strings.Contains(raw, "futureField") || strings.Contains(raw, "unknownCounter") {
		t.Fatalf("raw is not an allow-list: %s", raw)
	}
	// It is still a usable audit payload.
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("raw is not JSON: %v", err)
	}
	for _, k := range []string{"entry", "type", "model", "provider", "input", "output", "totalTokens"} {
		if _, ok := got[k]; !ok {
			t.Errorf("raw lost the %q field: %s", k, raw)
		}
	}
}

// emitted renders everything an observation carries into searchable strings,
// including the fields tagged json:"-" on the exported shapes.
func emitted(t *testing.T, obs adapter.Observation) []string {
	t.Helper()
	var out []string
	for _, e := range obs.Events {
		out = append(out, fmt.Sprintf("%+v", e), e.Raw, e.DedupKey, e.PriceSource)
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	for _, c := range obs.Activity {
		out = append(out, fmt.Sprintf("%+v", c))
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	for _, c := range obs.TurnContexts {
		out = append(out, fmt.Sprintf("%+v", c))
	}
	for _, s := range obs.Snapshots {
		out = append(out, fmt.Sprintf("%+v", s), s.Raw)
	}
	if obs.Checkpoint != nil {
		out = append(out, fmt.Sprintf("%+v", *obs.Checkpoint), obs.Checkpoint.State)
	}
	return out
}
