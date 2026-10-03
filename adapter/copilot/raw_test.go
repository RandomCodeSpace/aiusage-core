package copilot

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// contentfulSpan is a realistic Copilot OTEL chat span emitted with message
// content capture switched on: the GenAI semantic-convention content keys
// (gen_ai.prompt / gen_ai.completion / gen_ai.input.messages /
// gen_ai.output.messages / gen_ai.system_instructions), a plausible
// vendor-specific one (copilot_chat.request.messages), and request metadata
// that is not usage either. The counters and identifiers alongside them are
// exactly what raw IS allowed to keep.
const contentfulSpan = `{"type":"span","traceId":"trace-leak","spanId":"span-leak",` +
	`"name":"chat claude-sonnet-4","endTime":[1775934264,967317833],"attributes":{` +
	`"gen_ai.operation.name":"chat",` +
	`"gen_ai.system":"github_copilot",` +
	`"gen_ai.provider.name":"anthropic",` +
	`"gen_ai.request.model":"claude-sonnet-4",` +
	`"gen_ai.response.model":"claude-sonnet-4",` +
	`"gen_ai.response.id":"resp-leak",` +
	`"gen_ai.conversation.id":"conv-leak",` +
	`"gen_ai.usage.input_tokens":19452,` +
	`"gen_ai.usage.output_tokens":281,` +
	`"gen_ai.usage.cache_read.input_tokens":123,` +
	`"gen_ai.usage.cache_creation.input_tokens":25,` +
	`"gen_ai.usage.reasoning.output_tokens":128,` +
	`"gen_ai.usage.total_tokens":19886,` +
	`"gen_ai.prompt":"LEAK-prompt rotate the production key hunter2",` +
	`"gen_ai.completion":"LEAK-completion here is the acquisition memo",` +
	`"gen_ai.input.messages":"[{\"role\":\"user\",\"content\":\"LEAK-input merge the cap table\"}]",` +
	`"gen_ai.output.messages":"[{\"role\":\"model\",\"content\":\"LEAK-output done\"}]",` +
	`"gen_ai.system_instructions":"LEAK-sysprompt internal codename bluebird",` +
	`"copilot_chat.request.messages":"[{\"text\":\"LEAK-vendor the passphrase is hunter2\"}]",` +
	`"url.full":"https://api.githubcopilot.com/LEAK-urlpath",` +
	`"user.email":"LEAK-email@example.com"}}`

// leakMarkers appear ONLY inside the parts of contentfulSpan that raw must
// never keep. Any of them surfacing in UsageEvent.Raw is a privacy leak.
var leakMarkers = []string{
	"LEAK-prompt", "LEAK-completion", "LEAK-input", "LEAK-output",
	"LEAK-sysprompt", "LEAK-vendor", "LEAK-urlpath", "LEAK-email",
	"hunter2", "bluebird", "cap table", "githubcopilot.com",
	"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages",
	"gen_ai.output.messages", "gen_ai.system_instructions",
	"copilot_chat.request.messages", "url.full", "user.email",
}

// TestRawIsUsageObjectOnly is the allow-list regression for issue #42: raw
// must hold span identity, timing and the allow-listed usage/identity
// attributes, and nothing else. It asserts the exact key set at every level,
// so a future attribute added to the payload fails here rather than quietly
// widening what the append-only ledger stores.
func TestRawIsUsageObjectOnly(t *testing.T) {
	home := writeOTEL(t, contentfulSpan)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(evs), evs)
	}
	raw := evs[0].Raw
	if raw == "" {
		t.Fatal("Raw is empty; the audit payload should still be stored by default")
	}

	for _, marker := range leakMarkers {
		if strings.Contains(raw, marker) {
			t.Errorf("Raw leaked %q:\n%s", marker, raw)
		}
	}
	if len(raw) >= len(contentfulSpan) {
		t.Errorf("Raw (%d bytes) is not smaller than the OTEL line (%d bytes)", len(raw), len(contentfulSpan))
	}

	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("Raw is not valid JSON: %v\n%s", err, raw)
	}
	assertKeys(t, "raw", top, "traceId", "spanId", "timestamp", "attributes")
	if top["traceId"] != "trace-leak" || top["spanId"] != "span-leak" {
		t.Errorf("span identity = %v/%v", top["traceId"], top["spanId"])
	}
	if got := top["timestamp"]; got != "2026-04-11T19:04:24.967Z" {
		t.Errorf("timestamp = %v", got)
	}

	attrs, ok := top["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes is not an object: %v", top["attributes"])
	}
	assertKeys(t, "raw.attributes", attrs,
		"gen_ai.operation.name", "gen_ai.system", "gen_ai.provider.name",
		"gen_ai.request.model", "gen_ai.response.model",
		"gen_ai.response.id", "gen_ai.conversation.id",
		"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens",
		"gen_ai.usage.cache_read.input_tokens",
		"gen_ai.usage.cache_creation.input_tokens",
		"gen_ai.usage.reasoning.output_tokens", "gen_ai.usage.total_tokens")

	// Counters are stored as the exporter reported them, not as the clamped
	// values in the token columns, so a mismatch between the two stays visible.
	for key, want := range map[string]float64{
		"gen_ai.usage.input_tokens": 19452, "gen_ai.usage.output_tokens": 281,
		"gen_ai.usage.cache_read.input_tokens": 123, "gen_ai.usage.total_tokens": 19886,
	} {
		if got, okNum := attrs[key].(float64); !okNum || got != want {
			t.Errorf("attributes[%q] = %v, want %v", key, attrs[key], want)
		}
	}
	if attrs["gen_ai.response.model"] != "claude-sonnet-4" {
		t.Errorf("model attribute = %v", attrs["gen_ai.response.model"])
	}
}

// wantAllowList pins the exact attribute allow-list. It is written out here
// rather than derived from rawAttrKeys so widening the adapter's list without
// a deliberate decision fails this test.
var wantAllowList = []string{
	"gen_ai.usage.input_tokens",
	"gen_ai.usage.output_tokens",
	"gen_ai.usage.cache_read.input_tokens",
	"gen_ai.usage.cache_write.input_tokens",
	"gen_ai.usage.cache_creation.input_tokens",
	"gen_ai.usage.reasoning.output_tokens",
	"gen_ai.usage.reasoning_tokens",
	"gen_ai.usage.total_tokens",
	"gen_ai.usage.total.token_count",
	"github.copilot.nano_aiu",
	"gen_ai.request.model",
	"gen_ai.response.model",
	"gen_ai.system",
	"gen_ai.provider.name",
	"gen_ai.operation.name",
	"event.name",
	"gen_ai.response.id",
	"gen_ai.conversation.id",
	"copilot_chat.session_id",
	"copilot_chat.chat_session_id",
	"session.id",
	"github.copilot.interaction_id",
	"turn.index",
	"copilot_chat.turn.index",
}

// TestRawRetainsExactlyTheAllowList drives a span carrying every allow-listed
// attribute plus a content attribute, and asserts the retained key set is
// exactly the allow-list: nothing listed is dropped, nothing unlisted is kept.
func TestRawRetainsExactlyTheAllowList(t *testing.T) {
	assertKeys(t, "rawAttrKeys", keySet(rawAttrKeys), wantAllowList...)

	var b strings.Builder
	b.WriteString(`{"type":"span","traceId":"trace-full","spanId":"span-full",`)
	b.WriteString(`"name":"chat m","endTime":[1775934264,0],"attributes":{`)
	for _, k := range wantAllowList {
		b.WriteString(`"` + k + `":`)
		switch {
		case strings.Contains(k, "tokens"), strings.Contains(k, "token_count"), strings.HasSuffix(k, "turn.index"), k == "turn.index":
			b.WriteString("11,")
		case k == "gen_ai.operation.name":
			b.WriteString(`"chat",`)
		default:
			b.WriteString(`"v-` + k + `",`)
		}
	}
	b.WriteString(`"gen_ai.prompt":"LEAK-prompt hunter2"}}`)

	home := writeOTEL(t, b.String())
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if strings.Contains(evs[0].Raw, "LEAK-prompt") || strings.Contains(evs[0].Raw, "hunter2") {
		t.Errorf("Raw leaked message content:\n%s", evs[0].Raw)
	}

	var top map[string]any
	if err := json.Unmarshal([]byte(evs[0].Raw), &top); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	attrs, ok := top["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes is not an object: %v", top["attributes"])
	}
	assertKeys(t, "raw.attributes", attrs, wantAllowList...)
}

// TestRawOmitsMtimeTimestamp: a record with no timestamp of its own must not
// present the file mtime as if the record carried it. The mtime is a property
// of the poll, and it moves on every append.
func TestRawOmitsMtimeTimestamp(t *testing.T) {
	rec := `{"_body":"GenAI inference: m","attributes":{` +
		`"event.name":"gen_ai.client.inference.operation.details",` +
		`"gen_ai.response.model":"m","gen_ai.conversation.id":"c",` +
		`"gen_ai.usage.input_tokens":10,"gen_ai.usage.output_tokens":5}}`
	home := writeOTEL(t, rec)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(evs[0].Raw), &top); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	assertKeys(t, "raw", top, "attributes")
	attrs, ok := top["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes is not an object: %v", top["attributes"])
	}
	assertKeys(t, "raw.attributes", attrs,
		"event.name", "gen_ai.response.model", "gen_ai.conversation.id",
		"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens")
}

// TestDedupKeyAndTotalsIndependentOfRaw pins the bytes the dedup key and the
// token totals are derived from. Neither reads the audit payload, so changing
// what raw stores can never re-ingest a user's history as duplicates.
func TestDedupKeyAndTotalsIndependentOfRaw(t *testing.T) {
	home := writeOTEL(t, contentfulSpan)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	e := evs[0]
	if e.DedupKey != "copilot|trace-leak:span-leak" {
		t.Errorf("DedupKey = %q, want copilot|trace-leak:span-leak", e.DedupKey)
	}
	if e.Raw != "" && strings.Contains(e.DedupKey, e.Raw) {
		t.Errorf("dedup key embeds the audit payload: %q", e.DedupKey)
	}
	// input 19452 - cache_read 123 = 19329; the exporter total is authoritative.
	if e.InputTokens != 19329 || e.OutputTokens != 281 || e.CacheReadTokens != 123 ||
		e.CacheCreationTokens != 25 || e.ReasoningTokens != 128 || e.TotalTokens != 19886 {
		t.Errorf("token map changed: in %d out %d cacheR %d cacheC %d reasoning %d total %d",
			e.InputTokens, e.OutputTokens, e.CacheReadTokens,
			e.CacheCreationTokens, e.ReasoningTokens, e.TotalTokens)
	}
	if e.SessionID != "conv-leak" || e.RequestID != "resp-leak" || e.Model != "claude-sonnet-4" {
		t.Errorf("identity fields changed: session %q request %q model %q",
			e.SessionID, e.RequestID, e.Model)
	}
}

// keySet turns a key slice into the map shape assertKeys compares.
func keySet(keys []string) map[string]any {
	m := make(map[string]any, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

// assertKeys fails unless m holds exactly the named keys.
func assertKeys(t *testing.T, label string, m map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Errorf("%s keys = [%s], want exactly [%s]", label, strings.Join(got, ","), strings.Join(sorted, ","))
	}
}
