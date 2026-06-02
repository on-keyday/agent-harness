// Package agentskills embeds the harness agent skill files so both the runner
// (which injects them into task worktrees) and harness-cli (which prints them
// on demand) share one copy. It imports only the standard library.
package agentskills

import "embed"

//go:embed all:harness-cli
var FS embed.FS

// Skill returns the SKILL.md bytes for a named skill (e.g. "harness-cli").
func Skill(name string) ([]byte, error) {
	return FS.ReadFile(name + "/SKILL.md")
}
