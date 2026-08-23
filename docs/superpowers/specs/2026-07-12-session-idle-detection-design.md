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
  read-only observation of a session). The **notify sink additionally
  requires `notify`** (checked in the handler): it reaches the same
  operator-notification egress the Notify RPC gates, and an egress gate that
  depends on which RPC you arrive by is not a gate — without this, a
  confined exec_attach-only task could spam operator notifications it could
  not send via `harness-cli notify`. The board sink needs no extra cap
  (agentboard sends are deliberately ungated for authenticated agents).
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

## Amendment (2026-08-20) — await_idle is ungated

The `exec_attach` requirement is removed. `await_idle` now needs **no
capability**; only `sink=notify` is gated, on `notify`, exactly as this spec
already specified for it.

The two "Decisions taken" bullets state the contradiction side by side and
never resolve it:

> - exec_attach gates await_idle. List already carries last_output_at under
>   the existing List gate (info visibility rules unchanged).

If `ls` hands `last_output_at` to any caller that can see the task, then a
watcher over that same value discloses nothing new. The gate did not protect
information — it made the **edge-triggered path cost strictly more authority
than polling for the same answer**, which taxes the cheap path and pushes a
confined caller toward the wasteful one. That is backwards from the Problem
statement, which names "a babysitter agent polls `session snapshot` on an
interval — wasteful and laggy" as the thing to fix.

The remaining gates are unchanged and sufficient:

- **Scope**: `inScope` in the handler. A session outside the caller's reach
  answers `not_found`, so the ungating widens no visibility.
- **`sink=notify`**: still `notify`, for the reason this spec gives — the
  egress is the privileged part, not the observation.
- **`sink=board`**: still nothing, per the board trust model.

Consequently `AwaitIdleStatus`'s schema note ("a caller lacking exec_attach is
rejected via the shared permission_denied response") now applies to the notify
sink only, and `AwaitIdle` is out of the `requiredCap` map.

Related, same date: `exec_attach` itself was split into `exec_view` /
`exec_cowrite` / `exec_control` — see the amendment in
`2026-06-20-harness-cli-capabilities-design.md`. Where this spec says
"`exec_attach` (same gate as snapshot/view attach)", the modern reading of
that comparison is `exec_view`; but await_idle needs neither.

## Amendment (2026-08-23) — the same signal, sampled client-side per capture

`session snapshot` now reports a third thing beside the screen: `live`
(`{window_ms, frames, bytes, anchored}`), measured in `cli/snapshot_raw.go`
while the capture is running. It is the SAME underlying observation this spec
builds on — PTY output arriving or not — taken at a different place and for a
different consumer, so the two do not compete:

|  | this spec (Layer 1/2) | `snapshot.live` |
|---|---|---|
| measured at | server, `SessionMux.runnerPump`, continuously | client, during one capture |
| shape | a timestamp + an edge watcher | a count over a stated window |
| answers | "has it been quiet for N ms" | "how much arrived while I looked" |
| reaches | `ls`, `session ls`, TUI, WebUI | `session snapshot` only |

Why it was added where the screen is rendered rather than derived from
`output_idle_ms`: `--detect`'s rules read the rendered grid, and a grid can be
corrupted by things that leave the byte stream untouched — a multiplexer's pane
border, an unrecognised UI, a size the renderer had to guess. On a real capture
inside a herdr pane every rule missed and the verdict was `unknown`, while the
session was demonstrably mid-turn; `live` reported 13 frames / 1598 B in
1261 ms from the same capture. Co-locating it with the verdict is the point —
`output_idle_ms` is one RPC away but arrives without the screen it explains.

**The window has to be anchored.** A view-attach opens with the server replaying
its ring at wire speed, so a count over the whole settle window measures the
LINK. The window therefore starts at the last synthesised frame — the repaint
the server appends to close a replay — and `anchored: false` reports the case
where none arrived, which makes the counts not a rate. Pinned by
`TestLiveWindowStartsAtTheLastSynthesisedFrame`; with the reset removed the same
capture reads 4 frames / 34 B / 1500 ms instead of 2 / 8 / 1300.

Two limits are inherited from this spec's "Known limits (accepted)" and one is
new:

- Silence still cannot separate "waiting for a human" from "finished". `live`
  narrows nothing there; that is what `--detect`'s `blocked` is for.
- A timer-driven repainter still reads as busy.
- **New: it is a 1500 ms sample, not a continuous measurement.** This spec's
  measured basis (spinner ~110 ms, max busy-side gap 0.498 s) is the yardstick
  for reading it — a working claude should not produce zero frames in a default
  window — but a slower producer can, which is why `window_ms` is reported
  beside the count and never elided.

**Not built here, deliberately:** no detection rule reads `live`. The condition
language in `detect_rules.json` matches text against screen regions, and giving
it a numeric input is a schema change that should follow evidence about which
thresholds actually separate the states, not precede it. The CLI report says
`(no rule reads this yet)` so a reader cannot mistake it for part of the
verdict.

### Corrected same day — the TUI grid pane reports it continuously

The paragraph above originally also said the two live renderers were left alone
because "they have no verdict to print it beside". That was wrong, and the
operator said so within the hour. `tui/grid.go` has had a per-pane diagnostic
overlay all along — `HARNESS_GRID_DIAG` gates `PaneStreamer.DiagLine()` onto
each pane's first body row — and it was already counting `rxBytes` and `reads`.
It is not a verdict, which is why looking for one missed it; it is the
diagnostic surface, which is what the question should have been about.

So the grid pane now carries the CONTINUOUS form of the same measurement:

```
rx=41230 rd=118 1274B/s 3.6rd/s at=1 vtp=0 sz=48x210 lc=44 cy=43 err=-
```

Rolling one-second window (`rateWindow`), anchored on the FIRST arrival rather
than on attach so a pane that was quiet for a minute does not average that
minute into its first reading. Only a COMPLETED window is displayed; silence for
a full window reads as 0.

**The displayed value must not move between arrivals**, and getting that wrong
is what the first version shipped. It derived the rate from the OPEN window as
count/elapsed, which "decays to zero on its own" — one render at a time. The
operator saw it immediately: 「なんかすげー勢いで数値がカウントダウンするみたいに
なってますけど」. The path is a burst SHORTER than a window (a replay burst, one
repaint): the window never rolls, so its count freezes while its elapsed keeps
growing and every tick renders a smaller number. A window that HAS rolled hides
the bug — its count is reset, so the same arithmetic yields a steady 0.

The cost of only showing completed windows is a beat of lag at both ends: a new
pane reports nothing for its first `rateWindow`, and a sub-window burst is
reported for a full window after it ends. Both beat a number that moves on its
own — a debug overlay is read by eye, and a still value is what makes a changing
one mean something. `TestPaneStreamerRateDoesNotDriftBetweenArrivals` pins the
general invariant without naming a value: between two arrivals, what the pane
reports may not change.

It needs none of the snapshot's `anchored` machinery: this stream's replay burst
arrives once at attach and the window rolls past it seconds later, whereas a
fresh capture is mostly replay by volume.

`HARNESS_GRID_DIAG` is joined by a **`diag [on|off]` cmdline verb** (bare `diag`
toggles). Both are needed and neither subsumes the other: the env var is the only
way to have the overlay on for the FIRST paint — a pane that is black from the
start — while a grid that goes wrong after ten minutes cannot be handed a new
launch environment without losing the state being diagnosed. An explicit
`on`/`off` is kept distinct from a toggle so a second operator cannot turn it off
by asking for it on.

The WebUI preview remains untouched: it renders through xterm.js in the browser
and has no equivalent diagnostic row.
