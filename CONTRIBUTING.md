# Contributing

## Text the harness injects into other repositories

Some files in this repository are not written for people working on it. The
runner writes them into every task worktree of every repository it drives:

- the skills under `runner/agentskills/` (embedded with `go:embed`), and
- the CLAUDE.md / AGENTS.md / GEMINI.md text `claudeMdMinimal` in
  `runner/agentskill.go`.

Most of their readers are agents in other repositories whose only harness
context is that text and `harness-cli`. When you write it:

- **Nothing that points into this repository.** No paths to its source or
  docs, no git history, no dated change notes. Files shipped beside a skill
  (its own `references/` or `scripts/`) are fine, because they travel with it.
  Provenance goes in the commit message.
- **No description of how the harness is implemented** that the reader cannot
  observe through `harness-cli` — internal size limits, truncation, buffering.
  The reader cannot check it, and it goes stale without anyone noticing. Write
  the rule the reader should follow instead.

## Editing an embedded skill

Edit `runner/agentskills/<skill>/` first, then copy the same files over
`.claude/skills/<skill>/` and `.agents/skills/<skill>/` in the same commit.
`make test` fails when a mirrored `SKILL.md` differs from the embedded one.
