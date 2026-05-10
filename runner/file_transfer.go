package runner

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrPathInvalid is returned by ValidateRelPath when the input cannot be
// safely resolved inside the worktree root. Callers map this to
// FileTransferStatus_PathInvalid / ListFilesStatus_PathInvalid.
var ErrPathInvalid = errors.New("rel path invalid")

// ValidateRelPath resolves a worktree-relative POSIX path against worktreeRoot.
// Returns the joined absolute path on success.
//
// Rejected:
//   - absolute paths (must be relative to the worktree)
//   - paths containing a NUL byte
//   - paths that, after filepath.Clean, escape worktreeRoot via "..".
//
// An empty rel string resolves to worktreeRoot itself (used by ls of the
// root directory). Trailing slashes are normalized away.
func ValidateRelPath(worktreeRoot, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", ErrPathInvalid
	}
	if rel == "" {
		return filepath.Clean(worktreeRoot), nil
	}
	if filepath.IsAbs(rel) {
		return "", ErrPathInvalid
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", ErrPathInvalid
	}
	full := filepath.Join(worktreeRoot, cleaned)
	rootClean := filepath.Clean(worktreeRoot)
	// Defense in depth: confirm the join did not escape the root.
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", ErrPathInvalid
	}
	return full, nil
}
