package qwencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// The planted fixture is a pair of real-shaped ledger records with a marker
// buried in every content-shaped field the harness could ever grow: a response
// excerpt, a prompt, a message list, tool arguments, a result, an error, a
// working directory, a git branch, a credential, a subagent's task. None of
// them exists in the surface today — which is the point. The adapter decodes
// into a typed struct, so a field it was never taught about is discarded by
// encoding/json as the line is parsed, and the day the harness adds one of these
// nothing changes here.
const leakMarker = "QWENLEAK-"

// TestPlantedContentReachesNoEmittedField walks every string field of every
// emitted record — usage events, turn contexts, activity, the audit payload,
// dedup keys, source paths — and fails on the marker anywhere in any of them.
func TestPlantedContentReachesNoEmittedField(t *testing.T) {
	root := filepath.Join("testdata", "planted")
	obs, err := collectRoot(t, root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(obs.Events) != 2 {
		t.Fatalf("got %d events, want 2: the planted content must not stop the record parsing", len(obs.Events))
	}
	if len(obs.TurnContexts) != 1 {
		t.Fatalf("got %d turn contexts, want 1", len(obs.TurnContexts))
	}

	for i, e := range obs.Events {
		assertNoLeak(t, "event["+string(rune('0'+i))+"]", reflect.ValueOf(e))
	}
	for i, c := range obs.TurnContexts {
		assertNoLeak(t, "context["+string(rune('0'+i))+"]", reflect.ValueOf(c))
	}
	for i, a := range obs.Activity {
		assertNoLeak(t, "activity["+string(rune('0'+i))+"]", reflect.ValueOf(a))
	}
}

// assertNoLeak fails when any string field of v (recursing into structs)
// contains the marker.
func assertNoLeak(t *testing.T, label string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), leakMarker) {
			t.Errorf("%s leaked planted content: %q", label, v.String())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			assertNoLeak(t, label+"."+v.Type().Field(i).Name, v.Field(i))
		}
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			assertNoLeak(t, label, v.Elem())
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			assertNoLeak(t, label, v.Index(i))
		}
	}
}

// TestRawIsUsageObjectOnly pins the audit payload's EXACT key set. A field added
// to auditRecord fails here rather than quietly widening what the ledger stores,
// and the planted record proves the payload is rebuilt from the typed decode
// rather than sliced out of the source line.
func TestRawIsUsageObjectOnly(t *testing.T) {
	root := filepath.Join("testdata", "planted")
	obs, err := collectRoot(t, root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	raw := obs.Events[0].Raw
	if raw == "" {
		t.Fatal("Raw is empty; the audit payload should still be stored by default")
	}

	line := firstLine(t, filepath.Join(root, "usage", "token-usage-2026-08.jsonl"))
	if len(raw) >= len(line) {
		t.Errorf("Raw (%d bytes) is not smaller than the source line (%d bytes): the payload "+
			"is being echoed rather than rebuilt", len(raw), len(line))
	}

	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("Raw is not valid JSON: %v\n%s", err, raw)
	}
	assertKeys(t, "raw", top,
		"schemaVersion", "id", "timestamp", "localDate", "localMonth", "sessionId",
		"model", "authType", "source",
		"inputTokens", "outputTokens", "cachedTokens", "thoughtsTokens", "totalTokens",
		"apiDurationMs")

	// The counters are the PROVIDER's, not the mapped values in the token
	// columns, so a disagreement between the two stays visible to an auditor.
	for key, want := range map[string]float64{
		"inputTokens": 100, "outputTokens": 10, "cachedTokens": 0,
		"thoughtsTokens": 0, "totalTokens": 110, "apiDurationMs": 900,
	} {
		if got, ok := top[key].(float64); !ok || got != want {
			t.Errorf("raw.%s = %v, want %v", key, top[key], want)
		}
	}
}

// TestAuditPayloadDropsAnUnknownField is the allow-list statement in its
// sharpest form: the second planted record's extra keys never reach the payload,
// so its raw is byte-identical to the payload built from the same record with
// those keys removed.
func TestAuditPayloadDropsAnUnknownField(t *testing.T) {
	clean := record{
		SchemaVersion: 1,
		ID:            "p-2",
		Timestamp:     "2026-08-16T04:05:34.251Z",
		LocalDate:     "2026-08-16",
		LocalMonth:    "2026-08",
		SessionID:     "s-planted",
		Model:         "qwen3-coder-plus",
		AuthType:      "openai",
		Source:        "managed-auto-memory-extractor",
		InputTokens:   50,
		OutputTokens:  5,
		CachedTokens:  10,
		ThoughtsToken: 7,
		TotalTokens:   62,
		APIDurationMs: 400,
	}
	obs, err := collectRoot(t, filepath.Join("testdata", "planted"))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := obs.Events[1].Raw; got != clean.auditPayload() {
		t.Errorf("audit payload for the record carrying extra keys:\n got %s\nwant %s",
			got, clean.auditPayload())
	}
}

// TestDedupKeyIsIndependentOfRaw pins the key to the bytes it has always been
// derived from — the provider's own id — so changing what the audit payload
// stores can never re-ingest a user's history as duplicates.
func TestDedupKeyIsIndependentOfRaw(t *testing.T) {
	obs, err := collectRoot(t, filepath.Join("testdata", "planted"))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, e := range obs.Events {
		if want := model.ToolQwenCode + "|" + e.MessageID; e.DedupKey != want {
			t.Errorf("DedupKey = %q, want %q", e.DedupKey, want)
		}
		if e.Raw != "" && strings.Contains(e.DedupKey, e.Raw) {
			t.Errorf("dedup key embeds the audit payload: %q", e.DedupKey)
		}
	}
}

// TestDecodeIsAnAllowList reads this package's own record type and fails when it
// grows a field that is not one of the ledger's known counter/identity keys.
// The decode is what makes the privacy guarantee structural: a map decode, or a
// new field named after something the harness might one day put content in,
// would make every other test here pass while the ledger started storing prose.
func TestDecodeIsAnAllowList(t *testing.T) {
	allowed := map[string]bool{
		"schemaVersion": true, "id": true, "timestamp": true,
		"localDate": true, "localMonth": true, "sessionId": true,
		"model": true, "authType": true, "source": true,
		"inputTokens": true, "outputTokens": true, "cachedTokens": true,
		"thoughtsTokens": true, "totalTokens": true, "apiDurationMs": true,
	}
	rt := reflect.TypeOf(record{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if !allowed[tag] {
			t.Errorf("record.%s decodes %q, which is not a known counter or identity field: "+
				"teach this test about it on purpose, or do not read it", f.Name, tag)
		}
		switch f.Type.Kind() {
		case reflect.String, reflect.Int, reflect.Int64:
		default:
			t.Errorf("record.%s is a %s: only scalars may be decoded here, since a nested "+
				"object is where arguments and prompts arrive", f.Name, f.Type.Kind())
		}
	}
}

// firstLine returns the file's first line.
func firstLine(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitN(string(b), "\n", 2)[0]
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
