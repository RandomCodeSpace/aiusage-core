package opencode

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
)

func TestSQLiteSourceLiteralPathsAreReadOnly(t *testing.T) {
	for label, name := range map[string]string{"question": "question?mode=rwc.db", "fragment": "fragment#.db", "percent": "percent%3F.db", "spaces": "with spaces.db", "unicode": "unicode-東京.db"} {
		for _, relative := range []bool{false, true} {
			t.Run(label+map[bool]string{true: "/relative", false: "/absolute"}[relative], func(t *testing.T) {
				dir := t.TempDir()
				original := writeDB(t, dir, [][3]string{{"sentinel", "s", msgData("sentinel", "s", 150)}})
				path := filepath.Join(dir, name)
				if err := os.Rename(original, path); err != nil {
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
				if relative {
					t.Chdir(dir)
					path = name
				}
				src := adapter.Source{Path: path, Meta: map[string]string{"kind": kindDB}}
				obs := incremental(t, src, nil)
				if len(obs.Events) != 1 || obs.Events[0].DedupKey != "opencode|sentinel" || obs.Checkpoint == nil {
					t.Fatalf("wrong source: %+v", obs)
				}
				idle := incremental(t, src, obs.Checkpoint)
				if len(idle.Events) != 0 || idle.Checkpoint != nil {
					t.Fatalf("literal path replay = %+v", idle)
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
					t.Fatalf("source read created an alternate file or sidecar: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestSQLiteMissingLiteralPathCreatesNothing(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(map[bool]string{true: "relative", false: "absolute"}[relative], func(t *testing.T) {
			dir := t.TempDir()
			name := "missing?mode=rwc#%東京.db"
			path := filepath.Join(dir, name)
			if relative {
				t.Chdir(dir)
				path = name
			}
			obs, err := (Adapter{}).Collect(t.Context(), adapter.Source{Path: path, Meta: map[string]string{"kind": kindDB}})
			if err == nil || len(obs.Events) != 0 || obs.Checkpoint != nil {
				t.Fatalf("missing source accepted: %+v, %v", obs, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("missing source read created files: %v, %v", entries, err)
			}
		})
	}
}
