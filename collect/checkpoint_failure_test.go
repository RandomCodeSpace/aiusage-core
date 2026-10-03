package collect

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

type checkpointFailureStore struct {
	Store
	fail bool
}

func (s *checkpointFailureStore) Checkpoint(ctx context.Context, tool, path string) (*model.SourceCheckpoint, error) {
	if s.fail {
		return nil, errors.New("checkpoint unavailable")
	}
	return s.Store.Checkpoint(ctx, tool, path)
}

type checkpointAdapter struct {
	fakeAdapter
	incrementalCalls int
	checkpoint       *model.SourceCheckpoint
	observation      adapter.Observation
}

func (a *checkpointAdapter) CollectIncremental(_ context.Context, _ adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	a.incrementalCalls++
	a.checkpoint = cp
	return a.observation, nil
}

func TestCheckpointReadFailureHoldsSourceUntilRetry(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "absent"
		if existing {
			name = "saved baseline"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ledger := realStore(t)
			tool := model.ToolCrush
			cp := &model.SourceCheckpoint{Tool: tool, SourcePath: tool + "/src", Offset: 1, State: `{"cost_baseline":100}`}
			if existing {
				if _, err := ledger.ApplyEvents(ctx, nil, cp); err != nil {
					t.Fatal(err)
				}
			}
			next := *cp
			next.Offset = 2
			obs := adapter.Observation{Events: []model.UsageEvent{cycleEvent("incremental", tool, refDay, 10)}, Checkpoint: &next}
			ad := &checkpointAdapter{fakeAdapter: fakeAdapter{id: tool, class: model.EventLevel}, observation: obs}
			st := &checkpointFailureStore{Store: ledger, fail: true}
			reg := adapter.NewRegistry(ad)
			stats, err := RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
			if err != nil || len(stats.Errors) != 1 || !strings.Contains(stats.Errors[0], "load checkpoint: checkpoint unavailable") {
				t.Fatalf("failed stats=%+v err=%v", stats, err)
			}
			if ad.calls != 0 || ad.incrementalCalls != 0 || stats.EventsInserted != 0 || stats.ActivityInserted != 0 || stats.TurnContextsInserted != 0 {
				t.Fatalf("failed read collected source: calls=%d incremental=%d stats=%+v", ad.calls, ad.incrementalCalls, stats)
			}
			st.fail = false
			stats, err = RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
			if err != nil || len(stats.Errors) != 0 || stats.EventsInserted != 1 || ad.incrementalCalls != 1 {
				t.Fatalf("retry stats=%+v err=%v", stats, err)
			}
			if existing && (ad.checkpoint == nil || *ad.checkpoint != *cp) {
				t.Fatalf("retry lost baseline=%+v", ad.checkpoint)
			}
			if !existing && ad.checkpoint != nil {
				t.Fatalf("first read checkpoint=%+v", ad.checkpoint)
			}
			stats, err = RunOnce(ctx, reg, st, adapter.DiscoverConfig{})
			if err != nil || len(stats.Errors) != 0 || stats.EventsInserted != 0 {
				t.Fatalf("repeat stats=%+v err=%v", stats, err)
			}
		})
	}
}

func TestCollectorReportsZeroAfterStoreRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	ledger, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_context BEFORE INSERT ON usage_turn_context BEGIN SELECT RAISE(ABORT,'injected context failure'); END`); err != nil {
		t.Fatal(err)
	}
	obs := activityObs("u1", 100, 1, refDay)
	obs.TurnContexts = []model.TurnContext{{UsageDedupKey: "u1", Dimension: model.DimensionSkill, Value: "reader", Tool: model.ToolClaudeCode, EventTime: refDay}}
	obs.Checkpoint = &model.SourceCheckpoint{Tool: model.ToolClaudeCode, SourcePath: model.ToolClaudeCode + "/src", Offset: 1}
	ad := &checkpointAdapter{fakeAdapter: fakeAdapter{id: model.ToolClaudeCode, class: model.EventLevel}, observation: obs}
	reg := adapter.NewRegistry(ad)
	stats, err := RunOnce(ctx, reg, ledger, adapter.DiscoverConfig{})
	if err != nil || len(stats.Errors) != 1 || stats.EventsInserted != 0 || stats.ActivityInserted != 0 || stats.TurnContextsInserted != 0 {
		t.Fatalf("rollback stats=%+v err=%v", stats, err)
	}
	if cp, err := ledger.Checkpoint(ctx, obs.Checkpoint.Tool, obs.Checkpoint.SourcePath); err != nil || cp != nil {
		t.Fatalf("rollback checkpoint=%+v err=%v", cp, err)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_context`); err != nil {
		t.Fatal(err)
	}
	stats, err = RunOnce(ctx, reg, ledger, adapter.DiscoverConfig{})
	if err != nil || len(stats.Errors) != 0 || stats.EventsInserted != 1 || stats.ActivityInserted != 1 || stats.TurnContextsInserted != 1 {
		t.Fatalf("retry stats=%+v err=%v", stats, err)
	}
	stats, err = RunOnce(ctx, reg, ledger, adapter.DiscoverConfig{})
	if err != nil || len(stats.Errors) != 0 || stats.EventsInserted != 0 || stats.ActivityInserted != 0 || stats.TurnContextsInserted != 0 {
		t.Fatalf("repeat stats=%+v err=%v", stats, err)
	}
}
