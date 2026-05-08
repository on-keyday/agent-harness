package protocol

import (
	"path"
	"strings"
)

// isAbsCrossOS reports whether p is absolute in either POSIX form
// ("/foo") or Windows drive-letter form ("C:/foo"). The latter is what
// Windows runners emit after filepath.ToSlash on a native path. The
// path package's IsAbs only recognizes POSIX form, so we explicitly
// accept drive-letter prefixes here. The drive letter itself is not
// case-normalized: callers are expected to pass back the same string the
// runner advertised (which is the case for the WebUI/CLI round-trip).
func isAbsCrossOS(p string) bool {
	if path.IsAbs(p) {
		return true
	}
	return len(p) >= 3 && isDriveLetter(p[0]) && p[1] == ':' && p[2] == '/'
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// IsUnderRoot reports whether repo is the same path as root or is contained
// within it, treating directory boundaries correctly.
//
// Wire-format contract: harness paths on the wire are slash-separated. They
// may be POSIX-absolute ("/home/foo") or Windows drive-letter absolute
// ("C:/Users/foo") — Windows runners convert via filepath.ToSlash before
// transmission. POSIX path.Clean is used either way; it treats "C:" as a
// regular path element and leaves it unchanged, so prefix matching works
// uniformly. That matters because the server and the runner can be on
// different OSes (e.g. Windows server + Linux runner, or Linux server +
// Windows runner) — using OS-native filepath here would convert separators
// inconsistently and fail prefix matches on what is in fact a valid
// absolute path the runner sent.
//
// This is the single source of truth for the allowed_roots prefix predicate
// shared by server (Registry.Candidates) and runner (Session.repoAllowed).
func IsUnderRoot(root, repo string) bool {
	return MatchLen(root, repo) > 0
}

// MatchLen returns a positive specificity score when repo is under root, and
// 0 otherwise. The score is the length of path.Clean(root) — a longer cleaned
// root means a more specific (deeper) match, so callers can rank competing
// roots by this score for longest-prefix-match selection.
//
// The match predicate itself follows IsUnderRoot's rules; see that doc for
// the wire-format / cross-OS contract.
func MatchLen(root, repo string) int {
	if !isAbsCrossOS(root) || !isAbsCrossOS(repo) {
		return 0
	}
	r := path.Clean(root)
	p := path.Clean(repo)
	if !strings.HasPrefix(p, r) {
		return 0
	}
	if len(p) == len(r) {
		return len(r)
	}
	// POSIX root "/" already has the separator at index 0.
	if r == "/" {
		return len(r)
	}
	// Boundary check: the next char in repo must be a separator, otherwise
	// /home/foo would falsely match a root of /home/fo.
	if p[len(r)] != '/' {
		return 0
	}
	return len(r)
}
