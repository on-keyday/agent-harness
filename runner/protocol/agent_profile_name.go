package protocol

import "strings"

// windowsExecExts are the executable filename extensions Windows (and
// exec.LookPath via PATHEXT) commonly appends to a bare command name. A
// profile named "claude.exe" and one named "claude" denote the same agent
// binary spelled two ways — on this project's mixed Windows/Linux fleet the
// spelling depends on which host derived the name, so profile-name identity
// must not depend on it.
var windowsExecExts = []string{".exe", ".bat", ".cmd", ".com"}

// NormalizeAgentProfileName strips exactly one trailing Windows executable
// extension (case-insensitively) from an agent profile name. A name that is
// nothing but the extension is returned unchanged rather than normalized to
// the empty string, because the empty profile name already means "the
// default profile" everywhere.
func NormalizeAgentProfileName(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range windowsExecExts {
		if strings.HasSuffix(lower, ext) && len(name) > len(ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}

// EqualAgentProfileName reports whether two agent profile names denote the
// same profile: exact match modulo NormalizeAgentProfileName. Every
// profile-name comparison (server-side runner filtering, runner-side
// ProfileSet resolution) must go through this so a task recorded under
// "claude.exe" still resumes on a runner now advertising "claude" and vice
// versa.
func EqualAgentProfileName(a, b string) bool {
	return NormalizeAgentProfileName(a) == NormalizeAgentProfileName(b)
}
