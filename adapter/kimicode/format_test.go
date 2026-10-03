package kimicode

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
	"github.com/RandomCodeSpace/aiusage-core/store"
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
	}{{0, "", "type"}, {0, "", "protocol_version"}, {0, "", "created_at"}, {1, "", "type"}, {1, "", "model"}, {2, "", "type"}, {2, "", "usage"}, {2, "usage", "inputOther"}, {2, "usage", "output"}, {2, "usage", "inputCacheRead"}, {2, "usage", "inputCacheCreation"}}
	for _, anchor := range anchors {
		mutations := []string{"missing", "renamed", "null", "wrong-type"}
		if anchor.key == "type" {
			mutations = append(mutations, "wrong-value")
		}
		for _, mutation := range mutations {
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
				case "wrong-value":
					target[anchor.key] = "unknown-kind"
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
				if anchor.record == 1 && len(obs.Events) != 0 {
					t.Fatal("rejected request emitted dependent usage")
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
		{name: "reserved protocol", body: `{"type":"metadata","protocol_version":"legacy","created_at":1}` + "\n", incompatible: true},
		{name: "versioned zero", body: `{"type":"metadata","protocol_version":"1.5","created_at":1}` + "\n" + `{"type":"usage.record","usage":{"inputOther":0,"output":0,"inputCacheRead":0,"inputCacheCreation":0}}` + "\n", offset: -1},
		{name: "legacy zero", body: `{"type":"usage.record","usage":{"inputOther":0}}` + "\n", offset: -1},
		{name: "legacy sparse", body: `{"type":"usage.record","usage":{"output":2}}` + "\n", events: 1, offset: -1},
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
	return adapter.Source{Tool: model.ToolKimiCode, Path: path, Meta: map[string]string{"agent": "main", "session": "s"}}
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
		if record["type"] == "usage.record" {
			delete(record["usage"].(map[string]any), "inputOther")
		}
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

// A held checkpoint alone cannot recover safely if dependent usage has already
// been stored with a rejected request's previous model or request identity.
func TestRejectedRequestRepairDoesNotDuplicateUsage(t *testing.T) {
	before, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "before.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "after.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		request int
		tail    bool
	}{
		{name: "first request without carried state", request: 0},
		{name: "second request with carried state", request: 1},
		{name: "second request after checkpoint", request: 1, tail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			a := Adapter{}
			src := formatSource(t, after)
			oracle, err := a.Collect(ctx, src)
			if err != nil || len(oracle.Events) != 2 {
				t.Fatalf("original capture: events=%d, error=%v", len(oracle.Events), err)
			}
			ledger, err := store.Open(filepath.Join(t.TempDir(), "recovery.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := ledger.Close(); err != nil {
					t.Error(err)
				}
			})
			inserted := 0
			apply := func(obs adapter.Observation) int {
				t.Helper()
				got, err := ledger.ApplyBatch(ctx, store.ObservationBatch{Events: obs.Events})
				if err != nil {
					t.Fatal(err)
				}
				inserted += got.Events
				return got.Events
			}
			var cp *model.SourceCheckpoint
			if tc.tail {
				if err := os.WriteFile(src.Path, before, 0o600); err != nil {
					t.Fatal(err)
				}
				first, err := a.Collect(ctx, src)
				if err != nil || first.Checkpoint == nil {
					t.Fatalf("first read: %v", err)
				}
				cp = first.Checkpoint
				apply(first)
			}
			var body bytes.Buffer
			request := 0
			for _, line := range bytes.Split(bytes.TrimSpace(after), []byte("\n")) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if record["type"] == recRequest {
					if request == tc.request {
						delete(record, "model")
						line, err = json.Marshal(record)
						if err != nil {
							t.Fatal(err)
						}
					}
					request++
				}
				// Keep every untouched record byte-for-byte, including the
				// prefix named by the original checkpoint in the tail case.
				body.Write(line)
				body.WriteByte('\n')
			}
			if err := os.WriteFile(src.Path, body.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			broken, err := a.CollectIncremental(ctx, src, cp)
			if !errors.Is(err, adapter.ErrSourceFormat) || broken.Checkpoint != nil {
				t.Fatalf("rejected request: checkpoint=%t, error=%v", broken.Checkpoint != nil, err)
			}
			apply(broken)
			if err := os.WriteFile(src.Path, after, 0o600); err != nil {
				t.Fatal(err)
			}
			repaired, err := a.CollectIncremental(ctx, src, cp)
			if err != nil || repaired.Checkpoint == nil {
				t.Fatalf("repaired read: %v", err)
			}
			apply(repaired)
			if inserted != len(oracle.Events) {
				t.Fatalf("rejected request followed by repair inserted %d events, want %d original usage identities", inserted, len(oracle.Events))
			}
			full, err := a.Collect(ctx, src)
			if err != nil {
				t.Fatal(err)
			}
			if got := apply(full); got != 0 {
				t.Fatalf("repaired full replay inserted %d events", got)
			}
			for i, event := range full.Events {
				if event.DedupKey != oracle.Events[i].DedupKey {
					t.Fatal("repair changed original usage identity")
				}
			}
		})
	}
}

func TestRejectedMetadataDoesNotReuseRequestContext(t *testing.T) {
	before, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "before.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join("testdata", "live-2026-09-12", "after.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var secondUsage []byte
	for _, line := range bytes.Split(bytes.TrimSpace(after[len(before):]), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["type"] == recUsage {
			secondUsage = line
		}
	}
	if len(secondUsage) == 0 {
		t.Fatal("capture has no second usage")
	}
	for _, metadata := range []string{
		`{"type":"metadata","created_at":1}`,
		`{"type":"unknown-kind","protocol_version":"1.5","created_at":1}`,
		`{"type":42,"protocol_version":"1.5","created_at":1}`,
	} {
		t.Run(metadata, func(t *testing.T) {
			src := formatSource(t, after)
			a := Adapter{}
			oracle, err := a.Collect(context.Background(), src)
			if err != nil || len(oracle.Events) != 2 {
				t.Fatalf("original capture: %v", err)
			}
			// Construct a rejected header followed by orphan usage. The next
			// valid request restores context for the actual captured usage.
			var body bytes.Buffer
			body.Write(before)
			body.WriteString(metadata + "\n")
			body.Write(secondUsage)
			body.WriteByte('\n')
			body.Write(after[len(before):])
			if err := os.WriteFile(src.Path, body.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			obs, err := a.Collect(context.Background(), src)
			if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil || len(obs.Events) != 2 {
				t.Fatalf("rejected metadata: events=%d, checkpoint=%t, error=%v", len(obs.Events), obs.Checkpoint != nil, err)
			}
			for i, event := range obs.Events {
				if event.DedupKey != oracle.Events[i].DedupKey {
					t.Fatal("rejected metadata changed independent usage identity")
				}
			}
		})
	}
}
