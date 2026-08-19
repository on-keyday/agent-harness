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
- Your inbound agentboard topic is ` + "`chat.<first-8-hex-of-HARNESS_TASK_ID>`" + `;
  the server subscribes you to it. There is no board-wide discovery topic.

Harness-injected files in this worktree are NOT your work — do not commit them
as your own: this file (CLAUDE.md/AGENTS.md/GEMINI.md), ` + "`.claude/`" + `, and
` + "`.agents/skills/`" + `. If you intentionally add project-specific content to
one of them, that addition IS legitimate work and may be committed.
`

// WriteAgentSkills materialises the bundled skills into both the Claude
// (.claude/skills) and cross-tool (.agents/skills) locations, and writes a
// minimal instruction pointer to CLAUDE.md/AGENTS.md/GEMINI.md when each is
// absent. Skill files are always overwritten so runner upgrades ship updated
// guidance; pointer files are never overwritten — a project may provide its own.
//
// src is the skill source: nil means the embedded copy (agentskills.FS), which
// is what a plain runner uses. A non-nil src is the --agentskills-dir override
// (an os.DirFS), read fresh here — and this function runs per task assign
// (session.go handleAssign / handleOpenExec), which is what makes an edited
// SKILL.md reach the NEXT task without restarting the runner. The embedded copy
// cannot do that: a running process keeps its loaded binary, so its embed FS is
// frozen at launch even after `make build` replaces bin/agent-runner.
//
// src is a parameter rather than a package-level default so the compiler names
// every call site when the source becomes configurable; a silently-defaulting
// global would let a new caller keep reading the frozen embed unnoticed.
func WriteAgentSkills(worktreeDir string, src fs.FS) error {
	if src == nil {
		src = agentskills.FS
	}
	for _, root := range []string{
		filepath.Join(worktreeDir, ".claude", "skills"),
		filepath.Join(worktreeDir, ".agents", "skills"),
	} {
		if err := materializeSkills(root, src); err != nil {
			return err
		}
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", "GEMINI.md"} {
		if err := writePointerIfAbsent(filepath.Join(worktreeDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// materializeSkills copies the skill tree in src into destRoot, overwriting
// existing files.
//
// It walks per skill directory named by agentskills.ListFS rather than walking
// src wholesale: src may be an on-disk directory that holds more than skills
// (os.DirFS("runner/agentskills") also carries embed.go and agentskills_test.go),
// and a wholesale walk would write those into every task worktree's
// .claude/skills as if they were agent guidance.
func materializeSkills(destRoot string, src fs.FS) error {
	names, err := agentskills.ListFS(src)
	if err != nil {
		return err
	}
	for _, name := range names {
		walkErr := fs.WalkDir(src, name, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			dst := filepath.Join(destRoot, filepath.FromSlash(p))
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			if !d.Type().IsRegular() {
				return nil // skip symlinks/devices an on-disk source may contain
			}
			data, err := fs.ReadFile(src, p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			return os.WriteFile(dst, data, 0o644)
		})
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

// writePointerIfAbsent writes claudeMdMinimal to path only when no file exists
// there, leaving a project's own pointer file untouched.
func writePointerIfAbsent(path string) error {
	if _, statErr := os.Stat(path); statErr == nil {
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return os.WriteFile(path, []byte(claudeMdMinimal), 0o644)
}
