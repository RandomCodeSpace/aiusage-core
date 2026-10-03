package adapter

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Shared filesystem/number helpers used by the adapters, so each one does not
// carry its own copy.

// NonNeg clamps a possibly-negative counter to zero.
func NonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// IsDir reports whether path exists and is a directory.
func IsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// FileStem returns the file name without directory or extension.
func FileStem(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// WalkEntryIsFile reports whether a directory-walk entry is usable as a source
// file. A walk reports each entry by its OWN metadata, so a dangling symlink
// arrives looking exactly like an ordinary file: it is not a directory, and it
// carries whatever extension the discovery filter is looking for. Discovery
// that trusts that hands the collector a source whose every read fails with
// ENOENT, which surfaces as a permanent per-cycle error against a tree nobody
// is going to repair.
//
// The common case costs no syscall: a plain file answers from the type bits the
// walk already carries. Only a symlink is resolved, because only a symlink can
// lie. Anything that is not ultimately a regular file is rejected, which also
// keeps a fifo out of a reader that would block on it forever.
func WalkEntryIsFile(d fs.DirEntry, path string) bool {
	if d == nil {
		return false
	}
	if d.Type()&fs.ModeSymlink == 0 {
		return d.Type().IsRegular()
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
