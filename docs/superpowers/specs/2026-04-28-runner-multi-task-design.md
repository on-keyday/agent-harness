# Multi-task per runner & multi-repo allowed roots — Design

Date: 2026-04-28

## Motivation

Operating one `agent-runner` process per repo, per machine is friction-heavy: starting and managing multiple processes against the same directory is painful, and the current 1-process / 1-task / 1-repo model wastes the natural concurrency available on modern machines. We want:

- One `agent-runner` per machine (typically), serving multiple repos
- Each runner declaring a configurable concurrency `max_tasks`
- Tasks routed to a specific runner via explicit pin (`--runner` / `--host` / `--ip`) when ambiguous, with safe failure otherwise
- End-to-end functional cancel (today the wire exists but the path is broken)
- Per-task crash domain (one task's panic must not take down siblings)

The design preserves cross-OS / cross-arch deployment (Windows server + Linux runner today, possible Pi later).

## Non-goals

- Dynamic capacity / back-pressure (fixed `--max-tasks` declared at startup)
- Runner-side capacity enforcement (server is authoritative; defense-in-depth is YAGNI for individual dogfood)
- Runner identity persistence across restarts (a re-connected runner is a new entity)
- Cross-runner task migration on runner failure (orphaned tasks are marked `Failed` with reason `runner_disconnected`; they are **not** re-dispatched to another runner)
- Multi-criterion selectors (one of runner_id / hostname / ip, mutually exclusive)

## Architecture overview

| Layer | Today | After |
| --- | --- | --- |
| Runner process | 1 process = 1 RepoPath = 1 task | 1 process = N allowed_roots = up to `MaxTasks` concurrent tasks (default 1) |
| Server registry | `RunnerEntry { RepoPath, Status, CurrentTask }` | `RunnerEntry { Hostname, AllowedRoots[], MaxTasks, ActiveTasks{} }` |
| Scheduler | `OldestIdleForRepo(repo) -> 1 candidate or none` | `Candidates(repo, selector) -> []`; ambiguity is a synchronous error; capacity check at dispatch |
| Wire `RunnerHello` | `repo_path` | `hostname`, `max_tasks`, `allowed_roots[]` |
| Wire `SubmitRequest` / `OpenInteractiveRequest` | `repo_path`, prompt | adds `selector :RunnerSelector` |
| `SubmitResponse` / `OpenInteractiveResponse` | terse | adds synchronous error codes (`ambiguous_runner`, `pinned_not_found`) |
| `AssignTask` / `OpenExecRunnerRequest` | implicit single repo | adds `repo_path` so runner knows which `WorktreeManager` to use |
| Cancel path | server marks state, never forwards to runner | server forwards `RunnerRequestType_CancelTask`; runner cancels per-task ctx |

Invariants preserved:

- 1 task = 1 worktree = 1 claude process. Process boundary is the claude child.
- `exec/frame` scope is unchanged (still 1 PTY exec's stdio + ptyctl). Multi-task multiplexing happens at the trsf layer (different streams, different exec/frame instances), not by extending FrameType.
- TaskID issuance, WAL, logstore semantics are unchanged.

Operating modes after the change:

| Configuration | Flag | Behavior |
| --- | --- | --- |
| Legacy compat | `--max-tasks 1` (default) | Identical to current single-task behavior |
| Personal multi-repo on one box | `--max-tasks 4 --roots /home/kforfk/workspace` | One runner serves any repo under workspace, up to 4 concurrent |
| Heterogeneous | gmkhost (`--max-tasks 4`) + Pi (`--max-tasks 1 --roots /home/pi/workspace`) | Each runner declares capability; scheduler narrows by `allowed_roots` prefix |

## Wire protocol changes (`runner/protocol/message.bgn`)

All schema changes live in this section to keep the spec the single source of truth (per the user's "no split schema" feedback). Generated `message.go` is regenerated via `ebm2go` after the `.bgn` is updated.

### New / changed formats

```bgn
format AllowedRoot:
    path_len :u16
    path :[path_len]u8

format RunnerHello:
    version :u8
    hostname_len :u8
    hostname :[hostname_len]u8
    max_tasks :u16                  # >=1; 0 -> server rejects connection
    allowed_roots_len :u8
    allowed_roots :[allowed_roots_len]AllowedRoot

format ActiveTaskRef:
    task_id :TaskID

format RunnerInfo:
    id :RunnerID
    hostname_len :u8
    hostname :[hostname_len]u8
    status :RunnerStatus            # Idle = has free slot, Busy = at capacity, Offline = disconnected
    max_tasks :u16
    allowed_roots_len :u8
    allowed_roots :[allowed_roots_len]AllowedRoot
    active_tasks_len :u16
    active_tasks :[active_tasks_len]ActiveTaskRef
    connected_at :u64
    last_seen :u64

enum RunnerSelectorKind:
    :u8
    none
    by_runner_id
    by_hostname
    by_ip

format Hostname:
    hostname_len :u8
    hostname :[hostname_len]u8

format IPAddr:
    ip_addr_len :u8                 # 4 or 16
    ip_addr_len == 4 || ip_addr_len == 16
    ip_addr :[ip_addr_len]u8

format RunnerSelector:
    kind :RunnerSelectorKind
    match kind:
        RunnerSelectorKind.none => ..
        RunnerSelectorKind.by_runner_id => runner_id :RunnerID
        RunnerSelectorKind.by_hostname => hostname :Hostname
        RunnerSelectorKind.by_ip => ip :IPAddr

format AssignTask:
    task_id :TaskID
    repo_path_len :u16              # NEW — runner uses this to pick the right WorktreeManager
    repo_path :[repo_path_len]u8
    prompt :[..]u8

format OpenExecRunnerRequest:
    task_id :TaskID
    stream_id :u64
    repo_path_len :u16              # NEW — same reason
    repo_path :[repo_path_len]u8

format SubmitRequest:
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    selector :RunnerSelector
    prompt_len :u32
    prompt :[prompt_len]u8

format OpenInteractiveRequest:
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    selector :RunnerSelector

enum SubmitStatus:
    :u8
    ok = "ok"
    no_runner_for_repo = "no_runner_for_repo"
    ambiguous_runner = "ambiguous_runner"
    pinned_not_found = "pinned_not_found"
    internal_error = "internal_error"

format SubmitResponse:
    status :SubmitStatus
    task_id :TaskID                 # zero on non-ok
    error_msg_len :u16
    error_msg :[error_msg_len]u8    # human-readable, e.g. "matches: gmkhost, raspi"

enum OpenInteractiveStatus:
    :u8
    ok  = "ok"
    no_runner_for_repo = "no_runner_for_repo"
    runner_busy = "runner_busy"     # all matching runners at capacity
    ambiguous_runner = "ambiguous_runner"
    pinned_not_found = "pinned_not_found"
    internal_error = "internal_error"
```

### Synchronous vs queued failure modes

| Condition | Submit | OpenInteractive |
| --- | --- | --- |
| 0 candidates (no runner with matching root) | sync error `no_runner_for_repo` | sync error `no_runner_for_repo` |
| ≥2 candidates without pin | sync error `ambiguous_runner` | sync error `ambiguous_runner` |
| Pin specified, no runner matches | sync error `pinned_not_found` | sync error `pinned_not_found` |
| 1 candidate, all matching runners at capacity | **Queued** (waits for capacity) | sync error `runner_busy` (interactive cannot queue) |
| 1 candidate with capacity available | `ok` + task_id, dispatch | `ok` + task_id + stream_id, splice |

### Breaking-change policy

Per the "individual dogfood" memory, the wire format is changed in place. Old runners reaching a new server (or vice versa) result in connection refusal. No v1↔v2 dual support layer.

## Server data model and scheduler (`server/registry.go`, `server/dispatch.go`, `server/taskstore.go`)

### `RunnerEntry`

```go
type RunnerEntry struct {
    ID           string                  // = ConnectionID.String()
    Hostname     string                  // from RunnerHello.hostname
    AllowedRoots []string                // absolute, filepath.Clean'd at Hello receipt
    MaxTasks     int                     // from RunnerHello.max_tasks (>=1)
    ActiveTasks  map[string]struct{}     // task_id (hex) set; len() = current load
    ConnectedAt  time.Time
    LastSeen     time.Time
    Conn         ConnHandle
}

func (e *RunnerEntry) Status() protocol.RunnerStatus {
    if e.Conn == nil { return protocol.RunnerStatus_Offline }
    if len(e.ActiveTasks) >= e.MaxTasks { return protocol.RunnerStatus_Busy }
    return protocol.RunnerStatus_Idle
}
```

### Registry method changes

| Old | New | Notes |
| --- | --- | --- |
| `SetStatus(id, status, currentTask)` | `BindTask(id, taskID) bool` / `UnbindTask(id, taskID)` | `BindTask` performs check-and-insert atomically under `mu`; returns false on capacity exceeded. |
| `SetIdleIfBoundTo(id, wantTaskID)` | `UnbindTask(id, taskID)` (idempotent on absent task) | Subsumes the old defensive cleanup path. |
| `OldestIdleForRepo(repo)` | `Candidates(repo string, sel RunnerSelector) []RunnerEntry` | Filters by allowed_roots prefix match (directory-boundary aware, see below) ∧ selector match. **Capacity-agnostic** — caller (dispatcher) handles capacity. |
| `Add` / `Remove` / `Get` / `List` / `SetLastSeen` | unchanged | snapshot semantics preserved. |

`Candidates` is capacity-agnostic by design: ambiguity must be detected even when matching runners are all busy, otherwise the user gets a "queued forever" mystery.

**Prefix-match semantics (binding contract for both server and runner):** A path `repo` is "under" an allowed root `r` iff `filepath.Rel(r, repo)` succeeds and the result is neither absolute nor begins with `..`. Both `r` and `repo` are `filepath.Clean`'d before the check. This rules out the naive-`HasPrefix` failure mode where `/home/foo` would falsely match a root of `/home/fo`. Server-side `Candidates` and runner-side `repoAllowed` (see Runner internals below) **must use the same predicate**, otherwise mismatched semantics produce confusing "queued forever" or "TaskFinished: not in allowed_roots" errors. Recommended: a single shared helper in a small package (e.g. `runner/protocol/pathmatch.go`) imported by both sides.

### `TaskEntry` additions

```go
type TaskEntry struct {
    // ... existing fields ...
    Selector       RunnerSelector  // NEW: persisted in WAL task_queued event
    BoundRunnerID  string          // NEW: ID of the unique candidate at submit time
}
```

`BoundRunnerID` records the runner chosen at submit time. The dispatcher reuses it as a hint, but re-evaluates `Candidates(repo, Selector)` each dispatch attempt. If the bound runner has disconnected and a new unique candidate appears, the task is rebound. If `Candidates` returns ambiguous or empty during dispatch, the task stays Queued silently (no synthetic error event — submit-time rejection is already the contract for "ambiguous").

### Submit handling (synchronous decision)

```go
func (s *Server) handleSubmit(req *SubmitRequest) SubmitResponse {
    cands := registry.Candidates(req.RepoPath, req.Selector)
    switch {
    case len(cands) == 0 && req.Selector.Kind != none:
        return SubmitResponse{Status: pinned_not_found}
    case len(cands) == 0:
        return SubmitResponse{Status: no_runner_for_repo}
    case len(cands) > 1:
        return SubmitResponse{
            Status:   ambiguous_runner,
            ErrorMsg: fmt.Sprintf("matches: %s", joinHostnames(cands)),
        }
    }
    bound := cands[0]
    task := taskstore.Add(req, bound.ID, req.Selector)  // Queued
    dispatcher.Wake()
    return SubmitResponse{Status: ok, TaskId: task.ID}
}
```

### Dispatcher loop

```go
func (d *Dispatcher) tryDispatch(task *TaskEntry) bool {
    cands := registry.Candidates(task.RepoPath, task.Selector)
    if len(cands) != 1 { return false }   // ambiguous or empty -> wait
    runner := cands[0]
    if !registry.BindTask(runner.ID, task.ID) { return false }  // at capacity -> wait
    task.BoundRunnerID = runner.ID
    if err := sendAssignTask(runner.Conn, task); err != nil {
        // Connection dropped between bind and send. Roll back the slot so the
        // task can be re-dispatched (to another candidate or the same runner
        // after reconnect) without leaking capacity. UnbindTask is idempotent.
        registry.UnbindTask(runner.ID, task.ID)
        task.BoundRunnerID = ""
        return false
    }
    return true
}
```

The bind-then-send-then-unbind-on-error sequence is the dispatcher's responsibility, not the registry's. The same rollback applies to `sendCancelTask` failures in the cancel path below — but there capacity is intentionally not released, since the eventual `TaskFinished` from the runner (whether cancellation propagates or the task completes naturally) will release it via the standard TaskFinished path.

### TaskFinished handling (capacity release)

`server/runner_handler.go` already processes incoming `TaskFinished`. The change here: it must call `registry.UnbindTask(runnerID, taskID)` to free the slot, in addition to the existing `taskstore` update. This applies on every TaskFinished — normal completion, timeout, panic, and cancel — so capacity is always released exactly once per task lifetime. Today's logic (single `CurrentTask` cleared via `SetIdleIfBoundTo`) collapses into `UnbindTask`.

`UnbindTask(id, taskID)` is **idempotent**: calling it on a runner that does not currently hold `taskID` (e.g. due to dispatcher rollback that already removed it, or a duplicate TaskFinished from a misbehaving runner) is a no-op rather than an error. This protects against double-release scenarios — the dispatcher's bind-then-send-then-unbind-on-error rollback path (below) and the TaskFinished receiver could theoretically race on the same task; idempotency makes that race benign.

### Cancel forwarding

```go
// taskstore.Cancel triggers OnCancel(taskID) after marking the state Cancelled.
// dispatcher subscribes:
func (d *Dispatcher) onCancel(taskID string) {
    task, ok := taskstore.Get(taskID)
    if !ok || task.BoundRunnerID == "" { return }  // never dispatched
    runner, ok := registry.Get(task.BoundRunnerID)
    if !ok { return }                              // runner gone
    sendCancelTask(runner.Conn, taskID)            // RunnerRequestType_CancelTask
}
```

The eventual `TaskFinished` from runner is dropped by `taskstore.Cancel`'s terminal-state idempotency at `taskstore.go:191`. The dispatcher must still call `UnbindTask` on TaskFinished so capacity is freed.

### Runner disconnect cleanup

When a runner's connection drops mid-flight, the existing `Registry.OnRemove` callback (at `server.go:149`) only publishes a `RunnerOffline` event; tasks bound to that runner stay in `Running` state until the next server restart, when WAL replay marks them `Failed`. Multi-task amplifies this: with `--max-tasks 4`, a single disconnect strands up to 4 tasks visible as `Running` in `harness-cli ls`.

The change here: extend `OnRemove` to mark all of the disconnecting runner's `ActiveTasks` as `Failed` with `DiffInfo = "runner_disconnected"`, before the entry is removed from the registry.

```go
s.registry.OnRemove = func(id string, snapshot RunnerEntry) {
    for taskID := range snapshot.ActiveTasks {
        s.tasks.MarkFailed(taskID, "runner_disconnected")  // NEW taskstore method
    }
    publishRunnerEvent(id, protocol.StatusEventKind_RunnerOffline, protocol.RunnerStatus_Offline)
}
```

Notes:

- `Registry.OnRemove` signature is widened from `func(id string)` to `func(id string, snapshot RunnerEntry)` so the callback can iterate the now-stranded tasks. The snapshot is taken atomically inside `Remove` under the registry's lock, before deletion.
- `taskstore.MarkFailed(taskID, reason string)` is a new method that transitions any non-terminal task to `Failed` with `ExitCode: -1` and `DiffInfo: reason`. It is idempotent on already-terminal tasks (same idempotency contract as `Cancel`).
- This is **not** task migration — the task is permanently failed; the user inspects `harness-cli ls` and re-submits manually if desired. Re-dispatch to another runner remains out of scope.
- WAL gets a standard `task_failed` event so server restart sees the cleaned state on replay.
- A late-arriving TaskFinished from the disconnecting runner (e.g., the runner came back briefly and flushed before the server noticed the drop) hits `MarkFailed`'s terminal-state idempotency and is silently dropped — no double-state-transition.

## Runner internals (`runner/session.go`, `runner/connect.go`, `runner/worktree.go`)

### `Session`

```go
type Session struct {
    AllowedRoots    []string
    ClaudeBin       string
    ExtraClaudeArgs []string
    MaxTasks        int                          // informational (server enforces)
    Timeout         time.Duration
    Sender          Sender
    Streams         peer.BidirectionalStreamLookup
    Logger          *slog.Logger
    Now             func() time.Time

    mu    sync.Mutex
    tasks map[string]*taskHandle                 // key = task_id (hex)

    wmsMu sync.Mutex
    wms   map[string]*WorktreeManager            // key = absolute repo path
}

type taskHandle struct {
    cancel context.CancelFunc                    // CancelTask -> calls this
    repo   string                                // for cleanup
}
```

### `handleAssign` (per-task panic recovery + ctx + cancel registration)

```go
func (s *Session) handleAssign(parentCtx context.Context, req *protocol.AssignTask) {
    taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
    repo := filepath.Clean(string(req.RepoPath))

    if !s.repoAllowed(repo) {
        s.sendTaskFinished(req.TaskId, -1, "repo not in allowed_roots: "+repo)
        return
    }
    // repoAllowed must use a directory-boundary prefix check, not naive
    // strings.HasPrefix, otherwise /home/kforfk/workspace-evil would falsely
    // match a root of /home/kforfk/workspace. Implementation:
    //   filepath.Rel(root, repo) returns a path that does not start with ".."
    //   and is not absolute. (Both root and repo are filepath.Clean'd up front.)

    taskCtx, cancel := context.WithCancel(parentCtx)
    s.mu.Lock()
    s.tasks[taskIDHex] = &taskHandle{cancel: cancel, repo: repo}
    s.mu.Unlock()
    defer func() {
        s.mu.Lock()
        delete(s.tasks, taskIDHex)
        s.mu.Unlock()
        cancel()
    }()

    defer func() {
        if r := recover(); r != nil {
            s.logger().Error("task panic",
                "task_id", taskIDHex, "panic", r, "stack", string(debug.Stack()))
            s.sendTaskFinished(req.TaskId, -1, fmt.Sprintf("panic: %v", r))
        }
    }()

    wm := s.getWorktreeManager(repo)
    // ... rest of existing handleAssign flow, with `taskCtx` instead of `ctx` ...
}
```

`handleOpenExec` mirrors the same shape (tasks map registration + panic recovery + per-repo WM).

### `getWorktreeManager`

```go
func (s *Session) getWorktreeManager(repo string) *WorktreeManager {
    s.wmsMu.Lock()
    defer s.wmsMu.Unlock()
    if wm, ok := s.wms[repo]; ok { return wm }
    wm := &WorktreeManager{Repo: repo}
    s.wms[repo] = wm
    return wm
}
```

### Cancel handler (`connect.go` `OnControl`)

```go
case protocol.RunnerRequestType_CancelTask:
    ct := req.CancelTask()
    if ct == nil { return }
    taskIDHex := hex.EncodeToString(ct.TaskId.Id[:])
    s.mu.Lock()
    h, ok := s.tasks[taskIDHex]
    s.mu.Unlock()
    if !ok {
        cfg.Logger.Info("cancel for unknown task", "task_id", taskIDHex)
        return
    }
    h.cancel()  // Process.Run's runCtx -> SIGTERM (5s grace) -> SIGKILL
```

The `runner: cancel not implemented` log line at `connect.go:81` is removed.

### Per-repo serialization in `WorktreeManager`

```go
type WorktreeManager struct {
    Repo string
    mu   sync.Mutex   // serializes Create/Remove on this repo to dodge git index.lock contention
}
```

Different repos run concurrently (different `WorktreeManager` instances). Same repo is serialized.

### `cmd/agent-runner/main.go` flags

```go
var (
    serverCID  = flag.String("server-cid", "ws:127.0.0.1:8539-*", "...")
    rootsCSV   = flag.String("roots", ".", "comma-separated absolute paths the runner is allowed to serve")
    maxTasks   = flag.Int("max-tasks", 1, "maximum concurrent tasks (>=1)")
    claudeBin  = flag.String("claude-bin", "claude", "...")
    claudeArgs = flag.String("claude-args", "", "...")
    wsPath     = flag.String("ws-path", "/ws", "...")
)
```

The old `-repo` flag is removed. Each path in `--roots` is `filepath.Abs` + `filepath.Clean`'d at startup.

## CLI / TUI / WebUI surface

### `harness-cli`

```
submit --repo PATH --task TEXT [--runner ID | --host NAME | --ip ADDR]
ls                              # display change (see below)
cancel TASK_ID                  # path now reaches the runner
prune ...                       # unchanged
prune-local ...                 # unchanged
logs TASK_ID                    # unchanged
watch                           # unchanged
interactive --repo PATH [--runner ID | --host NAME | --ip ADDR]
```

Specifying two or more of `--runner`, `--host`, `--ip` is a client-side validation error; the request is never sent. Empty selector serializes as `RunnerSelectorKind.none`.

### `harness-cli ls` output

```
RUNNERS
  Idle    host=gmkhost  tasks=0/4  roots=/home/kforfk/workspace,/home/kforfk/projects  id=ws:192.168.3.14:33994-1
  Busy    host=raspi    tasks=1/1  roots=/home/pi/workspace                            id=ws:192.168.3.99:54012-2
TASKS
  ...                       host=gmkhost  ...
```

`tasks=N/M` shows `len(ActiveTasks)/MaxTasks`. The `host=` column on tasks resolves from `BoundRunnerID` to hostname for legibility.

### TUI

- Runner pane: `repo` column → `roots`, plus a `tasks` column.
- Task submit dialog: existing repo selector stays; add an optional pin dropdown (None / each connected hostname).
- Interactive attach: existing path → runner 1:1 mapping is replaced by `Candidates` lookup. On `ambiguous_runner`, the dialog prompts the user to pick a runner instead of failing silently.

### WebUI

- Runner select dropdown shows roots; same pin UI as TUI.
- Client-side validation mirrors the CLI's mutual-exclusion rule.

## Backward-compatibility / migration

- Wire format is broken in place; rebuild the trio (server, runner, cli/tui/webui) together. `RunnerHello.version` stays at `1`. Mismatched builds fail at decode time (any of the new mandatory fields will misalign), which is acceptable for individual dogfood — there is no need for a runtime version-detection path.
- The default `--max-tasks 1` makes the new build behave exactly like the old build for users who don't opt in.
- WAL replay: existing `task_queued` events lack the `Selector` field. Replayed tasks default to `Selector.Kind = none`, which is functionally identical to the old "any runner with matching repo" semantics.

## Test strategy

### Unit (new / changed)

| Target | Test |
| --- | --- |
| `server/registry.go` | `Candidates` filters: prefix match, selector kinds, 0 / 1 / N candidates, capacity-agnostic |
| `server/registry.go` | `BindTask`: capacity-exceeded returns false; concurrent `BindTask` race-safe under `-race` |
| `server/registry.go` | `UnbindTask` idempotency on absent task |
| `server/dispatch.go` | `tryDispatch`: BoundRunnerID hint + `Candidates` re-eval; ambiguous mid-flight stays Queued |
| `server/task_handler.go` | Synchronous Submit error codes: `no_runner_for_repo`, `ambiguous_runner`, `pinned_not_found` |
| `server/task_handler.go` | Cancel forwarding: Queued -> state-only; Running -> CancelTask sent (mock conn) |
| `runner/session.go` | `handleAssign` panic recovery: `TaskFinished{ExitCode: -1, "panic: ..."}` sent; deregistration |
| `runner/session.go` | Per-repo `WorktreeManager` lazy creation and reuse |
| `runner/connect.go` | `CancelTask` receipt invokes the registered cancel func |
| `runner/worktree.go` | Per-repo mutex serializes concurrent `Create` on the same repo |
| `server/taskstore.go` | `MarkFailed(taskID, reason)` transitions non-terminal -> Failed; idempotent on terminal |
| `server/server.go` | `OnRemove` callback marks all of the disconnecting runner's `ActiveTasks` as `Failed` with reason `runner_disconnected`, before publishing `RunnerOffline` |

### Integration

| Case | Expectation |
| --- | --- |
| `--max-tasks 2` with 2 concurrent submits | Both run in parallel; both `TaskFinished` arrive (order-independent) |
| `--max-tasks 1` with 2 sequential submits | First Running, second Queued; second auto-dispatches on first's TaskFinished |
| 2 runners with overlapping roots, no pin | Submit returns `ambiguous_runner` synchronously; nothing queued |
| `--runner ID` valid pin | Submit `ok`, runs only on that runner |
| `--runner BAD-ID` | Submit returns `pinned_not_found` |
| Cancel a Running task | claude killed within ~5s; `TaskFinished` received; state stays Cancelled (idempotent) |
| Inject panic in one task | Sibling tasks survive; panic task ends with `ExitCode: -1, DiffInfo: "panic: ..."` |
| Runner disconnect with N Running tasks (`--max-tasks 4`) | All N transition to `Failed` with `DiffInfo: "runner_disconnected"`; `harness-cli ls` no longer shows zombie `Running`; capacity is freed (entry removed) |

### Backward-compat regression

Existing tests run unchanged with `--max-tasks 1` defaulted. A single new test asserts `Candidates + BindTask` with N=1 is behaviorally equivalent to the removed `OldestIdleForRepo`.

## Risks / verification items

1. **Cross-OS `git worktree`**: Concurrent `git worktree add` against a repo on Windows. The per-repo mutex serializes us, but verify the lock is held long enough across `git`'s own staging on a real Windows host.
2. **WAL Selector replay**: Old WAL files lack the Selector field; replay defaults to `none`. Documented; no rejection or migration path.
3. **`RunnerID.IpAddrLen` invariant** (per project memory): `--ip` selector must never serialize an empty IP — empty selector fields collapse to `RunnerSelectorKind.none` on the client side, so the encoder never sees a zero IP.
4. **`harness-cli ls` line width**: Multiple long absolute roots can exceed terminal width. Cosmetic; deferred. A future flag (`-w`, wrap or compact) handles it without protocol changes.
5. **Live `MaxTasks` / `AllowedRoots` change**: Out of scope. Restart the runner to re-Hello.

## Out of scope

- Dynamic capacity / back-pressure.
- Runner-side capacity enforcement.
- Persistent runner identity across reconnect.
- Cross-runner task migration on runner failure (orphaned tasks become `Failed`, not re-dispatched).
- Compound selectors (one of runner_id / hostname / ip; not "host AND ip").
