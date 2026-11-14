package transcript

import (
	"os"
	"path/filepath"
)

// openRegular opens base/rel for reading only if it is a regular file reached
// without following a symlink, and is still the file that was checked.
//
// An agent can write into its own store — a sandbox mounts it read-write — so
// a file there can be swapped for a symlink to anything on the runner host
// between listing and reading, or be a FIFO or a device that never ends
// (review of pillar B Tasks 2–4, C1 and I2). os.Root keeps every path
// component inside base, and the opened file must be the one Lstat saw, a
// regular file: a symlink (os.Root follows one that stays inside base), a
// FIFO or a device swapped in between is refused. openFlags keeps that open
// from blocking on a FIFO (Task 5 review, I1).
func openRegular(base, rel string) (*os.File, os.FileInfo, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	rel = filepath.FromSlash(rel)
	lfi, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, nil, ErrNoFile
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, nil, ErrNoFile
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(lfi, fi) {
		f.Close()
		return nil, nil, ErrNoFile
	}
	return f, fi, nil
}

// isRegular reports whether path is a regular file itself, not a symlink to
// one: candidates are filtered by it before anything opens them.
func isRegular(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}
