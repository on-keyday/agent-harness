package protocol

import (
	"path"
	"strings"
)

// IsUnderRoot reports whether repo is the same path as root or is contained
// within it, treating directory boundaries correctly.
//
// Wire-format contract: harness paths on the wire are POSIX-style ('/'
// separator). This function uses the path package (POSIX-only) rather than
// path/filepath, so it behaves identically regardless of the OS the caller
// runs on. That matters because the server and the runner can be on
// different OSes (e.g. Windows server + Linux runner) — using OS-native
// filepath here would make the Windows-running server convert '/'-paths to
// '\'-paths and then fail filepath.IsAbs on what is in fact a valid POSIX
// absolute path the runner sent.
//
// Both arguments must be POSIX-absolute (start with '/'). Linux runners
// already produce POSIX-abs paths natively; future Windows runners should
// convert via filepath.ToSlash before transmission.
//
// This is the single source of truth for the allowed_roots prefix predicate
// shared by server (Registry.Candidates) and runner (Session.repoAllowed).
func IsUnderRoot(root, repo string) bool {
	if !path.IsAbs(root) || !path.IsAbs(repo) {
		return false
	}
	r := path.Clean(root)
	p := path.Clean(repo)
	if !strings.HasPrefix(p, r) {
		return false
	}
	// Exact match counts as "under".
	if len(p) == len(r) {
		return true
	}
	// Special case: root is POSIX root "/". Any absolute path is under it,
	// and the boundary char IS the separator at index 0.
	if r == "/" {
		return true
	}
	// Boundary check: the next char in repo must be a separator, otherwise
	// /home/foo would falsely match a root of /home/fo.
	return p[len(r)] == '/'
}
