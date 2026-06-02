package runner

// HarnessInjectedPaths lists worktree-relative paths the runner injects on
// behalf of the agent (settings, skills, minimal CLAUDE.md). These are
// excluded from the worktree's "did the user/agent do real work?" check
// performed by WorktreeManager.RemoveIfClean — modifications here are
// always the runner's own doing, not work to preserve.
//
// Entries that name a directory must end with `/` so prefix-matching in
// the dirty-check distinguishes `.claude/skills/foo` (injected) from a
// hypothetical `.claude/skillsmate` (not injected).
//
// Keep this list in sync with the writers in this package (settings.go,
// agentskill.go) — adding a new injected file without listing it here will
// make worktrees with that file appear "dirty" and stop being cleaned up.
var HarnessInjectedPaths = []string{
	"CLAUDE.md",
	"AGENTS.md",
	"GEMINI.md",
	".claude/settings.json",
	".claude/skills/",
	".agents/skills/",
}
