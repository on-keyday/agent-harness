This task runs inside a harness-managed worktree.

- `harness-cli` is on PATH; `HARNESS_*` env vars are pre-set by the runner.
- For agent-to-agent messaging via the agentboard, consult the
  `harness-cli` skill at `.claude/skills/harness-cli/SKILL.md`.
- Reserved well-known topic for the initial handshake: `harness.hello`.

## Before writing or reviewing code in this repo

Read `.claude/skills/implementation-pitfalls/SKILL.md` — concrete past
failure modes on this project (spec scope contraction, sibling-code
pattern mismatch, peer.Conn vs objproto.Conn close semantics, bind-addr
vs dial-addr confusion, etc.) plus subagent dispatch prompt augmentation
checklists. Cheaper to read it now than to rediscover by symptom.
