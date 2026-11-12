package protocol

import "path"

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

// RootMatches reports whether repo IS root: both absolute, and equal after
// path.Clean. A repo below a root does not match (G9 of
// docs/superpowers/specs/2026-11-12-run-modes-and-records-grand-design.md):
// containment let a broad runner take a task silently while the repo's own
// runner was reconnecting, and roots exist to keep paths out of remote reach,
// which an explicit list serves better than a subtree.
//
// Wire-format contract: harness paths on the wire are slash-separated. They
// may be POSIX-absolute ("/home/foo") or Windows drive-letter absolute
// ("C:/Users/foo") — Windows runners convert via filepath.ToSlash before
// transmission. POSIX path.Clean is used either way; it treats "C:" as a
// regular path element and leaves it unchanged. That matters because the
// server and the runner can be on different OSes — OS-native filepath here
// would convert separators inconsistently. The drive letter is not
// case-normalized (see isAbsCrossOS).
//
// This is the single roots predicate, shared by the server
// (Registry.Candidates) and the runner (Session.repoAllowed).
func RootMatches(root, repo string) bool {
	if !isAbsCrossOS(root) || !isAbsCrossOS(repo) {
		return false
	}
	return path.Clean(root) == path.Clean(repo)
}
