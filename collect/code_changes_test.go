package collect

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func TestCycleCollectsCodeChangesWithoutUsage(t *testing.T) {
	ctx := context.Background()
	ledger := realStore(t)
	change := model.CodeChange{
		Tool: model.ToolOpenCode, ChangeID: "turn", SessionID: "session", Project: "/sample",
		Known: true, LinesAdded: 12, LinesRemoved: 4, UpdatedAt: refDay,
	}
	cp := &model.SourceCheckpoint{Tool: change.Tool, SourcePath: change.Tool + "/src", Offset: 1}
	ad := &checkpointAdapter{
		fakeAdapter: fakeAdapter{id: change.Tool, class: model.EventLevel},
		observation: adapter.Observation{CodeChanges: []model.CodeChange{change}, Checkpoint: cp},
	}
	reg := adapter.NewRegistry(ad)
	for _, step := range []struct {
		name   string
		change model.CodeChange
		writes int
		want   store.CodeChangeSummary
	}{
		{"initial", change, 1, store.CodeChangeSummary{LinesAdded: 12, LinesRemoved: 4, KnownChanges: 1}},
		{"repeat", change, 0, store.CodeChangeSummary{LinesAdded: 12, LinesRemoved: 4, KnownChanges: 1}},
		{"decrease", func() model.CodeChange {
			c := change
			c.LinesAdded = 3
			c.LinesRemoved = 1
			c.UpdatedAt = c.UpdatedAt.Add(time.Second)
			return c
		}(), 1, store.CodeChangeSummary{LinesAdded: 3, LinesRemoved: 1, KnownChanges: 1}},
		{"unknown", func() model.CodeChange {
			c := change
			c.Known = false
			c.LinesAdded = 0
			c.LinesRemoved = 0
			c.UpdatedAt = c.UpdatedAt.Add(2 * time.Second)
			return c
		}(), 1, store.CodeChangeSummary{UnknownChanges: 1}},
	} {
		t.Run(step.name, func(t *testing.T) {
			ad.observation.CodeChanges = []model.CodeChange{step.change}
			stats, err := RunOnce(ctx, reg, ledger, adapter.DiscoverConfig{})
			if err != nil || len(stats.Errors) != 0 || stats.CodeChangesUpdated != step.writes {
				t.Fatalf("stats=%+v err=%v", stats, err)
			}
			if stats.EventsInserted != 0 || stats.ActivityInserted != 0 || stats.TurnContextsInserted != 0 {
				t.Fatalf("line counts created unrelated observations: %+v", stats)
			}
			if got, err := ledger.SessionCodeChanges(ctx, change.Tool, change.SessionID, change.Project); err != nil || got != step.want {
				t.Fatalf("summary=%+v want=%+v err=%v", got, step.want, err)
			}
			if got, err := ledger.Checkpoint(ctx, cp.Tool, cp.SourcePath); err != nil || got == nil || got.Offset != cp.Offset {
				t.Fatalf("checkpoint=%+v err=%v", got, err)
			}
		})
	}
}
