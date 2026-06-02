package runner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/on-keyday/agent-harness/runner/agentskills"
)

// claudeMdMinimal is written to <worktree>/{CLAUDE,AGENTS,GEMINI}.md only when
// that file does not already exist. It tells a cold-started agent (claude,
// codex, gemini, …) that harness-cli + the bundled skill are available, how to
// read the skill in any agent, and that harness-injected files are not its work.
const claudeMdMinimal = `This task runs inside a harness-managed worktree.

- ` + "`harness-cli`" + ` is on PATH; ` + "`HARNESS_*`" + ` env vars are pre-set by the runner.
- Read the harness-cli skill for agent-to-agent messaging on the agentboard:
  run ` + "`harness-cli skill harness-cli`" + ` (works in any agent), or open
  ` + "`.claude/skills/harness-cli/SKILL.md`" + ` / ` + "`.agents/skills/harness-cli/SKILL.md`" + `.
- Reserved well-known topic for the initial handshake: ` + "`harness.hello`" + `.

Harness-injected files in this worktree are NOT your work — do not commit them
as your own: this file (CLAUDE.md/AGENTS.md/GEMINI.md), ` + "`.claude/`" + `, and
` + "`.agents/skills/`" + `. If you intentionally add project-specific content to
one of them, that addition IS legitimate work and may be committed.
`

// WriteAgentSkills materialises bundled skill files into
// <worktree>/.claude/skills/<name>/... and, when no CLAUDE.md already exists
// in the worktree, writes a minimal pointer CLAUDE.md.
//
// Skill files are always overwritten so that runner upgrades ship updated
// guidance to the agent. CLAUDE.md is never overwritten — the worktree's
// repository may already provide one with project-specific instructions.
func WriteAgentSkills(worktreeDir string) error {
	skillsDir := filepath.Join(worktreeDir, ".claude", "skills")

	err := fs.WalkDir(agentskills.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		dst := filepath.Join(skillsDir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := agentskills.FS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		return err
	}

	claudeMd := filepath.Join(worktreeDir, "CLAUDE.md")
	if _, statErr := os.Stat(claudeMd); statErr == nil {
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return os.WriteFile(claudeMd, []byte(claudeMdMinimal), 0o644)
}
