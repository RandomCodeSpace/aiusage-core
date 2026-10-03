package crush

import (
	"context"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestCorruptCheckpointNeverResetsCost(t *testing.T) {
	path := buildDB(t, t.TempDir(), "costly.sql")
	src := adapter.Source{Tool: model.ToolCrush, Path: path, Meta: map[string]string{"project": "/proj"}}
	first, err := (Adapter{}).CollectIncremental(context.Background(), src, nil)
	if err != nil || first.Checkpoint == nil {
		t.Fatalf("first read=%+v,%v", first, err)
	}
	bumpCost(t, path, "sess-paid-single", 0.02)
	for _, raw := range []string{`{`, `null`, `{}`, `{"dbSize":"bad"}`, `{"dbSize":4096,"cost":{"sess-paid-single":"bad"}}`} {
		t.Run(raw, func(t *testing.T) {
			cp := *first.Checkpoint
			cp.State = raw
			obs, err := (Adapter{}).CollectIncremental(context.Background(), src, &cp)
			if err == nil || len(obs.Events) != 0 || obs.Checkpoint != nil || cp.State != raw {
				t.Fatalf("corrupt state reset accounting: %+v,%v", obs, err)
			}
		})
	}
	retry, err := (Adapter{}).CollectIncremental(context.Background(), src, first.Checkpoint)
	if err != nil || len(retry.Events) != 1 {
		t.Fatalf("valid checkpoint retry=%+v,%v", retry, err)
	}
	if got, ok := retry.Events[0].Cost(); !ok || got != 7654 {
		t.Fatalf("cost delta=%d,%v", got, ok)
	}
}
