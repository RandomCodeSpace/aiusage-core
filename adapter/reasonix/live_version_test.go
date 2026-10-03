package reasonix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func TestLive1253LedgerPrefixReplayAndAppend(t *testing.T) {
	body, err := os.ReadFile("testdata/live-1.25.3.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	split := bytes.IndexByte(body, '\n') + 1
	if split != 319 || len(body) != 638 {
		t.Fatalf("capture boundaries changed: %d/%d", split, len(body))
	}
	path := filepath.Join(t.TempDir(), "2026-09-12.jsonl")
	write := func(b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(body[:split])
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	src := adapter.Source{Tool: model.ToolReasonix, Path: path}
	ctx := context.Background()
	a := Adapter{}
	ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	collect := func(cp *model.SourceCheckpoint) adapter.Observation {
		t.Helper()
		obs, err := a.CollectIncremental(ctx, src, cp)
		if err != nil {
			t.Fatal(err)
		}
		return obs
	}
	apply := func(obs adapter.Observation, want int) {
		t.Helper()
		got, err := ledger.ApplyObservation(ctx, obs.Events, obs.Activity, obs.Checkpoint)
		if err != nil || got.Events != want {
			t.Fatalf("apply = %+v, %v; want %d", got, err, want)
		}
	}
	prefix := collect(nil)
	if len(prefix.Events) != 1 || prefix.Checkpoint == nil || prefix.Checkpoint.Offset != 319 {
		t.Fatalf("prefix = %+v", prefix)
	}
	apply(prefix, 1)
	apply(collect(nil), 0)
	idle := collect(prefix.Checkpoint)
	if len(idle.Events) != 0 || idle.Checkpoint != nil {
		t.Fatalf("unchanged poll = %+v", idle)
	}
	write(body)
	appended := collect(prefix.Checkpoint)
	if len(appended.Events) != 1 || appended.Checkpoint == nil || appended.Checkpoint.Offset != 638 {
		t.Fatalf("append = %+v", appended)
	}
	apply(appended, 1)
	replay := collect(nil)
	apply(replay, 0)
	if len(replay.Events) != 2 {
		t.Fatalf("full replay events = %d", len(replay.Events))
	}
	keys := []string{"reasonix|ae7e0e2afbf72a52c1712ebb0fc08b8b", "reasonix|23f16d948f089f2d91571398a0139660"}
	timestamps := []string{"2026-09-12T16:12:23.207649364Z", "2026-09-12T16:14:59.331484567Z"}
	for i, ev := range replay.Events {
		wantTime, _ := time.Parse(time.RFC3339Nano, timestamps[i])
		if ev.DedupKey != keys[i] || !ev.EventTime.Equal(wantTime) || ev.InputTokens != 5088 || ev.OutputTokens != 3 || ev.TotalTokens != 5091 || ev.ReasoningTokens != 0 || ev.CacheReadTokens != 0 || ev.CacheCreationTokens != 0 || ev.Model != "ollama/gemma4:31b-cloud" || ev.Provider != "ollama" || ev.Tool != model.ToolReasonix || ev.Kind != model.KindUsage || ev.SourcePath != path || ev.SessionID != "" || ev.Project != "" || ev.CostMicroUSD != nil || ev.PriceSource != "" {
			t.Fatalf("event %d = %+v", i, ev)
		}
	}
	stored, err := ledger.ListEvents(ctx, store.Filter{})
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored = %d, %v", len(stored), err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, body) {
		t.Fatal("collector changed captured source")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode() != before.Mode() {
		t.Fatal("collector changed source mode")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("collector created source sidecars: %+v, %v", entries, err)
	}
}

func TestCurrentLedgerTimestampAnchorHoldsCheckpoint(t *testing.T) {
	body, err := os.ReadFile("testdata/live-1.25.3.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(body), []byte{'\n'})
	for _, value := range []string{"missing", "", "not-a-time", "wrong-type", "null", "renamed"} {
		name := value
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			var rec map[string]any
			if err := json.Unmarshal(lines[1], &rec); err != nil {
				t.Fatal(err)
			}
			switch value {
			case "missing":
				delete(rec, "ts")
			case "wrong-type":
				rec["ts"] = 42
			case "null":
				rec["ts"] = nil
			case "renamed":
				rec["timestamp"] = rec["ts"]
				delete(rec, "ts")
			default:
				rec["ts"] = value
			}
			damaged, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "2026-09-12.jsonl")
			bad := append(append(append([]byte{}, lines[0]...), '\n'), append(damaged, '\n')...)
			if err := os.WriteFile(path, bad, 0600); err != nil {
				t.Fatal(err)
			}
			a := Adapter{}
			ctx := context.Background()
			src := adapter.Source{Tool: model.ToolReasonix, Path: path}
			obs, err := a.CollectIncremental(ctx, src, nil)
			if !errors.Is(err, adapter.ErrSourceFormat) || !strings.Contains(err.Error(), "ts") || !strings.Contains(err.Error(), path) || obs.Checkpoint != nil || len(obs.Events) != 1 {
				t.Fatalf("damaged current ledger = %+v, %v", obs, err)
			}
			ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			applied, err := ledger.ApplyObservation(ctx, obs.Events, obs.Activity, obs.Checkpoint)
			if err != nil || applied.Events != 1 {
				t.Fatalf("valid prefix = %+v, %v", applied, err)
			}
			cp, err := ledger.Checkpoint(ctx, model.ToolReasonix, path)
			if err != nil || cp != nil {
				t.Fatalf("checkpoint advanced: %+v, %v", cp, err)
			}
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			retry, err := a.CollectIncremental(ctx, src, cp)
			if err != nil || len(retry.Events) != 2 || retry.Checkpoint == nil {
				t.Fatalf("repaired replay = %+v, %v", retry, err)
			}
			applied, err = ledger.ApplyObservation(ctx, retry.Events, retry.Activity, retry.Checkpoint)
			if err != nil || applied.Events != 1 {
				t.Fatalf("repaired delta = %+v, %v", applied, err)
			}
		})
	}
}

// The exact installed writer uses omitempty for these fields. Absence cannot
// distinguish a zero/unknown value from format drift, so it is not an error.
func TestCurrentWriterOptionalFieldsAndLegacyFallback(t *testing.T) {
	body, err := os.ReadFile("testdata/live-1.25.3.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Split(body, []byte{'\n'})[0]
	for _, field := range []string{"model", "prompt", "completion", "total", "usage_source"} {
		t.Run(field, func(t *testing.T) {
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			delete(rec, field)
			mutated, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := parseRecord(mutated, time.Now(), "stats.jsonl"); err != nil {
				t.Fatalf("writer's optional %s rejected: %v", field, err)
			}
		})
	}
	mtime := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	ev, ok, err := parseRecord([]byte(`{"prompt":4,"completion":2}`), mtime, "legacy.jsonl")
	if err != nil || !ok || !ev.EventTime.Equal(mtime) || ev.TotalTokens != 6 {
		t.Fatalf("legacy fallback = %+v, %t, %v", ev, ok, err)
	}
	for _, line := range []string{`{"ts":"2026-09-12T00:00:00Z","source":"cli","turn":true}`, `{"ts":"2026-09-12T00:00:00Z","model":"p/m","requests":1,"usage_source":"executor"}`} {
		if _, ok, err := parseRecord([]byte(line), mtime, "stats.jsonl"); err != nil || ok {
			t.Fatalf("bookkeeping = %t, %v", ok, err)
		}
	}
}

// This is a source-backed shape assertion, not a fabricated live observation.
// v1.25.3 recorder.go copies Usage.ReasoningTokens into record.Reasoning;
// record.go serializes it as reasoning,omitempty. See the capture provenance.
func TestVersionedWriterReasoningMapping(t *testing.T) {
	ev, ok, err := parseRecord([]byte(`{"ts":"2026-09-12T00:00:00Z","model":"p/m","prompt":10,"completion":7,"reasoning":5,"total":17,"usage_source":"executor"}`), time.Time{}, "stats.jsonl")
	if err != nil || !ok || ev.ReasoningTokens != 5 || ev.OutputTokens != 7 || ev.TotalTokens != 17 {
		t.Fatalf("reasoning mapping = %+v, %t, %v", ev, ok, err)
	}
}

func TestCurrentLedgerPartialEOFRemainsUnconsumed(t *testing.T) {
	body, err := os.ReadFile("testdata/live-1.25.3.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	split := bytes.IndexByte(body, '\n') + 1
	path := filepath.Join(t.TempDir(), "2026-09-12.jsonl")
	partial := body[:len(body)-20]
	if err := os.WriteFile(path, partial, 0600); err != nil {
		t.Fatal(err)
	}
	a := Adapter{}
	src := adapter.Source{Tool: model.ToolReasonix, Path: path}
	obs, err := a.CollectIncremental(context.Background(), src, nil)
	if err == nil || len(obs.Events) != 1 || obs.Checkpoint == nil || obs.Checkpoint.Offset != int64(split) {
		t.Fatalf("partial EOF = %+v, %v", obs, err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	retry, err := a.CollectIncremental(context.Background(), src, obs.Checkpoint)
	if err != nil || len(retry.Events) != 1 || retry.Events[0].DedupKey != "reasonix|23f16d948f089f2d91571398a0139660" || retry.Checkpoint == nil || retry.Checkpoint.Offset != int64(len(body)) {
		t.Fatalf("completed EOF = %+v, %v", retry, err)
	}
}

func TestCurrentLedgerRejectsUnrecognizedCompleteObjects(t *testing.T) {
	for _, body := range []string{`{"new_usage_shape":{"tokens":12}}` + "\n", `{}` + "\n", `null` + "\n", `[]` + "\n"} {
		path := filepath.Join(t.TempDir(), "2026-09-12.jsonl")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		obs, err := (Adapter{}).CollectIncremental(context.Background(), adapter.Source{Tool: model.ToolReasonix, Path: path}, nil)
		if !errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint != nil || len(obs.Events) != 0 || !strings.Contains(err.Error(), path) {
			t.Fatalf("unrecognized %q = %+v, %v", body, obs, err)
		}
	}
	for _, body := range []string{`{"prompt":0,"completion":0,"total":0}`, `{"ts":"2026-09-12T00:00:00Z","source":"cli","turn":true}`, `{"requests":1}`, `{"turns":0}`} {
		if _, ok, err := parseRecord([]byte(body), time.Now(), "stats.jsonl"); err != nil || ok {
			t.Fatalf("recognized zero %q = %t, %v", body, ok, err)
		}
	}
	path := filepath.Join(t.TempDir(), "2026-09-12.jsonl")
	if err := os.WriteFile(path, []byte(`{"ts":`), 0600); err != nil {
		t.Fatal(err)
	}
	obs, err := (Adapter{}).CollectIncremental(context.Background(), adapter.Source{Tool: model.ToolReasonix, Path: path}, nil)
	if err == nil || errors.Is(err, adapter.ErrSourceFormat) || obs.Checkpoint == nil || obs.Checkpoint.Offset != 0 || len(obs.Events) != 0 {
		t.Fatalf("incomplete first EOF = %+v, %v", obs, err)
	}
}
