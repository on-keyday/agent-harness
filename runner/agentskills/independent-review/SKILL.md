---
name: independent-review
description: Use when you want a second opinion that does not inherit your framing — adversarial review of a diff or design, catching plausible-but-wrong work, or questioning whether the whole approach is right. Explains when a built-in subagent suffices vs. when to spawn a truly independent agent. For the harness-cli commands themselves, see the harness-cli skill.
---

# independent-review (adversarial second opinion)

A reviewer who shares your framing shares your blind spots. The value of a
second opinion is **independence** — it reasons from the work itself, not from
your justification of it. Reach for this when you need to catch
plausible-but-wrong output, or to surface that the premise itself is wrong, not
just the details.

## Pick the right independence level

| Need | Use | Why |
|------|-----|-----|
| **Mechanical checks** — untested claims, boundary breaks, spec violations, contradictions | **Claude Code built-in subagent** (`@`-mention it, or name it in your prompt) | Fresh context window, zero setup. The parent crafts the subagent's task prompt, so it inherits your framing — but that is fine here, because mechanical flaws don't depend on independence. |
| **Questioning the premise** — "is this approach even right?", subtle design-sense calls | **A harness-spawned agent** (`harness-cli session new -d`) | Boots a fresh claude on its own worktree with no shared context — true independence. A subagent cannot give this: its prompt is written by the very agent whose work is under review, so it inherits that agent's blind spots and self-justification. |

The built-in subagent is **claude-only**. The harness-spawned path works for any
agent and is the one to use when independence is load-bearing.

## The adversarial brief

Whichever you pick, tell the critic to **attack**, in two layers:

1. **Refute the details** — untested claims, boundary breaks, spec violations,
   contradictions. Most defects live here, and a subagent handles them well.
2. **Refute the premise** — list at least 3 ways the whole approach could be
   wrong, plus any alternative framing. Tell it explicitly **not** to assume the
   approach is correct.

Ask for **findings and evidence only — the critic must not decide for you.** A
summary substitutes for your judgment and can mislead (LLM summaries go wrong
plausibly); pointers to real flaws do not. Give the critic **the artifact, not
your reasoning** — reasoning from scratch is the whole point.

Independence has a limit worth stating: a critic is itself an LLM and shares
some of the worker's blind spots, so it reliably raises the floor on mechanical
flaws but does not replace your judgment on the subtle, premise-level calls.

## Driving a harness-spawned critic

Spawn it, hand it the artifact (push the files, or point it at a committed
branch — never your justification), send the adversarial brief, then **end your
turn** — its findings arrive through the inbox hook, not a blocking wait.

**For the actual commands — `session new -d`, `agent send`, `file push`, the
`chat.<short-id>` inbound-topic convention, and the async/inbox rules — see the
harness-cli skill** (`harness-cli skill harness-cli`, or
`.claude/skills/harness-cli/SKILL.md`). This skill is the *when / why*;
harness-cli is the *how*.
