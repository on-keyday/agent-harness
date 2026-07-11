# Interactive-session idle detection (last-output timestamp + await-idle) — design

Date: 2026-07-12
Status: design approved (conversation), implementing.

## Problem statement

An interactive session running an agent TUI (claude etc.) gives the operator
and coordinating agents no way to tell whether the foreground program is
mid-turn (generating / thinking) or waiting for input, short of taking a
`session snapshot` and reading the screen. Concretely:

- A human who delegates a long turn to a worker and walks away has no signal
  for "the worker finished its turn / hit a permission prompt and is waiting
  on me". They either poll snapshots or discover the stall much later.
- A babysitter agent driving a worker session polls `session snapshot` on an
  interval — wasteful and laggy — because nothing exposes "is this session
  producing output right now".
- Nothing in `ls` / `session ls` / TUI / WebUI distinguishes a busy session
  from an idle one; TaskStatus stays `Running` throughout.

We want (all three client surfaces, per project rule):

- **(Layer 1, pull)** a per-interactive-session **last-output timestamp**
  visible in `harness-cli ls`, `session ls`, the TUI task table, and the
  WebUI task list, so any consumer can derive busy/idle without new
  round-trips.
- **(Layer 2, edge)** a **one-shot await-idle** primitive: "when this session
  next goes idle, tell me once" — long-poll for scripts/humans, `notify` sink
  for the away operator, agentboard sink for agents (which must never block a
  turn).

Non-goals: no always-on notifications (operator feedback: notifications are
on-demand); no per-turn semantic parsing of the screen (that stays in
`session snapshot`); no persistent/recurring watch in v1.

## Measured basis (2026-07-12, claude v2.1.207, PTY observation)

Recorded every PTY read of an interactive `claude --model haiku` session with
timestamps (script: pty_observe.py, scratchpad):

- **In-flight turn** (thinking + streaming): spinner repaints ~every 110ms;
  max inter-read gap observed **0.498s**. The spinner is client-timer-driven,
  so the gap bound does NOT grow with model latency.
- **Idle at the input prompt**: **zero bytes** — 17.2s and 116s windows with
  no output at all (no cursor blink, no keep-alive).
- Permission / trust dialogs are also zero-byte idle. That is desirable for
  Layer 2: "stuck on a prompt" is exactly what the operator wants to hear
  about.

Therefore a threshold anywhere in 1–10s cleanly separates busy from idle.
Default **2500ms** (5× the measured busy-side max gap).

Known limits (accepted): a full-screen program that repaints on a timer while
semantically "waiting" (e.g. a clock in a status bar) reads as busy; a
frozen-but-alive process reads as idle. Both are inherent to byte-level
quiescence and out of scope — consumers needing more read the screen.

## Design

### Detection point (server-side, no runner change)

`server/session_mux.go` `runnerPump` is the single choke point every
interactive session's output already flows through, attached or not. Stamp a
`lastOutput atomic.Int64` (unix **nanoseconds**, matching TaskInfo timestamp
convention) on every `Stdout`/`Stderr` frame (control frames excluded).
Expose `(*SessionMux).LastOutputUnixNano() int64` (0 = no output yet).

No TaskStore writes per frame: the value lives in the mux and is **pulled**
at List time and by await-idle watchers. When the mux is gone (session
terminal), the field reads 0 — idleness of a dead session is meaningless.

### Wire schema (single authoritative block — every byte listed)

`runner/protocol/message.bgn`:

```
# TaskInfo — append after ring_buffer_bytes:
    last_output_at :u64       # unix nanos (SERVER clock) of the last
                              # Stdout/Stderr frame the server received from
                              # this session's runner stream. 0 = not an
                              # interactive session, no output yet, or session
                              # mux already gone. Populated at List time from
                              # the live SessionMux.
    output_idle_ms :u64       # ms between last_output_at and List time,
                              # computed on the server clock. Meaningful only
                              # when last_output_at > 0. Consumers derive
                              # busy/idle from THIS, never from a local
                              # now()-last_output_at: client and server run on
                              # different hosts (Windows client / Linux server
                              # in production) and clock skew would distort a
                              # locally-derived age.

# TaskControlKind — new member:
    await_idle = "await_idle"

# New formats:
enum AwaitIdleSink:
    :u8
    reply = 0                 # long-poll: response is delayed until fired
    notify = 1                # server-side operator notification on fire
    board = 2                 # server-side agentboard publish on fire

# A caller lacking exec_attach is rejected via the shared permission_denied
# response (requiredCap map) — same shape as every other gated kind — so the
# enum carries no dead `denied` member.
enum AwaitIdleStatus:
    :u8
    fired = 0                 # idle edge observed (sink=reply terminal reply)
    armed = 1                 # watcher registered (sink=notify/board immediate reply)
    session_stopped = 2       # session ended before the idle edge
    not_found = 3             # unknown task id or no live session mux
    bad_request = 4           # e.g. sink=board with empty topic

format AwaitIdleRequest:
    task_id :TaskID
    threshold_ms :u32         # 0 → server default 2500
    sink :AwaitIdleSink
    topic_len :u16            # sink=board: agentboard topic to publish to
    topic :[topic_len]u8      # ignored for other sinks

format AwaitIdleResponse:
    status :AwaitIdleStatus
    last_output_at :u64       # unix nanos at decision time (0 if none)

# TaskControlRequest match arm:
    TaskControlKind.await_idle => await_idle :AwaitIdleRequest
# TaskControlResponse match arm:
    TaskControlKind.await_idle => await_idle :AwaitIdleResponse
```

Board-sink fire payload (JSON, agentboard message from the server):

```
{"kind":"session_idle","task":"<32-hex>","last_output_at_unix_ms":<int>,
 "status":"fired"|"session_stopped"}
```

Notify-sink fire text: `session <short-id> idle (Xs since last output)` /
`session <short-id> ended`, level=info, title="await-idle".

### Layer 2 semantics (decisions, not options)

- **One-shot**: a watcher fires exactly once (idle edge OR session stop),
  then is removed. No persistent mode in v1.
- **Already idle at arm time → fire immediately.** "await-idle" answers "is
  it idle yet"; if yes, the answer is now. (A caller wanting "next idle edge
  after current activity" can arm during activity — the common flow.)
- **No output ever yet** (lastOutput==0, e.g. armed during process boot):
  treat as busy, wait for first output then the edge. Rationale: arming
  before boot output exists means the caller wants the boot turn's end, not
  an instant fire on a session that hasn't spoken.
- **Watcher mechanics**: one goroutine per armed watcher, polling the mux's
  lastOutput atomic on a 500ms ticker and selecting on the mux ctx (Stop
  cancels it → session_stopped). Watchers are rare and one-shot, so
  per-watcher goroutines beat a shared list + arm/disarm machinery on
  simplicity; worst-case fire latency is threshold+500ms.
- **Capability**: `exec_attach` (same gate as snapshot/view attach — this is
  read-only observation of a session).
- **Long-poll transport**: request_id-correlated delayed response on the same
  conn; the CLI holds the connection open. If the conn drops, the watcher is
  NOT garbage — it fires into a closed conn send which is a no-op error;
  acceptable for v1 (no leak: it is removed on fire or mux stop either way).
- **Board sink publisher identity**: the message is published by the server
  itself on behalf of the requester (from_task = requester's principal task
  id when present, else zero). No new agentboard capability gate (reads and
  this publish stay within the existing board trust model — no invented cap
  gates per project rule).

### Surfaces (Layer 1)

- `cmd/harness-cli/session.go` `runSessionLs`: add `last_output_at` (unix
  nanos, 0 allowed) and `idle_ms` (the wire output_idle_ms; -1 when
  last_output_at==0) to the JSON line.
- `cli/list.go` `renderList` (top-level `ls`): for tasks with
  last_output_at>0, append ` act=busy` (idle < 3s) or ` act=idle:Xs`
  to the task row. Badge helper `cli.ActivityStr(outputIdleMs)` +
  `cli.ActivityBusyThreshold`, shared with the TUI.
- TUI `tui/tasks.go` `SetRows`: same badge via `cli.ActivityStr` in a new
  "Act" column; blank when no live session output.
- WebUI: `cmd/harness-webui-wasm/main.go` task marshal adds `outputIdleMs`
  (server-computed ms, -1 sentinel for "no output"); `webui/static/main.js`
  `renderTaskList` appends the same badge text via `activityBadge()`. Dark
  theme + mobile layout unaffected (text-only addition to existing rows).
- While in `handleList`: also populate `ring_buffer_bytes` from
  `mux.RingBufferLen()` — the field exists on the wire and in every renderer
  but has had no production writer (always 0, a lying field). Same
  enrichment point, one line.

### CLI (Layer 2)

```
harness-cli session await-idle [--threshold-ms N] [--notify] [--topic T] <id>
```

- default: long-poll (sink=reply); prints one JSON line
  `{"status":"fired","last_output_at":...}` and exits 0 (session_stopped →
  exit 3, so scripts can branch).
- `--notify`: sink=notify, replies armed immediately, exit 0.
- `--topic T`: sink=board, replies armed immediately, exit 0. An agent arms
  with `--topic chat.<its-short-id>` and ends its turn; the fire arrives via
  its inbox hook.
- `--notify` and `--topic` are mutually exclusive (bad_request otherwise).
- TUI/WebUI get Layer 1 only in this change; arming from TUI/WebUI is a
  follow-up if wanted (the wire + server support them already — CLI is the
  only new consumer surface here, which is a deliberate v1 scope statement,
  not an oversight: the interactive surfaces already show busy/idle live, so
  the human watching them does not need an arm button).

## Decisions taken

- Server-side detection at SessionMux (not runner-side): zero runner/protocol
  changes for Layer 1 detection; the server already sees every frame.
- unix nanos on the wire (TaskInfo convention); ms only at the JS boundary
  and in JSON convenience fields (named *_ms).
- Idle age is computed on the SERVER clock at List time (output_idle_ms) and
  consumers use it verbatim; the absolute last_output_at stays informational.
  Rationale: cross-host clock skew (real topology: client, server, runner on
  three hosts) would distort any client-side now()-timestamp derivation.
- Pull-model timestamp (atomic in mux, read at List/arm time), no per-frame
  TaskStore writes.
- Watcher ticker 500ms: worst-case fire latency threshold+500ms — irrelevant
  at human/agent timescales, and avoids any per-frame timer churn.
- exec_attach gates await_idle. List already carries last_output_at under
  the existing List gate (info visibility rules unchanged).
- One-shot only; fire-on-already-idle; lastOutput==0 waits for first output.

## Verification plan

- Unit: SessionMux stamps lastOutput on Stdout/Stderr frames only; watcher
  fires on edge / immediately-when-idle / session_stopped on Stop; one-shot
  removal.
- E2E (dev server + runner): spawn a bash interactive session; `session ls`
  shows last_output_at advancing on `echo`; `await-idle` long-poll returns
  after the prompt settles; `--topic` sink delivers a board message.
- `make check` (+ wasm-check) before landing, per project rule.
