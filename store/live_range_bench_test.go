package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func BenchmarkLiveSnapshotSummary(b *testing.B) {
	path := os.Getenv("AIUSAGE_PERF_DB")
	if path == "" {
		b.Skip("AIUSAGE_PERF_DB is not set")
	}
	reader, err := OpenReadOnly(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { reader.Close() })
	for _, dim := range []string{"day", "model", "session"} {
		for _, exact := range []bool{false, true} {
			name := dim + "/aligned"
			until := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
			if exact {
				name = dim + "/exact"
				until = until.Add(-time.Second)
			}
			b.Run(name, func(b *testing.B) {
				f := Filter{Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Until: until, GroupBy: []string{dim}}
				b.ReportAllocs()
				for b.Loop() {
					if _, err := reader.Summarize(context.Background(), f); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
	for _, ledger := range []bool{true, false} {
		name := "populated_tail/accelerated"
		if ledger {
			name = "populated_tail/ledger"
		}
		b.Run(name, func(b *testing.B) {
			f := Filter{
				Since:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
				Until:   time.Date(2026, 8, 30, 12, 15, 42, 0, time.UTC),
				GroupBy: []string{"model"},
			}
			b.ReportAllocs()
			for b.Loop() {
				query := reader.Summarize
				if ledger {
					query = reader.summarizeLedger
				}
				if _, err := query(context.Background(), f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
