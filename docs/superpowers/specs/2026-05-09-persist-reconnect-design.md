# Persistent Connection (--persist) — Design

- Date: 2026-05-09
- Status: Draft
- Scope: `agent-runner`, `harness-tui`, `harness-webui-wasm`

## 1. Goal

Long-running harness processes (`agent-runner`, `harness-tui`,
`harness-webui-wasm`) currently exit (or freeze in a "disconnected"
state) the moment their WebSocket session to `harness-server` dies.
This spec adds a `--persist` flag that makes them automatically
re-dial with exponential backoff and re-establish their per-process
state (Hello / subscriptions / UI bindings) until the user explicitly
shuts them down.

The default for these three binaries flips to **persist=on**; opt out
with `--no-persist`.

## 2. Non-goals

- **Server-side preservation** of the `RunnerEntry` or in-flight tasks
  across a runner reconnect. The existing `Registry.OnRemove` path
  (`server/server.go:183`) marks the runner's `ActiveTasks` as
  `runner_disconnected → Failed` immediately on disconnect; the
  reconnected runner registers under a fresh `ConnectionID` and starts
  empty. Carrying tasks across reconnects (grace window) is a future
  spec.
- **Interactive PTY reattach across a connection drop.** That is
  handled by `2026-05-08-interactive-detach-reattach-design.md`'s
  detachable-session feature, which is itself in flight. The two specs
  intentionally compose: persist mode keeps the WS alive; detachable
  sessions keep the PTY alive on the runner. Neither requires the
  other.
- **Pubsub message replay during the disconnect window.** Events
  published while the client is offline are lost; clients reconcile by
  re-issuing `RefreshSnapshot` after reconnect.
- **`harness-cli`'s short-lived subcommands** (`submit`, `ls`,
  `cancel`, `prune`, `prune-local`, `watch`, `interactive`, `agent ...`,
  `logs`). These exit on connection error as today; `--persist` is not
  surfaced on them.

## 3. Existing architecture (要点)

### 3.1 Connection ownership

```
parentCtx (signal-aware)
   └── cli.Dial / runner.Dial
        └── peer.Dial      ← objproto Connection + trsf.Streams + AutoSend + AutoPing
             └── pc.Start  ← spawns AutoReceive goroutine
                  └── pc.Done() ← chan struct{} closed when AutoReceive returns
```

`peer.Conn.Done()` is the single, reliable disconnect signal. It
closes when **any** of:

1. The peer sends a wire-level `Close` (`trsf/api.go:161`).
2. The underlying WS read errors and a subsequent send (e.g. the next
   `AutoPing`) hits `connMap.Get` miss → `rawSess.CannotSend(pkt)` →
   `endpoint.closeCannotSend` → `activeConnection.closeUnlocked` →
   `messageChannel.CloseChannel` → `ReceiveMessageContext` returns
   `ErrChannelClosed` → `AutoReceive` returns
   (`transport/websocket.go:108-117`, `objproto/objproto.go:498-526`).
3. `objproto.AutoGarbageCollect` deletes the connection after
   `connectionTimeout` (default 1 min) without traffic
   (`objproto/objproto.go:656`).

Worst-case detection delay = `peer.DialConfig.PingInterval`. Today
that defaults to 30 s (`peer/conn.go:77`); this spec lowers it to
**15 s** so persist-mode reconnects feel responsive on idle runners.

### 3.2 Per-process boot sequence today

| Process | Bootstrap (current) |
|---|---|
| `agent-runner` | `cmd/agent-runner/main.go` → `runner.Run` → `peer.Dial` → PSK exchange → `RunnerHello` → `pc.Wait`. Returns on disconnect → `os.Exit(1)`. |
| `harness-tui` | `cmd/harness-tui/main.go` → goroutine: `cli.Dial` → `c.SayHello(ClientKind_Tui)` → `app.BindClient(c)` → `RefreshSnapshot` → `SubscribeTaskStatus` + `SubscribeRunnerStatus` → `<-ctx.Done()`. On dial failure: `ConnectionMsg{Connected:false, Err: ...}` → "disconnected" banner forever. |
| `harness-webui-wasm` | `cmd/harness-webui-wasm/main.go::harnessConnect` → `cli.Dial` → `c.SayHello(ClientKind_Webui)` → store in package var. JS layer subscribes via separate calls. No reconnect. |

### 3.3 Existing failure modes worth preserving

- Runner: in-flight tasks marked Failed on disconnect; runner main
  process exits (currently 1) so the OS reaps any still-running
  `claude` subprocesses.
- TUI / WebUI: client process keeps running; user sees a static
  "disconnected" message with no path forward except restart.

## 4. UX

### 4.1 CLI flags (added to `agent-runner` and `harness-tui`)

```
--persist                  enable auto-reconnect on disconnect (default: true)
--no-persist               disable auto-reconnect (process exits on first
                           disconnect, current legacy behaviour)
--ping-interval=DURATION   override peer.DialConfig.PingInterval; lower
                           values speed disconnect detection at the cost
                           of slightly more wire traffic
                           (default: 15s; minimum: 1s)
--reconnect-initial=DURATION   first backoff after disconnect (default 500ms)
--reconnect-max=DURATION       backoff cap (default 30s)
```

`--no-persist` is just sugar for `--persist=false`. The four
`--reconnect-*` / `--ping-interval` knobs are intentionally exposed so
test harnesses (and curious users) can tune them; defaults match the
common case.

### 4.2 WebUI

`harness.connect(cidStr, { persist: true, pingInterval: "15s" })`
gains an options bag (currently it takes a single string). Defaults:

| Call form                              | persist | pingInterval |
|----------------------------------------|---------|--------------|
| `harness.connect(cid)`                 | `false` | 15 s         |
| `harness.connect(cid, {})`             | `true`  | 15 s         |
| `harness.connect(cid, {persist:false})`| `false` | 15 s         |

The single-string form keeps `persist=false` so existing JS that
called `harness.connect("ws:...")` doesn't silently get a persistent
connection; the options-bag form defaults to `true` to match the CLI
default. `webui/static/main.js` is updated to use the options-bag
form.

A new event handler is exposed:

```
harness.onConnectionChange((state) => {
  // state: { phase: "connecting"|"connected"|"reconnecting"|"closed",
  //         attempt: number, nextRetryMs?: number, error?: string }
});
```

`webui/static/main.js` consumes this to render a top-of-page banner.

### 4.3 TUI

`tui/app.go` extends `ConnectionMsg`:

```go
type ConnectionMsg struct {
    Connected    bool          // current connectedness
    Reconnecting bool          // true while in backoff between attempts
    Attempt      int           // 1-based attempt counter
    NextRetry    time.Duration // sleep until next attempt; 0 if N/A
    Err          error
}
```

Status line text:

| Phase                   | Text                                                          |
|-------------------------|---------------------------------------------------------------|
| Connected               | (no banner, normal status footer)                             |
| Initial dial failing    | `connecting… (attempt 3, next try in 4s)`                     |
| Disconnected, retrying  | `disconnected — reconnecting (attempt 3, next try in 4s)`     |
| Persist=off, dropped    | `disconnected: <error>` (current behaviour)                   |
| `--persist` PSK fatal   | `auth failed: <error>` followed by program exit               |

The footer text updates in real time as backoff counts down (1 Hz
ticker, capped to existing TUI tick cadence to avoid flicker).

## 5. Architecture (changes)

### 5.1 `cli/persist.go` — new shared helper

```go
package cli

// PersistConfig configures the auto-reconnect loop.
type PersistConfig struct {
    Enabled        bool          // false → run exactly one iteration, propagate errors
    InitialBackoff time.Duration // first sleep after a transient failure (default 500ms)
    MaxBackoff     time.Duration // cap (default 30s)
    BackoffFactor  float64       // multiplicative factor (default 2.0)
    Jitter         float64       // ±fraction added to each sleep (default 0.25)
    StableReset    time.Duration // if a connection stays up >= this, reset attempt counter (default 60s)
    Logger         *slog.Logger  // optional
    OnState        func(PersistState) // optional, called on each phase transition
    Now            func() time.Time   // injectable for tests; default time.Now
    Sleep          func(context.Context, time.Duration) error // injectable; default ctx-aware time.Sleep
}

type PersistState struct {
    Phase        PersistPhase // Connecting | Connected | Reconnecting | Closed
    Attempt      int          // 1-based; 0 before first attempt
    NextRetry    time.Duration
    LastError    error
}

// PersistDialer dials a fresh connection. The returned cleanup must
// fully tear down the connection (peer.Conn.Close) when invoked.
type PersistDialer func(ctx context.Context) (handle PersistHandle, err error)

type PersistHandle interface {
    Done() <-chan struct{} // closes on disconnect
    Close()                // idempotent teardown
}

// OnConnect is invoked once per successful dial. It runs in the foreground
// (PersistLoop is blocked on it) and may register subscriptions, fire Hello,
// etc. The runCtx is cancelled when the connection dies; OnConnect MUST
// derive any spawned goroutine ctxs from it.
type PersistOnConnect func(runCtx context.Context, h PersistHandle) error

// PSKAuthError is returned by PersistDialer when PSK authentication fails.
// PersistLoop treats it as fatal (no retry) regardless of cfg.Enabled.
type PSKAuthError struct{ Err error }
func (e *PSKAuthError) Error() string { return "psk auth: " + e.Err.Error() }
func (e *PSKAuthError) Unwrap() error { return e.Err }

// PersistLoop runs Dial / OnConnect / Done() in a loop. Returns nil on
// graceful ctx cancel, *PSKAuthError on fatal PSK mismatch, or the last
// dial/onConnect error if Enabled=false.
func PersistLoop(
    ctx context.Context,
    dial PersistDialer,
    onConnect PersistOnConnect,
    cfg PersistConfig,
) error
```

Behaviour:

```
attempt := 0
for {
    attempt++
    emit OnState{Connecting, attempt}
    h, err := dial(ctx)
    if err != nil {
        var pskErr *PSKAuthError
        if errors.As(err, &pskErr) { return err }   // fatal: psk mismatch
        if !cfg.Enabled { return err }
        if ctx.Err() != nil { return ctx.Err() }
        sleep backoff(attempt); continue
    }
    runCtx, runCancel := context.WithCancel(ctx)
    connectedAt := now()
    emit OnState{Connected, attempt}
    if err := onConnect(runCtx, h); err != nil {
        runCancel(); h.Close()
        if !cfg.Enabled { return err }
        sleep backoff(attempt); continue
    }
    select {
    case <-h.Done():                 // disconnect
    case <-ctx.Done():               // shutdown
    }
    runCancel(); h.Close()
    if ctx.Err() != nil { return nil }
    if !cfg.Enabled {
        if h was alive >= StableReset → return nil (graceful close from peer)
        else return errors.New("connection closed")
    }
    if now()-connectedAt >= StableReset { attempt = 0 } // flap reset
    emit OnState{Reconnecting, attempt+1, next}
    sleep backoff(attempt+1)
}
```

backoff = `min(MaxBackoff, InitialBackoff * BackoffFactor^(attempt-1)) * (1 ± Jitter)` with crypto-safe random jitter. Sleep is ctx-aware so Ctrl-C wakes immediately.

### 5.2 `peer/conn.go`

Single line change: replace `cfg.PingInterval = 30 * time.Second`
default with `15 * time.Second`. Update the `DialConfig.PingInterval`
godoc accordingly. Existing callers that pass an explicit value (none
do today, all rely on the default) are unaffected.

### 5.3 `runner/connect.go`

Currently `runner.Run` does:

```go
go session.handleAssign(ctx, at)        // line 191
```

with `ctx` being the function's input parameter (the parent ctx). When
`pc.Wait` returns (disconnect), `runner.Run` returns but spawned
`handleAssign` goroutines keep running with claude subprocesses
attached to the dead conn's `peerSender`. In single-shot mode the
process exits immediately so this is invisible; in persist mode the
loop would race the next iteration against zombie writers.

Change: derive a Run-scoped ctx and propagate it.

```go
func Run(ctx context.Context, cfg Config) error {
    runCtx, runCancel := context.WithCancel(ctx)
    defer runCancel()
    // ... existing setup ...
    go session.handleAssign(runCtx, at)         // was ctx
    go session.handleOpenExec(runCtx, oer)      // was ctx
    // dispatchRunnerRequest signature unchanged — the ctx it receives
    // is already runCtx because the dispatcher closure captures it.
    return pc.Wait(ctx)
}
```

`pc.Wait(ctx)` continues to use the parent `ctx` so an upstream
cancel still wakes it. The defer ensures any tasks spawned in this
Run see ctx.Done() before the next iteration starts. claude
subprocesses are SIGTERM'd via the existing per-task cancel chain
(`runner/process.go::ExecCommand` honours ctx).

To make the dispatcher closure capture `runCtx`, the existing call
site in `runner.Run`:

```go
pc.SetOnControl(func(kind, payload) {
    ...
    dispatchRunnerRequest(ctx, session, ...)   // currently parent ctx
})
```

is changed to capture `runCtx` instead. `dispatchRunnerRequest`'s
signature is unchanged; only the closure binding moves.

### 5.4 `cmd/agent-runner/main.go`

```go
var (
    persist        = flag.Bool("persist", true, "auto-reconnect on disconnect")
    noPersist      = flag.Bool("no-persist", false, "disable --persist (legacy behaviour)")
    pingInterval   = flag.Duration("ping-interval", 15*time.Second, "ping cadence (also = max disconnect detection delay)")
    reconnectInit  = flag.Duration("reconnect-initial", 500*time.Millisecond, "first backoff after disconnect")
    reconnectMax   = flag.Duration("reconnect-max", 30*time.Second, "backoff cap")
)
// ...
enabled := *persist && !*noPersist
err := cli.PersistLoop(ctx,
    func(ctx context.Context) (cli.PersistHandle, error) {
        return runner.Connect(ctx, runner.Config{
            ...
            PingInterval: *pingInterval,
        })
    },
    func(runCtx context.Context, h cli.PersistHandle) error {
        return runner.OnConnect(runCtx, h.(*runner.RunHandle))
    },
    cli.PersistConfig{
        Enabled:        enabled,
        InitialBackoff: *reconnectInit,
        MaxBackoff:     *reconnectMax,
    })
```

`runner.Run` is split into two halves to fit `PersistDialer` /
`PersistOnConnect`:

- `runner.Connect(ctx, cfg) (*RunHandle, error)` — `peer.Dial`, ECDH,
  PSK exchange, returns a `*RunHandle` that wraps `peer.Conn` and
  holds the `Session` skeleton.
- `runner.OnConnect(runCtx, h) error` — sends `RunnerHello`, awaits
  `RunnerHelloResponse`, then blocks on `pc.Wait`. Handles per-Run
  state.

A thin compatibility shim `runner.Run(ctx, cfg)` that internally
calls `Connect` then `OnConnect` exactly once (no loop, no retry,
errors propagated as-is) is retained so existing tests / callers
(`runner/connect_test.go`) keep compiling. Only
`cmd/agent-runner/main.go` moves to the `PersistLoop` form;
`runner.Run` is the single-shot path used both by the shim and by
tests that want the legacy semantics.

### 5.5 `cmd/harness-tui/main.go` and `tui/app.go`

`cmd/harness-tui/main.go`'s dial-once goroutine becomes:

```go
go func() {
    err := cli.PersistLoop(ctx,
        func(dialCtx context.Context) (cli.PersistHandle, error) {
            c, err := cli.Dial(dialCtx, peerCID)
            if err != nil { return nil, err }
            return &cliClientHandle{c: c}, nil
        },
        func(runCtx context.Context, h cli.PersistHandle) error {
            c := h.(*cliClientHandle).c
            if err := c.SayHello(runCtx, protocol.ClientKind_Tui); err != nil {
                return err
            }
            app.BindClient(c)
            program.Send(tui.ConnectionMsg{Connected: true})
            program.Send(tui.RefreshSnapshot(c)())
            go tui.SubscribeTaskStatus(runCtx, c, program)
            go tui.SubscribeRunnerStatus(runCtx, c, program)
            // Re-issue the active task-log follow if any.
            if id := app.FollowingTaskID(); id != "" {
                go tui.SubscribeTaskLog(runCtx, c, program, id)
            }
            <-runCtx.Done()
            return nil
        },
        cli.PersistConfig{
            Enabled: enabled,
            OnState: func(s cli.PersistState) {
                program.Send(tui.ConnectionMsg{
                    Connected:    s.Phase == cli.PersistPhaseConnected,
                    Reconnecting: s.Phase == cli.PersistPhaseReconnecting,
                    Attempt:      s.Attempt,
                    NextRetry:    s.NextRetry,
                    Err:          s.LastError,
                })
            },
        })
    if err != nil {
        program.Send(tui.ConnectionMsg{Connected: false, Err: err})
    }
}()
```

`tui/app.go` changes:

- `BindClient(*cli.Client)` becomes idempotent / re-entrant: stores
  the new client, drops the previous client reference. Goroutines
  spawned by the previous iteration's `OnConnect` (Subscribe*,
  followTask) have already exited because their `runCtx` was
  cancelled by `PersistLoop` before the new dial began —
  `BindClient` only needs to swap the pointer, not chase down
  workers.
- New method `FollowingTaskID() string` returns
  `a.logs.TaskID()` so reconnect can re-issue the log subscription
  for the currently-followed task. (`tui.LogsModel.TaskID()`
  already exists; this just lifts it to the App level.)
- `update()` handles the extended `ConnectionMsg` and renders the
  status line text per §4.3.
- A 1 Hz ticker (`tea.Tick`) decrements `NextRetry` for live
  countdown display while `Reconnecting`. Stopped when not
  reconnecting.

### 5.6 `cmd/harness-webui-wasm/main.go` and `webui/static/main.js`

`harnessConnect` accepts an options bag:

```go
// JS: harness.connect("ws:...")               → persist:false (compat)
// JS: harness.connect("ws:...", {persist: true, pingInterval: "15s"})
```

Internally calls `cli.PersistLoop` in a goroutine, with `OnState`
forwarding to `harness.onConnectionChange`. Successful connects swap
the package-level `client` pointer under `clientMu`; in-flight JS
calls in `harnessSubmit` etc. continue to call `currentClient` which
now returns the most recent live client (or "not connected" during a
reconnect window).

`webui/static/main.js`:

- Calls `harness.onConnectionChange` once on page boot to register
  the banner renderer.
- Existing `subscribeTaskStatus` / `subscribeRunnerStatus` invocations
  are wrapped in a "(re)subscribe on connected" callback so that
  reconnects automatically re-issue them. (Concretely: subscriptions
  live in `connectedHandlers[]` array; the JS-side
  `onConnectionChange` callback iterates this array on each
  `connected` event.)

## 6. Concurrency model

- Per-process: one `PersistLoop` goroutine, one `peer.Conn` at a time.
  All "current connection" goroutines (Subscribe*, handleAssign,
  RefreshSnapshot) derive from `runCtx` and exit when the connection
  dies. There is never overlap between iteration N's goroutines and
  iteration N+1's: `runCancel()` runs before the next dial.
- TUI: `program.Send` is the only cross-goroutine signal. Bubble Tea
  serialises message handling so `BindClient` re-entry is safe. The
  `app.client` field is read/written only on the bubbletea
  `update()` thread.
- WebUI WASM: `clientMu` guards `client` pointer swaps. JS calls run
  on the WASM main goroutine, Go work runs in `go func()` per Promise.
  Only the `clientMu` invariant matters.

## 7. Edge cases

| Case | Handling |
|---|---|
| Server is down at runner startup | `cli.Dial` errors, `PersistLoop` retries; runner never registers until server comes up. Logged at info level on each attempt. |
| Server restarts cleanly mid-session | Server sends wire `Close` → `pc.Done()` fires immediately → reconnect within the next backoff window. |
| Server killed -9 mid-session | `Receive` errors silently; next `AutoPing` (≤ 15 s) hits `CannotSend` → `pc.Done()` fires; reconnect. |
| PSK file rotated to wrong value | Initial dial fails with `PSKAuthError`; persist mode exits 1 immediately. Logged loudly. |
| Network flap (drop + recover within 1 s) | Backoff starts at `InitialBackoff`. After ≥ `StableReset` (60 s) of healthy connection, attempt counter resets so the next flap also starts at 500 ms. |
| User Ctrl-C during backoff sleep | ctx-aware sleep wakes immediately; loop returns nil. |
| TUI is mid-`SubscribeTaskLog` when the conn drops | The follow goroutine sees `runCtx.Done()` and exits. On reconnect, `OnConnect` re-issues `SubscribeTaskLog` for the still-active follow target (`app.FollowingTaskID()`). User sees a brief gap in the log stream (no replay of missed bytes; log stream is best-effort). |
| Runner has a task running locally when conn drops | `runCancel()` cancels the per-task ctx → `runner.Session.handleAssign` cancels its `taskCtx` → the spawned `claude` process is SIGTERM'd by the existing `process.go::ExecCommand` ctx-cancel path. The new connection registers a fresh runner with no active tasks. |
| WebUI page reload during reconnect | WASM module restarts; `harness.connect` is called again from JS; previous Go goroutines die with the WASM runtime. |
| Ctrl-C between dial success and `OnConnect` returning | `runCtx` derives from `ctx` (parent), so parent cancel propagates; `pc.Close` runs in defer; loop returns nil. |

## 8. Affected files

```
cli/persist.go                  (new)
cli/persist_test.go             (new)
peer/conn.go                    (PingInterval default 30s → 15s)
runner/connect.go               (split Run → Connect + OnConnect; runCtx scoping)
runner/connect_test.go          (cover runCtx propagation to handleAssign)
cmd/agent-runner/main.go        (--persist / --no-persist / --ping-interval / --reconnect-* flags + PersistLoop wiring)
cmd/harness-tui/main.go         (PersistLoop wiring; persist flags)
tui/app.go                      (ConnectionMsg fields; BindClient re-entrancy; FollowingTaskID getter; reconnect status line)
tui/events.go                   (subscribeAndStream already ctx-aware; no change)
cmd/harness-webui-wasm/main.go  (harness.connect options bag; PersistLoop goroutine; onConnectionChange dispatch)
webui/static/main.js            (banner UI; resubscribe-on-connected pattern)
integration/persist_test.go     (new; build tag: integration)
```

## 9. Testing strategy

### 9.1 Unit (`cli/persist_test.go`)

- `TestPersistLoop_HappyPath`: dial succeeds, OnConnect runs, fake handle's `Done()` is closed by the test, loop sleeps `InitialBackoff`, redials. Verify state transitions via `OnState` callback log.
- `TestPersistLoop_ExponentialBackoff`: dial returns error 5×; verify sleeps follow `500ms, 1s, 2s, 4s, 8s` (within jitter band). Use injectable `Sleep` to capture durations, injectable `Now` for `StableReset` boundary.
- `TestPersistLoop_PSKAuthIsFatal`: dial returns `*PSKAuthError`; loop returns immediately even with `Enabled=true`.
- `TestPersistLoop_CtxCancelDuringBackoff`: parent ctx cancel during the sleep; loop returns `nil`.
- `TestPersistLoop_StableResetCounterResets`: connection alive ≥ `StableReset`, then drops; verify next backoff is `InitialBackoff` (counter reset to 1) not `InitialBackoff * factor^N`.
- `TestPersistLoop_DisabledMatchesLegacy`: `Enabled=false` runs exactly one iteration and propagates the error from dial / OnConnect / `pc.Done()`.

### 9.2 Unit (`runner/connect_test.go`)

- `TestRunCtxCancelsHandleAssign`: build a fake `peer.Conn`, fake an `AssignTask` arrival, hook `testHookHandleAssign` to capture the ctx; close the fake conn; verify the captured ctx receives `Done()`. Existing test scaffold can be reused.

### 9.3 Unit (`tui/app_test.go`)

- `TestBindClientReplacesPrevious`: call `BindClient(c1)`, then `BindClient(c2)`; verify `app.client == c2` and that c1's pubsub references are dropped.
- `TestConnectionMsgRendering`: feed each phase variant; assert status line substring.

### 9.4 Integration (`integration/persist_test.go`, build tag `integration`)

- `TestRunnerPersistAcrossServerRestart`:
  1. Start server (port :0), runner with `--persist=true --ping-interval=2s --reconnect-initial=200ms`.
  2. Wait for `Registry.List()` to contain the runner.
  3. Stop the server, wait 1 s, start a fresh server on the same port.
  4. Within 10 s, assert `Registry.List()` contains a runner with the same hostname and allowed roots.
- `TestRunnerNoPersistExitsOnDisconnect`:
  1. Start server, runner with `--no-persist`.
  2. Stop server.
  3. Assert runner process exits within 5 s with non-zero code.
- `TestTuiPersistReconnectsLogStream` (skip on CI without TTY): start server + TUI, submit a task via CLI, drop server briefly, restart, assert `app.View()` contains "reconnecting" then "connected" and the followed log stream resumes.

### 9.5 Manual smoke checklist (added to plan)

- `--no-persist` matches today's UX exactly.
- `--persist` runner: `kill -9` server, restart, no manual intervention.
- `--persist` TUI: server bounces; banner appears and disappears; submit popup remains usable in between (queued for retry-after-reconnect would be a future feature; today it errors with "not connected" and the user retries).
- WebUI: same bounce; banner appears in the page; subscriptions resume.

## 10. Implementation order

1. `peer/conn.go`: PingInterval default 30 s → 15 s.
2. `cli/persist.go` + `cli/persist_test.go`: `PersistLoop`, `PersistConfig`, `PSKAuthError`, unit tests.
3. `runner/connect.go`: split Run → Connect + OnConnect; runCtx scoping; update `runner/connect_test.go`.
4. `cmd/agent-runner/main.go`: persist flags + `PersistLoop` wiring.
5. `cmd/harness-tui/main.go` + `tui/app.go`: TUI persist + `BindClient` re-entrancy + `FollowingTaskID` + reconnect banner.
6. `cmd/harness-webui-wasm/main.go` + `webui/static/main.js`: WebUI persist + JS event hookup.
7. `integration/persist_test.go`.
8. **Commonalisation review**: with all three call sites in place,
   inspect for duplication in the `OnConnect` recipe (subscribe
   re-issue patterns, banner-update boilerplate). If a clean
   abstraction emerges (e.g. a `cli.SubscribeOnConnect` registrar
   that survives reconnects), extract it. If the duplication is
   incidental ("two of the three subscribe to the same two topics")
   leave it alone — three call sites is the YAGNI threshold for
   helpers. Record the decision (extracted X / kept duplication for
   Y) as a final commit on top of the feature, and amend this spec
   with the verdict.

## 11. Open questions

(none — defaults and scope confirmed during brainstorming.)

## 12. Future work

- Server-side `RunnerEntry` grace window so a runner re-using the
  same hostname / allowed-roots can resume in-flight tasks across a
  reconnect (separate spec; cf. §2 non-goals).
- Pubsub replay buffer on the server so subscribers reconnecting
  within N seconds catch up on missed events without a full
  `RefreshSnapshot`.
- CLI subcommand persistence for `harness-cli watch` (the only
  long-lived non-TUI client today).
