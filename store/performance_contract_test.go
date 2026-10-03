package store

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// TestLongLedgerAccelerationEquivalence is run by the production-performance
// job against long-ledger-v1. Ordinary package tests skip it because generating
// and scanning one million rows belongs in GitHub CI, not every edit loop.
func TestLongLedgerAccelerationEquivalence(t *testing.T) {
	dbPath := os.Getenv("AIUSAGE_PERF_DB")
	if dbPath == "" {
		t.Skip("AIUSAGE_PERF_DB is not set")
	}
	reader, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open long ledger: %v", err)
	}
	defer reader.Close()

	clock := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	filters := []Filter{
		{},
		{Since: clock.AddDate(0, 0, -1)},
		{Since: clock.AddDate(0, 0, -7)},
		{Since: clock.AddDate(0, 0, -30)},
		{GroupBy: []string{"day", "tool", "model"}},
		{Tools: []string{model.ToolCodex}, GroupBy: []string{"day"}},
		{Models: []string{"model-07"}, GroupBy: []string{"month"}},
		{Projects: []string{"/fixture/project-042"}, GroupBy: []string{"tool"}},
		{Sessions: []string{"session-0042"}, GroupBy: []string{"provider"}},
	}
	for _, dimension := range []string{"hour", "day", "week", "month", "tool", "model", "provider", "project", "session"} {
		filters = append(filters, Filter{Since: clock.AddDate(0, 0, -30), GroupBy: []string{dimension}})
	}

	ctx := context.Background()
	for _, filter := range filters {
		accelerated, err := reader.Summarize(ctx, filter)
		if err != nil {
			t.Fatalf("accelerated summary %+v: %v", filter, err)
		}
		direct, err := reader.summarizeLedger(ctx, filter)
		if err != nil {
			t.Fatalf("direct summary %+v: %v", filter, err)
		}
		if !reflect.DeepEqual(accelerated, direct) {
			t.Fatalf("summary mismatch for %+v\naccelerated: %#v\ndirect: %#v", filter, accelerated, direct)
		}

		acceleratedUnpriced, err := reader.UnpricedGroups(ctx, filter)
		if err != nil {
			t.Fatalf("accelerated unpriced %+v: %v", filter, err)
		}
		directUnpriced, err := reader.unpricedGroupsLedger(ctx, filter)
		if err != nil {
			t.Fatalf("direct unpriced %+v: %v", filter, err)
		}
		if !reflect.DeepEqual(acceleratedUnpriced, directUnpriced) {
			t.Fatalf("unpriced mismatch for %+v\naccelerated: %#v\ndirect: %#v", filter, acceleratedUnpriced, directUnpriced)
		}
	}
}
