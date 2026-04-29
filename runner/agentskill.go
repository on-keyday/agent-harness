package runner

import (
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:agentskills
var agentSkillsFS embed.FS

// claudeMdMinimal is written to <worktree>/CLAUDE.md only when no CLAUDE.md
// already exists in the worktree. Its sole job is to ensure a cold-started
// agent learns that harness-cli and the bundled skill are available, even
// before Claude Code surfaces skill descriptions in its system reminder.
const claudeMdMinimal = `This task runs inside a harness-managed worktree.

- ` + "`harness-cli`" + ` is on PATH; ` + "`HARNESS_*`" + ` env vars are pre-set by the runner.
- For agent-to-agent messaging via the agentboard, consult the
  ` + "`harness-cli`" + ` skill at ` + "`.claude/skills/harness-cli/SKILL.md`" + `.
- Reserved well-known topic for the initial handshake: ` + "`harness.hello`" + `.
`

// WriteAgentSkills materialises bundled skill files into
// <worktree>/.claude/skills/<name>/... and, when no CLAUDE.md already exists
// in the worktree, writes a minimal pointer CLAUDE.md.
//
// Skill files are always overwritten so that runner upgrades ship updated
// guidance to the agent. CLAUDE.md is never overwritten — the worktree's
// repository may already provide one with project-specific instructions.
func WriteAgentSkills(worktreeDir string) error {
	const root = "agentskills"
	skillsDir := filepath.Join(worktreeDir, ".claude", "skills")

	err := fs.WalkDir(agentSkillsFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		if rel == "" {
			return nil
		}
		dst := filepath.Join(skillsDir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := agentSkillsFS.ReadFile(p)
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
