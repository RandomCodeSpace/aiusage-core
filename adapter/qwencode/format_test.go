package qwencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestRequiredAnchorsDiagnoseDrift(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "before.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var original []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		original = append(original, record)
	}
	anchors := []struct {
		record      int
		parent, key string
	}{{0, "", "id"}, {0, "", "inputTokens"}, {0, "", "outputTokens"}, {0, "", "totalTokens"}}
	for _, anchor := range anchors {
		for _, mutation := range []string{"missing", "renamed", "null", "wrong-type"} {
			t.Run(anchor.parent+"/"+anchor.key+"/"+mutation, func(t *testing.T) {
				cloned, err := json.Marshal(original)
				if err != nil {
					t.Fatal(err)
				}
				var records []map[string]any
				if err := json.Unmarshal(cloned, &records); err != nil {
					t.Fatal(err)
				}
				target := records[anchor.record]
				if anchor.parent != "" {
					target = target[anchor.parent].(map[string]any)
				}
				switch mutation {
				case "missing":
					delete(target, anchor.key)
				case "renamed":
					target[anchor.key+"_renamed"] = target[anchor.key]
					delete(target, anchor.key)
				case "null":
					target[anchor.key] = nil
				case "wrong-type":
					target[anchor.key] = []any{true}
				}
				var body bytes.Buffer
				for _, record := range records {
					b, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					body.Write(b)
					body.WriteByte('\n')
				}
				src := formatSource(t, body.Bytes())
				obs, err := (Adapter{}).Collect(context.Background(), src)
				if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil {
					t.Fatalf("format diagnosis = %v, checkpoint present = %t", err, obs.Checkpoint != nil)
				}
			})
		}
	}
}

func TestFormatAdmissionPreservesLegacyZeroAndPartialEOF(t *testing.T) {
	cases := []struct {
		name, body   string
		events       int
		incompatible bool
		offset       int64
	}{
		{name: "whole unknown", body: "{\"future_record\":true}\n", incompatible: true},
		{name: "whole unknown unterminated complete", body: "{\"future_record\":true}", incompatible: true},
		{name: "versioned zero and omitted optional buckets", body: `{"schemaVersion":1,"id":"zero","inputTokens":0,"outputTokens":0,"totalTokens":0}` + "\n", offset: -1},
		{name: "null schema", body: `{"schemaVersion":null,"id":"zero","inputTokens":0,"outputTokens":0,"totalTokens":0}` + "\n", incompatible: true},
		{name: "wrong type schema", body: `{"schemaVersion":[],"id":"zero","inputTokens":0,"outputTokens":0,"totalTokens":0}` + "\n", incompatible: true},
		{name: "explicit null optional bucket", body: `{"schemaVersion":1,"id":"zero","inputTokens":0,"outputTokens":0,"totalTokens":0,"cachedTokens":null}` + "\n", incompatible: true},
		{name: "legacy zero", body: `{"id":"legacy-zero","inputTokens":0}` + "\n", offset: -1},
		{name: "legacy sparse", body: `{"id":"legacy-usage","outputTokens":2}` + "\n", events: 1, offset: -1},
		{name: "partial only", body: "{\"type\":", offset: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := formatSource(t, []byte(tc.body))
			obs, err := (Adapter{}).Collect(context.Background(), src)
			if tc.incompatible {
				if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil {
					t.Fatalf("format diagnosis = %v, checkpoint = %t", err, obs.Checkpoint != nil)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(obs.Events) != tc.events || obs.Checkpoint == nil {
				t.Fatalf("events = %d, checkpoint = %t", len(obs.Events), obs.Checkpoint != nil)
			}
			want := tc.offset
			if want < 0 {
				want = int64(len(tc.body))
			}
			if obs.Checkpoint.Offset != want {
				t.Fatalf("offset = %d, want %d", obs.Checkpoint.Offset, want)
			}
		})
	}
	valid, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "before.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	body := append(append([]byte{}, valid...), []byte("{\"type\":\"future.unrelated\",\"extra\":true}\n")...)
	body = append(body, []byte("{\"unfinished\":")...)
	obs, err := (Adapter{}).Collect(context.Background(), formatSource(t, body))
	if err != nil || len(obs.Events) != 1 || obs.Checkpoint == nil {
		t.Fatalf("valid prefix = %d events, checkpoint = %t, error = %v", len(obs.Events), obs.Checkpoint != nil, err)
	}
	if obs.Checkpoint.Offset != int64(len(body)-len("{\"unfinished\":")) {
		t.Fatal("checkpoint crossed incomplete EOF")
	}
}

func formatSource(t *testing.T, body []byte) adapter.Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.jsonl")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return adapter.Source{Tool: model.ToolQwenCode, Path: path, Meta: map[string]string{"agent": "main", "session": "s"}}
}

func TestCompletePoisonIsReportedAndConsumed(t *testing.T) {
	valid, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "before.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	body := append(append([]byte{}, valid...), []byte("{\"broken\":\n")...)
	obs, err := (Adapter{}).Collect(context.Background(), formatSource(t, body))
	if err == nil || errors.Is(err, adapter.ErrSourceFormat) || len(obs.Events) != 1 || obs.Checkpoint == nil || obs.Checkpoint.Offset != int64(len(body)) {
		t.Fatalf("poison record: events=%d, checkpoint=%t, error=%v", len(obs.Events), obs.Checkpoint != nil, err)
	}
}

func TestAppendedAnchorFailureWithholdsCheckpoint(t *testing.T) {
	before, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "before.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "after.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	src := formatSource(t, before)
	a := Adapter{}
	first, err := a.Collect(context.Background(), src)
	if err != nil || first.Checkpoint == nil {
		t.Fatalf("first checkpoint: %v", err)
	}
	var body bytes.Buffer
	body.Write(before)
	for _, line := range bytes.Split(bytes.TrimSpace(after[len(before):]), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		delete(record, "totalTokens")
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(encoded)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(src.Path, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	obs, err := a.CollectIncremental(context.Background(), src, first.Checkpoint)
	if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil || len(obs.Events) != 0 {
		t.Fatalf("appended anchor failure: events=%d, checkpoint=%t, error=%v", len(obs.Events), obs.Checkpoint != nil, err)
	}
}
