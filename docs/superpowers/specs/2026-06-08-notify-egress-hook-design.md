# notify — server-mediated notification egress hook

- **Date:** 2026-06-08
- **Status:** Design (approved for plan)
- **Scope:** New `harness-cli notify` subcommand → `TaskControlKind.notify` wire
  message → server exec hook → operator-supplied external "translation layer"
  command that performs the actual delivery (Telegram / ntfy / Discord / custom).

## 1. Problem / motivation

The operator drives long-lived agent tasks across several runner slots and is
frequently away from the terminal. There is no way for a running task — or the
operator from another shell — to push a short text notification that reaches the
operator's phone when no WebUI/TUI client is attached.

The existing pubsub status stream (`tasks.status` / `runners.status`) is an
**inward fan-out to connected clients**; it cannot reach a phone whose browser
tab is closed. Native browser push (Web Push / service worker) does not exist in
the WebUI and fits poorly with the LAN-closed, multi-host topology (it needs an
external push service + HTTPS).

The chosen direction is **server-side egress**: the harness relays a notification
to an operator-configured external command, which owns delivery to whatever
medium the operator uses. This reaches the phone with the client disconnected,
and keeps all external-service code + secrets **out of the harness** (and out of
this public repo).

## 2. Non-goals

- No in-harness integration with any specific chat/push service. The harness
  invokes one opaque command; the operator's script does the rest.
- No delivery guarantee. The server ack means "accepted / hook launched", not
  "delivered". Retry/back-off/medium-specific formatting belong to the external
  script.
- No inbound path. This is egress-only — it opens no new externally-reachable
  surface (contrast with the chat-bot "channels" model, which has an inbound
  leg).
- No persistent notification queue or catch-up for disconnected clients.
- No long-text transport. Notifications are short one-liners (see §7).

## 3. Architecture

```
 caller (agent task | operator shell)
   │  harness-cli notify "text" [--title T] [--level info|warn|error]
   │     · reads HARNESS_* env → origin metadata (worker) or origin=external
   │     · client-side MTU truncate guard (see §7)
   ▼
 TaskControlRequest{ kind=notify, NotifyRequest } ── objproto SendMessage ──▶ server
   │                                                  (single message, MTU-bound)
   ▼
 server TaskHandler.Handle  ── case TaskControlKind.notify ──▶ handleNotify
   │     · assembles hook JSON (wire fields + server-injected conn_id, ts)
   │     · exec.CommandContext(--notify-hook), stdin=JSON, env=HARNESS_NOTIFY_*
   │     · Start() → accepted; reap in goroutine w/ timeout; bad path → spawn_failed
   │     · no hook configured → no_hook
   ▼
 NotifyResponse{ status } ──▶ caller (CLI exits; agent does not wait — see §8)
   │
 external "translation layer" command (operator-owned, outside repo)
       reads JSON on stdin → delivers to Telegram / ntfy / Discord / custom
```

`notify` rides the existing **`TaskControlKind`** control RPC channel (which
already carries non-task control such as `client_hello`, `dial_runner`,
`open_port_forward`), **not** a new `AppKind`. This reuses `request_id`
correlation (`RoundTripTaskControl`) and the existing dispatch path with minimal
plumbing.

## 4. Schema (`runner/protocol/message.bgn`)

Add `notify` to the control-kind enum and two match arms; add the new formats.

```
enum TaskControlKind:        # existing enum — append one value
    :u8
    submit
    list
    cancel
    prune_tasks
    get_task_log
    open_interactive
    client_hello
    attach_session
    open_file_transfer
    list_files
    dial_runner
    open_port_forward
    notify                   # ← added

enum NotifyLevel:
    :u8
    info
    warn
    error

enum NotifyOrigin:
    :u8
    worker
    external

# Origin metadata, present only when the caller ran inside a worker (a task /
# bash-worker shell with HARNESS_* env). Carried as text (not the typed
# RunnerID) deliberately: a zero-valued protocol.RunnerID trips the IpAddrLen
# assertion and panics the encoder, and the hook only needs display strings.
format WorkerInfo:
    task_id_len   :u16
    task_id       :[task_id_len]u8
    runner_id_len :u16
    runner_id     :[runner_id_len]u8
    repo_len      :u16
    repo          :[repo_len]u8
    hostname_len  :u16
    hostname      :[hostname_len]u8

format NotifyRequest:
    level  :NotifyLevel
    origin :NotifyOrigin
    if origin == NotifyOrigin.worker:
        worker :WorkerInfo
    title_len :u16
    title     :[title_len]u8     # len=0 → no title
    text_len  :u16
    text      :[text_len]u8      # bounded; client guarantees MTU fit (see §7)

enum NotifyStatus:
    :u8
    accepted        # hook process launched (NOT a delivery guarantee)
    no_hook         # server has no --notify-hook configured; request ignored
    spawn_failed    # exec of the configured hook failed to start

format NotifyResponse:
    status :NotifyStatus
```

Match arms:

```
# in TaskControlRequest match kind:
        TaskControlKind.notify => notify :NotifyRequest
# in TaskControlResponse match kind:
        TaskControlKind.notify => notify :NotifyResponse
```

**Every byte on the wire is described by the schema.** When `origin == external`
the `WorkerInfo` block is absent entirely (no empty-length padding); when
`origin == worker` it is present. The discriminant is the explicit `origin`
enum, so "sent from outside a worker" is a first-class, unambiguous state — not
inferred from empty fields.

Server-injected context (`conn_id`, receive `ts`) is **not** on the wire; the
server adds it only into the hook's stdin JSON (§6), keeping the wire schema to
exactly the caller-provided bytes.

## 5. Origin population (CLI)

`harness-cli notify` reads its own process environment:

| env (set by the runner inside a worker) | example                       | WorkerInfo field |
|-----------------------------------------|-------------------------------|------------------|
| `HARNESS_TASK_ID`                       | `0f0d4dd6…`                   | task_id          |
| `HARNESS_RUNNER_ID`                     | `ws:…:55538-13478`            | runner_id        |
| `HARNESS_REPO_PATH`                     | `…/remote-agent-harness`      | repo             |
| `HARNESS_HOSTNAME`                      | `gmkhost`                     | hostname         |

- `HARNESS_TASK_ID` present → `origin = worker`, fill `WorkerInfo` from env.
- absent → `origin = external`, omit `WorkerInfo`.

One subcommand serves both the agent and the operator. An operator firing from
the `bash` worker shell has the same `HARNESS_*` env as an agent, so it is
correctly reported as `worker` (identical conditions, no separate "agent
subcommand" needed). An operator firing from outside any worker is reported as
`external`.

Origin is **caller-asserted** (the CLI reads its own env; a caller could lie).
This is acceptable because origin is contextual display data for the human
reading the notification — it is **never** an authorization input. The
hook-invocation trust boundary is the server's PSK auth, independent of origin.

## 6. Hook contract (server → external command)

- Server config: `--notify-hook <command>` flag (env fallback
  `HARNESS_NOTIFY_HOOK`). Empty → feature is a no-op (`NotifyStatus.no_hook`);
  the feature is strictly opt-in.
- `<command>` is treated as a path to an executable, invoked **directly via
  argv, never through a shell**. The notification text is passed on **stdin**,
  never as an argument, so caller text presents no shell-injection surface. An
  operator who needs arguments/pipelines wraps them in their own script.
- **stdin** — one JSON object:

  ```json
  {
    "level": "info|warn|error",
    "origin": "worker|external",
    "title": "…",
    "text": "…",
    "task_id":  "0f0d4dd6…",                 // worker origin only
    "runner_id":"ws:…:55538-13478",          // worker origin only
    "repo":     "/…/remote-agent-harness",   // worker origin only
    "hostname": "gmkhost",                    // worker origin only
    "conn_id":  "ws:…-NNNN",                 // server-injected: caller's conn
    "ts":       1717800000                    // server-injected: receive unix ts
  }
  ```

- **env** (convenience duplicates for quick scripting; full payload is on
  stdin): `HARNESS_NOTIFY_LEVEL`, `HARNESS_NOTIFY_ORIGIN`, `HARNESS_NOTIFY_TITLE`.
- **Spawn semantics:** `exec.CommandContext` with a bounded timeout
  (default 10s). `Start()` succeeds → respond `accepted` immediately and reap
  the process in a goroutine, killing it if it exceeds the timeout (a slow or
  hung sink must not wedge the server). `Start()` fails (e.g. command not found /
  not executable) → respond `spawn_failed`. The server does **not** wait for hook
  completion, so `accepted` is "launched", not "delivered".

This is the server's **first** spawn of an external process (today only the
runner execs claude/git). The exec surface is kept minimal: no shell, payload on
stdin, bounded timeout, operator-configured command only.

## 7. MTU constraint + client-side truncate guard

`RoundTripTaskControl` sends the whole `TaskControlRequest` via
`conn.Connection().SendMessage(...)` — a **single objproto message**, not a trsf
stream. On UDP transport this is path-MTU-bound (`trsf.DefaultInitialMTU = 1200`,
growing to `DefaultMaxMTU = 1500` after path-MTU discovery). An oversized message
**does not arrive at all** on UDP — so a server-side "too large" rejection is
impossible (the server never sees it). The bound must be enforced **client-side,
before send.**

- `cli.Notify` encodes the assembled `NotifyRequest`, measures the encoded size
  against a conservative budget derived from `DefaultInitialMTU` minus envelope
  overhead (AppKind + kind + request_id + level + origin + optional WorkerInfo +
  title + objproto/trsf framing), and **truncates `text` to fit**, appending an
  ellipsis marker.
- Truncation is **not silent**: a warning is written to stderr noting the
  original vs truncated length.
- `text_len` / `title_len` are `u16`; the client guarantees the total fits the
  budget regardless.

Rationale (validated against use cases, §9): notifications are short one-liners;
detail belongs in the task log. We assume the **agent will not self-limit** and
will send long text, so the cap is load-bearing in normal operation, not a rare
guard. Truncation prioritizes the primary goal — the ping reaches the phone —
over completeness. Long-form transport (streaming `text` like `SubmitRequest`'s
prompt) was considered and rejected as over-engineering for this path.

Layered handling of "long text clogs the destination":
1. **harness MTU cap (~hard):** never breaks transport; truncate → always
   delivers.
2. **brevity norm (soft nudge):** the `notify` help and the `harness-cli` skill
   state "one short line; detail goes in the task log." Documented because LLM
   callers lose convention across sessions; the code cap, not the doc, is the
   real enforcement.
3. **sink-specific trimming (delegated):** the external script trims/format for
   its medium (a push banner shows 1–2 lines; Telegram tolerates long text).
   Medium UX is the script's responsibility, consistent with "delivery +
   formatting are the script's job."

## 8. CLI / agent ergonomics

- `harness-cli notify "text" [--title T] [--level info|warn|error]`
  (default level `info`).
- `cmd/harness-cli/main.go`: add `case "notify":`.
- `cli/notify.go` (new): `func (c *Client) Notify(...)` and a thin
  `func Notify(ctx, peerCID, ...)` wrapper that dials, calls, closes. Expose a
  `NotifyWith(client)` form so the TUI/WebUI reuse a long-lived `*cli.Client`
  rather than re-handshaking.
- **Agent usage:** the agent invokes `notify` **fire-and-forget and ends its
  turn**. It does not block its reasoning turn awaiting downstream effects. This
  matches the existing inbox/agentboard discipline (send-only, no synchronous
  wait/dispatch from an agent turn). This is distinct from an interactive
  user-decision prompt (which blocks for a choice) — `notify` is a one-way ping.
  The `harness-cli` short-lived CLI process itself still does a normal
  sub-second request→ack→exit.

## 9. Validated use cases

Agent/task origin (`worker`), fire-and-forget — the primary driver (away-from-
keyboard phone ping):

1. **Completion** — `info` "task 0f0d4… done; PR up".
2. **Decision needed** — `warn` "which approach for X?" (a call-the-human ping,
   preceding any option prompt the operator answers elsewhere).
3. **Approval gate reached** — `warn` "waiting on bash-command approval".
4. **Failure / crash** — `error` "make check failed on lint runner",
   "gmkhost OOM".

Operator origin:

5. **Self cross-device ping** (`worker`, from the bash worker shell) — push a
   reminder to the phone, or vice-versa; a personal push gateway.
6. **Out-of-worker manual ping** (`external`) — a note tied to no task.

All are short one-liners → confirms the short-text + truncate posture, and
exercises `level` (push priority), `title` (banner heading), and
`origin`/`WorkerInfo` ("which of several concurrent runners pinged me", and
worker-vs-external).

## 10. Security & tradeoffs

- **Egress-only:** opens no new inbound externally-reachable surface.
- **Trust boundary:** any PSK-authenticated client may trigger the hook — the
  same boundary as submitting a task. No new privilege escalation. Origin is
  caller-asserted display data, not an authz input.
- **Secrets stay out of the repo:** bot tokens / endpoint URLs live in the
  operator's external script and its environment, never in committed content.
- **New risk — server-spawned process:** mitigated by no-shell argv invocation,
  stdin payload (no text in args), bounded timeout + reap, and opt-in config.
- **Limitation:** not a client screen notification (distinct from pubsub
  fan-out). Reaches the phone without a connected client, but actual delivery
  depends on the external script + medium; the harness guarantees only
  `accepted`.

## 11. Insertion points

| Component                         | File                                  | Action                                   |
|-----------------------------------|---------------------------------------|------------------------------------------|
| `TaskControlKind` enum + formats  | `runner/protocol/message.bgn`         | add `notify`; add Notify*/WorkerInfo; 2 match arms |
| CLI subcommand dispatch           | `cmd/harness-cli/main.go`             | add `case "notify":`                     |
| CLI helper (+ truncate guard)     | `cli/notify.go` (new)                 | `Notify` / `NotifyWith` / wrapper        |
| Server control dispatch           | `server/task_handler.go`              | add `case TaskControlKind.notify:` → `handleNotify` |
| Notify hook impl (exec)           | `server/` (new method/file)           | exec.CommandContext, stdin JSON, env, timeout |
| Server config flag                | `cmd/harness-server/main.go`          | add `--notify-hook` (+ `HARNESS_NOTIFY_HOOK`) |
| Server config field               | `server/server.go` (`Config`)         | add `NotifyHook string`                  |
| Brevity norm doc                  | `.claude/skills/harness-cli/SKILL.md` | document `notify` one-line norm          |

Exec-from-server is greenfield (no existing `os/exec` in `server/`).

## 12. Testing

- **Schema round-trip:** encode/decode `NotifyRequest` for both origins (worker
  with `WorkerInfo`, external without); assert the worker block is absent for
  `external`.
- **CLI truncate guard:** oversize `text` → encoded size within budget, ellipsis
  appended, stderr warning emitted.
- **Server handler:** `no_hook` when unconfigured; `spawn_failed` on a bad
  command path; `accepted` launches the process with the expected stdin JSON +
  env (test hook script writes stdin/env to a temp file for assertion).
- **End-to-end:** `harness-cli notify` → server → test hook script invoked once
  with correct payload.

## 13. Open questions / future

- Concurrency cap on simultaneous hook spawns (low-frequency today; defer).
- Optional server-origin notifications (server restart / runner offline) — the
  `origin` enum can gain a `server` value later without breaking the wire.
