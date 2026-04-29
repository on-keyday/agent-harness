This task runs inside a harness-managed worktree.

- `harness-cli` is on PATH; `HARNESS_*` env vars are pre-set by the runner.
- For agent-to-agent messaging via the agentboard, consult the
  `harness-cli` skill at `.claude/skills/harness-cli/SKILL.md`.
- Reserved well-known topic for the initial handshake: `harness.hello`.
