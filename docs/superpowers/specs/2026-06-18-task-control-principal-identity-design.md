# Unified connection identity via ClientHello (P1b, consolidated)

Date: 2026-06-18
Status: design — awaiting review

## Problem

There are two separate hello handshakes, each on its own app-kind, with
asymmetric authentication:

- **Task-control** (`appwire.AppKind_TaskControl` = 0x41): a connection announces
  `ClientHello{kind}` (`runner/protocol/message.bgn:213`, `cli/hello.go:25`)
  carrying only `ClientKind` (cli/tui/webui). The server records connID→kind
  (`server/task_handler.go:277-282`) for origin attribution. **No verifiable
  identity.** An in-task agent's `harness-cli file pull` / `cancel` / `submit` is
  byte-for-byte indistinguishable from the operator's CLI.
- **Agentboard** (`appwire.AppKind_AgentMessage` = 0x44): a connection sends
  `AgentBridgeHello{runner_id, task_id, auth_ticket}`
  (`agentboard/agentboard.bgn:34-37`, `cli/agent/conn.go:232`), validated by
  `agentboard.Registry.Validate(...) → HelloStatus` (`agentboard/registry.go:49`).
  On success the server attaches a `ConnState` via `Board.Attach(rid,tid,host)`
  (`server/agent_handler.go:108`) keyed per-connID
  (`agentConn`, `agent_handler.go:17-35`); every later agent op gates on
  `ac.helloed` and reads identity from `ac.state.Identity()` (e.g.
  `agent_handler.go:131`).

The two hellos are separate only because each `harness-cli` subcommand is a
separate short-lived process that dials its own `peer.Conn`. **It is not a
protocol constraint**: both `cli.Client` (`cli/client.go:28,48`) and
`cli/agent.Conn` (`cli/agent/conn.go:174`) own a `*peer.Conn` from the same
`DialPeerConn`, and the app-kind is a per-message prefix byte — one connection
can carry both 0x41 and 0x44, routed per-message by the server.

## Decision

Collapse the two hellos into one: **`ClientHello` becomes the single connection
identity hello**, and `AgentBridgeHello` (plus its message/response/enum) is
deleted. A connection asserts identity once, via `ClientHello`; both the
task-control handler and the agentboard handler read that one established
identity.

This dissolves the credential-duplication question entirely: with a single hello
there is no second format to keep in sync.

The identity primitives that already work are **reused unchanged**:
`agentboard.Registry.Validate`, `Board.Attach`/`Board.Detach`,
`ConnState.Identity()`, the per-connID `agentConn` store. Only *where the hello
is received and decoded* changes.

## Scope

P1b — establish a **verified principal identity** per connection, surfaced for
attribution, and unify the two hello paths onto it.

- "Verified": a connection that claims `kind=agent` must present a valid ticket
  or the hello is rejected (`bad_ticket`/`unknown_task`/`runner_mismatch`).
- Operator clients (CLI without an injected ticket, TUI, WebUI) send
  `kind=cli`/`tui`/`webui` and remain the unrestricted "operator" principal.

### Non-goals

- **No resource gating.** No task-control handler rejects an operation based on
  principal. An identified agent can still `cancel` any task / `file pull` any
  worktree / `forward` anywhere. Gating is deferred (later phases). The recorded
  principal is consulted by no gating handler in P1.
- **No forcing task-control identity.** The server verifies claims; it cannot
  force an in-task client to send `kind=agent`. A client sending `kind=cli` is
  treated as operator. Acceptable because P1 gates nothing — it only weakens
  *attribution* for a deliberately-evasive agent. (The agentboard path, by
  contrast, still effectively requires a valid agent identity, because every
  agent op gates on `ac.helloed`.)

## Design

### Schema

**`runner/protocol/message.bgn`** — `ClientHello` carries the optional credential
(conditional field idiom, mirrors `if x11_enabled == 1` at `message.bgn:127`).
`RunnerID` (`message.bgn:319`) and `TaskID` (`message.bgn:71`) already exist in
this package.

```
enum ClientKind:
    :u8
    unspecified
    cli
    tui
    webui
    agent                          # NEW

format AgentInfo:                  # NEW — the credential triple + hostname
    runner_id   :RunnerID
    task_id     :TaskID
    auth_ticket :[16]u8
    hostname    :[..]u8            # was AgentBridgeHello.Hostname (optional)

format ClientHello:
    kind :ClientKind
    if kind == ClientKind.agent:   # bytes on the wire only for agents
        agent_info :AgentInfo

enum ClientHelloStatus:            # mirrors agentboard HelloStatus (registry.go:12-15)
    :u8
    ok = "ok"
    bad_ticket                     # NEW
    unknown_task                   # NEW
    runner_mismatch                # NEW
```

**`agentboard/agentboard.bgn`** — delete the now-dead hello surface:

- remove `format AgentBridgeHello`, `format AgentBridgeHelloResponse`
- remove `AgentMessageKind.hello`, `AgentMessageKind.hello_response` and their
  `match` arms (`agentboard.bgn:162-163`)
- the `HelloStatus` enum is superseded by `protocol.ClientHelloStatus`; either
  remove it or keep it solely as the `Validate` return type. `registry.Validate`
  continues to return its current type; the server maps it to
  `protocol.ClientHelloStatus`.

All on-wire bytes remain schema-described; nothing moves to convention.

### Server

1. **Receive identity at the ClientHello.** `handleClientHello`
   (`server/task_handler.go:265`), when `kind == agent`:
   - convert `protocol.RunnerID`/`protocol.TaskID` → agentboard types (a
     server-side converter mirroring `protoToBoardRunnerID`/`protoToBoardTaskID`,
     currently client-only at `cli/agent/conn.go:127-140`),
   - `status := Board.Registry().Validate(rid, tid, ticket)`,
   - on `Ok`: establish the per-connID `agentConn` — `ac.state =
     Board.Attach(rid, tid, host)`, `ac.helloed = true` — via a Server-provided
     hook (see below). Respond `ClientHelloStatus_Ok`.
   - on non-Ok: respond the mapped status; attach nothing.
   - `Board == nil` (test wiring, `task_handler.go:58-59`): degrade to
     attribution-only (record claimed identity unverified); no error.
   - `kind != agent`: unchanged (record kind, respond Ok).
2. **Wiring across structs.** `agentConn`/`getOrCreateAgentConn`/`Board.Attach`
   live on `Server` (`agent_handler.go:22`); `handleClientHello` is a
   `TaskHandler` method. Add a hook on `TaskHandler` (e.g.
   `OnAgentHello func(conn, rid, tid, host) ClientHelloStatus`) that `Server`
   implements with `getOrCreateAgentConn` + `Board.Attach`. (Cleaner future:
   promote ClientHello handling to `Server.handleConnection` as a connection-layer
   identity step, since it is no longer task-control-specific. Out of scope for
   P1 — keep it a `TaskControlKind` with a hook.)
3. **Agentboard handler.** `handleAgentMessage` (`agent_handler.go:59`): remove
   the `AgentMessageKind_Hello` case and `agentHandleHello`. Subsequent
   `Send`/`Subscribe`/… on the same connID find `ac.helloed` already true from
   the ClientHello. The `!ac.helloed` gates stay (now meaning "no ClientHello
   identity on this connection"). `removeAgentConn` (`Board.Detach`) is unchanged.
4. **Attribution.** A task created over a `kind=agent` connection is tagged
   origin `agent` (a distinct `ClientKind` from `cli`), flowing through the
   existing origin path (`lookupClientKind` → `handleSubmit`/`handleOpenInteractive`).
   Recording *which* principal (creator task id) on the task itself is P2 lineage
   and is out of scope; P1 only distinguishes agent-origin from operator-origin.

   **Resume attribution.** Today `TaskInfo.origin_kind`
   (`runner/protocol/message.bgn:385`, `TaskEntry.OriginKind`) is set only at
   `Create` and is **sticky** — `TaskStore.Resume` (`server/taskstore.go:200`)
   takes no origin, so a task keeps its first creator's kind across every resume.
   That loses the fact that, e.g., an agent resumed an operator-created task —
   which is now expressible because `agent` is a `ClientKind`. P1 therefore adds a
   second attribution field, `resumed_by_kind :ClientKind`:
   - `origin_kind` stays the **first creator** (sticky, unchanged).
   - `resumed_by_kind` records the `ClientKind` of the connection that performed
     the **latest** resume (overwritten each resume); `Unspecified` until first
     resumed.
   - It records the **kind only** (agent/cli/tui/webui). Recording the resuming
     agent's specific task id is lineage = P2, out of scope.

### Client

1. **Agentboard client** (`cli/agent/conn.go` `ConnectAgent`): after PSK, send
   `TaskControlRequest{ClientHello{kind=agent, agent_info{rid,tid,ticket,host}}}`
   (app-kind 0x41) instead of `AgentMessage{Hello}` (0x44); wait for
   `ClientHelloResponse` instead of `HelloResponse`. Then send agentboard
   `Send`/`Subscribe`/… (0x44) on the same connection as today. The
   `protoToBoardRunnerID`/`protoToBoardTaskID` converters move server-side (they
   are no longer needed for the client hello, which now sends `protocol` types).
2. **Task-control commands.** The in-task task-control subcommands send
   `ClientHello{kind=agent, agent_info}` resolved from `HARNESS_RUNNER_ID` /
   `HARNESS_TASK_ID` / `HARNESS_AUTH_TICKET` / `HARNESS_HOSTNAME` (resolution
   exists: `cli/cliopts`, `cli/agent/conn.go:23`). `SayHello` (`cli/hello.go:23`)
   gains an agent-aware variant. Commands that hand-roll
   `SayHello(ClientKind_Cli)` (`cmd/harness-cli/main.go:117,257,276,397`) call
   it; commands that send no hello today (`cancel`, `logs`, `prune`,
   `session attach`) now send one first on the same connection.
3. **Operator surfaces** (operator CLI without a ticket, TUI, WebUI): unchanged —
   send `kind=cli`/`tui`/`webui`, no `agent_info`.

## Components & boundaries

- **Schema** — one credential format (`AgentInfo`) in `protocol`; agentboard
  hello surface deleted. Single source of truth, no duplication.
- **Identity primitives** (`Registry.Validate`, `Board.Attach/Detach`,
  `ConnState`) — reused unchanged; only the call site moves to
  `handleClientHello` via the `OnAgentHello` hook.
- **ClientHello receive** — the one new decision point (validate + attach).
- **Client hello send** — unified: `cli/agent` and task-control commands both
  emit `ClientHello{kind=agent}`.

## Error handling

- Forged/unregistered claim → `bad_ticket`/`unknown_task`/`runner_mismatch`;
  client surfaces a hello failure (same shape as `cli/hello.go:36` and the
  agentboard client's current `status != Ok` exit, `conn.go:250`).
- Missing env triple in an in-task task-control client → falls back to
  `kind=cli` (operator semantics); no crash.
- `Board == nil` → attribution-only degrade; no validation error.

## Testing

- Schema round-trip: `ClientHello{kind=agent,...}` carries `agent_info` bytes;
  `kind=cli` carries none.
- Server: valid ticket → `ac.helloed` + `ac.state` set, `Ok`; wrong ticket →
  `bad_ticket`, no attach; unregistered → `unknown_task`; runner mismatch →
  `runner_mismatch`; `kind=cli` → unchanged; `Board==nil` → attribution-only.
- Agentboard over the unified hello: ClientHello{agent} then `Send`/`Subscribe`
  on the same connection works without an AgentBridgeHello; sender attribution
  still comes from `ac.state.Identity()`.
- Update `agentboard/e2e_test.go` hello helper (`:181-205`) to send ClientHello;
  `Board.Attach`/`Validate`-direct tests (`board_test.go`, `registry_test.go`)
  are unaffected.
- Client: in-task env present → `kind=agent` + triple; absent → `kind=cli`.
- E2E: a task submitted over a `kind=agent` connection shows origin `agent` in
  `ls`; an operator CLI submit shows `cli`.
- Resume attribution: a task created `cli` then resumed over a `kind=agent`
  connection keeps `origin_kind=cli` and gains `resumed_by_kind=agent`; an
  un-resumed task has `resumed_by_kind=Unspecified` (renders blank in `ls`).
- Regression: operator CLI/TUI/WebUI flows unaffected (no gating added).

## Migration / rollout

Single-user dogfood — no compatibility shim.

**Runner↔server is unaffected.** Runner registration uses `RunnerHello`
(`runner/connect.go:239` ↔ `server/runner_handler.go:120`), a separate handshake
on the RunnerMessage/RunnerRequest family. The runner binary sends neither
`ClientHello` nor `AgentBridgeHello` (confirmed — no usage in `runner/` or
`cmd/agent-runner/`, only the shared generated `protocol` definitions). Its wire
behavior does not change.

**What changes wire behavior:** the `ClientHello`/`AgentBridgeHello` hello paths,
spoken by the **server** and by the hello-speaking clients — in-task
`harness-cli` (both task-control and agentboard paths), **TUI**, **WebUI**. These
must be rebuilt and restarted together: an old client's `AgentBridgeHello` hits a
server that no longer decodes it, and vice versa.

**What merely recompiles:** every binary that imports the regenerated
`protocol`/`agentboard` packages — including the runner — must be rebuilt to
compile, even though the runner's handshake is unchanged. So the deploy is a full
fleet rebuild + server restart + runner restart, but only the
server/CLI/TUI/WebUI hello *behavior* differs. Treat as a quality fix, not a
versioned migration.

## Future phases (out of scope, recorded for continuity)

- **Promote ClientHello to the connection layer** — handle it in
  `Server.handleConnection` (like the PSK gate) rather than as a `TaskControlKind`,
  making identity a first-class connection property both app protocols read.
- **P2 lineage** — server records which principal created which task.
- **P3 topic namespacing** — server-owned/derived `chat.<task-short>` or
  capability-granted topic prefixes, so topic scope becomes enforceable.
- **Capability tokens + enforcement** — gate each chokepoint (topic / task-id /
  repo / runner-cid / host:port) by the principal's capability set; operator =
  root.
