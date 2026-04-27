package protocol

import (
	"path/filepath"
	"strings"
)

// IsUnderRoot reports whether repo is the same path as root or is contained
// within it, treating directory boundaries correctly. Both arguments are
// filepath.Clean'd and require absolute paths; callers that pass relative
// paths get false.
//
// This is the single source of truth for the allowed_roots prefix predicate
// shared by server (Registry.Candidates) and runner (Session.repoAllowed).
// Server and runner MUST use this same function so they cannot disagree on
// what "is in allowed_roots" means.
func IsUnderRoot(root, repo string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(repo) {
		return false
	}
	r := filepath.Clean(root)
	p := filepath.Clean(repo)
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	// On Windows, filepath.Rel returns an absolute path when root and repo
	// are on different drive letters; treat that as "not under root".
	return !filepath.IsAbs(rel)
}
