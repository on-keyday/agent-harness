# Connection visibility (objproto-level) — design

Date: 2026-06-25
Status: design approved (brainstorming), pending implementation plan.

## Problem statement

The server tracks every live wrapped objproto connection in
`server/server.go` `activeConns map[objproto.ConnectionID]streamingConn`
(registered at conn setup, deleted on teardown). This set is the ground truth
for "what is connected to the server right now" — clients (cli/tui/webui),
in-task agents, runners, **and connection attempts that never complete the
handshake** (probes, auth failures, half-open conns).

Today this set is observable **only** via the `SIGUSR1` `DumpTrsfState`
text dump on the server host. No client surface exposes it:

- `harness-cli ls` shows logical **runners + tasks** (the registry /
  TaskStore), not the raw connection set. Operator client conns
  (cli/tui/webui) and failed-handshake attempts never appear as
  "connections" anywhere a client can see.
- The operator runs a multi-host fleet and primarily uses the WebUI/TUI, not
  the server host's terminal. There is no way to answer "how many / who is
  connecting to my server right now?" from a client.
- This also has a security-observability angle: a `kind=Client` cap-escape
  attempt (see `project_cap_escape_kind_client_operator`) connects and is
  rejected at the gate — today that rejected attempt leaves no client-visible
  trace.

We want connection-level visibility, **including connection attempts that
never finish the handshake**, across **all three client surfaces**
(CLI + TUI + WebUI), capability-gated like `ls`.

### In scope

- **(a) Snapshot**: a point-in-time list of live connections + count. Each
  entry: connection id, role, remote address, principal task (for agents),
  connected-since.
- **(c) Live events**: a stream of connect / identify / disconnect events so a
  watcher sees connections appear and disappear in real time.
- **Surfaces**: CLI (`conns` + `conns -f`), TUI (a connections view),
  WebUI (a radial hub-and-spoke topology, mobile-degrading to a grouped list).
- **Capability gating**: `info_global` required for the global view; a
  confined task sees only its own subtree, consistent with `ls`.

### Out of scope (explicit)

- **(b) Time-series history graphs** of connection count over time. Not built.
- **trsf stream-internal gauges** (cwnd / accept-queue depth / sendTrigger
  rate). Already observable via the TUI `trsf` debug command
  (`tui/app.go`) and `SIGUSR1` `DumpTrsfState`. This design is the
  **objproto connection layer**, not the trsf stream layer.

## Architecture

```
 server.activeConns (CID -> streamingConn)        ← ground truth
   + identity map (RecordClientIdentity: CID -> ClientKind + principal task)
   + runner registry (CID -> runner)
        │  join + derive ConnRole
        ▼
   ConnInfo[]                          ConnStatusEvent (conns.status topic)
        │ (a) snapshot                       │ (c) live, pubsub
        │ TaskControlKind.list_conns         │ JoinAndGetStream("conns.status")
        ▼                                     ▼
   ┌────────────────────────────────────────────────────┐
   │ cli.ConnList / cli.WatchConns  (+ *With(client))     │
   └───────┬──────────────┬───────────────────┬──────────┘
       CLI conns      TUI conns view      WebUI topology
                                          (poll-diff, no event sub in wasm)
```

Two independent data paths reusing existing machinery:

1. **(a) snapshot** rides the streamed list-response pattern (mirrors
   `ListResult` / `ListResultBody`): a new `TaskControlKind.list_conns`
   returns a `stream_id`, server writes a `ConnListResultBody` to that trsf
   send-stream, client decodes. Streamed (not inline) for the same MTU reason
   as `ListResultBody`.
2. **(c) live events** ride the existing pubsub status-event pattern
   (mirrors `publishTaskEvent` / `publishRunnerEvent` and the `tasks.status` /
   `runners.status` topics): a new `conns.status` topic carrying
   `ConnStatusEvent`.

## Schema (single source — all additions in `runner/protocol/message.bgn`)

Per `feedback_no_split_schemas` / `feedback_no_schema_invisible_bytes`, the
full wire schema lives here, in one place, and is added in one plan task.

```
# --- ConnRole: the kind of a LIVE objproto connection. Distinct from
# ClientKind (which tags a TaskControl caller in ClientHello and never carries
# `runner`, since a runner does RunnerHello, not ClientHello). ConnRole is
# derived server-side and covers every conn kind including runner + the
# not-yet-identified state. ---
enum ConnRole:
    :u8
    unspecified   # handshake not yet completed (ConnOpened but not ConnIdentified)
    cli
    tui
    webui
    agent
    runner

# --- ConnInfo: one live (or just-opened/just-closed) connection. ---
format ConnInfo:
    cid_len :u8
    cid :[cid_len]u8            # ConnectionID canonical String() — the unique key
                               # used to correlate open/identified/closed events
    role :ConnRole
    remote_addr_len :u8
    remote_addr :[remote_addr_len]u8   # "ip:port"; WebUI groups by the ip portion
    principal_task :TaskID     # agent conn: its principal task id.
                               # all-zero for cli/tui/webui/runner/unidentified.
    connected_at :u64          # unix nano, stamped at activeConns register
    identified :u1             # 1 once role is established (handshake done);
                               # 0 = opened-but-never-authed (probe / failed)
    reserved :u7

# --- (a) snapshot: streamed body, mirrors ListResult / ListResultBody ---
format ConnListQuery:
    reserved :u8               # no filters in v1; reserved for forward-compat
                               # (server always returns the caller's visible set)

format ConnListResult:
    stream_id :u64             # server-initiated send-stream; client reads
                               # ConnListResultBody until EOF then decodes.
                               # 0 = no stream available (treat as error).

format ConnListResultBody:
    conns_len :u16
    conns :[conns_len]ConnInfo

# --- TaskControlKind: append (ordinals stay stable) ---
# enum TaskControlKind: ... existing ... , list_conns

# request union:   TaskControlKind.list_conns => list_conns :ConnListQuery
# response union:  TaskControlKind.list_conns => list_conns :ConnListResult

# --- StatusEventKind: append (ordinals stay stable) ---
# enum StatusEventKind: ... existing ... ,
#     conn_opened       # activeConns register; role likely unspecified
#     conn_identified    # identity established (ClientHello or RunnerHello);
#                        # carries the now-known role
#     conn_closed        # activeConns teardown

# --- (c) live event, mirrors TaskStatusEvent / RunnerStatusEvent ---
format ConnStatusEvent:
    kind :StatusEventKind      # conn_opened | conn_identified | conn_closed
    ts :u64
    info :ConnInfo
```

## Server

File: `server/server.go` (+ `server/capabilities.go` for gating,
`server/task_handler.go` for the identity-established hook point).

- Add `connectedSince time.Time` to `streamingConn` (currently
  `{Connection, trans}`); stamp it where `activeConns[cid] = wrapped` is set.
- **Three event emissions** (reuse the `publishTaskEvent` /
  `publishRunnerEvent` closures' pattern; publish to a new `conns.status`
  topic):
  - `conn_opened` — immediately after the `activeConns` register
    (role `unspecified`, `identified=0`). This is what makes failed/half-open
    handshakes and probes visible (the explicitly-chosen behavior).
  - `conn_identified` — at identity establishment. **Two hook points**:
    (1) client conns: where `RecordClientIdentity` records the `ClientKind`
    (`task_handler.go`, at ClientHello); (2) runner conns: at RunnerHello /
    registry register (alongside the existing `RunnerRegistered` runner event).
    Carries the now-known role.
  - `conn_closed` — in the existing teardown `defer` that deletes from
    `activeConns`.
- `ConnList(viewerSubtree)` — build `[]ConnInfo` by joining `activeConns`
  (CID, connectedSince; remote addr from the CID's AddrPort) × identity map
  (ClientKind → ConnRole + principal task) × runner registry (CID present →
  role runner). Conns with no identity yet → role `unspecified`, `identified=0`.
  Count is `len`.
- New handler for `TaskControlKind.list_conns`: allocate a send-stream, encode
  `ConnListResultBody`, return `ConnListResult{stream_id}` (copy the existing
  `list` handler shape exactly).

### Capability gating

- The global conn view requires `info_global` (the cap that already gates
  `ls` / `agent topics` / runner-list per `project_caps_enforcement_verified`).
- Without `info_global`, a confined task's `list_conns` returns only its own
  subtree: its own connection + descendant agent connections (same subtree
  filter `ls` applies to `ListResultBody`). Reuse that filter; do not invent a
  second visibility rule.
- The `conns.status` live subscription is gated the same way: a confined
  subscriber receives only events for conns in its subtree.

## CLI

File: `cli/conns.go` (new), wired into the `harness-cli` subcommand dispatch.

- `harness-cli conns` — snapshot. Table columns: remote-addr, role,
  principal-task (short), age (from connected_at), and an `unident` marker when
  `identified=0`. `--json` emits `ConnInfo` JSON Lines.
- `harness-cli conns -f` / `--follow` — subscribe to `conns.status` and stream
  `ConnStatusEvent` lines (reuse the `cli/notify_watch.go`
  `JoinAndGetStream(ctx, "...", topic)` pattern; text + `--json` line formatters
  like `notifyEvent*Line`).
- Expose **both** `ConnList(ctx, serverCID)` /
  `ConnListWith(ctx, c *Client)` and `WatchConns` / `WatchConnsWith`
  (per `feedback_reuse_long_lived_client` — TUI/WebUI call the `*With` variant
  against their existing long-lived client; never dial+close).

## TUI

File: `tui/` (new connections view + key binding; follow the existing view
pattern).

- A connections view rendering the snapshot, live-updated from the
  `conns.status` stream. **Sibling-pattern obligation**
  (`feedback_check_existing_patterns_before_extend`, Pitfall 3): grep how the
  existing task/runner status view threads `a.client` and consumes the
  `tasks.status` / `runners.status` watch; the new view MUST follow that exact
  pattern (`ConnListWith(a.client)` + `WatchConnsWith(a.client)`), not the
  CLI binary's dial+close form.

## WebUI

Files: `cmd/harness-webui-wasm/main.go` (new export), `webui/static/main.js`
(render), `webui/static/style.css`.

- **Desktop (radial hub-and-spoke, custom SVG, no new JS dependency)**: server
  node at center; one cluster per remote IP (the ip portion of `remote_addr`);
  each connection a leaf node on a spoke from its IP cluster to the server.
  Color encodes `role`; opacity/shade encodes age (connected_at). Nodes
  animate in on appear and out on disappear.
- **Mobile (`<=600px`)**: degrade to a grouped list — one card per IP listing
  its connections, with a connector line to a server node (the option-C
  layout). Same data, no force/SVG topology.
- **Liveness via snapshot poll + client-side diff** (locked decision): the
  WebUI reuses the existing ~5s snapshot poll
  (`webui/static/main.js refreshSnapshot`) extended with the conn list, and
  diffs successive snapshots to drive the in/out animation. It does **not**
  open a `conns.status` event subscription in wasm — the (c) event stream
  serves CLI/TUI. 5s granularity is acceptable for a topology view; if
  sub-5s liveness is later required, add an event subscription then.
- **Theme/responsive**: dark `#1e1e1e` / `#d4d4d4` palette and the `<=600px`
  layout from the first cut (`feedback_webui_dark_theme_and_mobile`); verified
  at desktop and 390px in Playwright (`project_playwright_webui_visual_check`).
- The snapshot data carrier: extend the WebUI snapshot path. Decision — the
  WebUI snapshot reshaper (`harnessSnapshot` in
  `cmd/harness-webui-wasm/main.go`) issues the new `list_conns` RPC alongside
  the existing `Snapshot` and merges `{conns:[...]}` into the JS payload, so
  the conn list rides the same poll cadence without a separate JS timer.

## Testing / verification

- **Unit**: `ConnList` join (cli/tui/webui/agent/runner/unidentified all map to
  the right `ConnRole`; principal_task only on agent); cap gating (a confined
  viewer sees only its subtree; `info_global` viewer sees all).
- **Integration**: open connections of several kinds (operator cli + a spawned
  agent + a runner), assert count and per-role entries; assert an
  opened-but-not-authed conn appears with `identified=0` then a `conn_closed`
  follows on teardown.
- **WebUI (Playwright, `project_playwright_webui_visual_check`)**: desktop +
  390px screenshots; dark theme; on a real connect/disconnect, assert a node
  animates in/out (and the mobile list adds/removes a row).

## Decisions taken (no implementer-punted choices)

1. **`conn_opened` fires at raw register** (role `unspecified`), so
   failed-handshake / probe / half-open conns are visible. `conn_identified`
   adds the role once known. (User-chosen "前者".)
2. **`ConnRole` is a dedicated enum**, not a reuse of `ClientKind` — keeps
   `ClientHello.kind` free of a `runner` value a client never sends.
3. **WebUI liveness = snapshot poll + diff**; no `conns.status` subscription in
   wasm. (c) events are for CLI/TUI.
4. **WebUI desktop = radial hub-and-spoke (custom SVG)**; mobile `<=600px`
   degrades to a grouped list. (User-chosen.)
5. **Gating reuses the existing `info_global` + subtree filter** from `ls`; no
   second visibility rule.
6. **Snapshot is streamed** (`ConnListResult.stream_id` + `ConnListResultBody`)
   mirroring `ListResult` / `ListResultBody`, not inline — same MTU rationale.
7. All surfaces (CLI + TUI + WebUI) ship together — no CLI-only first cut
   (user: "ui対応は後回しとかしないで全部やれ").
