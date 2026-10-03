package adapter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestErrSourceFormatWraps(t *testing.T) {
	err := fmt.Errorf("codex session: missing token_count discriminator: %w", ErrSourceFormat)
	if !errors.Is(err, ErrSourceFormat) {
		t.Fatalf("wrapped format error is not discoverable with errors.Is: %v", err)
	}
}

// entryFor returns the walk entry a directory walk would report for name, which
// is the only metadata the discovery filters get: Lstat semantics, so a symlink
// describes the link and not its target.
func entryFor(t *testing.T, dir, name string) fs.DirEntry {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range ents {
		if e.Name() == name {
			return e
		}
	}
	t.Fatalf("no entry named %q in %s", name, dir)
	return nil
}

func TestWalkEntryIsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "real.jsonl"), filepath.Join(dir, "linked.jsonl")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "absent.jsonl"), filepath.Join(dir, "dangling.jsonl")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "todir")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.jsonl"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	cases := []struct {
		name string
		want bool
		why  string
	}{
		{"real.jsonl", true, "an ordinary file is the whole point"},
		{"linked.jsonl", true, "a symlink that resolves to a file is a usable source"},
		{"dangling.jsonl", false, "a symlink to nothing reads as ENOENT on every cycle"},
		{"todir", false, "a symlink to a directory is not a source file"},
		{"sub", false, "a directory is not a source file"},
		{"pipe.jsonl", false, "opening a fifo blocks the collector indefinitely"},
	}
	for _, c := range cases {
		if got := WalkEntryIsFile(entryFor(t, dir, c.name), filepath.Join(dir, c.name)); got != c.want {
			t.Errorf("WalkEntryIsFile(%s) = %v, want %v: %s", c.name, got, c.want, c.why)
		}
	}

	if WalkEntryIsFile(nil, filepath.Join(dir, "real.jsonl")) {
		t.Error("a nil entry carries no metadata to trust and must be rejected")
	}
}
