This task runs inside a harness-managed worktree.

- `harness-cli` is on PATH; `HARNESS_*` env vars are pre-set by the runner.
- Read the harness-cli skill for agent-to-agent messaging on the agentboard:
  run `harness-cli skill harness-cli` (works in any agent), or open
  `.claude/skills/harness-cli/SKILL.md` / `.agents/skills/harness-cli/SKILL.md`.
- Reserved well-known topic for the initial handshake: `harness.hello`.

Harness-injected files in this worktree are NOT your work — do not commit them
as your own: this file (CLAUDE.md/AGENTS.md/GEMINI.md), `.claude/`, and
`.agents/skills/`. If you intentionally add project-specific content to
one of them, that addition IS legitimate work and may be committed.
