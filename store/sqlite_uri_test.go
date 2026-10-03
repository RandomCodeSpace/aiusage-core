package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

func TestLedgerOpensLiteralURICharacters(t *testing.T) {
	for _, name := range []string{"usage?mode=ro.db", "usage#fragment.db", "usage%3Fescaped.db", "usage space ü.db"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			ctx := context.Background()
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := st.InsertEvents(ctx, []model.UsageEvent{ev("literal", model.ToolCodex, time.Unix(1_750_000_000, 0), 17)}); err != nil || n != 1 {
				t.Fatalf("insert n=%d err=%v", n, err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			before := snapshotFile(t, path)
			ro, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			if sum, err := ro.Summarize(ctx, Filter{}); err != nil || sum.Totals.Total != 17 {
				t.Fatalf("literal read=%+v err=%v", sum, err)
			}
			if err := ro.Close(); err != nil {
				t.Fatal(err)
			}
			if verified, err := Verify(ctx, path); err != nil || verified.State != VerificationOK {
				t.Fatalf("verify=%+v err=%v", verified, err)
			}
			if after := snapshotFile(t, path); before != after {
				t.Fatalf("read mutated database before=%+v after=%+v", before, after)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != name && entry.Name() != name+"-wal" && entry.Name() != name+"-shm" {
					t.Fatalf("opened unintended filename %q", entry.Name())
				}
			}
			backup := filepath.Join(t.TempDir(), "backup?x#y%z.db")
			if result, err := Backup(ctx, path, backup); err != nil || result.RowCounts["usage_events"] != 1 {
				t.Fatalf("backup=%+v err=%v", result, err)
			}
			restored := filepath.Join(t.TempDir(), "restored?x#y%z.db")
			if err := restoreOnline(ctx, backup, restored); err != nil {
				t.Fatalf("restore literal backup: %v", err)
			}
			if result, err := Verify(ctx, restored); err != nil || result.State != VerificationOK || result.RowCounts["usage_events"] != 1 {
				t.Fatalf("restored=%+v err=%v", result, err)
			}
		})
	}
}

func TestReadOnlyLiteralURIMissingFileDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing?mode=rwc#x.db")
	if st, err := OpenReadOnly(path); err == nil {
		st.Close()
		t.Fatal("read-only open created missing ledger")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing target stat=%v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("created entries=%v", entries)
	}
}

func TestLedgerRelativeLiteralURIPath(t *testing.T) {
	t.Chdir(t.TempDir())
	path := "relative ?#%.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if uri := sqliteFileURI(path, true); !strings.Contains(uri, "%3F") || !strings.Contains(uri, "%23") || !strings.Contains(uri, "%25") {
		t.Fatalf("unescaped URI=%q", uri)
	}
}
