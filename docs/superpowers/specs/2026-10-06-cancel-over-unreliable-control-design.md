# A cancel survives a lost CancelTask

`CancelTask` is a control message, and every control message is one objproto
datagram sent with `SendMessage`: no acknowledgement, no retransmission
(`peer/conn.go`, the `DatagramHandler` doc). Over ws the TCP stream underneath
delivers it while the connection lives. Over UDP it can be lost, and nothing on
either end is told.

Before this change the server marked the row `Cancelled` first
(`TaskStore.Cancel`, WAL `task_cancelled`, `task_ended` event) and then sent
one `CancelTask` (`Dispatcher.OnCancel`), discarding the outcome. Capacity
release and ticket revocation both waited for the runner's `TaskFinished`.

## What a lost CancelTask did (measured 2026-10-06)

Dummy instance (`scripts/dummy-harness.sh up --agent fake --udp`), runner on
UDP, one interactive `bash` session per run. The loss was simulated with a
temporary, uncommitted hook in `Dispatcher.OnCancel` that returned before
`SendMessage` while a marker file existed — the same silence a dropped
datagram produces:

```go
if _, err := os.Stat("<scratch>/drop-cancel"); err == nil {
	slog.Warn("EXPERIMENT: dropping CancelTask", "task", taskID)
	return
}
```

| | CancelTask delivered (control) | CancelTask dropped |
|---|---|---|
| `harness-cli ls` row | Succeeded | **Cancelled** |
| child `bash` process | gone | **alive** until the dummy was torn down |
| runner `tasks=` | 0/4 | **1/4** — slot never released |
| task's ticket after cancel (`agent send` with the child's env) | `BadTicket` | **`status ok`** |
| `harness-cli cancel` again | — | no second send: `TaskStore.Cancel` was a no-op on `Cancelled` |

So the operator saw a cancelled task while its agent kept running with full
board access and a held slot, and had no retry: only a runner restart ended it.

The control column's `Succeeded` is a separate, pre-existing behaviour, not
changed here: an interactive task killed by our own cancel reports exit 0
(`runner/session.go`, the `taskCtx.Err() != nil` case of handleOpenExec's
TaskFinished), and `TaskStore.Finish` overwrites `Cancelled` with it.

## Decisions taken

| Decision | Decided by |
|---|---|
| Fix with (1) revoke the ticket at cancel and (2) resend CancelTask until a TaskFinished; the general periodic task-set reconciliation is NOT part of this change | operator, 2026-10-06 |
| Write down how it was done in a spec | operator, 2026-10-06 |
| Resend rather than move CancelTask onto a trsf stream: a stream would not cover a lost TaskFinished, and would change the wire | Claude, 2026-10-06 (proposed to the operator before implementation) |
| A runner answers a CancelTask for a task it does not have with a TaskFinished, and refuses an AssignTask/OpenExec whose cancel overtook it while it was pending (Amendment A narrowed this from "for that id, 10 min") | Claude, 2026-10-06 |
| Resend schedule 3, 6, 12, 24, 30, 30, 30, 30 s (9 sends, ~2.75 min), then give up with a Warn | Claude, 2026-10-06 |
| The server sends CancelTask only to a task that was dispatched (`AssignedTo` set), never to the submit-time `BoundRunnerID` (Amendment A) | Claude, 2026-10-06 |

## Server

**Only a dispatched task is sent a CancelTask.** `OnCancel` does nothing for a
task with no `AssignedTo` — a Queued one has no runner-side state (Amendment A).

**The ticket is revoked at the cancel** (`Dispatcher.OnCancel`), under
`AssignedTo`. This takes the
agent's board and harness access whether or not the runner ever hears of the
cancel. `Board.Revoke` also drops the task's subscriptions, so from the cancel
on, a publish to its `chat.<short-id>` reports `delivered_to: 0`; the topic
itself is kept while it holds anything readable (`agentboard/board.go`).
`TaskFinished` still revokes too; revoking an absent ticket is two map deletes
and a nil check.

**The CancelTask is resent while the row reads `Cancelled`**
(`Dispatcher.resendCancelUntilFinished`). `Cancelled` is exactly "decided, no
TaskFinished yet": `TaskStore.Finish` replaces it with the runner's outcome. A
goroutine per task sleeps through `CancelResendDelays` (nil = the default
above) and before each resend re-reads the row and the registry. It stops when:

- the row is no longer `Cancelled` (a TaskFinished arrived), or the task is gone;
- the runner is no longer registered — its disconnect path ends the task;
- the schedule runs out (logged at Warn).

One loop per task id (`Dispatcher.resending`); a second `OnCancel` sends once
and joins the running loop.

The first delay (3 s) outlasts the runner's kill ladder (SIGHUP → SIGTERM →
SIGKILL, ~2 s), so an ordinary cancel is answered before anything is resent.

**A repeated cancel resends.** `TaskStore.Cancel` on a `Cancelled` row now
fires `OnCancelRepeated` instead of returning silently; `server.New` wires it
to `Dispatcher.OnCancel`. `OnCancel` (the hook) still fires only on the first
transition, so the WAL record and the `task_ended` event are not repeated.

## Runner

**An unknown-task CancelTask is answered** (`Session.handleCancelTask`). Before,
it was logged and dropped. Now it sends
`TaskFinished{exit -1, "cancel: task not running on this runner"}`, the same
shape `reportRebindFailed` uses for a request it cannot honour. There are two
ways to get here, and both leave the server with a row only a TaskFinished can
close:

- the task ran and its own TaskFinished was lost;
- its AssignTask (or OpenExec) never arrived.

The server records this as `Failed` with that reason: it cannot know the real
outcome in the first case.

**A cancel that overtakes its own AssignTask/OpenExec refuses it.** The
AssignTask body and the OpenExec stream are fetched on a goroutine of their own
(`dispatchRunnerRequest`), so a cancel can find the id unknown while the task
is still on its way in. The answer above would then be false: the task would
start with its slot released and its ticket revoked. So:

1. The receive loop marks the id pending (`Session.beginPendingTask` →
   `TaskRegistry.pending`) synchronously, before handing the request to its
   goroutine. Control messages are delivered one at a time, so a CancelTask that
   arrives after the AssignTask/OpenExec always finds the mark.
2. `TaskRegistry.cancelTask` cancels a registered task; for a pending one it
   sets the mark to "cancelled", under the same lock.
3. Both spawn paths register through `putUnlessCancelled`, which clears the
   mark and refuses a cancelled one. A refused task sends
   `TaskFinished{exit -1, "cancel: cancelled before it started"}` and returns
   before anything is spawned.
4. The goroutine defers `endPending`, so a spawn path that gives up before
   registering leaves no mark.

A cancel for an id that is neither registered nor pending records nothing, so
it cannot affect the next run of that id (a resume reuses it).

A CancelTask that arrives BEFORE its AssignTask — two datagrams reordered on
the path — is not covered: the task would start after the server has closed
its row. The server sends the cancel only after the AssignTask, separated by an
operator action, so this needs a reordering longer than that.

## Not covered

These are found, not fixed:

- **A lost AssignTask on a task nobody cancels.** `TryDispatch` marks the row
  Running as soon as `SendMessage` returns (`server/dispatch.go`). If the
  envelope is lost, the row stays Running until the runner disconnects. The
  answer here covers it only once someone cancels.
- **A lost TaskFinished on a task nobody cancels.** The row stays Running, the
  slot stays held and the ticket stays valid until the runner disconnects. The
  disconnect then marks it Failed (`failAndRevokeTasksOf`), overwriting a run
  that may have succeeded.
- **A lost wake** (`emitTaskWake`): the message stays on the board, but an idle
  agent is not woken until the next wake.

The general answer to all three is periodic reconciliation: generalise
`HeldTasksReport` (today sent only on the reconnect after a hold) to every
reconnect and a timer, and kill what the server did not accept. The runner
already does that for held tasks (`killHeldExcept`). The operator deferred it,
2026-10-06.

**Spec/code discrepancy, not resolved here.**
`2026-09-10-task-hold-across-server-restart-design.md` §6a.1 says
`HoldTasksRequest` and its ack "ride trsf streams, so they retransmit". In the
code both are `SendMessage`: `server/hold.go` (the request) and
`runner/hold.go` `handleHoldTasks` (the ack). Which one is wrong is the
operator's call.

## Testing

Unit tests, each checked against its negative control (the fix's piece removed
→ the test fails):

| Test | Negative control |
|---|---|
| `server/cancel_resend_test.go` `TestOnCancelRevokesTicketBeforeAnyTaskFinished` | revoke removed from `OnCancel` |
| `TestCancelResentUntilTaskFinished`, `TestCancelResendGivesUp` | `resendCancelUntilFinished` call removed |
| `TestRepeatedCancelResends` | `OnCancelRepeated` not called |
| `runner/connect_test.go` `TestRunnerAnswersCancelForUnknownTaskWithTaskFinished` (replaces `TestRunnerHandlesCancelTaskUnknownIsNoOp`) | no TaskFinished on an unknown id |
| `TestCancelBeforeAssignRefusesTheLateTask` | `cancelTask` does not mark a pending task |
| `TestCancelOfUnknownTaskDoesNotRefuseALaterRun` | the 10-minute id record of the first version, imitated |
| `server/cancel_resend_test.go` `TestCancelOfQueuedTaskSendsNothing` | the first version (failed before the Amendment A fix) |

`make vet`, `make test` and `make test-integration` pass.

End to end, on the same dummy setup as the measurement above, with the hook
changed to drop exactly ONE CancelTask (`os.Remove` of the marker instead of
`os.Stat`, placed in `Dispatcher.sendCancel`):

```
02:07:56 server WARN EXPERIMENT: dropping CancelTask
         t+0.5s: row Cancelled, tasks=1/4, child alive, ticket → BadTicket
02:07:59 server INFO dispatcher: resending CancelTask; no TaskFinished yet
02:07:59 runner INFO agent child exited ... state="signal: killed" task_ctx_err="context canceled"
         row Succeeded (the exit-0 behaviour noted above), tasks=0/4
```

One resend, then none: the TaskFinished took the row out of `Cancelled`.

## Amendment A (2026-10-06) — two regressions in the first version

The first version (commit `7be1f325`) had two defects, both found by reading
after it landed, not by a symptom:

1. **Cancelling a Queued task turned it Failed.** `OnCancel` routed by
   `AssignedTo`, falling back to `BoundRunnerID`. `handleSubmit` sets
   `BoundRunnerID` on every submitted task (`server/task_handler.go`, the
   `Tasks.Create` call), so a Queued task was sent a CancelTask too. That was
   harmless while the runner ignored an unknown id. With the runner now
   answering `TaskFinished(-1)`, `Finish` replaced the operator's `Cancelled`
   with `Failed`. Fixed by sending only when `AssignedTo` is set;
   `TestCancelOfQueuedTaskSendsNothing` failed on the first version.
2. **The overtaken-assignment record was keyed by task id for 10 minutes.** Any
   unknown-id cancel left one, and a resume reuses the id, so a resume within
   10 minutes of such a cancel would have been refused with "cancelled before
   it started". Fixed by replacing the record with the pending mark above,
   which exists only between the AssignTask/OpenExec and its registration.

For (2), the trigger I first named — an interactive session ending, the server
cancelling it from `afterMuxStopped` while the runner has already forgotten it,
then `session new --resume` — did NOT occur on a dummy instance with either
build: after `exit` the TaskFinished arrived first and the row went straight to
Succeeded. So the defect is shown at unit level (the imitation in the Testing
table), not live. The resume flow itself was run end to end on both builds
(`session new` → `session send 'exit\r'` → `session new --resume`) and resumed
on both.
