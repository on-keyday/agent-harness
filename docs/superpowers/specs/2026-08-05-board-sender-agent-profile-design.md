# Sender agent-profile attestation on the agentboard

Date: 2026-08-05

## Problem

- **A delivered message says where its sender runs, never what it is.**
  `DeliveredMessage` carries `from_runner_id`, `from_task_id`,
  `from_hostname` and nothing else about the sender
  (`agentboard/agentboard.bgn:86-98`), surfaced to the receiving agent as
  `from.{runner_id,task_id,hostname}` (`cli/agent/json_emit.go:21-31`).
  Those are machine coordinates. Which agent runtime produced the message
  is absent.

- **The receiver's next action depends on the answer.** Delivery to a peer
  is not uniform across runtimes: the auto-inbox hook lives in Claude's
  `.claude/settings.json`, so a gemini / codex peer sees a message only if
  it polls `harness-cli agent inbox` itself (`.claude/skills/harness-cli/SKILL.md`,
  "Non-Claude agents still need an inbox path"). An agent that cannot tell a
  Claude peer from a non-Claude one cannot know whether "I sent it" implies
  "it will be read".

- **The server knows, but the receiver has no route to it.**
  `TaskEntry.AgentProfile` (`server/taskstore.go:40-45`) is resolved at
  Create/Resume and exposed as `TaskInfo.agent_profile`
  (`runner/protocol/message.bgn:516-517`) — but only through `ls` /
  `session ls` / `board topics`, all gated on `Capability_InfoGlobal`
  (`server/capabilities.go:32-33`, `server/agent_handler.go:358-372`). A
  confined worker spawned with narrowed `--caps` cannot reach it at all,
  while board delivery itself is uncapped. The gap is not "the data is
  missing", it is "the data sits behind a capability the receiver of a
  message is not required to hold".

- **A profile is per-open, not per-task, so read-time lookup is wrong.**
  `TaskStore.Resume` overwrites `e.AgentProfile` (`server/taskstore.go:275`;
  the field is documented as per-open at `:234`), and `session new --resume
  <task-id>` deliberately reuses the task id — and therefore its
  `chat.<short-id>` inbound topic. One topic can hold messages published by
  codex and, after a resume, by claude, under a single task id. The topic
  ring outlives the `taskState` that produced its entries
  (`agentboard/topic.go:21-50` vs `agentboard/board.go:147` Revoke), so any
  scheme that answers "what is this task" at *read* time misattributes
  history.

## Non-goals

- **Role / purpose of the sender** ("reviewer", "implementer"). The server
  cannot verify it, so it stays a payload convention.
- **Model identity.** A profile name is a runner-local launch-profile label
  (`runner/agent_profile.go:14-32`); `"claude"` advertised by two runners is
  not a claim of equal version or model. This field carries the runtime
  family the harness launched, nothing more.
- **New discovery RPC or capability change.** No new agentboard message
  kind, no change to what `info_global` gates.

## Design

Freeze the sender's agent profile into the message at publish time. This is
the path `from_hostname` already takes: captured into the `taskState` at
attach and stamped into each `RetainedMessage` when the topic ring appends
(`agentboard/topic.go:33-50`). The profile rides the same rails, so a
message published under codex still reads as codex after the task id is
resumed under claude.

### Where the value comes from

The server resolves it; the agent never supplies it.

- `establishAgentIdentity` (`server/agent_handler.go:107-120`) looks up
  `s.tasks.Get(<hex task id>).AgentProfile` and passes it to `Board.Attach`
  alongside the hostname.
- `Board.RegisterTask` (`agentboard/board.go:81-93`) seeds the same
  `taskState` at dispatch time and must pass the profile too, because
  `Attach` calls `setIdentity` wholesale and would otherwise clobber a
  value seeded there. Its three call sites already have the value: the task
  entry at `server/dispatch.go:198` (`task.AgentProfile`, already read at
  `:123`), the resolved profile at `server/task_handler.go:1022`, and a
  `s.tasks.Get` on the replay path at `server/server.go:1148`.

### Meaning of the empty string

`""` means **the server could not attribute a runtime**. It never means
"runner default": both the submit and the open-interactive paths resolve to
a concrete name before `Create` (`server/task_handler.go:611-613`, `:949`).
Two cases produce it:

1. **Server-originated publishes** — `server/await_idle_handler.go:165` calls
   `Board.Send` directly with a placeholder runner id and hostname
   `"server"`, with no task behind it. This is the case that actually occurs
   today. Consumers that need to recognise it use the existing
   `from_hostname == "server"` plus placeholder runner id; no sentinel
   profile value is introduced.
2. **The store lookup misses** at hello time. Not reachable on any current
   path — the ticket registry `Validate` passes only for a (rid, tid) that
   `RegisterTask` seeded from a live task entry — so this is a defined
   fallback rather than an observed case: the encoder emits an empty field
   instead of failing.

The `Board == nil` degrade at `server/agent_handler.go:108-110` is not a
third case: it returns before `Attach`, leaving `ac.helloed` unset, so
`agentHandleSend` rejects the publish outright and no message is produced.

### Wire changes

Both formats gain the same pair, placed immediately after the existing
`from_hostname` pair so the sender-attestation fields stay contiguous:

```
from_agent_profile_len :u8
from_agent_profile :[from_agent_profile_len]u8
```

1. `agentboard/agentboard.bgn` — `DeliveredMessage` (line 86) and
   `RetainedMeta` (line 192).
2. `runner/protocol/message.bgn` — `BoardMessageRow` (line 673), the
   operator board view.

Regenerate with `make protoregen ARGS='agentboard/agentboard.bgn'` and
`make protoregen ARGS='runner/protocol/message.bgn'`.

This is a breaking wire change: an old decoder reading a new
`DeliveredMessage` misparses. Roll out **server first** — an old server
paired with a new runner is the failure mode recorded for wire changes on
this project.

### In-memory changes

| File | Change |
|---|---|
| `agentboard/topic.go` | `RetainedMessage` += `FromAgentProfile string`; `topic.append` takes and stores it |
| `agentboard/taskstate.go` | `setIdentity` / `identity` carry the profile |
| `agentboard/conn.go` | `ConnState.Identity()` returns it as a 4th value |
| `agentboard/board.go` | `Attach(rid, tid, hostname, agentProfile)`, `RegisterTask(rid, tid, ticket, agentProfile)`, `Send(topic, payload, fromRid, fromTid, fromHost, fromProfile)` |
| `server/agent_handler.go` | resolve at hello; thread through `agentHandleSend` (`:151`), inbox / wait / deliver builders, and `agentHandleListRetained` |
| `server/board_handler.go` | `handleBoardRead` sets the new `BoardMessageRow` field (`:68-74`) |

### Surfaces

The field is sender attestation, so every place that renders sender
attestation gets it. Partial adoption reappears later as an inconsistency
report.

| Surface | Site | Rendering |
|---|---|---|
| Agent JSON (`inbox` / `wait` / `dispatch`) | `cli/agent/json_emit.go:26-30` | `from.agent` |
| `agent retained` | `cli/agent/retained.go:93-94` | `"from_agent"` |
| Operator CLI `board read` | `cli/board.go:24-26,86-88`; print at `cli/cmd_board.go:51` | `agent=` |
| TUI board view | `tui/board.go:283` header, `:353` list line | `agent=` after `host=` |
| WebUI board view | `cmd/harness-webui-wasm/main.go:685` (`agentProfile`); `webui/static/main.js:3451-3455` | span after the `host=` span |

### Skill / documentation

`.claude/skills/harness-cli/SKILL.md` documents `from.agent` and what to do
with it: when a peer's `from.agent` is not `claude`, do not assume the
auto-inbox hook delivered your message — say so explicitly in the payload or
expect the peer to poll. The embed source `runner/agentskills/` is the
source of truth and the `.claude/skills/` copy must be synced, then the
binary rebuilt.

## Testing

- **The regression this design exists for** (`agentboard` unit): publish to a
  topic with the taskState holding profile A, re-attach the same (rid, tid)
  with profile B, publish again, then read the retained ring and assert the
  first entry still reports A.
- **Attested end to end** (`agentboard/e2e_test.go`, `cli/agent/*_e2e_test.go`):
  `from.agent` equals the task's resolved profile and is not agent-supplied —
  assert it holds even when the sending client puts a contradictory value in
  the payload body.
- **Unknown path**: a server-originated await-idle publish yields
  `from.agent == ""` with `from.hostname == "server"`.
- **Operator read path** (`cli/board_e2e_test.go`): `board read` rows carry
  the profile.
- `make check` and `make wasm-check` before landing; `./...` alone hides
  explicit-pattern breaks.

## Rejected alternatives

- **A `task_id → profile` resolution RPC, uncapped.** Zero per-message
  overhead, but it answers with the *current* profile and therefore
  misattributes every retained message published before a profile-changing
  resume — the exact case `--resume` makes routine. It also costs a new
  message-kind pair, a CLI surface, and a fresh capability decision.
- **Sender self-declares, in the payload or in `AgentInfo`.** Unverifiable
  by the server, and it leaves the harness's own vocabulary encoded as a
  payload convention while the authoritative value sits in the taskstore.
  `AgentInfo.Hostname` is client-supplied today, but hostname has no
  server-side authority to defer to; the profile does.

## Cost

About 7 bytes per delivered message for a typical profile name, and
`DeliveredMessage` appears in arrays in inbox / wait responses
(`msgs_len :u16`). Accepted: correctness under resume is not obtainable from
a read-time lookup at any price.
