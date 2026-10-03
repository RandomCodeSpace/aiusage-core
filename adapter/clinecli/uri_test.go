package clinecli

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteLiteralPathsRemainObservational(t *testing.T) {
	read := func(path string) (bool, error) {
		rows := indexRows(context.Background(), path)
		if len(rows) == 0 {
			return false, nil
		}
		if len(rows) != 1 || rows[0].sessionID != "sentinel" || rows[0].model != "model" {
			return false, fmt.Errorf("wrong index rows: %v", rows)
		}
		return true, nil
	}
	for label, name := range map[string]string{"question": "question?mode=rwc.db", "fragment": "fragment#.db", "percent": "percent%3F.db", "spaces": "with spaces.db", "unicode": "東京.db"} {
		for _, form := range []string{"absolute", "relative"} {
			t.Run(label+"/"+form, func(t *testing.T) {
				dir := t.TempDir()
				seed := filepath.Join(dir, "seed.db")
				db, err := sql.Open(driverName, seed)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`CREATE TABLE sessions(session_id TEXT,messages_path TEXT,cwd TEXT,workspace_root TEXT,provider TEXT,model TEXT); INSERT INTO sessions VALUES('sentinel','messages.json','/project','/workspace','provider','model')`); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, name)
				if err := os.Rename(seed, path); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if form == "relative" {
					t.Chdir(dir)
					path = name
				}
				for i := 0; i < 2; i++ {
					ok, err := read(path)
					if err != nil || !ok {
						t.Fatalf("intended sentinel not read: %v, %v", ok, err)
					}
				}
				if ok, _ := read(path + "-missing"); ok {
					t.Fatal("missing path unexpectedly read")
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("source bytes changed: %v", err)
				}
				afterInfo, err := os.Stat(path)
				if err != nil || afterInfo.Mode() != info.Mode() || !afterInfo.ModTime().Equal(info.ModTime()) {
					t.Fatalf("source metadata changed: %v", err)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 || entries[0].Name() != name {
					t.Fatalf("source read created alternate file/sidecars: %v, %v", entries, err)
				}
			})
		}
	}
}
