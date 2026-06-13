# Interactive view-only (read-only) attach — design

**Date:** 2026-06-14
**Status:** approved (brainstorming), pending implementation plan

## Problem

A detachable interactive session streams to **exactly one** client at a time.
`SessionMux` holds a single `tui` stream (`server/session_mux.go:79`); a new
`Attach` force-closes the previous one (`old.CloseBoth()`, `session_mux.go:171`)
— takeover semantics. So when a human attaches to a session an agent is running
in, or to watch one agent work, they kick whoever was streaming. There is no way
to **passively observe** a live interactive session without taking it over, and
interactive output is not server-logged (PTY ring buffer only; `logs`/`watch`
cover one-shot tasks), so there is no read-only replay path either.

Motivation: be able to **watch an agent work live** (multiple browser tabs /
terminals) without claiming the session — primarily for delight/observability on
this single-user dogfood tool.

## Goal

Add a **view-only attach mode**: a read-only observer that receives the same
live stream (and ring replay) as the controller, **cannot** send input, **does
not** take over the writer slot, and **cannot** destabilize the real session.
Surface it on all three clients (WebUI, CLI, TUI).

## Non-goals (v1)

- **No watcher-count / "👁 N watching" indicator.** Viewers are silent; neither
  the writer nor other viewers are told who is watching. (Avoids pushing viewer
  membership changes to every client.)
- **No auto-reconnect** of a dropped viewer. A dropped viewer re-attaches
  manually.
- **No in-place role promotion.** Mode is fixed at attach time. To go
  viewer→controller, detach and re-attach as controller (normal takeover).

## Key invariant

**A viewer must never be able to wedge or backpressure the real session.** This
is the load-bearing constraint (cf. `project_trsf_accept_queue_wedge`: undrained
queues wedge the streams layer). Everything below is shaped to honor it: the
controller (writer) path is **unchanged**, and viewers are an isolated additive
concern that can only ever drop *themselves*.

## Architecture

```
runner (claude PTY) ──frames──> [server: SessionMux] ──┬─ sync   ─> writer  (m.tui, 1 slot, UNCHANGED)
                                    │ ring + modeTracker └─ async  ─> viewers[] (N slots, droppable)
                                                                       ↑ input: writer forwards to runner;
                                                                         viewer input is read-and-DISCARDED
```

- `SessionMux` lives **server-side** and brokers the runner stream
  (`m.runner`) and client streams. Fan-out to viewers is purely a server-side
  concern. **The runner protocol and runner code are unchanged** — the runner
  neither knows nor cares how many clients observe.
- The **writer (control) path is byte-for-byte unchanged** from today, including
  its synchronous `tui.AppendData` in `runnerPump` (`session_mux.go:142`) and
  its takeover semantics in `Attach`. Zero blast radius on the proven path.

## Schema change

`runner/protocol/message.bgn`, at `AttachSessionRequest` (`message.bgn:539`):

```
enum AttachMode:
    :u8
    control = "control"   # default (ordinal 0): today's takeover-writer behavior
    view    = "view"      # read-only observer; no input, no takeover

format AttachSessionRequest:
    task_id :TaskID
    mode :AttachMode      # NEW. control == 0, so the default is the existing behavior
```

- Explicit wire field, not a convention (`feedback_no_schema_invisible_bytes`,
  `feedback_protocol_explicit_over_convention`).
- `AttachSessionResponse` is **unchanged** (`status`, `stream_id`,
  `replay_bytes`). For a viewer, `replay_bytes` = bytes replayed to that viewer.
- `AttachSessionStatus` is **unchanged**; `mode=view` adds no new error states
  (a view attach to a not-found / non-interactive / terminal task returns the
  same statuses as control).
- Backward compat: single-user dogfood, all binaries rebuilt together
  (`feedback_individual_dogfood`); `control == 0` keeps any unset field on the
  existing behavior regardless.

## Server: `server/session_mux.go`

### New state

```go
type viewerConn struct {
    stream trsf.BidirectionalStream
    ch     chan []byte        // bounded; initial cap = 256 frames (tunable)
    cancel context.CancelFunc
}
// added to SessionMux, guarded by m.mu:
viewers map[*viewerConn]struct{}
```

The 256-frame cap is a starting value; it bounds per-viewer memory (frames are
PTY chunks, individually bounded by the runner read size). Tune in the plan if
needed.

### `AttachViewer(ctx, stream) error` — new

Same signature shape as `Attach` (returns `error`; the handler computes
`replay_bytes` itself, see routing below). Does **not** take over the writer,
fire `onAttach`, or forward input:

1. Under `m.mu` (single critical section): bail if stopped; create
   `v := &viewerConn{...}` with a child context; add to `m.viewers`; capture the
   ring `Snapshot()` and `m.modes.preamble()` **while still holding the lock**,
   so a concurrent `runnerPump` fan-out cannot interleave between "added to
   viewers" and "snapshot taken". Release the lock.
2. Replay **directly** to `stream` (as `Attach`, `session_mux.go:178-194`): the
   preamble as a Stdout frame, then the snapshot bytes. On error, `dropViewer(v)`
   and return the error.
3. Start the two goroutines **after** replay completes, so replay bytes always
   precede live frames (live frames buffer in `v.ch` during replay and are
   flushed by the output drain only once it starts). A small, bounded
   duplication window remains — the same benign one the writer path already has
   (`Attach`); a terminal emulator re-rendering a few duplicate frames is
   cosmetically invisible. The two goroutines:
   - **output drain**: `for { select case b := <-v.ch: stream.AppendData(b);
     case <-ctx.Done(): return }`. On `AppendData` error → `dropViewer(v)`.
   - **input drain** (`viewerInputDrain`): read and **discard** the viewer's
     incoming direction so the bidi recv side never backpressures/wedges, and so
     a client-side close is detected promptly:
     ```go
     func (m *SessionMux) viewerInputDrain(ctx context.Context, v *viewerConn) {
         const maxRead = 32 * 1024
         for {
             _, eof, err := v.stream.ReadDirectContext(ctx, maxRead) // bytes DISCARDED
             if eof || err != nil { m.dropViewer(v); return }
         }
     }
     ```
     `ReadDirectContext` (not `ReadDirect`) so `cancel()`/`Stop()` unblock the
     read immediately instead of leaking until the next byte. **Read-only is
     enforced here**: the difference from the writer's `tuiPump`
     (`session_mux.go:206`) is exactly that `tuiPump` forwards bytes to
     `m.runner` while this drain discards them. Viewer input never reaches the
     runner.

### `runnerPump` change

After the existing synchronous writer delivery (`session_mux.go:138-147`), add
a **non-blocking** fan-out to viewers:

```go
m.mu.Lock()
for v := range m.viewers {
    select {
    case v.ch <- frameBytes:
    default:
        m.dropViewerLocked(v)   // slow viewer: drop ONLY itself, never block the pump
    }
}
m.mu.Unlock()
```

A full queue means the viewer cannot keep up → it is dropped. The pump never
blocks on a viewer, so the runner read loop, the ring, the writer, and other
viewers are unaffected.

### `dropViewer` / `dropViewerLocked`

Idempotent (model after `detachLocked`, `session_mux.go:242`): if `v` is not in
`m.viewers`, no-op; else remove, `v.cancel()`, `v.stream.CloseBoth()`. Both the
output-drain and input-drain goroutines may call it; the membership check makes
double-drop safe. **Does not** call `onDetach` (viewers are orthogonal to the
task state machine).

### `Stop` change

`Stop` (`session_mux.go:269`) additionally drains and closes every `viewerConn`
(cancel + `CloseBoth`), clearing `m.viewers`.

## Server: `server/task_handler.go` routing

`AttachMode` rides on `AttachSessionRequest` only, so the **only** site that
branches is the `attach_session` (reattach) handler — the `mux.Attach` call at
`task_handler.go:741`:

- `control` → `mux.Attach(parentCtx, tuiStream)` (unchanged).
- `view` → `mux.AttachViewer(parentCtx, tuiStream)`.

`replay_bytes` is still computed by the handler as
`uint64(mux.RingBufferLen())` captured **before** the attach call (`:734`),
identical for both modes; `AttachSessionResponse` (status / stream_id /
replay_bytes) is built the same way.

The `open_interactive` initial-attach site (`task_handler.go:626`) is **not**
touched: opening a fresh session makes its opener the controller by definition,
and `OpenInteractiveRequest` carries no `mode`. View mode is meaningful only
against an already-live session (reattach).

## State machine

Viewer attach/detach **must not** call `MarkAttached` / `SetDetached`
(`task_handler.go:613-614`). Consequences (consistent with
`project_interactive_pty_no_detach`):

- A session with a writer attached stays **Running**, whether or not viewers are
  present.
- A session with **only viewers** (no writer) stays **Detached** — there is no
  controller. This is the primary use case: **observe a Detached agent session
  without claiming it.** Liveness remains "runner + claude process alive", not
  "a client is attached".

## Clients (all three surfaces)

The schema + server core is shared; each client adds a way to request
`mode=view`:

- **CLI** (`cli/attach_native.go`, `cli/attach_js.go`): `AttachSession` and
  `attachSessionRPC` gain a `mode` argument. New flag: `harness-cli session
  attach --view <id>`. The view client prints the read-only stream; Ctrl-]
  detaches the viewer (it never forwards input regardless).
- **WebUI** (`webui/static/main.js`): a per-task **"👁 View only"** button beside
  the existing per-task **Reattach** button, calling `AttachSession` with
  `mode=view` (mirrors where Reattach already lives, so it works for any live
  task row). The "stream dropped" / end-of-stream UI reuses the existing
  attach-status line (the `detached: … (…)` element in the Interactive section)
  plus a re-attach affordance.
- **TUI** (`tui/taskaction.go`): a view-only attach key/flag alongside the
  existing reattach action.

## Edge cases

| Case | Behavior |
|------|----------|
| Viewer lags (queue full) | Dropped (close). Client shows "stream dropped (lagged) — re-attach"; no auto-reconnect (v1). |
| Viewer self-closes | Input drain's `ReadDirectContext` returns EOF/err → `dropViewer` promptly. No lingering viewer. |
| Session ends (runner EOF/err) | `Stop()` closes all viewers; clients see stream end. |
| Writer takeover (a control client attaches) | Viewers **unaffected** — `Attach` only `CloseBoth`s the prior writer; it never touches `m.viewers`. |
| View attach to not-found / non-interactive / terminal task | Same `AttachSessionStatus` as control; no new statuses. |
| Replay duplication window | Mirrors `Attach`'s existing set-then-snapshot ordering; pre-existing behavior, not introduced here. |

## Testing (`server/session_mux_test.go`)

1. `AttachViewer` receives replay (mode preamble + ring snapshot) then live
   frames.
2. Multiple viewers all receive the same live frames (fan-out).
3. **Load-bearing:** a viewer whose queue is full / whose `AppendData` errors is
   dropped **without** stalling the runner pump, the writer, or other viewers.
4. Viewer attach does **not** fire `onAttach` (status stays Detached); a control
   attach still does.
5. Bytes sent on a viewer stream are read-and-discarded and **never** forwarded
   to the runner (read-only).
6. A control takeover leaves existing viewers streaming.
7. `Stop()` closes all viewers.
8. Routing: `task_handler` dispatches `mode=view` to `AttachViewer`; CLI/wasm
   `AttachSession` transmits `mode`.

## Verification

Touches wasm (`cli/attach_js.go`, WebUI) and the schema (regenerate from
`message.bgn`). Run with explicit package patterns, not `./...`
(`feedback_verify_with_make_targets_not_adhoc`): `make check` + **`make
wasm-check`** + `go vet` + `go test` on the affected packages.

## Out of scope / future

- Watcher-count indicator (would need viewer-membership push to clients).
- Auto-reconnect of dropped viewers.
- Promoting a viewer to controller in place.
