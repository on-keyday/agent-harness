# Server-side port-forward registry: remote list & kill

**Date:** 2026-07-30
**Status:** Approved (brainstorming)

## Problem

`harness-cli forward <task> -L …` holds the foreground terminal until Ctrl-C
(`cmd/harness-cli/main.go:510-525`). With several such terminals open, the
operator loses track of which terminal owns which forward, and there is no way
to stop one without finding its terminal.

Verbatim: *"local port forwarding がこうターミナルいくつも開いてるとどのターミナル
でやったかわからんくなるからリモートから把握して切れるようにしたい"*

### Why it cannot be answered today

- A `-L` forward's listener is **client-local**: `cli.RunForward`
  (`cli/port_forward.go:164`) binds the listener and holds it in process
  memory. The server only ever sees a per-connection `OpenPortForward` request
  at the moment a connection is accepted (`server/port_forward.go:41-71`), so an
  **idle `-L` forward is invisible server-side**.
- A `-R` forward *is* registered server-side
  (`server/remote_forward_registry.go`, keyed by a server-assigned
  `forwardId`), but there is no RPC to enumerate or close a registration.
- The TUI's `ForwardsModal` (`tui/portforward.go:301`, spec
  `2026-07-22-tui-port-forward-list-design.md`) lists **only the forwards that
  TUI process itself opened**, read-only.

### Supersedes

`2026-07-22-tui-port-forward-list-design.md` declared out of scope: *"Global
cross-client view (`-L` forwards are inherently client-local and cannot be
enumerated globally)."* This design removes that limitation by having the
client register the forward with the server. The TUI modal built by that spec
is repointed at the server-side list rather than replaced.

## Decisions

1. **Scope: harness forwards only.** OS-level `ssh -L` tunnels are not
   enumerated. No `lsof`/`netstat`-style host probing.
2. **Goal is remote kill**, not locating the owning terminal. The listing
   carries only what is needed to disambiguate two similar entries, not
   PID/tty/hostname.
3. **State ownership: the client registers, the server holds the registry.**
   Rejected alternative — the server fanning a query out to each connected
   client — because **no server→client request direction exists**:
   `cli.Client.dispatchControl` (`cli/client.go:123`) decodes inbound
   `AppKind_TaskControl` payloads as `TaskControlResponse` and matches them by
   `request_id`; a server-initiated request would be mis-decoded and dropped.
   Building that direction is strictly more work than reusing the `-R`
   registry pattern, and kill needs a server→client push channel regardless.
4. **Register after a successful local bind; registration failure is fatal**
   to that forward. A forward that is running but not listed must not exist.
5. **`-R` migrates onto the same registration RPC.** One registration concept,
   one RPC. `OpenPortForward` is reduced to its per-connection meaning.

## Architecture

### Registry (server)

`remoteForwardRegistry` (`server/remote_forward_registry.go`) is generalised to
`portForwardRegistry`, holding both directions. Entry fields:

| Field | Source |
| --- | --- |
| `forwardID` u64 | `add()` assigns `next++` — **monotonic, so id order == creation order**; no timestamp field is needed |
| `direction` | Local / Remote, from the registration request |
| `taskIDHex`, `runnerID` | request + `TaskEntry.AssignedTo` |
| `control` | server-created bidi stream; the client picks it up by id (existing pattern) |
| `clientCxn`, `clientCID`, `clientKind` | the registering connection (`ConnHandle.ConnectionID()`, `h.clientKinds[cid]`) |
| `bindAddr`, `bindPort`, `targetHost`, `targetPort` | request; rendered in the listing |

The existing `pending` bind-result machinery (`addPending`/`signalBind`) stays
and is used by the `-R` branch only. `snapshot()` keeps working — it feeds the
trsf debug dump (`server/server.go:811`) — and gains the direction field.

### Lifecycle

1. **Register.** Client binds its listener (`-L`) or asks the runner to bind
   (`-R`, unchanged runner exchange), then registers. The server allocates the
   `forwardId` and the control stream and returns both.
2. **Live.** The control stream carries server→client events: per-connection
   notifications (`-R` only) and the close event (both directions).
3. **Deregister.** Whichever comes first:
   - the client's control stream EOFs (Ctrl-C, process death, transport loss) —
     detected by the existing `watchRemoteForwardControl` loop, generalised to
     both directions, which drops the registration and (for `-R`) tells the
     runner to stop listening;
   - a `KillPortForward` RPC arrives — the server pushes a `Closed{killed}`
     event, drops the registration, and (for `-R`) sends `ClosePortForward` to
     the runner.

   No heartbeat and no polling: the stray-terminal case self-heals because the
   terminal's death closes its control stream.
4. **`-L` registration never contacts the runner.** It validates only that the
   task is `Running` or `Detached`. Per-connection dialing keeps its existing
   path, including its existing `RunnerOffline` failure.

## Wire protocol

Client↔server only. **`runner/protocol` messages exchanged with the runner
(`RunnerOpenPortForwardRequest`, `ClosePortForwardRequest`, `RemoteForwardConn`,
`RemoteForwardBindResult`) are unchanged**, so runners do not need to ship in
lockstep. Clients and the server do.

All of the following goes in `runner/protocol/message.bgn`; `message.go` is
generated from it.

### New `TaskControlKind` members (appended — existing ordinals stay stable)

```
enum TaskControlKind:
    ...                        # 0..19 unchanged, through await_idle
    register_port_forward   # 20: register a -L/-R forward, get forward_id + control stream
    list_port_forwards      # 21: enumerate visible registrations
    kill_port_forward       # 22: close one registration by id
```

### Registration

```
# Direction decides the meaning of the address pair:
#   local  : bind_* is the CLIENT's listener; target_* is what the RUNNER dials.
#   remote : bind_* is the RUNNER's listener; target_* is what the CLIENT dials.
format RegisterPortForwardRequest:
    task_id         :TaskID
    direction       :PortForwardDirection
    bind_addr_len   :u16
    bind_addr       :[bind_addr_len]u8
    bind_port       :u16
    target_host_len :u16
    target_host     :[target_host_len]u8
    target_port     :u16

# Reuses OpenPortForwardStatus (ok / no_such_task / runner_offline /
# internal_error / bind_failed). A local registration can only return ok,
# no_such_task or internal_error: its listener is already bound client-side and
# the runner is not consulted.
format RegisterPortForwardResponse:
    status     :OpenPortForwardStatus
    forward_id :u64
    stream_id  :u64   # server-created control stream; 0 on failure
```

### Listing

```
format PortForwardListQuery:
    task_id :TaskID    # all-zero = every forward visible to the caller

format PortForwardListResult:
    stream_id :u64     # server-initiated send-stream carrying
                       # PortForwardListResultBody until EOF (mirrors ConnListResult).
                       # 0 = no stream available (treat as error).

format PortForwardInfo:
    forward_id      :u64
    direction       :PortForwardDirection
    task_id         :TaskID
    bind_addr_len   :u16
    bind_addr       :[bind_addr_len]u8
    bind_port       :u16
    target_host_len :u16
    target_host     :[target_host_len]u8
    target_port     :u16
    origin_kind     :ClientKind   # cli / tui / webui / agent — which surface holds it
    origin_cid_len  :u8
    origin_cid      :[origin_cid_len]u8   # ConnectionID canonical String(), as in ConnInfo.cid;
                                          # the only way to tell two identical specs apart

format PortForwardListResultBody:
    forwards_len :u16
    forwards     :[forwards_len]PortForwardInfo
```

### Kill

```
enum KillPortForwardStatus:
    :u8
    ok             = "ok"
    no_such_forward = "no_such_forward"
    internal_error = "internal_error"

format KillPortForwardRequest:
    forward_id :u64

format KillPortForwardResponse:
    status :KillPortForwardStatus
```

A forward the caller cannot see returns `no_such_forward`, not a distinct
"denied" — an invisible id must not become an existence oracle for a confined
agent. A caller that *can* see the forward but lacks the direction's capability
gets the existing `PermissionDenied` response kind via `denyTaskControl`, with
`required_cap` set (see Capabilities).

### Control-stream framing change

Today the `-R` control stream carries bare `RemoteForwardConnNotify` records and
the client slices them by **fixed length** (`cli/port_forward.go:251-267`:
`remoteForwardConnNotifySize = 8`, `parseConnNotifies`). A second record type
cannot be distinguished under that framing, so the stream moves to a tagged
record:

```
enum PortForwardEventKind:
    :u8
    conn_notify   # remote forwards only: dial your local target and pick up stream_id
    closed        # both directions: stop this forward

enum PortForwardCloseReason:
    :u8
    killed         # an operator/agent called kill_port_forward
    task_gone      # the owning task left Running/Detached (see Failure modes)

format PortForwardEvent:
    kind :PortForwardEventKind
    match kind:
        PortForwardEventKind.conn_notify => conn_notify :RemoteForwardConnNotify
        PortForwardEventKind.closed      => closed      :PortForwardClosed
        .. => error("Unexpected port forward event")

format PortForwardClosed:
    reason :PortForwardCloseReason
```

`RemoteForwardConnNotify` itself is unchanged; it becomes a variant payload.
The client parser is rewritten to consume tagged records across `ReadDirect`
boundaries (records are no longer a single fixed size, so the "consume whole
records, keep the remainder" loop must decode a length from the tag rather than
assume 8).

**Close is an explicit record, not a stream EOF.** EOF still happens when the
server or transport dies, and the two must stay distinguishable so a terminal
can print `killed remotely` versus `server connection lost`.

### Slimmed existing messages

With `-R` registration moved out, `OpenPortForward` means exactly one thing:
open the data stream for one accepted local-forward connection. Fields that
would no longer be read are removed rather than left lying on the wire:

```
format OpenPortForwardRequest:
    task_id         :TaskID
    remote_host_len :u16
    remote_host     :[remote_host_len]u8
    remote_port     :u16
    # removed: direction (always local now), bind_addr, bind_port

format OpenPortForwardResponse:
    status    :OpenPortForwardStatus
    stream_id :u64
    # removed: forward_id (was the -R registration handle)
```

`PortForwardDirection` stays — it is still used by `RegisterPortForwardRequest`,
`PortForwardInfo`, and the unchanged `RunnerOpenPortForwardRequest`.

### Rollout

Verified by inspection: the runner decodes exactly four port-forward messages —
`RunnerOpenPortForwardRequest` and `ClosePortForwardRequest` inbound
(`runner/port_forward.go:74`, `runner/connect.go:521`), `RemoteForwardBindResult`
and `RemoteForwardConn` outbound (`runner/port_forward.go:132,169`). None of them
change, and it never decodes `OpenPortForwardRequest`, `TaskControlRequest` or
`RemoteForwardConnNotify`. For a local forward the server's
`RunnerOpenPortForwardRequest` becomes byte-identical to today's — the only
difference is that `Direction` is now the literal `local` rather than a copy of a
client-supplied field. Killing a `-R` forward needs no new runner code:
`ClosePortForward` is already handled.

So **runner daemons do not have to ship with this change**. What does have to
ship together is the server and *every client binary*, including the
`bin/harness-cli` that in-task agents invoke on runner hosts — a client is a
client regardless of which host it sits on, and an old one would still send the
pre-slimming `OpenPortForwardRequest`.

## Client behaviour (`cli/port_forward.go`)

- `RunForward` gains a per-spec sub-context. Per spec: `net.Listen` → register →
  hold the control stream → start a reader goroutine → `acceptLoop`. A spec that
  fails to register aborts the whole call with an error (decision 4); listeners
  already bound for earlier specs are closed on the way out, exactly as the
  existing bind-failure path does (`cli/port_forward.go:170-176`).
- **`forward -L a -L b` produces two independent registrations**, killable
  separately. `RunForward` returns when every spec's sub-context is done, so the
  CLI process exits — and the terminal returns to a prompt — only once all of its
  forwards are gone.
- **Kill tears down established connections too.** `acceptLoop` attaches a
  watcher per accepted connection that closes the `net.Conn` and the trsf stream
  when the forward's sub-context is cancelled. `ssh -O cancel` leaves existing
  channels running; we do not, because the CLI process exits immediately after
  and would drop them anyway — matching TUI/WebUI-started forwards to the CLI's
  observable behaviour is worth more than ssh parity.
- `RunRemoteForward` / `ServeRemoteForwardControl` consume the same tagged
  events; `closed` stops the forward the same way.
- New client API used by all three surfaces:
  `(*Client).ListPortForwards(ctx, taskFilter string) ([]PortForwardInfo, error)`
  and `(*Client).KillPortForward(ctx, id uint64) error`. Both are plain
  `RoundTripTaskControl` calls on the long-lived `*Client` — no fresh dial.

## Surfaces

All three UIs get list + kill. **Starting a `-L` forward from the WebUI stays
absent**: a browser cannot bind a local listener. That is a functional
impossibility, not a scoping choice.

- **CLI** — `harness-cli forward ls [--task <task-id>] [--json]` and
  `harness-cli forward kill <forward-id>…`. Dispatch on `args[0]` before the
  task-id path; `ls`/`kill` can never collide with a task id because both
  contain non-hex letters.
- **TUI** — `ForwardsModal` (key `f`) switches its data source from
  `sortedForwards(a.activeForwards)` to a `ListPortForwards` RPC issued as a
  `tea.Cmd` on open, so it shows **every** visible forward, not just this
  process's. Columns become `id · dir · task · spec · origin`. An `x` key arms a
  y/n confirmation and `y` kills the selected row via the RPC, then refreshes.
  Not `k`: the pinned bubbles table binds `k` to LineUp and `tui/` never rebinds
  it, so `k` would kill the row a vim-reflex user was scrolling past — and `x`/`X`
  is already this repo's key for destructive full-screen-overlay actions. The
  confirm is there because the row may belong to another operator's session and
  only that owner can re-establish it. The tasks pane's `P`/`B`
  stop-picker keeps working, but its stop action also goes through
  `KillPortForward` — the process-local `PortForwardSession.Cancel` is no longer
  a second way to stop a forward, it is just the plumbing the `closed` event
  triggers. `PortForwardStoppedMsg` already fires on every exit path
  (`tui/app.go:656`), so a remotely-killed TUI forward leaves `activeForwards`
  with no new bookkeeping.
- **WebUI** — `harness.snapshot()` (`cmd/harness-webui-wasm/main.go:82`) gains a
  `forwards` array, rendered in the existing 接続 tab (`webui/index.html`
  `data-tabgroup="n"`) next to the connection list, following the
  `renderConnList` pattern in `webui/static/main.js`. Each row gets a kill
  button wired to a new `harness.forwardKill(id)` export. Dark palette and the
  ≤600px layout apply as with every other panel.

## Capabilities

Composed from existing gates; no new capability bit and no new gating concept.

- **register** — `Capability_ForwardLocal` / `Capability_ForwardRemote` by
  direction. This is the gate currently applied to `OpenPortForward`
  (`server/task_handler.go:370-379`), moved to the registration kind. The
  per-connection `OpenPortForward` keeps requiring `ForwardLocal`.
- **list** — mirrors `handleListConns` (`server/task_handler.go:1355-1374`): an
  operator (zero principal) or an `InfoGlobal` holder sees everything; any other
  caller sees only forwards whose task is in its own subtree per
  `visibleToCaller`.
- **kill** — visibility first (invisible → `no_such_forward`), then the target
  forward's direction capability, reported through the existing
  `denyTaskControl` path. The direction is not known until the registry is
  consulted, so this check lives in the handler rather than the dispatch switch.

## Failure modes

- **Server or transport dies.** The control stream EOFs with no `closed`
  record. The client stops the forward and reports `server connection lost`,
  distinct from `killed remotely`. `harness-cli forward` does not use
  `PersistLoop`, so it exits.
- **Owning task leaves Running/Detached.** Reaped **lazily, in the list
  handler**: an entry whose task is no longer live gets a `closed{task_gone}`
  push and is dropped before the response is built. We deliberately do *not*
  wire a task-terminal interceptor: terminal status is reached from `Finish`,
  `Cancel`, `MarkFailed` and the runner-loss paths in `server/taskstore.go`, and
  hooking a shared transition at some of its call sites is a failure mode this
  repo has already paid for. Consequence, accepted: between a task dying and
  anyone running `forward ls`, its forwards stay listed and connections through
  them fail with the existing `NoSuchTask`.
- **Local bind fails** (port in use). Nothing is registered — bind precedes
  registration — and the CLI exits with the existing listen error.
- **Registration fails.** The forward aborts (decision 4). The just-bound
  listener is closed on the way out.
- **Two clients kill the same id concurrently.** `registry.remove` returns
  `(entry, ok)`; the loser sees `ok == false` and answers `no_such_forward`.

## Testing

- `runner/protocol/port_forward_test.go` — round-trip every new message;
  `PortForwardEvent` tag dispatch for both variants.
- `cli/port_forward_test.go` — the rewritten record parser: records split
  across `ReadDirect` boundaries, several records coalesced in one read, and a
  trailing partial record retained for the next read.
- `server/port_forward_test.go` — register → list → kill → registration gone;
  control-stream EOF deregisters; lazy reap on a task that left Running;
  visibility filtering for a confined caller; concurrent kill returns
  `no_such_forward` to exactly one caller.
- `integration/port_forward_test.go` — end-to-end `-L`: register, list from a
  *second* client, kill, and assert the first client's `RunForward` returned and
  its listener port is free.
- `tui/portforward_test.go` — `ForwardsModal` renders server rows, `x` arms the
  confirm, `y` issues a kill, `n` does not, and `j`/`k` still navigate.
- **Live check before claiming done** (`.claude/skills/dummy-harness`): stand up
  a dummy server + runner, open `harness-cli forward -L` in one terminal, run
  `forward ls` and `forward kill` from another, and confirm the first terminal
  returns to its prompt with `killed remotely`. Then the same list/kill from the
  TUI modal and from the WebUI panel in a browser.
- `make check` and `make wasm-check` before landing.

## Out of scope

- Enumerating or killing OS-level `ssh -L` tunnels.
- Origin metadata beyond `origin_kind` + `origin_cid` (no PID, tty, hostname).
- Reconnect/re-registration of a forward across a server restart.
- Starting a forward from the WebUI (browser cannot bind a listener).
- Per-forward traffic counters or connection counts in the listing.
