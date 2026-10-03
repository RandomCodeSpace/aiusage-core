package hermes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/collect"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func liveDB(t *testing.T, path, stage string) {
	t.Helper()
	body, err := os.ReadFile("testdata/live-2026.9.11-" + stage + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	wantHash := map[string]string{
		"before": "2b66e9499bcadeb8ea3b70c80bb949699292252c494bc8030caf297fda15478c",
		"after":  "6ad6bebcd17de4f3ae24014db1445bdea723a65bf82d813e4e6f08d9eef2e079",
	}[stage]
	if fmt.Sprintf("%x", sha256.Sum256(body)) != wantHash {
		t.Fatal("captured fixture hash changed")
	}
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS sessions;" + string(body)); err != nil {
		t.Fatal(err)
	}
}

func TestLive20260911SnapshotReplayAndDelta(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, dbName)
	liveDB(t, path, "before")
	src := adapter.Source{Tool: model.ToolHermes, Class: model.Aggregate, Path: path}
	a := Adapter{}
	first, err := a.CollectIncremental(ctx, src, nil)
	if err != nil || len(first.Snapshots) != 1 || first.Checkpoint == nil {
		t.Fatalf("first = %+v, %v", first, err)
	}
	check := func(obs adapter.Observation, input, output, cache, total int64, ended string) {
		t.Helper()
		if len(obs.Snapshots) != 1 || len(obs.Events) != 0 || len(obs.Activity) != 0 || len(obs.TurnContexts) != 0 {
			t.Fatalf("unexpected surfaces: %+v", obs)
		}
		s := obs.Snapshots[0]
		wantTime, _ := time.Parse(time.RFC3339Nano, ended)
		if s.Key != "20260912_165604_8bc81a" || s.SessionID != s.Key || s.Tool != model.ToolHermes || s.Model != "gemma4:31b-cloud" || s.Provider != "custom" || s.Project != "hermes" || s.SourcePath != path || s.InputTokens != input || s.OutputTokens != output || s.CacheReadTokens != cache || s.CacheCreationTokens != 0 || s.ReasoningTokens != 0 || s.TotalTokens != total || s.ObservedTime.Sub(wantTime).Abs() > time.Microsecond {
			t.Fatalf("snapshot = %+v, want time %s", s, wantTime)
		}
	}
	check(first, 1796, 24, 3776, 5596, "2026-09-12T16:56:11.677864Z")
	ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	run := func(want int) {
		t.Helper()
		stats, err := collect.RunOnce(ctx, adapter.NewRegistry(a), ledger, adapter.DiscoverConfig{Overrides: map[string]string{model.ToolHermes: home}})
		if err != nil || len(stats.Errors) != 0 || stats.EventsInserted != want || stats.ActivityInserted != 0 || stats.TurnContextsInserted != 0 {
			t.Fatalf("run = %+v, %v, want %d", stats, err, want)
		}
	}
	run(1)
	run(0)
	idle, err := a.CollectIncremental(ctx, src, first.Checkpoint)
	if err != nil || len(idle.Snapshots) != 0 {
		t.Fatalf("idle = %+v, %v", idle, err)
	}
	liveDB(t, path, "after")
	second, err := a.CollectIncremental(ctx, src, first.Checkpoint)
	if err != nil || second.Checkpoint == nil {
		t.Fatalf("second = %+v, %v", second, err)
	}
	check(second, 3700, 44, 7680, 11424, "2026-09-12T16:59:18.5970588Z")
	run(1)
	run(0)
	// Recreate the identical full capture to invalidate the filesystem gate.
	liveDB(t, path, "after")
	run(0)
	events, err := ledger.ListEvents(ctx, store.Filter{})
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %d, %v", len(events), err)
	}
	var total int64
	for _, ev := range events {
		total += ev.TotalTokens
		if ev.CostMicroUSD != nil || ev.PriceSource != "" || ev.ReasoningTokens != 0 {
			t.Fatalf("invented accounting = %+v", ev)
		}
		if ev.TotalTokens == 5828 && (ev.InputTokens != 1904 || ev.OutputTokens != 20 || ev.CacheReadTokens != 3904) {
			t.Fatalf("delta = %+v", ev)
		}
	}
	if total != 11424 {
		t.Fatalf("stored total = %d", total)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Collect(ctx, src); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("collector changed source")
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 1 {
		t.Fatalf("source sidecars = %+v, %v", entries, err)
	}
	// The live model reports zero reasoning. This constructed mutation checks
	// the exact writer's output-detail mapping and non-additive total rule.
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET reasoning_tokens = 7"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	reasoning, err := a.Collect(ctx, src)
	if err != nil || len(reasoning.Snapshots) != 1 || reasoning.Snapshots[0].ReasoningTokens != 7 || reasoning.Snapshots[0].OutputTokens != 44 || reasoning.Snapshots[0].TotalTokens != 11424 {
		t.Fatalf("reasoning subset = %+v, %v", reasoning, err)
	}
}

func TestLive20260911RequiredSchemaHoldsCheckpoint(t *testing.T) {
	mutations := map[string]string{"sessions": "ALTER TABLE sessions RENAME TO renamed", "id-empty": "UPDATE sessions SET id = ''", "id-null": "UPDATE sessions SET id = NULL", "counter-type": "UPDATE sessions SET input_tokens = 'bad'"}
	for _, field := range []string{"id", "model", "billing_provider", "started_at", "ended_at", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
		mutations[field] = "ALTER TABLE sessions RENAME COLUMN " + field + " TO renamed"
	}
	for name, query := range mutations {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), dbName)
			liveDB(t, path, "before")
			db, err := sql.Open(driverName, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			rowMutation := name == "id-empty" || name == "id-null" || name == "counter-type"
			if rowMutation {
				if _, err := db.Exec("INSERT INTO sessions (id, source, model, started_at, input_tokens, output_tokens) VALUES ('valid-prefix', 'cli', 'gemma4:31b-cloud', 1789232170, 1, 2)"); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolHermes, Path: path})
			if !errors.Is(err, adapter.ErrSourceFormat) || !strings.Contains(err.Error(), path) || obs.Checkpoint != nil {
				t.Fatalf("damaged = %+v, %v", obs, err)
			}
			if rowMutation && (len(obs.Snapshots) != 1 || obs.Snapshots[0].Key != "valid-prefix" || obs.Snapshots[0].TotalTokens != 3) {
				t.Fatalf("valid row lost: %+v", obs)
			}
		})
	}
}

func TestLive20260911OptionalNullCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)
	liveDB(t, path, "before")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET input_tokens = NULL, output_tokens = NULL, cache_read_tokens = NULL, cache_write_tokens = NULL, reasoning_tokens = NULL, ended_at = NULL, billing_provider = NULL"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	obs, err := (Adapter{}).Collect(context.Background(), adapter.Source{Tool: model.ToolHermes, Path: path})
	if err != nil || len(obs.Snapshots) != 1 || obs.Snapshots[0].TotalTokens != 0 || obs.Checkpoint == nil {
		t.Fatalf("optional null = %+v, %v", obs, err)
	}
}

func TestUnixTimestampAndLegacyFallback(t *testing.T) {
	for _, value := range []string{"1789232171.677864", "1.789232171677864e+09"} {
		got := parseTime(value)
		want := time.Date(2026, 9, 12, 16, 56, 11, 677864000, time.UTC)
		if got.Sub(want).Abs() > time.Microsecond {
			t.Fatalf("%s -> %s", value, got)
		}
	}
	for _, value := range []string{"NaN", "+Inf", "-Inf", "1e100", "not-a-time", ""} {
		if !parseTime(value).IsZero() {
			t.Fatalf("invalid timestamp accepted: %s", value)
		}
	}
}
