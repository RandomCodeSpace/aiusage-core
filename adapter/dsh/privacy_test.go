package dsh

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
)

// secret is planted in EVERY content-bearing field of the transcript below. It
// must not appear in any field of any emitted record, in the audit payload, or
// on the Source itself.
const secret = "SUPERSECRET-PROMPT-abc123"

// secretTranscript is a structurally faithful DSH session with the secret in
// every place content can live: the user's prompt, the assistant's text, the
// tool call's arguments (twice — the streamed delta and the durable record),
// the tool result and its private meta, the request header's system prompt and
// tool schemas, the session title and the title-LLM request, and the packed
// chunk rows.
//
// The session id, cwd, model and provider deliberately do NOT carry it: those
// are identities the ledger is supposed to store, and hiding them in the same
// assertion would make the test pass for the wrong reason.
func secretTranscript() []string {
	s := secret
	return []string{
		`{"type":"session","version":0,"id":"session-p","createdAt":1000,"cwd":"/work","delegationDepth":0,"agentPreset":"reviewer"}`,
		`{"type":"agent/inbox/spliced","seq":0,"time":1010,"data":{"target":"next-turn","start":0,"inserted":[{"content":[{"type":"text","text":"` + s + `"}],"source":{"kind":"user"},"role":"user","id":"u1"}]}}`,
		`{"type":"user/message","seq":1,"time":1020,"data":{"content":[{"type":"text","text":"` + s + `"}],"source":{"kind":"plugin","plugin":"p","form":"snapshot","sections":[{"name":"n","text":"` + s + `"}]},"role":"user","id":"u2"},"surfaceOp":"append"}`,
		`{"type":"session/title","seq":2,"time":1030,"data":{"title":"` + s + `","messageSeqs":[1],"source":{"kind":"fallback"}}}`,
		`{"type":"request/header","seq":3,"time":1040,"data":{"header":{"config":{"provider":"prov","model":"mdl","maxTokens":4096},"system":"` + s + `","tools":[{"name":"bash","description":"` + s + `","parameters":{"cmd":"` + s + `"}}]},"reason":"initial"}}`,
		`{"type":"request/context","seq":4,"time":1050,"data":{"provider":"prov","model":"mdl","contextWindow":1024}}`,
		`{"type":"session/title-llm-request","seq":5,"time":1060,"data":{"titleProvider":"t","messageSeqs":[1],"route":{"provider":"prov","model":"mdl"},"system":"` + s + `","messages":[{"content":[{"type":"text","text":"` + s + `"}],"role":"user","id":"t1"}],"maxTokens":64}}`,
		`{"type":"assistant/chunk","seq":6,"time":1070,"data":{"turn":1,"step":1,"chunk":{"type":"tool-call-delta","index":0,"id":"c1","name":"bash","argumentsDelta":"{\"command\":\"` + s + `\"}"}}}`,
		`{"type":"text-chunks","seq0":7,"time0":1080,"data":{"turn":1,"step":1,"index":0,"dt":[0,1],"texts":["` + s + `","` + s + `"]}}`,
		`{"type":"assistant/chunk","seq":9,"time":1090,"data":{"turn":1,"step":1,"chunk":{"type":"block-end","index":0,"block":{"type":"text","text":"` + s + `"}}}}`,
		`{"type":"assistant/chunk","seq":10,"time":1095,"data":{"turn":1,"step":1,"chunk":{"type":"usage","usage":{"inputTokens":10,"outputTokens":2}}}}`,
		`{"type":"assistant/message","seq":11,"time":1100,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"text","text":"` + s + `"},{"type":"tool-call","id":"c1","name":"bash","arguments":"{\"command\":\"` + s + `\"}"}],"source":{"kind":"model","provider":"prov","model":"mdl","replayState":{"kind":"pi-ai","responseModel":"mdl-served","responseId":"resp-1","prompt":"` + s + `"}},"id":"msg-1"},"usage":{"inputTokens":10,"outputTokens":2}},"sourceEventSeqs":[6,7,8,9,10],"surfaceOp":"append"}`,
		`{"type":"tool/call","seq":12,"time":1110,"data":{"turn":1,"step":1,"callId":"c1","name":"bash","arguments":"{\"command\":\"` + s + `\",\"description\":\"` + s + `\"}"}}`,
		`{"type":"tool/result","seq":13,"time":1120,"data":{"turn":1,"step":1,"message":{"source":{"kind":"tool","callId":"c1"},"content":[{"type":"tool-result","toolCallId":"c1","content":[{"type":"text","text":"` + s + `"}],"isError":false}],"role":"user","id":"r1"},"meta":{"shape":"paths","paths":["` + s + `"]}},"sourceEventSeqs":[12],"surfaceOp":"append"}`,
		`{"type":"todo/write","seq":14,"time":1130,"data":{"todos":[{"content":"` + s + `","status":"pending"}]}}`,
		`{"type":"turn/end","seq":15,"time":1140,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	}
}

// TestNoContentReachesAnyEmittedField plants the secret everywhere content can
// live and walks EVERY field of every emitted record — including Raw and the
// Source's own strings — asserting none of them carries it.
//
// The walk is reflective on purpose: an assertion listing today's fields would
// stop covering the type the day it grows one.
func TestNoContentReachesAnyEmittedField(t *testing.T) {
	home := t.TempDir()
	plantSession(t, home, "--work--", "session-p", secretTranscript())

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("sources = %d, want 1", len(srcs))
	}
	scanForSecret(t, "Source", reflect.ValueOf(srcs[0]))

	obs, err := a.Collect(context.Background(), srcs[0])
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// The transcript must actually have produced something, or the scan proves
	// nothing at all.
	if len(obs.Events) != 1 || len(obs.Activity) != 1 || len(obs.TurnContexts) != 1 {
		t.Fatalf("want 1 event / 1 activity / 1 context, got %d / %d / %d",
			len(obs.Events), len(obs.Activity), len(obs.TurnContexts))
	}
	if obs.Events[0].Raw == "" {
		t.Fatal("no audit payload was built; the raw allow-list is untested")
	}

	for i, e := range obs.Events {
		scanForSecret(t, fmt.Sprintf("Events[%d]", i), reflect.ValueOf(e))
	}
	for i, e := range obs.Activity {
		scanForSecret(t, fmt.Sprintf("Activity[%d]", i), reflect.ValueOf(e))
	}
	for i, e := range obs.TurnContexts {
		scanForSecret(t, fmt.Sprintf("TurnContexts[%d]", i), reflect.ValueOf(e))
	}
	if obs.Checkpoint != nil {
		scanForSecret(t, "Checkpoint", reflect.ValueOf(*obs.Checkpoint))
	}
}

// scanForSecret walks every string reachable from v and fails on the secret.
func scanForSecret(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), secret) {
			t.Errorf("%s carries planted content: %q", path, v.String())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			scanForSecret(t, path+"."+v.Type().Field(i).Name, v.Field(i))
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			scanForSecret(t, path+"[key]", iter.Key())
			scanForSecret(t, fmt.Sprintf("%s[%v]", path, iter.Key()), iter.Value())
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			scanForSecret(t, fmt.Sprintf("%s[%d]", path, i), v.Index(i))
		}
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			scanForSecret(t, path, v.Elem())
		}
	}
}

// TestDecodeIsAnAllowList is the structural half of the privacy story: the
// record types are read by NAME, and the ones that are read expose no content
// field to decode into. A future field carrying content therefore contributes
// nothing until this package is taught about it on purpose.
//
// The check is on the decode shapes rather than on output, because output-only
// assertions pass right up until someone adds a field and forwards it.
func TestDecodeIsAnAllowList(t *testing.T) {
	forbidden := map[string]bool{
		"content": true, "arguments": true, "argumentsdelta": true,
		"text": true, "textdelta": true, "system": true, "tools": true,
		"title": true, "prompt": true, "meta": true, "result": true,
		"texts": true, "todos": true, "sections": true, "inserted": true,
		"data": false, // envelope.Data is raw and only decoded per allowed type
	}
	for _, shape := range []any{
		header{}, tokenUsage{}, messageSource{}, assistantData{},
		toolCallData{}, requestCtxData{}, auditRecord{}, auditUsage{},
	} {
		walkJSONNames(t, reflect.TypeOf(shape), func(name, where string) {
			if forbidden[strings.ToLower(name)] {
				t.Errorf("%s decodes a content field %q; DSH activity and usage are names and counts only", where, name)
			}
		})
	}
}

// walkJSONNames visits every json name in a struct type, recursively.
func walkJSONNames(t *testing.T, typ reflect.Type, fn func(name, where string)) {
	t.Helper()
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := f.Name
		if tag := f.Tag.Get("json"); tag != "" && tag != "-" {
			name = strings.Split(tag, ",")[0]
		}
		fn(name, typ.Name()+"."+f.Name)
		ft := f.Type
		for ft.Kind() == reflect.Ptr || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != typ {
			walkJSONNames(t, ft, fn)
		}
	}
}
