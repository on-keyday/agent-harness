# Agent Runtime Adapter Boundary

Date: 2026-07-02
Status: implemented for flag aliases and server-seeded inbound topics; adapter profile remains design guidance

## Problem

The harness started as a Claude Code runner, so several public names and hooks
used `claude` even after the protocol gained runtime-neutral fields such as
`agent_bin` and `skills_injected`. That creates two problems:

- Operators running Codex, Gemini, or custom binaries still see
  `--claude-bin`, `--claude-args`, and `--claude-arg`.
- The id-directed inbound topic was historically initialized by a Claude-only
  `.claude/settings.json` `SessionStart` hook, so non-Claude agents had to know
  to run `harness-cli agent subscribe --self`.

The adapter boundary must preserve compatibility with existing Claude scripts
while moving the common behavior out of Claude-specific hooks.

## Current Common Contract

These features are runtime-neutral and belong outside adapter-specific code:

- Task identity, auth tickets, and `HARNESS_*` environment injection.
- Agentboard topics, retained inbox, send/wait/inbox/subscribe commands, and
  wake delivery through `TaskWake`.
- Cross-tool instruction and skill injection through `AGENTS.md`, `GEMINI.md`,
  `.agents/skills/`, plus Claude-compatible `.claude/skills/`.
- Runner status fields `agent_bin` and `skills_injected`.
- The id-directed inbound topic convention:
  `chat.<first-8-hex-chars-of-task-id>`.

## Decisions

1. Add neutral CLI aliases:
   `--agent-bin`, `--agent-args`, and `--agent-arg`.
2. Keep `--claude-bin`, `--claude-args`, and `--claude-arg` as deprecated
   aliases. Do not break existing scripts.
3. Seed every task's `chat.<short-id>` subscription on the server when the
   agentboard ticket is registered for a runner assignment.
4. Remove the Claude-only `SessionStart` `subscribe --self` hook. Existing
   worktrees prune it through the existing stale harness hook mechanism.
5. Leave `UserPromptSubmit` inbox delivery as a Claude adapter feature for now.
   Non-Claude agents still need polling or a runtime-specific equivalent.
6. Do not add `WakeReason`, `stdin notice`, auto-inbox display state, or a new
   wire protocol field for this change.

## Adapter Boundary

An adapter is a thin runtime integration profile. It should describe how a
specific agent process accepts prompts, resumes conversation state, and exposes
hooks. It should not own harness identity, agentboard addressing, tickets, or
task lifecycle.

Common layer:

- Creates and revokes task tickets.
- Registers the task's self topic on each runner assignment.
- Delivers `TaskWake` to the runner that owns the task.
- Starts the configured binary with the configured argv templates.

Runtime adapter layer:

- Claude: `.claude/settings.json` `UserPromptSubmit` hook for inbox delivery,
  Claude-specific permission/resume flags, and `.claude/skills/` compatibility.
- Codex/Gemini/custom: no assumed hook contract. They can consume
  `.agents/skills/` and either poll `harness-cli agent inbox` or gain a future
  runtime-specific hook adapter.

## Server-Seeded Self Topic

The subscription state is keyed by `(runner_id, task_id)`. Because `runner_id`
comes from the runner connection id, it can change on runner reconnect or
resume onto another runner. A creation-time-only subscription is therefore
incorrect. The server must register the self topic whenever it issues the
agentboard ticket for a runner assignment.

The self-topic derivation lives in `agentboard.SelfTopic`, and CLI
`subscribe --self` delegates to the same function. This avoids separate
implementations of the `chat.<short-id>` convention.

`subscribe --self` remains useful as a manual repair or debugging command, but
it is no longer part of normal startup.

## Migration

- New docs and examples should prefer `agent-*` flags.
- Existing `claude-*` flags remain accepted without a removal date.
- Existing `.claude/settings.json` files containing harness-managed
  `SessionStart` subscribe hooks are cleaned the next time
  `WriteAgentSettings` runs.
- `harness.hello` remains opt-in discovery only.

## Tradeoffs

Functional:

- Server-seeded self topics remove one startup requirement from every agent
  runtime.
- Non-Claude agents still need an inbox delivery mechanism; this change does
  not make their inbox automatic.

Security:

- Tickets remain validated by the same registry. Agents cannot choose their
  sender identity.
- The server now asserts the conventional inbound subscription. This is less
  opt-out friendly than purely agent-owned subscription, but it matches the
  documented id-directed addressing contract.

Non-functional:

- Keeping deprecated aliases avoids script breakage at the cost of internal
  names such as `ClaudeBin` and `ExtraClaudeArgs` remaining for now.
- Moving self-topic registration to the server reduces runtime-specific hook
  coupling but makes the `chat.<short-id>` convention part of server behavior.
