# Holding a task across a DELIBERATE server restart — Design

Status: design, not implemented.
Prerequisite: `2026-09-09-runner-identity-decoupling-design.md`, landed
(`0d02d851`…`7d312fd2`). That change is what makes this one possible; §1 of it
states the dependency from the other side.

Scope word used throughout: **hold**. A task is *held* when the server has told
its runner, before going down on purpose, to keep the task's child process alive
with no server to report to, and the runner has agreed. IN scope: the agent
child process, its PTY or pipes, and the task's identity in the store. OUT of
scope, stated here because the word could be read wider: exec runs, port
forwards, file transfers, board tickets held by *clients*, and any client's seat
in a session (§2).

## 1. Problem

A deliberate server restart kills every task in the fleet, and the adaptation
has been to stop keeping sessions up at all.

The mechanism, in the order it fires today:

1. The operator stops the server (`SIGINT` / `SIGTERM` / the `--shutdown-file`
   sentinel — `cmd/harness-server/main.go:46,116-125`). Connections come down.
2. Each runner connection's teardown reaches `registry.OnRemove`, which calls
   `failAndRevokeTasksOf` (`server/server.go:488-491`): every task active on
   that runner is marked Failed and its agentboard registration revoked.
3. On the runner, the per-connection `runCtx` is cancelled. Task contexts are
   its children — `OnConnect` hands `runCtx` to `dispatchRunnerRequest`
   (`runner/connect.go:480-491`), which reaches `handleAssign`, which derives
   `taskCtx` from it (`runner/session.go:489-491`) — so every agent child gets
   SIGTERM, then SIGKILL 5s later (`runner/process.go:150-182`).
4. Independently of (3), an interactive session's child is reaped by
   `exec.ExecuteCommand`'s own SIGHUP→SIGTERM→SIGKILL ladder when its stream
   reaches EOF (`runner/session.go:611-624`). Two kill paths, not one.
5. The runner process itself survives and re-dials (`--persist`,
   `cmd/agent-runner/main.go:147-151`; backoff 500ms→30s, ±25% jitter,
   `cli/persist.go:127-181`), and since the identity change it comes back under
   the *same* `RunnerID`. It re-registers with nothing to do.
6. On the next server start, replay rebuilds the store. A task that was assigned
   and never finished replays as Running (`server/taskstore.go:835-848`), so
   whether the operator sees a phantom Running row depends on (2)'s
   `task_failed` having reached the log before `serve`'s deferred `wal.Close()`
   ran (`server/server.go:681-685`) — an ordering this code does not currently
   pin either way. Interactive survivors that were Detached are explicitly
   Cancelled (`server/server.go:674-679`), because the `SessionMux` was in
   memory.

   That unpinned ordering is inherited, not introduced here, but the hold path
   cannot leave it unpinned: §5 orders the hold writes ahead of any teardown
   precisely because a `task_failed` racing a `task_held` decides the fate of a
   live child.

Everything in that list is correct for a *crash*. None of it distinguishes a
crash from a restart the operator asked for, and the restart is the case worth
money: it is planned, it is short, and the operator knows it is coming.

## 2. Non-goals

`Decided-by` in §3 says who chose; this section says what a later reader must
not read into the change. These are v1 boundaries, not judgements that the ideas
are wrong.

- **Surviving a crash.** A server that dies without asking for a hold leaves
  exactly today's behaviour: children killed, tasks Failed. This is the
  load-bearing simplification (D1) and the reason the change has no fail-open
  path.
- **Restoring scrollback.** The byte ring is server-side and dies with the
  process, so the history a reattaching client would normally replay is gone.
  The SCREEN is a different matter and is not a non-goal — see D14.
- **Restoring client seats.** Cowrite/view seats, `--control` ownership and PTY
  size ownership are rebuilt by the clients reconnecting and attaching again.
  The server does not remember who was watching.
- **Holding exec runs, port forwards or file transfers.** Each exists only for
  the duration of a client's connection; their clients are gone too. They are
  torn down at shutdown as they are today, and a held task comes back with none
  of them.
- **Holding across a runner restart.** Impossible by construction and
  deliberately so: the identity is minted per process and never persisted, so a
  restarted runner presents a different `RunnerID`, which IS the statement "my
  children are gone" (identity spec D3).
- **More than one server.** No handover to a *different* server process on
  another host, no HA. The restarted server must find the same `--data-dir`.
- **Reconnecting a listen-mode runner.** A runner the server reverse-dialed
  cannot re-establish the link itself and the reverse-dial set is not
  persisted, so its held tasks expire unless an operator dials it back inside
  the window (§6a.3).

## 3. Decisions taken

`Decided-by` is provenance, not emphasis. Only rows marked `operator` were
settled by the operator; everything else is the author's call and can be argued
with on its merits.

| # | Decision | Decided-by |
|---|---|---|
| D1 | Only a DELIBERATE restart holds. A crash recovers nothing | operator |
| D2 | The server explicitly asks the runner to hold, before it goes down | operator |
| D3 | The runner hands the held state back on reconnect; the restarted server reconciles it against the WAL | operator |
| D4 | The runner kills its children if it cannot reconnect within a window | operator |
| D5 | The hold is a two-message exchange: the runner ACKs with the exact task set it will keep, and that ack is what the server persists | author |
| D6 | Nothing in the runner's report grants authority. caps, scope, worktree, profile and args are re-derived from the WAL | author |
| D7 | The report rides in `RunnerHello`, not in a message after the handshake | author |
| D8 | One new WAL record, `task_held`, per held task. No runner-scoped record | author |
| D9 | `TaskStatus` gains `held`, appended | author |
| D10 | The window `T` is chosen by the SERVER and carried in the request | author |
| D11 | The runner's report is authoritative for liveness: a held task it does not report is Failed | author |
| D12 | While held, the runner STOPS DRAINING the child's output. The kernel buffer is the gap buffer | author |
| D13 | On re-adoption the server opens a fresh stream and the runner re-binds the live PTY to it — via a runner-owned relay interposed on every interactive session, not a rebind inside `agentexec` | author — forced, see below |
| D14 | The server captures each held session's screen as a repaint program at hold time and persists it; re-adoption replays it. A resize nudge is the fallback when no snapshot exists | author — corrected, see below |
| D15 | A re-adopted interactive task lands in `Detached`; a oneshot lands in `Running` | author |
| D16 | Re-adoption re-binds capacity (`Registry.BindTask`), closing the gap the identity spec's §2 deferred | author |
| D17 | No shim, no compat window: server-first restart with a fleet restart, as always | author — dogfood scope |
| D18 | `RunnerHelloResponse` answers with the ACCEPTED ids, not the refused ones, and the runner kills every held child not named | author |

**D1 is the whole design, not a scope cut.** An automatic hold — the runner
noticing a drop and holding on its own — cannot distinguish a deliberate
shutdown from a network partition to a server that is still running. In the
partition case the live server has already failed those tasks via (2), so the
runner would sit on children nobody will ever re-adopt. Requiring an explicit,
authenticated instruction means the hold state is unreachable by accident, and
the crash path keeps a behaviour that has been exercised for months.

**D5 exists because the two ends know different halves.** The server knows which
tasks it has recorded as assigned; only the runner knows whether each child is
actually alive and whether it can keep it. Persisting the server's *intent*
would record tasks the runner never held. Persisting the runner's *ack* records
exactly the set that can come back. The cost is a round trip inside the
shutdown path, bounded by D10's ack timeout.

**D6 is the security half, and the WAL is what makes it free.** A runner holds
the PSK, so a MAC over the token would prove nothing against the runner itself;
the authority has to come from the server's own disk. It already does: a
`task_assigned` record carries the assignee's identity hex
(`server/taskstore.go:535`), and the WAL also holds `Capabilities`, `ScopeBase`,
`ScopeIDs`, `ScopeOverrides`, `WorktreeDir`, `AgentProfile` and `ExtraArgs`
(`server/wal.go:116-149`). So re-adoption takes no *authority* from the runner:
caps, scope, worktree, profile and args are read from the log, and a runner
claiming a task assigned to another identity is refused by comparing against
`task_held.runner_id`.

**The agentboard ticket is the one exception, and it grants nothing new.** The
board's registry is `map[ticketKey][16]byte` held in memory
(`agentboard/registry.go:23-29`); nothing persists it. So a restarted server
does not know what any task's ticket was, while the surviving agent still
presents the one frozen into its env at spawn — and `Validate` is an exact
compare (`agentboard/registry.go:61-73`), so re-registering a freshly minted
ticket answers `BadTicket` and leaves a live agent unable to use the board or
`harness-cli` at all. The runner therefore reports the ticket back and the
server re-registers *that* value.

Why this is not a hole in D6: the runner received the ticket in
`AssignTaskBody.AuthTicket` and wrote it into the agent's environment itself
(`runner/session.go:552`, `runner/agentenv.go:73-74`), so it can already hand
that task's board identity to any process it starts. Reporting it grants a
capability it has by construction. What it must NOT be allowed to do is name a
ticket for a task the log does not say is its own — which is the same identity
comparison as every other entry, so the check is already there. The alternative
was persisting the ticket in the WAL, which would leave a live credential in
plaintext in `events.log` for the lifetime of the task; the wire carries it
under an authenticated, encrypted connection to a peer that already has it.

**D7**: a report sent *after* the handshake would need the server to hold a
window open before it may fail held tasks — a new timing dependency in exactly
the code path where a missed window means a killed session. In the hello, the
report arrives with the identity, at the identity gate, and registration and
re-adoption are one decision. The cost is that `RunnerHello` changes shape,
which is the wire-skew class that once killed twelve slots (Pitfall 10) — §8
takes the same server-first rule as its prerequisite did.

**D8**: `task_held` carries the task, the runner identity, the hold id and the
deadline, so the server's intent, the runner's agreement and the expiry are one
record. A separate runner-scoped record would have to be correlated back to the
tasks on replay for no gain. Replay is order-sensitive, which resolves the
awkward case by itself: if a shutdown writes `task_held` and the process then
carries on and finishes the task, the later `task_finished` wins.

**D11 answers the disagreement D4 creates.** If the server's re-adoption
deadline outlives the runner's kill deadline, a re-adoption can name a child
that is already dead. Making the report authoritative removes the ordering
question entirely: the server never re-adopts a task the runner did not just
say it was holding. The server's deadline stays as an upper bound for the case
where the runner never comes back at all.

**D12 is chosen against a ring buffer.** Options were: a bounded ring (drops
oldest, needs a "N bytes dropped" marker on every surface, needs a size), a disk
spill (needs a file, a cap and cleanup), or not reading. Not reading is the only
one with nothing to size: the PTY or pipe buffer fills, the child blocks in
`write`, and no byte is lost or invented. It is defensible *because* of D1 — a
deliberate restart is seconds, and D4 kills the child if it turns out not to be.
What it forbids is worth writing down: an agent that treats a stalled write as a
fatal condition would die during the hold rather than block. This is measured
per agent in §10, not assumed.

**D13 is forced, and its cost is not the message.** For an interactive task the
SERVER creates the bidi stream and passes its id to the runner
(`OpenExecRunnerRequest.StreamId`), which looks it up and hands it to
`agentexec.ExecuteCommandWithOption` (`runner/session.go:611-624,892-905`). The
stream is per-connection and cannot survive; the PTY behind it can. So
re-adoption is necessarily "here is a new stream id, attach the thing you are
already holding to it" — a new server→runner request, since the runner cannot
initiate it.

The message is trivial. **What is not trivial is that the stream is a
constructor argument to a call that blocks for the whole session and defers
`stream.CloseBoth()`** (`runner/session.go:803`). There is no rebind point in
that API: `exec.ExecuteCommandWithOption(ctx, stream, …)` takes the stream once
and owns the PTY, both copy loops and the process lifetime until it returns
(searched the package for a stream-swap entry point — `setstream|rebind|
reattach|swapstream|replacestream` over `exec/*.go` returns only two comments
about the harness's own mode tracker).

**So the deliverable is an interposition, not a rebind.** `stream` is typed
`trsf.BidirectionalStream`, an interface (`trsf/api.go:62-66`: SendStream +
ReceiveStream + CloseBoth). The runner passes its OWN implementation from the
start of every interactive session — a relay whose far end it can swap — and
`agentexec` never learns that anything changed. That keeps the work inside this
repo; changing objtrsf instead would mean a publish plus a `go.mod` bump for a
facility only the harness needs.

Everything else the hold needs turns out to live in that one object, which is
why it is the right shape rather than a workaround:

- D12's "stop draining" is the relay declining to copy, with no code in the
  session path.
- "Never close the PTY master" is the relay not forwarding `CloseBoth`.
- **The EOF→SIGHUP ladder becomes unreachable instead of suppressed.** That
  ladder fires when the stream `agentexec` holds reaches EOF; after
  interposition, the stream it holds is the relay, and the relay does not EOF
  because a server went away. §6 no longer needs a hold-aware exception there,
  and §9.1 is about failing to interpose rather than failing to suppress.

**D14 is a correction, and the first draft of this spec had it wrong.** That
draft said a resize nudge was "the only mechanism that reaches a full-screen
application", on the premise that a restart leaves nothing to replay. The
premise was stale. `SessionMux` already holds a live screen model — `screen
*vtgrid.Terminal`, fed from the same byte stream as the ring
(`server/session_mux.go:183-193,347`) — and **every** attach and reattach
already sends `screenRepaint()`, outside any replay cap, so a client gets a
correct screen even when it asked for no history at all
(`server/session_mux.go:439-442`, `:517-536`). `vtgrid.Repaint` synthesises the
program that puts a terminal into that state: screen selection first, then the
modes needed to address cells absolutely, every row, the cursor and the title
(`vtgrid/repaint.go`). Alt-screen content on reattach is therefore *solved*
today, not deferred.

What a restart destroys is that model, because it lives in the mux's memory —
not the ability to reconstruct a screen. And the server is still running at hold
time, which is exactly when it can call `screenRepaint()` for every session it
is about to hold and write those bytes beside the WAL. Re-adoption then replays
the snapshot into the rebuilt mux, the runner resumes draining, and the bytes
the child produced during the gap — which D12 left sitting in the kernel buffer
rather than dropping — land on top. Snapshot-at-hold plus everything-since is
the child's current screen by construction, with no dependency on the
application being willing to redraw.

The resize nudge survives only as the fallback for a session with no snapshot
(the hold was armed but the capture did not land) and for a plain shell, which
repaints nothing on SIGWINCH anyway and has no screen worth restoring. Keeping
it is cheap; leading with it would have been choosing a hack over a facility
this repo already ships.

One consequence for §6b: a repaint program for an 80×24 screen costs a few KB
(the code notes ~380 bytes for a *blank* one), so it can never travel in a
control message. It goes to disk on the server, never over the wire to the
runner.

**D15**: a re-adopted interactive task has a live child and no client, which is
what `Detached` means. It must not be re-adopted into `Running` — that would
claim a client is attached — and the startup sweep that Cancels Detached
survivors (`server/server.go:674-679`) must not see it before re-adoption,
which it will not: replay puts it in `held`, a status that sweep does not match.

## 4. Wire changes — all of them, in one place

```diff
 enum TaskStatus:
     :u8
     Queued
     Running
     Succeeded
     Failed
     Cancelled
     Detached
+    # A deliberate server shutdown asked this task's runner to keep its child
+    # alive with no server to report to, and the runner agreed. Alive, no
+    # server-side session state. Ends as Running/Detached (re-adopted), Failed
+    # (the runner did not come back with it, or the deadline passed) or
+    # Cancelled (the operator said so while it was held).
+    Held

+# HoldID names ONE shutdown's hold. Echoed by the runner on reconnect and
+# recorded in the WAL beside every task it covers, so a report can be matched
+# against the shutdown that authorised it rather than against a task list alone.
+format HoldID:
+    id :[16]u8

+# HeldTask is one task id in a hold exchange. A format rather than a bare
+# TaskID list so a later field (a child pid, a byte count) has somewhere to go.
+format HeldTask:
+    task_id :TaskID
+    # The agentboard ticket this task's agent is HOLDING, in the env it froze at
+    # spawn (HARNESS_AUTH_TICKET). The board's registry is an in-memory map, so a
+    # restart forgets every ticket while the surviving agent keeps presenting
+    # its own; re-registering a freshly minted one would answer BadTicket. The
+    # runner is the right carrier because it already knows this value — it
+    # received it in AssignTaskBody.AuthTicket and wrote the env itself. See D6.

+# --- server → runner, RunnerRequestType.hold_tasks ---
+# Sent to every registered runner as the FIRST step of a deliberate shutdown,
+# before any connection is torn down. Not sent on a crash — there is nothing to
+# send it from, which is the point (D1).
+format HoldTasksRequest:
+    hold_id :HoldID
+    # How long the runner keeps its children alive with no server. The server
+    # owns this value so one operator setting governs the whole fleet; the
+    # runner enforces it on its own monotonic clock, so no clocks are compared.
+    hold_ms :u32

+# --- runner → server, RunnerMessageType.hold_tasks_ack ---
+# The exact set of tasks whose children this runner commits to keeping. The
+# server persists THIS, not what it asked for (D5). An empty list is a valid
+# ack: the runner had nothing to hold.
+format HoldTasksAck:
+    hold_id :HoldID
+    tasks_len :u16
+    tasks :[tasks_len]HeldTask

+# HeldTasksReport rides in RunnerHello: what this runner process is still
+# holding from the hold named by hold_id. tasks_len == 0 with a zero hold_id is
+# the normal case for every reconnect that follows no hold.
+format HeldTasksReport:
+    hold_id :HoldID
+    tasks_len :u16
+    tasks :[tasks_len]HeldTask

 format RunnerHello:
     version :u8
     runner_id :RunnerID
     ...
     agent_profiles_len :u8
     agent_profiles :[agent_profiles_len]AgentProfileName
+    # Appended at the END: what this process still holds (D7). The server reads
+    # it at the identity gate, so registration and re-adoption are one step.
+    held :HeldTasksReport

 enum RunnerRequestType:
     :u8
     ...
     trsf_state
+    hold_tasks        # keep your children alive, I am going down on purpose
+    rebind_session    # attach a held task's live PTY to a new stream (D13)

 enum RunnerMessageType:
     :u8
     ...
     trsf_state_response
+    hold_tasks_ack    # the exact set I will keep

+ format RunnerHelloResponse:
+     your_runner_id :RunnerID
+    # Which reported held tasks the server ACCEPTED. The polarity is deliberate:
+    # a refused-list that the server forgets to fill leaves the runner holding a
+    # child nobody will adopt, while an accepted-list that is forgotten kills
+    # children — loud, and recoverable, instead of silent. So the runner kills
+    # every held child whose id is absent here, and this is the ONLY channel
+    # that reaches a task cancelled while it was held: CancelTask is best-effort
+    # and its send site returns when the runner is not registered, which is
+    # every moment a task spends in Held (server/dispatch.go:230-258).
+    accepted_len :u16
+    accepted :[accepted_len]TaskID   # ids only — the ticket is not echoed back

+# --- server → runner, RunnerRequestType.rebind_session ---
+# Re-adoption of an INTERACTIVE task: the server has created a fresh bidi
+# stream and the runner must splice the PTY it is already holding onto it. The
+# oneshot path needs no rebind — its output goes to the log topic, which is
+# addressed by task id and not by a stream.
+format RebindSessionRequest:
+    task_id :TaskID
+    stream_id :u64
```

WAL — one new record type, written by the shutdown path and read by replay:

```
{"type":"task_held","task_id":<hex>,"runner_id":<identity hex>,
 "hold_id":<hex>,"hold_deadline_ns":<int64>,"ts":<int64>}
```

`WALEvent` gains `HoldID string` and `HoldDeadlineNs int64`; the shadow struct
`walEventJSON` gains both, or `TestWALEventJSONRoundTripCopiesEveryField` fails
(`server/wal.go:129-131` says why that test exists). `RunnerID` on this record
holds the identity hex, the same form `task_assigned` writes since
`server/taskstore.go:535`.

**The disk axis, stated deliberately.** The prerequisite change lost the whole
task history by treating a persisted format as a wire format
(`RunnerSelector` in the WAL), so this spec names what is being written to disk
and what an older binary does with it. `task_held` is a new `type` string;
replay switches on `ev.Type` with no default arm (`server/restore.go:86-101`,
`server/taskstore.go:835-899`), so a rollback to a binary that predates it
ignores the record and replays the task as Running from its `task_assigned` —
a phantom Running row, not a corrupt file, and `prune` clears it. No field on an
existing record changes meaning, and nothing wire-encoded is persisted, so the
class of failure that cost the history here cannot recur through this change.
`TestOnlySelectorEmbedsAWireFormatInTheWAL` keeps that true.

## 5. Server

**Shutdown, in order.** The hold is the first thing a deliberate shutdown does
and it happens inside `serve`, before the deferred `wal.Close()`
(`server/server.go:681-685`) can run:

1. `--hold-window` is 0 → skip everything below and shut down as today. **The
   flag's default is `90s`, i.e. the feature is ON with no argv change**, and
   that default is load-bearing rather than a preference: the deployed restart
   procedure is `scripts/restart.py harness-server`, which reads the running
   process's argv and replays it, so a flag nobody has typed yet can only take
   effect through its default. Enabling it by argv instead would mean every
   restart until someone passed `scripts/restart.py harness-server
   --hold-window 90s` behaved as though the change had not landed.
2. Mint one `HoldID`. Send `HoldTasksRequest{hold_id, hold_ms}` to every
   registered runner.
3. Collect acks, bounded by `--hold-ack-timeout` (default `3s`). A runner that
   does not ack in time holds nothing: its tasks take the normal path.
4. For every task in an ack, write `task_held` and move the store to `Held`.
   For an interactive one, also capture its screen: `SessionMux.screenRepaint()`
   is still callable at this moment, and its bytes go to
   `<data-dir>/held/<task-id>.screen` (D14). A capture that fails is logged and
   the hold continues — a held task with no snapshot falls back to the resize
   nudge, which is worse than a snapshot and much better than not holding.
5. **Suppress `failAndRevokeTasksOf` for held tasks** for the rest of the
   process's life. This is the single most important line in the change: the
   teardown in step 6 fires `registry.OnRemove` for every runner
   (`server/server.go:488-491`), and without the suppression the WAL ends with
   `task_failed` after `task_held` — replay is order-sensitive, so the hold
   would be silently undone and the children left orphaned.
   Note what this does *not* buy. The `Revoke` half of that function is
   irrelevant here: the ticket registry is in memory
   (`agentboard/registry.go:23-29`) and dies with the process whether or not
   the shutdown revokes. Skipping the revoke is worth doing only for tidiness
   in the case where the shutdown is aborted. The agent's credential survives
   because the runner reports the ticket back and the restarted server
   re-registers it (D6), not because this step declined to delete a map entry.
6. Tear down connections and exit as today.

**Startup.** After `ReplayEvents`:

- A task whose last record is `task_held` is `Held`, carrying its `hold_id`,
  `runner_id` and `hold_deadline_ns` in memory.
- If `now > hold_deadline_ns`, it is Failed immediately with
  `reason="hold_expired"` — the restart took longer than the window the runner
  was given, so its child is already dead by D4.
- The Detached→Cancel sweep (`server/server.go:674-679`) is untouched: a held
  task is not Detached.
- A sweeper fails any task still `Held` when its deadline passes.

**Re-adoption**, at the identity gate, in the same step as registration:

- For each `HeldTask` in the hello's report, re-adopt iff a `Held` task exists
  with that id AND `hold_id` matches AND `runner_id` equals the identity in the
  hello. Otherwise refuse that entry.
- Every `Held` task belonging to that identity and NOT in the report is Failed
  with `reason="not_held_by_runner"` (D11).
- Re-adopted tasks: `Registry.BindTask` for capacity (D16), board
  `Register(identity, task id, ticket)` with **the ticket the runner reported**
  — not a fresh one, or the surviving agent gets `BadTicket` (D6) — status →
  `Detached` for interactive / `Running` for oneshot (D15), and a
  `task_readopted` WAL record for the audit trail. The ticket is not written to
  that record.
- An interactive re-adoption rebuilds the `SessionMux` and, before the runner
  resumes draining, feeds it the persisted screen bytes so the model and the
  ring both start from the screen as it was at hold time (D14). The ordering is
  the whole point: bytes buffered during the gap arrive after the snapshot and
  paint on top of it. Feed them in the other order and the snapshot overwrites
  the newer output. Delete the file once it has been fed — a stale snapshot
  replayed into a later session would show an operator a screen from before the
  restart with no sign that it is old.
- The answer is the ACCEPTED id list on `RunnerHelloResponse` (D18); the runner
  kills every held child not named there. Per task, not per hello: a runner
  reporting one stale task still registers and keeps the rest.
- **Cancel needs no special case, but it does depend on that channel.**
  `TaskStore.Cancel` marks the store unconditionally and the wire message is
  best-effort: `Dispatcher.OnCancel` resolves the assignee and simply returns
  when it is not registered (`server/dispatch.go:230-258`), with no retry. A
  held task is never registered — re-adoption happens *at the identity gate*,
  so there is no moment where the runner is registered and the task is still
  `Held` — therefore a `CancelTask` for a held task can never be delivered.
  What kills that child is its absence from the accepted list, which is why
  D18's polarity is not cosmetic: with a refused-list the operator's cancel
  would leave a live agent working in a worktree for a task the store calls
  Cancelled. Ordering in the WAL takes care of itself: `task_cancelled` after
  `task_held` replays as Cancelled, so the restarted server does not offer it.

## 6. Runner

- **Task contexts move above the connection.** `handleAssign` currently derives
  `taskCtx` from the dispatch ctx, which is the per-connection `runCtx`
  (`runner/connect.go:480-491`, `runner/session.go:489-491`). It must derive
  from a process-level ctx instead, and the runner must cancel every task ctx
  explicitly on disconnect **unless a hold is armed**. Today's behaviour becomes
  the else branch of one visible decision rather than a side effect of context
  parentage.
- **Interpose a relay on every interactive session (D13).** The runner passes
  its own `trsf.BidirectionalStream` to `agentexec` instead of the server's
  stream, and copies between the two itself. This is the load-bearing piece:
  the rebind, D12's stop-draining, "do not close the PTY master", and the
  disarming of the second kill path are all properties of that one object.
- **The second kill path, for the record.** An interactive child is also reaped
  by `exec.ExecuteCommand`'s SIGHUP→SIGTERM→SIGKILL ladder when the stream it
  holds reaches EOF (`runner/session.go:611-624`) — a mechanism whose comment
  describes it as detach handling, so fixing only the ctx leaves the child
  dying anyway. With the relay in place the ladder cannot fire on a server
  disconnect, because the stream `agentexec` holds is the relay. Without it,
  this needs a hold-aware exception inside a third-party package's control
  flow, which is the reason the relay is not optional.
- **Stop draining (D12).** For an interactive task, stop copying from the PTY
  master and do not close it. For a oneshot, the sink that receives decoded
  stdout lines blocks instead of dropping them, which pushes back through
  `os/exec`'s copier into the pipe. Neither path may close its fd: closing the
  PTY master delivers SIGHUP and kills exactly the child being preserved.
- **The window is the server's (D10).** Arm a monotonic timer for `hold_ms` on
  receipt. On expiry, kill every held child by the normal ladder and forget the
  hold. Also kill immediately when the server refuses a reported task, and when
  the reconnect ends in a non-retryable PSK rejection (there is no server that
  will ever adopt them).
- **Keep the ticket where the hold path can reach it.** `AuthTicket` arrives in
  `AssignTaskBody` and is currently consumed inline while building the agent's
  env (`runner/session.go:552`, `runner/agentenv.go:73-74`); the per-task
  `taskEntry` holds only `{cancel, repoPath}`. It gains the ticket, because the
  report needs it (D6) and a value that only exists in a goroutine's frame is
  not reachable from the hold handler.
- **Report on every reconnect.** The report is in `RunnerHello`, so it goes out
  with the identity. Nothing held → zero-length list and a zero `hold_id`.
- **Re-bind, then resume draining.** On `RebindSessionRequest`, splice the held
  PTY onto the new stream and resume draining. The screen is restored by the
  server replaying its own snapshot ahead of those bytes (D14, §5), so the
  runner does nothing about it — except in the no-snapshot fallback, where it
  resizes the PTY by one column and back to make a full-screen agent redraw.
- The identity must NOT change across any of this — it is minted once per
  process above `PersistLoop` (`cmd/agent-runner/main.go:438-439`), and a
  held-task report from a new identity is refused by §5's rule, correctly.

## 6a. Transport — a WS shutdown and a UDP shutdown are not the same event

The link is objtrsf over **either** WebSocket **or** UDP
(`transport.UDPWebsocketDualStackEndpoint`, and the live fleet runs both), and
four things in §5/§6 depend on which one it is. Numbered 6a rather than folded
into §6 because three of the four are server-side obligations.

1. **The hold instruction is reliable; the disconnect notice is not.**
   `HoldTasksRequest` and its ack ride trsf streams, so they retransmit. The
   disconnect that follows is a single `trsf.Close` (which is why
   `peer.Conn.Close`'s send drain is load-bearing); over UDP, a lost Close
   leaves the runner unaware until `trsf.AutoPing` misses — bounded by
   `PingInterval`, 15s by default (`peer/conn.go:109-114,197`).
   **Therefore the window starts when the hold is ARMED, not when the
   disconnect is detected** (§6). Starting it at detection would let a runner
   that missed the Close begin its window up to a ping interval late and
   outlive the server's own deadline, which is the one disagreement D11 cannot
   repair — the runner would still be reporting a task the server has already
   failed.
2. **`--hold-ack-timeout` is sized for a retransmit, not a LAN RTT.** 3s is
   chosen for that reason. A runner whose ack does not arrive in time holds
   nothing and its tasks take the normal path, so loss here fails safe.
3. **A listen-mode runner cannot reconnect at all.**
   `runner.ListenAndServe` only listens (`runner/listen.go:46-64`); its link is
   established by the server, or by an operator's `server dial-runner`. Nothing
   on disk records which runners the server had reverse-dialed, so after a
   restart it does not know to dial them back. A held task on such a runner
   therefore expires: the child is killed by D4 and the task Fails, unless an
   operator re-dials inside the window. Persisting the reverse-dial set would
   fix it and is out of scope (§2) — dialing runners are the fleet's common
   case, and a reverse-dialed one is set up by hand today anyway.
4. **An in-task agent's connection dies even though its credential lives.**
   Phase B proxies an agent's link through its runner
   (`HARNESS_PROXY_VIA_RUNNER`), and that link rides the runner's conn; the
   reconnect builds a fresh `Session`, so it does not survive. A `harness-cli`
   one-shot re-dials per invocation and notices nothing beyond a failure during
   the gap; an agent holding a long-lived `*cli.Client` across the gap must
   re-dial. What makes that re-dial succeed is precisely the ticket surviving
   (identity-keyed, §5 step 5), so this is the intended shape rather than a
   shortfall — but "the credential survives" must not be read as "the
   connection survives".

## 6b. Every control message is ONE datagram, and an oversized one vanishes

This is a property of the existing transport that the change has to be sized
against, not something the change introduces.

**The mechanism.** Every control message on both sides goes through
`Connection().SendMessage` — the runner's sender is
`pc.Connection().SendMessage` (`runner/connect.go:695-698`) and the server's
handlers call it directly (`server/agent_wake.go:77`,
`server/board_handler.go:43`, …). That is one objproto *application message*,
which becomes one datagram: objproto does not fragment, it only refuses a packet
whose total exceeds `0xffff` (`objproto/objproto.go:1455-1457`). The PSK
handshake, and therefore `RunnerHello`, takes the same path
(`runner/connect.go:353-356`).

**Why the failure is invisible.** Over UDP a datagram above the path MTU is
dropped and nothing is told: `transport/udp.go` routes EMSGSIZE past
`CannotSend` deliberately, because reporting it would tear the connection down,
and objproto's own interface doc states the consequence — *"Nothing carries
'this one datagram did not fit' up to trsf"* (`objproto/session.go:136-146`).
Over WebSocket the identical message is fine, because there it rides a TCP
stream. So an oversized hello presents as a runner that registers over WS and,
over UDP, retries forever with no error on either end. On Windows this is
additionally the `WSAEMSGSIZE ≠ syscall.EMSGSIZE` case
(`transport/udp_msgsize_windows.go`).

**The budget.** trsf's PLPMTUD floor is `DefaultInitialMTU = 1200` UDP payload
bytes (`trsf/conn.go:979-986`), and that floor is the right number to design
against here for a second reason: the MTU tracker belongs to trsf, and these
messages do not go through trsf, so nothing clamps them to the discovered path
MTU at all. Subtracting objproto's 8-byte header and its AEAD tag
(`connectionSecret.Overhead()`) leaves roughly 1170 bytes for a control
message's encoded body.

**Where this change spends it.**

- `HoldTasksRequest` is 20 bytes, fixed. `RebindSessionRequest` is 24.
- `RunnerHello` is the one to watch. It already carries two `u8`-counted
  variable lists — `allowed_roots` (paths at `u16` each) and `agent_profiles` —
  and `HeldTasksReport` is a third. Measured on the current fleet, the widest
  hello is a runner with three roots totalling ~160 bytes of path, so a real
  hello today is ~300 bytes and the headroom is genuine. It does not stay
  genuine by itself: the standing guidance is to bundle repositories into one
  runner's `--roots` rather than add slots, so the first list grows over time.
- **The report is bounded when the runner ACKs, not truncated when it sends.**
  The runner already builds its hello every connect, so it can compute
  `K = (budget − len(hello without the report)) / 32` and ack at most `K` tasks.
  32, not 16: a `HeldTask` carries the task id and the agentboard ticket (D6).
  Tasks beyond `K` are simply not held and take today's path — killed, Failed.
  A runner with many roots therefore holds *fewer tasks*, which is legible; the
  alternative shapes are a hello that cannot be sent (silent, and it takes the
  whole runner down, not one task) or a report truncated at send time (the
  server would then Fail tasks whose children are alive, per D11).
  At current settings `K` is not binding and should be recognised as a guard
  rather than a live limit: with a ~300-byte hello it is about 27, against a
  fleet running `--max-tasks 8`. It becomes binding for a runner configured
  with a large `--max-tasks` or a long root list, which is exactly the
  configuration that would otherwise fail silently.
- **A guard that goes red.** One test encodes a worst-case hello — roots at
  their real path lengths, profiles, `K` held tasks — and fails above the
  budget. It has to be *demonstrated* red by inflating the input before it is
  believed, because a size guard that cannot fail is worse than none, and this
  one is guarding a limit that the WS transport hides. The same test covers the
  pre-existing exposure, which nothing checks today.

## 7. Surface matrix

Walked against the `surface-parity-checklist` numbering; the numbers are what
the implementation's own walk must return a verdict for.

| # | Surface | Change |
|---|---|---|
| 11 | `ls` text rows | `status=held` renders; no new column |
| 12 | `ls --json` | `status` carries `held`; `held_until` (RFC3339) and `hold_id` added, never elided |
| 16 | TUI task table | `held` in the status cell, with its own colour — not the Failed colour |
| 17 | TUI task detail (`d`) | `held until …` line, plus the runner identity it is held by |
| 19 | TUI picker rows | `held` is a status a picker row can show |
| 20 | WebUI task row meta | `held` in the status chip |
| 21 | WebUI task detail sheet | `held until …` |
| 23 | wasm snapshot | `held` label AND the raw deadline, per item 22's raw-value rule |
| 24 | `cancel` on a held task | Cancels in the store; the child dies when the runner is refused at re-adoption. Written down because "cancel" on a task with no live runner connection is a path with its own meaning |
| 28 | Persistence | `task_held` + `task_readopted`; replay meaning for both, and for their absence. Plus `<data-dir>/held/<task-id>.screen`, which is state on disk that is NOT in the WAL — deleted after it is fed, and orphans swept at startup |
| 35 | `README.md` | the two server flags, and one paragraph stating that a CRASH recovers nothing |
| 37 | This spec | an Amendment section if the shipped behaviour differs |
| — | Server flags | `--hold-window`, `--hold-ack-timeout`. Not verb-table surfaces (item 1 does not reach server flags), so they need the README and the runner-up preset docs instead |

Deliberate omissions, recorded as omissions rather than left silent:

- `ls` text rows get no `held_until` column (item 11) — the row is
  over-subscribed and the status word plus `--json`/detail carry it. Item 31's
  rule is about hiding a value because of what it IS; this is a column that does
  not exist on any row, which is a different axis.
- No new `RunnerInfo` field (items 18/18a): a runner is not registered during
  the interesting interval, so there is nothing for a runner row to show.
- Live screen panes (item 38) are unchanged: a held task has no live screen, and
  after re-adoption it has an ordinary one.

## 8. Rollout

`RunnerHello` gains a field, so this is the same class of change as
`d4f7a5a`/Pitfall 10. Appending at the END of the hello means an old server hits
a decode failure and answers `NoIdentity`, which is retryable — a skew costs
reconnects, not a wipe — but the order still matters.

The deployed procedure, in order:

1. On the server host: `make build`, **then** `scripts/restart.py
   harness-server`. Build before restart, not after — this is the existing
   operation and it is what makes the new binary the one that comes up.
2. Then the runner fleet: `scripts/build_and_restart_all.py`.

Two consequences of `restart.py` replaying the running process's argv:

- Any flag this change adds must work from its default (§5 step 1), because the
  restart inherits an argv typed before the flag existed. A flag that must be
  set explicitly needs `scripts/restart.py harness-server --hold-window …`, and
  that is a manual step the procedure does not currently include.
- The old process performs its own shutdown. So **the restart that lands this
  change holds nothing**: the binary executing the shutdown path predates the
  hold request, and the runners in the fleet at that moment do too. Expect
  exactly today's behaviour on that one restart, and do not read it as the
  feature failing. The first restart that can hold anything is the one after the
  fleet is back on the new binary.

Also:

- `scripts/wire-skew-check.sh` must be run and must show reject-then-heal in
  both directions.
- Rollback: a binary that predates `task_held` ignores the record and shows
  phantom Running rows for whatever was held (§4). `prune` clears them.

## 9. What could go wrong

1. **The relay is skipped and the ctx alone is fixed** (D13, §6).
   `exec.ExecuteCommand`'s EOF ladder then kills the child anyway. Presents as:
   the hold exchange works, the WAL says `task_held`, the runner reports the
   task on reconnect — and the child is gone. This is the single easiest way to
   ship a change that looks correct, and the tell during implementation is
   anyone proposing to make a third-party package's ladder hold-aware instead
   of interposing.
2. **A pump closes the PTY master on write error.** Same symptom as (1) via
   SIGHUP, from the drain side rather than the reap side.
3. **`failAndRevokeTasksOf` not suppressed** (§5 step 5) → `task_failed` after
   `task_held` → replay undoes the hold and the children are orphaned for the
   full window with nobody to adopt them. Worse than a plain failure, because
   the runner still believes it is holding.
4. **The runner process dies during the hold.** Nothing then reaps its children:
   they were setsid'd out of the runner's control group deliberately
   (`3ce441c`), and whether a claude child exits when its PTY master closes is
   agent- and OS-dependent — this project has seen an agent survive a stdin EOF
   on Windows. This hole is NOT closed by this design; the window it opens is
   `hold_ms` wide. Closing it needs the held child's pid recorded where the next
   runner process in the same slot can reap it, which is deliberately left out
   of v1 and named here so it is not discovered as a surprise.
5. **An agent that dies on a stalled write** (D12's forbidden case). Measured
   per agent in §10; if one turns out to behave this way, the answer is a disk
   spill for that agent, not a ring for everyone.
6. **A held task re-adopted onto a dead child.** Prevented by D11, and the
   failure mode if D11 is implemented as "trust the WAL" instead is a phantom
   Running task nobody can attach to.
7. **The shutdown does not actually exit** after writing `task_held`. Replay is
   order-sensitive, so whatever the still-live server writes afterwards wins;
   the hold records are then stale rather than wrong. Accepted.
8. **A fresh ticket minted at re-adoption instead of the reported one** (D6).
   The agent survives and its session looks perfect; every `harness-cli` call
   it makes answers `BadTicket`, and an `agent send` from it silently reaches
   nobody. A session you can watch but that cannot report is the worst of the
   available failures, because nothing about the screen says so. `UnknownTask`
   is the same defect one step earlier — the task never re-registered at all.
9. **The screen snapshot fed in the wrong order, or left on disk.** Fed after
   the gap's buffered bytes it overwrites newer output with older; left
   undeleted it can repaint a later session with a screen from before the
   restart. Both look correct in a test where the child is idle across the
   restart, which is the test anyone writes first (§10.1a exists for this).
10. **A hello that outgrows its datagram** (§6b). Symptom to recognise: a runner
   registers over WebSocket and, over UDP, loops on the handshake with no error
   logged at either end. Nothing in the stack reports it, so it will not be
   found by reading logs — only by noticing that the transport is the variable.
   The `K` bound and its test are what keep this unreachable; a report that is
   truncated instead would trade it for D11 failing tasks whose children live.

## 10. Testing

- Unit: the ack→`task_held` write; replay of `task_held` into `Held`; deadline
  already passed at startup → Failed; re-adoption accept/refuse across the three
  match conditions (id, `hold_id`, identity); a `Held` task absent from the
  report → Failed; capacity re-bound after re-adoption; the suppression in §5
  step 5 (a teardown after a hold must not write `task_failed`); an unknown WAL
  `type` is ignored, pinned so the rollback claim in §4 is measured rather than
  asserted.
- `scripts/wire-skew-check.sh` — reject-then-heal, both directions.
- Live, on `scripts/dummy-harness.sh`, because nothing above crosses a process
  boundary. The parser and key-dispatch layers are also only reachable this way
  (Pitfall 13), so the status must be read through the real command lines:
  1. An interactive session with a live child, running a FULL-SCREEN app (not a
     shell prompt — a shell cannot distinguish a restored screen from an empty
     one). Restart the server deliberately. The child must still be the same
     process (compare pids), the task must report `held` between the two
     servers and `detached` after, and `session snapshot` after the reattach
     must match what it reported before the restart. That comparison is D14's
     proof, and it is available headlessly because snapshot renders through the
     same screen model the repaint comes from.
  1a. The same, with the child made to write during the gap (a clock or a
     progress line). The reattached screen must show the LATER content, not the
     snapshot — this is the ordering in §6 that a wrong implementation gets
     backwards, and it looks correct in test 1 either way.
  1b. The credential, which is the check the prerequisite spec could not run at
     all: from inside the surviving task, `agent send` must return
     `delivered_to=1` and `agent inbox` must read it back with
     `from.runner_id` equal to the runner's identity — AFTER the restart, with
     the agent process never having been respawned. This is the only proof that
     the reported ticket was re-registered rather than replaced.
  2. A oneshot mid-run across the same restart: its exit code must arrive, and
     the log must have no hole where the gap was (D12 claims no loss, so a
     missing chunk falsifies it).
  3. **The negative control: `kill -9` the server.** Children must die and tasks
     must be Failed, exactly as today. A hold that survives a crash means the
     instruction is not what armed it.
  4. `hold_ms` elapsing with no server: the children must be gone, and the next
     server start must Fail those tasks rather than offer them.
  5. A task Cancelled while held: absent from the accepted list, child killed,
     and no CancelTask is ever sent (it cannot be — §5). Check the child's pid
     is gone, not just that the row says Cancelled: the row says that the
     moment the operator types it, which is exactly why this case needs a live
     check.
  6. A runner PROCESS restart during the hold: the new identity must be refused
     and nothing re-adopted (and note (4) above — its children are the orphans
     §9.4 describes).
- **Both transports, not just the one the dummy defaults to.** §6a.1's timing
  differs by transport, so the live list above is run twice: once over
  WebSocket and once with the runner on UDP (`dummy-harness.py up --udp`, whose
  env emits `UDP_CID` beside `CID` — `scripts/dummy-harness.py:358-363,531`).
  The UDP pass is where a lost Close can be simulated by killing the server
  with the datagram dropped, which is the only way to exercise "the runner
  learns via ping, not via Close".
- Per-agent: D12's stall behaviour for claude, codex and agy. An agent that
  cannot block on write is a fact about that agent, and it belongs in the table
  before the design leans on it.
- Windows is a separate pass: the ConPTY path, the resize nudge and the two kill
  paths are all platform-specific there, and the runner's own handshake
  (`sendRunnerMergedHandshake`) is a distinct code path from the client's.
