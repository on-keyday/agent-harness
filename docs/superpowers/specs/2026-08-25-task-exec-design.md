# `exec` — running a command in a task's worktree, out of band — design

## Problem

There is no way to run a command in a task's worktree as an independent
process. Three things come close and each is the wrong shape:

- **`session exec`** (`cli/exec_native.go:53`) injects a line into the
  session's **foreground shell** and watches for a sentinel. That is
  deliberate — its own doc says a runner-side out-of-band exec "cannot serve
  this use", because the point is to run in whatever shell context is live
  there, ssh hops and netns included. The consequences are equally deliberate:
  it needs a POSIX shell in the foreground (a session running an agent TUI has
  none), stdout and stderr arrive interleaved on one PTY, and the injected line
  appears in the scrollback of whoever is watching that session.
- **`submit --agent bash --task "<cmd>"`** runs an arbitrary command properly,
  as its own process — but in a **fresh worktree of its own**, so it cannot see
  the task's working tree, and it leaves a task row and a worktree behind for
  every command.
- **`git TASK_ID …`** does run a program in the task's worktree, out of band,
  with stdout and stderr separated (`runner/git_query.go:472`) — but its argv is
  built from a fixed enum, because it is a read-only git view, not an exec.

So the operator can type a command into a session's shell, or start a whole new
task, but cannot simply run one in the tree an agent is working in. Concretely
this blocks:

- **P1.** `make test` / `git status` / `ls` against a task's actual tree from a
  script, with the command's own exit code as the caller's exit code.
- **P2.** The same from the TUI and the WebUI without taking over, or typing
  into, the session an agent is using.
- **P3.** `ssh <task>@gateway <command>` meaning what it means everywhere else.
  The ssh gateway refuses `exec` today
  (`docs/superpowers/specs/2026-08-25-ssh-gateway-design.md`) precisely because
  the only mapping available was `session exec`, whose semantics are not ssh's.

This spec is about **the exec, not about ssh**. `harness-cli exec` is the
deliverable; the gateway's `exec` request becomes a thin mapping onto it, and
would be worth building even if the gateway did not exist.

## Decisions taken

Every line here is decided. The third column records who decided: **operator**
means the human chose it in conversation, **this spec** means the author chose
it while writing — those are the rows worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Out-of-band: a separate process in the task's worktree, never the session's foreground shell | operator |
| D2 | No task is created. Visibility comes from a server-side registry, the way port forwards get theirs | operator |
| D3 | CLI, TUI and WebUI all get it in v1 | operator |
| D4 | The wire carries **argv**; a caller that wants a shell sends `["sh","-c",line]` itself | this spec |
| D5 | cwd is the task's live worktree; a task without one is refused | this spec |
| D6 | stdin is carried | this spec |
| D7 | No timeout. `exec kill` and dropping the control stream are the ways to stop one | this spec |
| D8 | New capability `exec_run = 0x8000`, and `all` widens to `0xffff` | this spec |
| D9 | The exit code travels on harness wire, NOT as a new `objtrsf/exec` frame type | this spec |
| D10 | **Synchronous only.** The child dies with the exec's streams; there is no detached form, because that form already exists and is called a task | this spec |
| D11 | An exec is visible to anyone who can see its target task, and `TaskInfo` carries a running-exec count so the task's own row reports it | this spec |

## Why the exit code is not a frame (D9)

The natural-looking place for it is the exec frame protocol, which already
carries Stdout / Stderr / Stdin / Control. It has no exit shape: `ControlType`
in `objtrsf/exec/frame/frame.bgn` is `terminal_window_size` and `signal`, and
nothing else.

Adding one is refused by a decision this project already made —
`project_exec_frame_legacy`: `exec/frame` is pre-trsf legacy, kept as generated,
and **sideband goes in separate trsf streams**. So the exit code rides a control
stream beside the data stream, which is also exactly how a `-R` port forward
reports its own out-of-band events (`server/port_forward_registry.go:16`: the
server "pushes tagged PortForwardEvent records onto it — conn_notify per
accepted connection, closed on teardown").

## Shape

```
harness-cli exec <task-id> -- <argv>...      # stdout→stdout, stderr→stderr, exit code→exit code
harness-cli exec ls [--task TASK_ID] [--json]
harness-cli exec kill <exec-id> [<exec-id> ...]
```

```
             ┌── control stream ──▶ exited{code} / killed / failed
 client ─────┤
             └── data stream ────▶ Stdout / Stderr frames, ◀── Stdin frames
                        │
                  harness-server  (registry: exec_id → entry)
                        │
                    agent-runner  ExecuteCommandWithOption(…, ptyEnabled=false, …)
                                  cwd = <repo>/.harness-worktrees/<task-id>/
```

The data plane is not new code. `runner/streamtask.go:139` already runs a
non-PTY child over one of these streams for the event-stream task kind:
separate `FrameType_Stdout` / `FrameType_Stderr`, `FrameType_Stdin` inbound. The
only thing missing is a request that names an argv instead of a profile's
adapter.

## Wire

All of it, in one place — this is the authoritative interface
(`feedback_no_split_schemas`). Added to `runner/protocol/message.bgn`.

New enum members:

```
enum TaskControlKind:      # client ↔ server
    …
    open_exec_run          # start one; returns the two stream ids
    exec_run_list          # registry listing
    exec_run_kill          # stop one by id

enum RunnerRequestType:    # server → runner
    …
    open_exec_run

enum RunnerMessageType:    # runner → server
    …
    exec_run_finished
```

New formats:

```
# One argv element. u16 per element matches ClaudeArg, for the same reason:
# individual arguments are short in practice and 64 KiB is past any real one.
format ExecArg:
    arg_len :u16
    arg :[arg_len]u8

format ExecArgv:
    argv_len :u16
    argv :[argv_len]ExecArg

# Client → server.
format ExecRunRequest:
    task_id :TaskID
    argv :ExecArgv
    # stdin_enabled=0 means the child's stdin is closed immediately, which is
    # what a non-interactive caller wants: a child that reads stdin gets EOF
    # instead of hanging forever on a pipe nobody will write to.
    stdin_enabled :u1
    reserved :u7

enum ExecRunStatus:
    :u8
    ok
    not_found            # unknown task id, or outside the caller's scope
    no_worktree          # the task has no live worktree to run in (D5)
    runner_unreachable
    empty_argv
    denied               # capability check failed
    internal_error

format ExecRunResponse:
    status :ExecRunStatus
    exec_id :u64
    data_stream_id :u64
    control_stream_id :u64

# Server → client, on the control stream. One record, then the stream closes.
enum ExecEventKind:
    :u8
    exited               # the child ran and ended; exit_code is its own
    killed               # exec_run_kill, or the runner was torn down
    failed               # the child never started (no such binary, cwd gone)

format ExecEvent:
    kind :ExecEventKind
    # The child's own exit code for `exited`, -1 for the other two — the same
    # convention TaskFinished uses, and the same reason: a signal death has no
    # exit code to report.
    exit_code :i32
    detail :[..]u8       # human-readable; empty for a clean `exited`

# Server → runner.
format RunnerExecRunRequest:
    exec_id :u64
    task_id :TaskID
    auth_ticket :[16]u8
    stream_id :u64       # bidi stream toward the runner, fed to ExecuteCommandWithOption
    argv :ExecArgv
    stdin_enabled :u1
    reserved :u7

# Runner → server.
format ExecRunFinished:
    exec_id :u64
    exit_code :i32
    kind :ExecEventKind
    detail :[..]u8

# Client ↔ server: the listing.
format ExecRunInfo:
    exec_id :u64
    task_id :TaskID
    started_unix_ms :u64
    argv :ExecArgv
    origin_kind :ClientKind
    origin_cid_len :u8
    origin_cid :[origin_cid_len]u8

format ExecRunListRequest:
    task_filter :TaskID  # all-zero = every exec the caller may see

format ExecRunListResponse:
    execs_len :u16
    execs :[execs_len]ExecRunInfo

format ExecRunKillRequest:
    exec_id :u64

format ExecRunKillResponse:
    status :ExecRunStatus
```

One field on an existing format:

```
format TaskInfo:
    …
    exec_count :u16      # running execs against this task's worktree (D11)
```


Capability enum change:

```
    exec_run       = 0x8000, "exec_run"
    all            = 0xffff, "all"
```

`all` is a literal, not "every defined bit", so it widens by hand — the same
edit `exec_view` / `exec_cowrite` / `exec_resize` each needed. The WAL persists
the NUMBER, so a task already granted `all` holds `0x7fff` and does **not** gain
`exec_run` until it is re-granted. That is the safe direction and needs no
migration.

## Runner behaviour

On `open_exec_run`:

1. Resolve the worktree for `task_id`. Absent → `no_worktree`; the task is
   terminal and its tree was removed, or it never had one. `git_query`'s
   fallback (run in the repo against the retained `harness/<id>` branch) is
   **not** copied here: it is safe for a read-only git view and wrong for an
   arbitrary command, which would silently run against a different tree.
2. `agentexec.ExecuteCommandWithOption(ctx, stream, log, argv[0], argv[1:], dir,
   false, env, ExecuteOption{OnProcessExit: …})` — `ptyEnabled=false`, the
   `streamtask.go` shape.
3. Report the outcome as `ExecRunFinished`. The exit code comes from
   `OnProcessExit`'s `*os.ProcessState`, not from the returned error: the
   errgroup inside `agentexec` surfaces whichever goroutine failed first, and
   the same race that made `session.go:851` read ProcessState applies here.

`env` is the task's environment as the runner already builds it for that task,
including `HARNESS_*`. A command run in a task's tree should see what that task
sees; needing a different environment is what `env VAR=x <cmd>` in the argv is
for.

The child is NOT the task's process group and does not touch the session's PTY:
an exec running while an agent works in the same tree is two processes sharing a
directory, exactly as two shells would be. Concurrency is the operator's
business, as it already is for `session exec` and `file push`.

## Server behaviour

An `execRegistry` beside `portForwardRegistry`, same shape: `exec_id → entry`,
a mutex, a monotonic counter starting at 1 (so 0 is never an id).

- `open_exec_run`: capability + scope check, allocate `exec_id`, create the two
  streams toward the client and the data stream toward the runner, splice the
  data pair with the existing `spliceBidi` (`server/task_handler.go:1443`),
  register, answer.
- `exec_run_finished` from the runner: look the entry up, push one `ExecEvent`
  onto its control stream, close it, drop the entry.
- `exec_run_kill`: cancel the runner side, push `killed`, drop the entry.
- **The entry's lifetime is its control stream's lifetime, and the child's too.**
  A client that dies takes its execs' registrations with it AND cancels their
  children (D10); there is no TTL to tune, no reaper to write, and nothing left
  running in a worktree with nobody watching it.

## Synchronous only (D10)

An exec lives exactly as long as its caller is there to receive it. When the
control stream goes away — the client exited, the terminal closed, the ssh
connection dropped — the runner cancels the child through the same
SIGHUP → SIGTERM → SIGKILL ladder a detaching session's agent gets, and the
registration is dropped. Nothing survives to be collected later.

That is a decision, not an omission, and it rests on the asynchronous form
already existing: **`submit` is the detached one.** A task runs without a
client attached, buffers its output in the log store, reports its status and
exit code, can be cancelled and can be pruned. Giving `exec` a detached mode
would mean a second, worse copy of all of that — starting with an output ring
per exec, because a child whose stdout nobody is reading needs somewhere to put
it, and that ring is exactly the machinery `SessionMux` already maintains for
detached sessions.

So the line is: **`exec` answers "run this now and tell me", `submit` answers
"run this and I will come back".** The CLI help says so, because an operator who
reaches for `exec` to start something long-running should be told where to go
instead rather than discovering it when their laptop sleeps.

The ssh gateway inherits the same semantics, which is the point: a dropped ssh
connection killing the command it was running is what `ssh host cmd` does
everywhere else.

## Who can see one (D11)

An exec is not private to the client that started it. The registry is shared,
the way the port-forward one is, and the rules follow the gates this server
already draws (`server/capabilities.go:10`):

| Question | Answer | Following |
| --- | --- | --- |
| May I list running execs? | **No capability needed**, bounded by task visibility | `ListPortForwards` is INFO-scoped — `visibleToCaller`, not a single cap. `AwaitIdle` needs none for the same reason: gating a fact `ls` already hands out would make the direct path cost more authority than polling for it |
| May I see the argv? | Yes, to anyone who can see the task | A task's `prompt` is already in its `ls` row; an exec's argv is the same kind of fact about the same subject |
| May I kill someone else's? | `exec_run` + the task in scope | The bit that authorizes running commands in a tree authorizes stopping them. Requiring `cancel` instead would leave a holder of `exec_run` able to start what it cannot stop |
| May I read its output? | **No** | There is one reader, by D10. `forward ls` does not let you read a forward's bytes either |

**And the task's own row reports it.** `TaskInfo` gains `exec_count :u16`, filled
from the registry, so an operator watching a task sees that commands are running
in its tree without going to look for them. That is the same answer the session
observer counts give (`cowrite=0 viewer=0`), for the same reason — someone else
is touching this thing — and it follows their rule too: **the count is printed
at zero.** A row that elides `exec=0` reads as "this row does not report execs",
which is exactly the ambiguity the observer counts were added to remove. It is
printed for a task that HAS a worktree to run in and omitted for one that does
not, gating on existence rather than on value.

## Capability and scope

`exec_run` gates starting and killing, and `--scope` bounds WHICH tasks it may
target — the same pair every task-targeted verb uses. Listing is scoped by
visibility instead, per D11.

It is a new bit but not a new class of authority: a holder of `exec_cowrite`
can already type any command into that session's shell, and a holder of `spawn`
can already run arbitrary commands on the runner through the `bash` profile.
What `exec_run` adds is a shape — synchronous, non-invasive, no task created —
not a power. It is default-off for agents like every other bit, and operator
surfaces hold it through `all`.

## Surfaces

| Surface | What |
| --- | --- |
| CLI | `exec <task-id> -- <argv>`, `exec ls`, `exec kill` |
| TUI cmdline | `exec <task-id> <cmd>…`, `exec ls`, `exec kill <id>` |
| TUI display | output into the log pane region; running execs listed like forwards |
| WebUI | a task-sheet action that runs a command and streams output into the task's output area; a listing beside the forwards one |
| WebUI command input | `exec …` in `runCmd` |
| wasm bridge | request construction for the above |
| ssh gateway | `exec` maps onto it, sending `["sh","-c",<command>]`; stdout → channel, stderr → extended data, exit code → `exit-status` |
| Display of `exec_count` | `ls` rows and `ls --json`, the TUI task table and detail popup, the WebUI task row meta and detail sheet, and the wasm snapshot conversion — the same set the observer counts had to reach, and for the same reason |

The gateway mapping is what makes `ssh <task>@gw 'git status'` behave the way
that syntax promises everywhere else — separate streams, real exit code, no
injection into anyone's terminal. Its refusal text and the two spec sections
that explain the refusal are replaced when it lands.

The `surface-parity-checklist` skill is walked item by item during plan writing.

## Errors

Every refusal names its subject and the reason. `not_found` covers both "no
such task" and "out of your scope", deliberately: reporting the difference would
tell a caller that a task it may not see exists, which is the rule
`handleSubmitResume` already follows (`server/task_handler.go:788`).

A child that cannot start is `failed` with the OS error in `detail`, not
`exited` with a made-up code — "127" is a shell's convention and there is no
shell here.

## Testing

Unit: argv encode/decode round trip, the registry's add/finish/kill/drop
transitions, `all` widening, the CLI's `--` argv split.

Integration (`integration/`), against a live server + runner + a task with a
worktree: a command's stdout and stderr arrive **separately** and in full; its
exit code reaches the client; a non-zero exit is reported as `exited` with that
code; a missing binary is `failed`, not `exited`; `exec ls` shows a running one
and stops showing it after it ends; `exec kill` ends one and the child dies;
dropping the control stream deregisters it; a terminal task with no worktree is
refused with `no_worktree`; a task outside the caller's scope is `not_found`.

Visibility (D11): a second client, with no `exec_run`, sees a running exec in
`exec ls` and sees `exec_count` on the task's row; the same client with
`exec_run` and the task in scope can kill it; a client that cannot see the task
sees neither. And `exec_count` is `0`, not absent, on a task with a worktree and
nothing running.

The one that needs saying out loud: **stdout and stderr must be asserted
separately**, with a command that writes to both. Merging them is the exact
defect this feature exists to avoid, and a test that reads only the combined
output would pass on the thing we are replacing.

Live: run it against a real session's worktree from all three surfaces, and
through the ssh gateway, with a command that takes long enough to watch
(`exec ls` while it runs, `exec kill` to stop it).

Wire discipline: `scripts/wire-skew-check.sh` before landing, and **restart the
server first** — this is a `.bgn` change and Pitfall 10 is about exactly this.

## Non-goals

- **A PTY.** This is the non-PTY path on purpose. `session attach` and the ssh
  gateway are where a terminal lives.
- **Replacing `session exec`.** It answers a different question — "run this in
  the shell context that is live in that session" — and the two coexist. The
  help text of each names the other.
- **Running on a runner without a task.** The target is a task's worktree; a
  runner-wide shell is a different feature with a different blast radius.
- **A timeout.** D7. A caller that wants one wraps the argv in `timeout(1)`.
- **A detached / resumable exec.** D10. `submit` is that feature, and it has the
  output store, the status machinery and the pruning an exec would have to grow
  from scratch.
