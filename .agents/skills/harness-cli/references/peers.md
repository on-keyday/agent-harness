# Peers may not be claude — or skill-injected

Read with "Peers may not be claude" in the harness-cli SKILL.md.

Don't assume the agent on the other end of a topic is a Claude process that has read
this skill. A runner decides what it spawns and how it injects (see the
agent-runner flags):

- `--agent-bin` sets the peer binary — it defaults to `claude`, but a runner
  can point it at `bash` or any other program. Such a peer won't know the
  handshake or the JSON `kind` convention. Reply routing survives it: the
  destination is resolved server-side from `--in-reply-to`, so a peer that
  passes the seq back needs no convention at all.
- Agent command lines are template-driven. Claude defaults use
  `--continue` for resume-conversation, but non-Claude runners should set
  `--agent-oneshot-argv`, `--agent-resume-oneshot-argv`, and
  `--agent-resume-interactive-argv` to the target CLI's own submit/resume
  syntax. For Codex, oneshot resume needs the non-interactive subcommand
  shape `exec resume --last {args} {prompt}`; top-level `resume` is
  interactive.
- `--no-worktree` (without `--force-inject-harness-settings`) skips injecting
  `.claude/settings.json` and `.claude/skills/` — so even a claude peer there
  has neither this skill nor the automatic inbox hook: it won't auto-receive
  your messages or follow these conventions.

`ls` shows each task's agent identity: an `agent=<name>` column (the agent
profile the task runs under — `claude` / `gemini` / `codex` / `bash` …), with
`+skills` when the runner it was assigned to declares it injects harness
instructions + this skill. Injection is **cross-tool** — `AGENTS.md`/`GEMINI.md`/
`CLAUDE.md` pointers plus the skill under both `.claude/skills/` and
`.agents/skills/` — so `+skills` means a skill-aware peer regardless of agent.
`.claude/settings.json` rides the same injection, so a `+skills` peer has the
hook FILE whatever its runtime; whether that runtime reads it is the part the
marker cannot tell you. So:

- `agent=<anything>+skills` — a skill-following peer that was given the hook
  file. It receives automatically if its runtime reads that file, which Claude
  Code does and at least one other runtime does. Do not assume from the name.
- `agent=claude` (no `+skills`), or `agent=bash` — the assigned runner declares
  no injection: no skill and no hook file (e.g. a `--no-worktree` runner
  without force-inject). This peer polls or hears nothing.

Neither line is about being woken. **A one-shot task of any runtime cannot be
woken at all** — it has no stdin writer — so `submit`-created peers read their
inbox once at turn start and then only if they ask.

The marker rides on the **task**, not on the RUNNERS section — which matters
because `ls` withholds RUNNERS entirely from a caller whose visibility rank is
below `global`. If
you are confined, you see zero runners and every task row still carries its own
marker. `ls --json` splits it out as a per-task `skills_injected` bool beside
the bare `agent` name, so a script never has to parse the suffix off a string.

**It is a declaration, and three things it does not tell you:**

- **A missing marker is not proof of a bare agent.** A `Queued` task has no
  runner yet, so it has nothing to declare and renders without one. The TUI `d`
  detail popup separates the cases — `unknown (not assigned to a runner yet)`
  vs `not declared by the assigned runner`; a compact row cannot.
- **It reports configuration, not outcome.** The bit is
  `!--no-worktree || --force-inject-harness-settings`, sent once at handshake.
  The injection write itself is non-fatal, so `+skills` alongside a worktree
  with no `.claude/skills/` is a reachable state. If you are asking about your
  OWN worktree, look at the directory instead — that is an observation.
- **It does not tell you whether the peer is actually pushed.** The marker
  covers the hook FILE being injected; whether the peer's runtime reads it,
  and whether the peer is interactive at all (a one-shot cannot be woken),
  are both invisible here.

Behaviour is still the final word (does it complete the handshake?). This
narrows the guess; it does not remove it. In particular, never conclude "no
reply is coming" from a missing marker — send, end the turn, and let the
absence of a reply over time be the evidence.

What you *can* rely on: `harness-cli` itself is generally usable in those
environments, so the peer can still send/receive on the agentboard. Coordinate
defensively — explicit self-describing JSON, no assumption of an auto-inbox on
the other end, and graceful degradation when a handshake never completes.
