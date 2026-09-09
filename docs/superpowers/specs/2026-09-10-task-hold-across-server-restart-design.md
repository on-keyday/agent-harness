# Holding a task across a DELIBERATE server restart — Design

Status: design, not implemented.
Prerequisite: `2026-09-09-runner-identity-decoupling-design.md`, landed
(`0d02d851`…`7d312fd2`). That change is what makes this one possible; §1 of it
states the dependency from the other side.

Scope word used throughout: **hold**. A task is *held* when the server has told
its runner, before going down on purpose, to keep the task's child process alive
with no server to report to, and the runner has agreed. IN scope: the agent
child process, its PTY or pipes, the task's identity in the store, and **the
agentboard ticket that task's agent is holding** (D6 — the agent survives, so
its credential has to survive with it). OUT of scope, stated here because the
word could be read wider: exec runs, port forwards, file transfers, and any
client's seat in a session (§2).

There is exactly one kind of ticket, and only an agent has one: it rides
`ClientHello` under `kind == ClientKind.agent` as
`AgentInfo{runner_id, task_id, auth_ticket}`
(`runner/protocol/message.bgn:423-425`), and an operator's CLI, TUI or WebUI
authenticates with the PSK and holds none. An earlier draft of this paragraph
put "board tickets held by clients" out of scope, naming a credential class that
does not exist — a reader would have gone looking for what this design drops.

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
6. On the next server start, replay rebuilds the store — and **`ReplayEvents`
   ends with a sweep that forces every still-`Running` task to
   `Failed("server_restart")`** (the tail of `ReplayEvents`,
   `server/taskstore.go`). So an interrupted task is failed by the replay
   itself, whether or not (2)'s `task_failed` reached the log first; the
   ordering against `serve`'s deferred `wal.Close()` is therefore cosmetic
   rather than load-bearing. Interactive survivors that were Detached are
   separately Cancelled (`server/server.go:674-679`), because the `SessionMux`
   was in memory.

   That sweep is the reason `Held` has to be exempt from it, which is the one
   line of this change that makes the whole thing anything but inert: a status
   that says "the child is alive on a runner that agreed to keep it" is exactly
   the claim the sweep exists to deny for every other status. Pinned by
   `TestReplaySweepDoesNotFailAHeldTask`, with an un-held task beside it as the
   control so the test proves an exemption rather than the absence of a sweep.
   (An earlier draft of this section had replay leaving a phantom `Running` row
   and built an argument about WAL write ordering on top of it. Both were
   wrong; the sweep had been there all along.)

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
- **Restoring an agent's own board subscriptions.** Re-adoption seeds the task's
  self-topic, because `Board.RegisterTask` does (§5). Any pattern the agent
  added at runtime with `agent subscribe` lived in the in-memory `taskState`
  and is gone, and nothing re-issues it: the runner never saw those calls, so
  it cannot report them, and they are not on the log. A held agent therefore
  comes back reachable on its own topic and no longer subscribed to whatever
  else it had asked for. Persisting the pattern list would fix it and is a
  separate change; the failure is silent, so it is also §9.13.

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
| D8 | Two new WAL records — `task_held` per held task and `task_readopted` per re-adoption. No runner-scoped record | author |
| D9 | `TaskStatus` gains `held`, appended | author |
| D10 | The window `T` is chosen by the SERVER and carried in the request; `--hold-window` default `90s` | operator (the value, 2026-09-10) |
| D11 | The runner's report is authoritative for liveness: a held task it does not report is Failed | author |
| D12 | While held, the runner STOPS DRAINING the child's output. The kernel buffer is the gap buffer | author |
| D13 | On re-adoption the server opens a fresh stream and the runner re-binds the live PTY to it — via a runner-owned relay interposed on every interactive session, not a rebind inside `agentexec` | author — forced, see below |
| D14 | The server captures each held session's screen as a repaint program at hold time and persists it; re-adoption replays it. A resize nudge is the fallback when no snapshot exists | author — corrected, see below |
| D15 | A re-adopted interactive task lands in `Detached`; a oneshot lands in `Running` | author |
| D16 | Re-adoption re-binds capacity (`Registry.BindTask`), closing the gap the identity spec's §2 deferred | author |
| D17 | No shim, no compat window: server-first restart with a fleet restart, as always | author — dogfood scope |
| D18 | `RunnerHelloResponse` answers with the ACCEPTED ids, not the refused ones, and the runner kills every held child not named | author |
| D19 | Screen captures happen LAST — after the ack and after the session streams are drained. Capturing early loses everything the child wrote in between | author — corrected |
| D20 | `daemon_down`'s timeout is raised for the server slot, so the hold is not racing a 5 s hard kill | operator |

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

One consumer downstream makes the re-registration matter beyond the agent
itself: `exec` hands a task's board identity to a process it starts in that
task's name, and it must look the EXISTING ticket up rather than issue one —
`registry.Ticket`'s doc says why ("Reuse, not reissue: a second Register for the
same pair OVERWRITES the entry, which would invalidate the credential the
running agent is already holding"). So a re-adoption that minted a fresh ticket
would break `exec` into that task as well, and it would break it silently.

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
tasks on replay for no gain. `task_readopted` is the closing half and is not
optional: without it `task_held` stays the last word about the task, and a
SECOND restart offers it for re-adoption again against the previous hold's id. Replay is order-sensitive, which resolves the
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
`agentexec` never learns that anything changed.

The reason is not that objtrsf is hard to change. It is a repo like this one
with its own landing policy, and a stream-swap entry point would be a small
addition there. The reason is **where the invariant belongs**: "do not close
the PTY when the stream goes away" and "stop draining while a hold is armed"
are statements about a HOLD, and `exec` cannot know when either is right — it
runs a command against a stream and has no notion of a server that will come
back. Teaching it one would put harness policy inside a general-purpose
package; the relay puts it in the process that owns the policy, and every
other piece the hold needs (D12, the non-close, the disarmed ladder) lands in
that same object rather than being spread across a module boundary.

Everything else the hold needs turns out to live in that one object, which is
why it is the right shape rather than a workaround:

- D12's "stop draining" is the relay declining to copy, with no code in the
  session path.
- "Never close the PTY master" is the relay not forwarding `CloseBoth` — and
  the consequence is not speculative: the child is a session leader with that
  PTY as its controlling terminal (go-pty sets `Setsid`+`Setctty`,
  `cmd_unix.go:44-47`), so closing the master hangs up its session. §9.4.
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
is about to hold and write those bytes beside the WAL. Re-adoption replays the
snapshot into the rebuilt mux, the runner resumes draining, and the bytes the
child produced during the gap — which D12 left sitting in the kernel buffer
rather than dropping — land on top.

**"Continuous by construction" holds only if three windows are closed, and
naming them is the whole of D19.** A byte the child writes in this shutdown can
be in one of four places, and only one of them takes care of itself:

1. Written before the capture ⇒ in the mux's `vtgrid` model ⇒ in the snapshot.
2. Written after the capture but before the runner stops draining ⇒ read off
   the PTY, forwarded, applied to a model that is about to be discarded, and in
   NO buffer anywhere. **Lost unless the capture happens after the drain**,
   which is why D19 inverted (§5 step 5).
3. Sent by the runner but not yet read by `runnerPump` when its context is
   cancelled — that loop abandons whatever is unread
   (`server/session_mux.go:328-337`). **Lost unless the shutdown drains the
   stream before capturing**, the other half of §5 step 5.
4. Read off the PTY by the relay but not forwardable, because the write failed.
   **Lost unless the relay keeps it** — one read's worth, held and prepended
   after the rebind (§6).
5. Still in the PTY buffer ⇒ D12 keeps it ⇒ delivered after the rebind. This is
   the only one that needed no work.

With 2, 3 and 4 closed the composition is exact and needs no cooperation from
the application. Without them the screen is stale by however much the child
wrote during the shutdown, and a full-screen app hides that (its next repaint
covers it) while a line-oriented one does not (those lines are simply gone).

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

**But `SetDetached` will refuse it.** That method rejects anything whose status
is not `Running` — `SetDetached: task %q status is %v, want Running`
(`server/taskstore.go:700-709`) — and it is also what stamps `DetachedAt` and
clears `IsAttached`, so bypassing it with a field assignment silently produces a
Detached task with a zero `DetachedAt`. Re-adoption therefore goes `Held →
Running` through the assign path (which already handles a `wasDetached` task
coming back, `server/taskstore.go:505-520`) and then calls `SetDetached`, or
`SetDetached` learns `Held` as a second accepted precondition. The first is
preferable: it reuses a transition that already exists and keeps the "want
Running" invariant true, and D16's `BindTask` belongs on that same assign step.

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
+# HeldTask is one entry of the RECONNECT report — the only message that needs
+# the ticket. The ack at shutdown carries bare TaskIDs instead: the server's
+# registry is still alive at that moment and already holds every ticket, so
+# sending them back would be a credential on the wire for no reason.
+format HeldTask:
+    task_id :TaskID
+    # The agentboard ticket this task's agent is HOLDING, in the env it froze at
+    # spawn (HARNESS_AUTH_TICKET). The board's registry is an in-memory map, so a
+    # restart forgets every ticket while the surviving agent keeps presenting
+    # its own; re-registering a freshly minted one would answer BadTicket. The
+    # runner is the right carrier because it already knows this value — it
+    # received it in AssignTaskBody.AuthTicket (oneshot) or
+    # OpenExecRunnerRequest.AuthTicket (interactive) and wrote the env itself.
+    # See D6.
+    ticket :[16]u8

+# HeldTask is fixed-size; expose its on-the-wire byte length so the runner's
+# budget arithmetic (§6b) divides by a value the SCHEMA owns. Same facility and
+# same reason as FileTransferAckSize (message.bgn:1899), which exists so
+# readers pre-allocate exactly what the format costs; here the consumer is a
+# capacity computation rather than a buffer, and the failure it prevents is
+# worse — a
+# stale divisor makes the hello outgrow its datagram, which over UDP is
+# silently dropped (§6b).
+HeldTaskSize ::= sizeof(HeldTask)

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
+    tasks :[tasks_len]TaskID   # ids only — see HeldTask on why the ticket is not here

+# HeldTasksReport rides in RunnerHello: what this runner process is still
+# holding from the hold named by hold_id, each entry carrying the ticket its
+# agent is still presenting. tasks_len == 0 with a zero hold_id is the normal
+# case for every reconnect that follows no hold. This is the message D6 means
+# by "the runner reports the ticket back" — HeldTaskSize per task, and the only
+# place a ticket travels in this design.
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
+    stream_id :u64   # u64 to match OpenExecRunnerRequest.stream_id (line 199)
+
+# No new response format. A rebind that CANNOT be honoured — the child died
+# between the report and this request — is answered with the existing
+# TaskFinished{exit_code:-1, error_message:"rebind_failed: …"}, which the
+# server already handles as Finish + UnbindTask + Revoke. A rebind with no
+# failure path at all would leave the server holding a task it believes is
+# alive on a stream nothing will ever write to.
```

WAL — two new record types, written by the shutdown path and by re-adoption,
both read by replay:

```
{"type":"task_held","task_id":<hex>,"runner_id":<identity hex>,
 "hold_id":<hex>,"hold_deadline_ns":<int64>,"ts":<int64>}

{"type":"task_readopted","task_id":<hex>,"runner_id":<identity hex>,
 "hold_id":<hex>,"ts":<int64>}
```

`task_readopted` is not decoration: it is what makes a SECOND restart correct.
Without it, replay sees `task_held` as the last word and offers the task for
re-adoption again, against a `hold_id` from the restart before. Its replay
meaning is "Running (or Detached) again on `runner_id`, and the hold named by
`hold_id` is closed". The ticket is NOT written to it (D6). §5 used this record
before this section declared it — the same omission as the ticket field, found
by the audit that produced §6c.

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
ignores the record and replays the task as Running from its `task_assigned`,
which its own sweep then turns into `Failed("server_restart")` — an ordinary
interrupted task, not a corrupt file and not a phantom row. Measured by
`TestUnknownWALRecordTypeIsIgnoredOnReplay`. No field on an
existing record changes meaning, and nothing wire-encoded is persisted, so the
class of failure that cost the history here cannot recur through this change.
`TestOnlySelectorEmbedsAWireFormatInTheWAL` keeps that true.

## 4a. The exchange, in order

Three processes and a disk. Every arrow is a message from §4; every `disk:`
line is a record or file that must exist before the step below it runs.

```
PHASE 1 — SHUTDOWN.  Everything here is inside a 5 s hard-kill window
                     owned by daemon.py, not by this design.

 daemon.py ─── touch <slot>.shutdown ───┐   both land at once on Linux;
 daemon.py ─── SIGTERM ─────────────────┴─▶ the 5 s clock starts HERE

 server: root ctx CANCELLED
   │
   ├─(a) holdCtx = WithTimeout(context.Background(), --hold-ack-timeout)
   │        ▲ NOT the root ctx. Sending on the cancelled one holds nothing,
   │          silently, and passes every test that calls shutdown directly
   │
   │     server ══ HoldTasksRequest{hold_id, hold_ms} ═══▶ runner   (fan-out,
   │                                                                parallel)
   │                                              runner: arm monotonic timer
   │                                                      at ARRIVAL, so its
   │                                                      window ends later
   │                                                      than the server's
   │     server ◀═ HoldTasksAck{hold_id, [task_id …]} ════ runner
   │                        only tasks whose child is ALIVE: OnStdinWriter
   │                        fired / command started, OnProcessExit not fired
   │                        AND: the ack means "I stopped reading their output",
   │                        so no NEW frame can follow it for a held session
   │
   ├─(b) drain each held session's stream until quiet, THEN capture
   │        └─▶ disk: <data-dir>/held/<task-id>.screen
   │            LAST, not first (D19). runnerPump is a synchronous
   │            read→model→ring loop that abandons what is unread when its
   │            ctx dies, so a snapshot taken before the drain misses every
   │            byte written between it and the ack — bytes that sit in no
   │            buffer anywhere. See D14's four windows.
   │
   ├─(c) merge = ack ∩ { t : t.AssignedTo == that identity }   ← intersection
   │        └─▶ disk: task_held{task_id, runner_id, hold_id, deadline_ns}
   │            written BY the store transition, which refuses anything not
   │            Running/Detached — that refusal is what closes the race with
   │            a TaskFinished landing during (b)
   │
   ├─(d) suppress failAndRevokeTasksOf AND afterMuxStopped for held tasks
   │
   ├─(e) cancel the SESSIONS ctx   ← after (c) and (d). Today the muxes hang
   │        off the ROOT ctx, so this step does not exist and their teardown
   │        RACES (c), cancelling held interactive tasks. §5 step 7.
   │
   └─(f) tear down connections, exit

PHASE 2 — THE GAP.  No server exists.

 runner:  keeps the child; STOPS DRAINING (D12) — the PTY/pipe buffer fills
          and the child blocks in write(), so nothing is dropped and there
          is no ring to size
          does NOT close the PTY master (SIGHUP would kill what it is keeping)
          keeps the one chunk it read but could not forward, for the rebind
          the relay makes exec's EOF→SIGHUP ladder unreachable (D13)
          timer fires ⇒ kill every held child, forget the hold (D4)

PHASE 3 — RESTART AND RE-ADOPTION.

 server: replay
   ├─ last record task_held ⇒ status Held, carrying hold_id + deadline
   ├─ now > deadline        ⇒ Failed("hold_expired") immediately
   ├─ arm ONE timer for the earliest outstanding deadline
   └─ delete <data-dir>/held/*.screen whose task is not Held
      ▲ all of this completes before the accept loop starts, so no hello can
        arrive mid-replay and nothing needs a lock

 runner ══ RunnerHello{runner_id, held: HeldTasksReport{hold_id,
           [HeldTask{task_id, ticket} …]}} ══▶ server        (at the PSK gate)
   │
   │  server, per reported task — accept iff ALL THREE:
   │     task exists and is Held  ∧  hold_id matches  ∧  runner_id == identity
   │
   │     accepted:  Registry.BindTask                       (capacity, D16)
   │                boardRegisterTask(Board, identity, task,
   │                                  ticket, task.AgentProfile)
   │                  ▲ the funnel, NOT registry.Register: it also seeds
   │                    SelfTopic, without which the credential validates
   │                    and `agent send` reaches nobody
   │                Held → Running → SetDetached (interactive), else Running
   │                disk: task_readopted{task_id, runner_id, hold_id}
   │                  ▲ without this, a SECOND restart re-offers the task
   │                    against the previous hold's id
   │     otherwise:  Failed("not_held_by_runner")
   │
 runner ◀══ RunnerHelloResponse{your_runner_id, accepted:[task_id …]} ══ server
   │
   └─ runner: kill every held child whose id is ABSENT from `accepted`
              ▲ accepted-list, not refused-list (D18): a forgotten entry
                kills a child (loud) instead of stranding one (silent).
                This is also the ONLY path that reaches a task cancelled
                while it was held — CancelTask can never be delivered to one

 per re-adopted INTERACTIVE task:
   server: rebuild SessionMux, feed it the persisted screen bytes
 server ══ RebindSessionRequest{task_id, stream_id} ═══════▶ runner
   runner: splice the held PTY onto the new stream, resume draining
           ── on failure (child died since the report) ──▶
 server ◀══ TaskFinished{-1, "rebind_failed: …"} ══════════ runner

   ordering that matters: snapshot FIRST, then the gap bytes the kernel
   buffered during PHASE 2. Reversed, the snapshot overwrites newer output —
   and a test with an idle child cannot tell the difference (§10.1a)
```

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
2. Mint one `HoldID`. (The screen capture used to be here; D19 explains why it
   moved to step 5.)
3. Send `HoldTasksRequest{hold_id, hold_ms}` to every registered runner, in
   parallel, **on a context that is not the one that just got cancelled**. Both shutdown triggers converge on `cancel()` of the root
   context — `signal.NotifyContext` (`cmd/harness-server/main.go:122`) and the
   sentinel watcher (`cli/shutdownwatch.go:44-47`) — so a hold that sends on
   the root context sends on a dead one, holds nothing, and says nothing. It
   would also pass any test that calls a shutdown routine directly, because
   only the real path arrives with the context already cancelled. Use
   `context.WithTimeout(context.Background(), ackTimeout)`.
4. Collect acks into a map keyed by (identity, task id), bounded by
   `--hold-ack-timeout` (default `1.5s`, see the ceiling below). A runner that
   does not ack in time holds nothing: its tasks take the normal path.
   **The ack means "I have stopped reading these tasks' output."** The runner
   stops draining when the request arrives, not when the connection dies
   (§6), so after its ack no NEW frame can be sent for a held session. That is
   what makes the next step's boundary a statement instead of a guess.
5. **Drain the session streams, then capture the screens** (D19). Everything
   the runner sent before its ack is still arriving — trsf is reliable, and the
   ack rides a different stream, so it carries no ordering against those
   frames. Read each held session's stream until it goes quiet (a short
   bounded window; there is no EOF to wait for, because nobody is closing
   anything), which lets `runnerPump` apply the last frames to the mux's
   `vtgrid` model. THEN call `SessionMux.screenRepaint()` per held interactive
   task and write the bytes to `<data-dir>/held/<task-id>.screen`.
   Order matters here and the first draft had it backwards: `runnerPump` is a
   synchronous read→model→ring loop that abandons whatever is unread when its
   context is cancelled (`server/session_mux.go:328-360`), so a snapshot taken
   before the drain misses every byte the child wrote between the capture and
   the runner's ack — read off the PTY, applied to a model that is about to be
   discarded, and present in no buffer anywhere. A capture that fails is
   logged and the hold continues: a held task with no snapshot falls back to
   the resize nudge, which is worse than a snapshot and much better than not
   holding.
6. **Merge by intersection, not union.** For each acked (identity, task id),
   accept it only if the store says that task is assigned to that identity
   (`task.AssignedTo`); drop and log anything else. This is the same check the
   re-adoption gate makes, applied at the near end so a bad entry never reaches
   the disk. Tasks the store believes are on that runner but which the ack does
   not name are simply not held — D11's polarity, at shutdown time: the runner
   is the authority on what it will keep.
   Then, per accepted task, one store transition writes `task_held` and moves
   the status, under the store's lock and in the store, the way every other
   transition does it (`server/taskstore.go:535`). That transition **re-checks
   the status and refuses anything not `Running`/`Detached`**, which is what
   closes the race the ack window opens: a `TaskFinished` can land while acks
   are being collected, and because replay is order-sensitive a `task_held`
   written after that task's `task_finished` would win and the restart would
   offer a task whose child has exited. `WAL.Write` flushes per record
   (`server/wal.go:261-277`), so an interrupted pass is a partial hold — some
   tasks held, the rest on the normal path — never a lost tail.
7. **Suppress `failAndRevokeTasksOf` for held tasks** for the rest of the
   process's life. This is the single most important line in the change: the
   teardown in step 8 fires `registry.OnRemove` for every runner
   (`server/server.go:488-491`), and without the suppression the WAL ends with
   `task_failed` after `task_held` — replay is order-sensitive, so the hold
   would be silently undone and the children left orphaned.
   **And it is not the only path that undoes a hold.** When a `SessionMux`
   stops, `afterMuxStopped` cancels any task still `Running`
   (`server/task_handler.go:1459-1461`). Every mux is a child of the server
   ROOT context — `s.taskHandler.Ctx = ctx` (`server/server.go:650-652`),
   consumed as `parentCtx` at `server/task_handler.go:1413-1418` — which is the
   same context whose cancellation triggers this whole sequence. So the mux
   teardown does not happen *after* the hold; it races it, and it races it for
   exactly the interactive tasks the hold exists to preserve. If the mux wins,
   the WAL gets `task_cancelled` after `task_held`, replay says Cancelled, and
   the child is killed at re-adoption: the feature silently does nothing for
   the majority case.
   The fix is to stop hanging the muxes off the root context. The server takes
   a separate sessions context (a child of the root, with its own cancel) and
   the shutdown path cancels it *after* the hold sequence returns, which makes
   the ordering a statement instead of a race. `afterMuxStopped` also gains
   `Held` to its skip condition — one extra comparison, and cheap insurance
   against a future path that stops a mux for its own reasons.
   Note what step 7 does *not* buy. The `Revoke` half of that function is
   irrelevant here: the ticket registry is in memory
   (`agentboard/registry.go:23-29`) and dies with the process whether or not
   the shutdown revokes. Skipping the revoke is worth doing only for tidiness
   in the case where the shutdown is aborted. The agent's credential survives
   because the runner reports the ticket back and the restarted server
   re-registers it (D6), not because this step declined to delete a map entry.
8. Tear down connections and exit as today.

**The whole sequence has a 5-second hard ceiling, and it is not ours.**
`scripts/restart.py harness-server` calls `daemon_down(slot, bin_name)` with no
timeout override (`scripts/restart.py:191`), and that default is `timeout=5.0`
(`scripts/daemon.py:320`): graceful terminate, then `p.kill()` five seconds
later. The sentinel is touched just before the signal (`scripts/daemon.py:356-363`),
so on Linux both triggers land at once and the clock starts immediately. A hold
that overruns is SIGKILLed mid-sequence — and SIGKILL is the crash case, which
by D1 recovers nothing, so whatever had not yet been written silently degrades
to today's behaviour.

That ceiling is what the capture ordering has to live inside, and measuring it
settles the tension: a repaint program is a few KB and the code notes ~380
bytes for a BLANK 80x24 screen, so `--max-tasks` of them is microseconds of
CPU and one small write each. The early-capture optimisation D19 originally
made was buying nothing and costing correctness.
It also sets `--hold-ack-timeout`'s real bound: `3s` would leave under two seconds
for every write plus teardown, which is too close to the edge to choose
casually. Take `1.5s` as the default and treat `restart.py` passing a larger
`timeout` as the operational change that buys more (§8), rather than assuming
the budget is ours to spend.

Nothing needs the server to delete the sentinel: the watcher only stats it and
cancels (`cli/shutdownwatch.go:36-51`), and `daemon.py` clears a stale one
before spawning (`scripts/daemon.py:247-256`). A server that exits with the file
present is the normal case.

**Startup.** After `ReplayEvents`:

- A task whose last record is `task_held` is `Held`, carrying its `hold_id`,
  `runner_id` and `hold_deadline_ns` in memory.
- If `now > hold_deadline_ns`, it is Failed immediately with
  `reason="hold_expired"` — the restart took longer than the window the runner
  was given, so its child is already dead by D4.
- The Detached→Cancel sweep (`server/server.go:674-679`) is untouched: a held
  task is not Detached.
- **Expiry is one timer, not a sweeper.** Every task held by a given shutdown
  shares that shutdown's deadline, so replay knows the whole schedule: arm a
  single timer for the earliest `hold_deadline_ns` still outstanding and fail
  what is still `Held` when it fires. The auto-prune block is the shape to copy
  for *where* this lives — a startup pass followed by a background loop, inside
  the `DataDir` block, cancelled by the server context
  (`server/server.go:725-751`) — but not for its cadence: its interval defaults
  to an hour, which is useless against a 90-second window. One timer needs no
  interval at all.
- **The orphan sweep for `held/`.** A `<data-dir>/held/<task-id>.screen` whose
  task is not `Held` after replay is deleted in the same startup pass. That is
  what makes D19's speculative captures free: a snapshot for a task that was
  never held, or that has since been re-adopted and fed, has no reader.
- **No race with a reconnecting runner, and nothing needs a lock for it.**
  Replay runs inside the `DataDir` block (`server/server.go:654-752`) and the
  accept loop is the `for`/`select` at the end of `serve`
  (`server/server.go:869-876`), so the store is fully rebuilt before any hello
  can arrive. Written down because the opposite assumption invites a lock
  around re-adoption that would serialise every registration.
- **The two clocks are never compared, and they disagree.** The server writes
  an absolute `hold_deadline_ns` from its own clock at shutdown; the runner arms
  a monotonic timer when the request *arrives* (§6). The runner's window
  therefore ends later, by the flight time plus skew, so the server can expire
  a task whose child is still alive. That resolves itself through the channel
  that already resolves cancel: the task is no longer `Held`, so it is absent
  from the accepted list and the runner kills the child (D18). No third rule,
  and no clock arithmetic across hosts.
- **A hold needs a data dir.** The entire WAL block is gated on
  `s.cfg.DataDir != ""` (`server/server.go:654`), so with it empty there is
  nowhere to write `task_held` and `--hold-window` must be treated as 0 —
  holding children whose records cannot be persisted would kill them at
  re-adoption after a pointless window. The flag defaults to `./harness-data`
  (`cmd/harness-server/main.go:31`) and the dummy harness passes one
  (`scripts/dummy-harness.py:436`), so this is a deliberate configuration
  rather than a common one.

**Re-adoption**, at the identity gate, in the same step as registration:

- For each `HeldTask` in the hello's report, re-adopt iff a `Held` task exists
  with that id AND `hold_id` matches AND `runner_id` equals the identity in the
  hello. Otherwise refuse that entry.
- Every `Held` task belonging to that identity and NOT in the report is Failed
  with `reason="not_held_by_runner"` (D11).
- Re-adopted tasks: `Registry.BindTask` for capacity (D16); then
  `boardRegisterTask(Board, identity, taskIDHex, ticket, task.AgentProfile)`
  with **the ticket the runner reported** — not a fresh one, or the surviving
  agent gets `BadTicket` (D6); then status → `Detached` for interactive /
  `Running` for oneshot (D15), and a `task_readopted` WAL record for the audit
  trail. The ticket is not written to that record.
- **Through the funnel, and not through `registry.Register`.** The ticket map is
  only half of what a registration is. `Board.RegisterTask` also creates the
  `taskState` and **seeds the task's inbound topic** —
  `ts.addPattern(SelfTopic(tid))`, which its doc calls the server-side
  equivalent of the old `agent subscribe --self` hook
  (`agentboard/board.go:85-100`). Register the ticket alone and the credential
  validates while `agent send` to that task matches no subscriber, which is a
  worse failure than `BadTicket` because it looks like a working agent that
  nobody can reach. `boardRegisterTask` is the funnel every other call site
  uses (`server/dispatch.go:174`, `server/server.go:1342`,
  `server/task_handler.go:1321`) and it carries the nil-Board guard and the
  zero-identity refusal (`server/boardkey.go:31-49`) that the prerequisite
  spec's D11 put there. `agentProfile` is its fourth argument and comes from
  the store's `task.AgentProfile`, which replay restores from the WAL
  (`server/wal.go:137`) — consistent with D6: everything but the ticket comes
  off the log.
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
- **And so does the task REGISTRY, which is the bigger half.** `Session` is
  created once per connection — its own doc says so (`runner/session.go:120-122`)
  and `connect.go:279` is the single construction site — and `s.tasks` lives in
  it. Re-parenting the context while leaving the map where it is produces held
  children whose `cancel`, whose `wakeWrite`, and whose relay are unreachable
  the moment the connection drops. So the map of live tasks becomes
  process-level state that each `Session` points at, the same move the identity
  change made for `RunnerID` (minted above `PersistLoop`, not per connection).
  `Session` keeps what is genuinely per-connection: its `Sender`, its streams.
- **`taskEntry` gains the fields the hold needs and nothing more.** Today it is
  `{cancel, repoPath, wakeWrite, lastWakeAt}` (`runner/session.go:104-118`). It
  gains the ticket (D6) and the relay handle (D13). It does **not** gain the
  task kind: the server knows whether a task is interactive
  (`TaskInfo.Kind`) and therefore decides on its own who gets a
  `RebindSessionRequest`, so reporting the kind would duplicate authoritative
  state on the wire for nothing.
- **`wakeWrite` is a consumer of that survival, not a bystander.** It is the
  closure `task_wake` writes through (`runner/session.go:108-112`,
  `WakeStdin` at `:971`), so an agent that survives a restart keeps its wake
  path exactly when the entries outlive the connection — and loses it silently
  if they do not. Worth checking by hand after re-adoption: a task whose board
  message arrives but whose agent never notices is this, not the board.
- **The ack names only tasks with a LIVE child.** An entry exists from
  registration, which is before the worktree is created and before anything is
  spawned (`runner/session.go:489-491` precedes the spawn by ~60 lines), so
  acking every entry would promise children that do not exist and the server
  would write `task_held` for them. The condition is per path: interactive —
  `OnStdinWriter` has fired and `OnProcessExit` has not; oneshot — the command
  was started and has not returned. Both hooks already exist
  (`objtrsf/exec.ExecuteOption`), so this needs bookkeeping rather than new
  plumbing.
- **The relay keeps what it could not forward.** When its write to the server
  fails it must not read further and must not discard the chunk in hand: that
  chunk was already taken off the PTY, so it exists nowhere else (D14's window
  4). Hold it and prepend it to the first write after the rebind. One read's
  worth, bounded by the read size, and it is the difference between exact
  continuity and a gap whose size depends on when the connection died.
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
  master and do not close it — closing it SIGHUPs the session (§9.4). For a
  oneshot, the sink that receives decoded
  stdout lines blocks instead of dropping them, which pushes back through
  `os/exec`'s copier into the pipe. Neither path may close its fd: closing the
  PTY master delivers SIGHUP and kills exactly the child being preserved.
- **The window is the server's (D10).** Arm a monotonic timer for `hold_ms` on
  receipt. On expiry, kill every held child by the normal ladder and forget the
  hold. Also kill immediately when the server refuses a reported task, and when
  the reconnect ends in a non-retryable PSK rejection (there is no server that
  will ever adopt them).
- **Keep the ticket where the hold path can reach it, on BOTH arrival paths.**
  `AuthTicket` is consumed inline while building the agent's env
  (`runner/agentenv.go:73-74`) and it arrives twice: `AssignTaskBody.AuthTicket`
  for a oneshot (`runner/session.go:552`) and `OpenExecRunnerRequest.AuthTicket`
  for an interactive session (`runner/session.go:793`). The per-task `taskEntry`
  holds only `{cancel, repoPath}`, so it gains the ticket — from both sites. The
  interactive one is the case the whole design exists for, and it is the second
  of the two, which is exactly how a single-site wiring passes review.
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
2. **`--hold-ack-timeout` is squeezed from both sides, and the two sides
   disagree.** Over UDP the ack may need a retransmit, which wants a longer
   window; §5's 5-second hard-kill ceiling — imposed by `daemon_down`, not by
   this design — wants a shorter one. The ceiling wins, so the default is
   `1.5s`, and the honest consequence is that on a lossy path a runner can miss
   the window and hold nothing. That direction fails safe (its tasks take the
   normal path: children killed, tasks Failed) but it means the feature
   degrades exactly where the link is bad. Raising `daemon_down`'s timeout for
   the server slot is what buys both ends room, which is why §8 names it as an
   operational change rather than leaving the tension inside a default nobody
   revisits.
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
  `K = (budget − len(hello without the report)) / protocol.HeldTaskSize` and ack
  at most `K` tasks. **No literal element size anywhere**: `HeldTaskSize ::=
  sizeof(HeldTask)` is declared in §4 and the divisor is the generated constant,
  because a field added to `HeldTask` must move `K` or the hello outgrows its
  datagram and is dropped in silence — the exact failure this item exists to
  prevent, reintroduced by its own guard. The `budget` half is derived the same
  way as far as it can be: `trsf.DefaultInitialMTU` is exported
  (`trsf/conn.go:980`), and the per-packet overhead objproto adds on top of the
  payload (`pktLen := 8 + len(data) + Overhead()`,
  `objproto/objproto.go:1455`) is a named constant in this repo with that
  citation beside it, since objproto exports neither the 8 nor the tag length.
  Tasks beyond `K` are simply not held and take today's path — killed, Failed.
  A runner with many roots therefore holds *fewer tasks*, which is legible; the
  alternative shapes are a hello that cannot be sent (silent, and it takes the
  whole runner down, not one task) or a report truncated at send time (the
  server would then Fail tasks whose children are alive, per D11).
  At current settings `K` is not binding and should be recognised as a guard
  rather than a live limit. Measured 2026-09-10: the widest hello in the fleet
  is ~300 bytes, which against a ~1170-byte budget leaves room for a couple of
  dozen entries, versus a fleet running `--max-tasks 8`. That figure is an
  illustration of the headroom, not an input to anything — nothing computes from
  it, and it goes stale the day a runner gains a root. `K` becomes binding for a
  runner configured with a large `--max-tasks` or a long root list, which is
  exactly the configuration that would otherwise fail silently.
- **A guard that goes red.** One test encodes a worst-case hello — roots at
  their real path lengths, profiles, `K` held tasks — and fails above the
  budget. It has to be *demonstrated* red by inflating the input before it is
  believed, because a size guard that cannot fail is worse than none, and this
  one is guarding a limit that the WS transport hides. The same test covers the
  pre-existing exposure, which nothing checks today. A second, cheaper test
  pins `protocol.HeldTaskSize` against the value the budget arithmetic was
  written for, the way `FileTransferAckSize` is pinned at
  `runner/file_transfer_test.go:1243`: a field added to `HeldTask` should fail
  a fast unit test with the arithmetic named in it, not a datagram-sized
  integration case.

## 6c. The liveness predicate is written out eight times

Items 11–23 walk the surfaces that must SHOW a status. Nothing in them reaches
the places that *branch* on one, and a new `TaskStatus` value silently falls
outside every such branch. There is no `IsLive(status)` function in this tree:
the notion is spelled `Status == Running || Status == Detached` at each site, so
adding `Held` is a decision at each. Enumerated, with the verdict:

| Site | What it gates | `Held` |
|---|---|---|
| `server/port_forward.go:28`, `:92` | register / close a port forward | **stays out** — no runner connection exists, so the forward has nowhere to go |
| `server/port_forward_list.go:75` | list a task's forwards | **stays out** — §2 does not hold forwards; there are none to list |
| `server/file_transfer.go:32`, `:112` | push / pull into a worktree | **stays out** — the transfer needs the runner leg |
| `cli/list.go:229` | prints the `cowrite=/viewer=` pair | **stays out** — those counts come from a live SessionMux, and a held task has none. "No session to describe" is a different thing from a session with zero watchers, which is the distinction that rule exists to keep |
| `tui/tasks.go:414` (`taskSessionAlive`) | reattach, grid tiling, the file picker | **stays out** — it gates ACTIONS, and its own doc says it mirrors the server's refusals. Held in here would offer a reattach the server then refuses |
| `webui/static/main.js` ×6 | the same predicate in the browser | **stays out**, same reason |
| `tui/taskaction.go:69` | `Running && Kind == Oneshot` — a per-row action gate | **stays out**: the action needs a live runner leg |
| `server/task_handler.go:1460` | `afterMuxStopped` cancels a still-`Running` task | **stays out**, and the ordering that makes it safe is a race today — see §5 step 7 |
| `tui/workspace.go:39` | may this task be resumed? (terminal statuses) | **stays out** — a held task is not terminal, its child is running; offering resume would spawn a second agent for a live task |
| `tui/taskaction.go:56` | what `r` does (terminal → Resume) | **stays out**, same reason |
| `tui/app.go:1780` | clears the activity badge on a terminal status | **stays out** and harmlessly so: a held task has no mux, so `LastOutputAt` is already 0 and the second arm of that condition covers it |
| `server/taskstore.go` `MarkFailed` | the disconnect path | **stays out**, enforced INSIDE the function — the guard is there rather than at the one call site so the next caller inherits it, and `FailHeld` is the only sanctioned exit |
| `server/taskstore.go` `Cancel` | the operator path | **goes in** (stays permissive): cancelling a held task is required, and since `CancelTask` can never be delivered to one, the store transition IS the cancel |

**Corrected while implementing: `Held` stays out of EVERY liveness predicate,
not just the refusals.** The first draft of this table had the two render
predicates taking it in, on the reasoning that a held task's child is alive.
Reading what they actually gate settles it the other way: `taskSessionAlive`
gates reattach, grid tiling and the file picker, and its own doc says it mirrors
the server's refusals; `cli/list.go:229` gates the observer counts, which come
from a mux that no longer exists. So the rule is simpler than the table
suggested — in this codebase "alive" means **a live server-side session**, and a
held task is exactly the case with a live CHILD and no session.

That also makes the refusals correct on purpose rather than by accident, which
is what the first draft could not claim: an implementer who "fixes" the
predicates by adding `Held` everywhere would make `forward`, `file push`,
`file pull`, reattach and the grid all accept a task whose runner is not
connected.

**And two label switches fall through to `"?"`.** `cli/list.go:745-751` and
`tui/tasks.go:425-432` map each status to a fixed-width label and `return "?"`
on anything unlisted, so a `Held` task ships as a literal question mark on the
`ls` rows and in the TUI table until both gain an arm. Not a crash, which is
why it would survive a demo.

## 7. Surface matrix

Walked against the `surface-parity-checklist` numbering; the numbers are what
the implementation's own walk must return a verdict for.

| # | Surface | Change |
|---|---|---|
| 11 | `ls` text rows | `status=held` renders — needs an arm in `cli/list.go:745-751` or it prints `?` (§6c); no new column |
| 12 | `ls --json` | `status` carries `held`; `held_until` (RFC3339) added, never elided. ~~`hold_id`~~ **omitted**: it names a server-internal shutdown generation no consumer can act on, and the operator's question — how long is left — is `held_until`. Adding it would be a field whose only reader is a debugging session that has the WAL anyway |
| 16 | TUI task table | `held` in the status cell (`tui/tasks.go`'s label switch, or it prints `?`). ~~its own colour, not the Failed colour~~ **omitted**: the table does not colour by status at all — no status-keyed style exists in `tui/` — so the row was promising a distinction against something that is not there. Colouring statuses is its own change, for all seven of them |
| 17 | TUI task detail (`d`) | `held until …` line, plus the runner identity it is held by |
| 19 | TUI picker rows | `held` is a status a picker row can show |
| 20 | WebUI task row meta | `held` in the status chip |
| 21 | WebUI task detail sheet | ~~`held until …`~~ **omitted**: the sheet is an ACTION list (`addItem`), not a field display, so a countdown has no place in it. The countdown lives in the row meta instead (item 20), which is where the WebUI shows a task's fields |
| 23 | wasm snapshot | `held` label AND the raw deadline, per item 22's raw-value rule |
| 24 | `cancel` on a held task | Cancels in the store; the child dies when the runner is refused at re-adoption. Written down because "cancel" on a task with no live runner connection is a path with its own meaning |
| — | Liveness branches | §6c: five refusal sites keep `Held` OUT, two render predicates take it IN. Not reachable from items 11-23 |
| 28 | Persistence | `task_held` + `task_readopted`; replay meaning for both, and for their absence. Plus `<data-dir>/held/<task-id>.screen`, which is state on disk that is NOT in the WAL — deleted after it is fed, and orphans swept at startup |
| 35 | `README.md` | the two server flags, and one paragraph stating that a CRASH recovers nothing |
| 37 | This spec | an Amendment section if the shipped behaviour differs |
| — | §4a sequence | The exchange in order, as one picture. Item 39's rule applies to it too: a step drawn there and not built is an `omitted` in the diagram, struck through with its reason |
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

The procedure also sets the hold's time budget, and it is tighter than it
looks: `daemon_down`'s default `timeout=5.0` hard-kills the server five seconds
after the graceful signal (`scripts/daemon.py:320`, called without an override
at `scripts/restart.py:191`). §5 is written to fit inside that. **Raising it is decided** (D20): `restart.py` passes a larger `timeout` for
the server slot — 15 s — in the same commit as the flag defaults. §5 still fits
inside 5 s so a stale `restart.py` degrades rather than breaks, and §6a.2's
ack window stops being squeezed against a ceiling nobody chose for it.

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

- `scripts/wire-skew-check.sh` must be run and must show reject-then-heal —
  **and it gains the direction it is currently missing, as part of this
  change.** Today it asserts NEW runner × OLD server in two phases and its
  header declares OLD runner × NEW server "NOT asserted: pre-fix runners exit
  fatally by construction" (`scripts/wire-skew-check.sh:25-26`). That reason
  expired: it describes runners built before `d4f7a5a` made `NoIdentity`
  retryable, and the prerequisite change already found the direction behaves
  correctly — by hand, which is the part to fix rather than repeat.
  The addition is small and mechanical: build `old-runner` in the detached
  worktree beside the existing `old-server` (`wire-skew-check.sh:108-126`
  builds only the server there), then a third phase mirroring phase 1's two
  assertions — rejected, retrying, still alive — with the old runner against
  the new server. Gate it on `git merge-base --is-ancestor d4f7a5a "$OLD_REF"`
  and, when that fails, SKIP WITH THE REASON PRINTED: a pre-`d4f7a5a` runner
  legitimately exits fatally, and a silent skip is how this direction went
  unchecked in the first place.
  A manual verification step in a spec is a defect in the spec. It survives
  exactly one landing and then nobody runs it.
  **Done**: phase 3 exists, and on this change it reports
  `skew exercised: server rejected: NoIdentity` / `stayed alive, kept
  retrying` — the same outcome the prerequisite change had to establish by
  hand.
- Rollback: a binary that predates `task_held` ignores the record, so whatever
  was held replays as an interrupted task and its own sweep fails it (§4) —
  the same outcome as today's restart. The children are then killed by their
  runner when re-adoption is refused.

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
3. **`failAndRevokeTasksOf` not suppressed** (§5 step 7) → `task_failed` after
   `task_held` → replay undoes the hold and the children are orphaned for the
   full window with nobody to adopt them. Worse than a plain failure, because
   the runner still believes it is holding.
4. **The runner process dies during the hold.** Smaller than the previous two
   drafts of this item claimed, and the reason is worth getting right because
   the spec asserted both P and not-P about it.
   Who can kill an agent child:
   - **the kernel, via the controlling terminal — and this is the case that
     matters.** The interactive child is started through go-pty
     (`p.CommandContext`, `objtrsf/exec/exec.go:350`), whose unix `Start` sets
     `Setsid` AND `Setctty` with the slave on all three fds
     (`aymanbagabas/go-pty@v0.2.2/cmd_unix.go:44-47`). So the child is a
     session leader and the PTY *is* its controlling terminal. When the runner
     dies, the master closes and the kernel hangs up that session: SIGHUP,
     default action, child gone. Nothing in the harness has to do anything.
     (A grep for `Setsid|Setctty` across `objtrsf/exec` and `runner/` returns
     nothing, which is how an earlier draft concluded the opposite — the flags
     are set by the library that starts the process, one call below the range
     that was searched.)
   - the runner itself: `procTree.kill` to the process GROUP
     (`objtrsf/exec/proctree_unix.go:44-56`), which is what reaches descendants
     the SIGHUP does not.
   - systemd, when the slot is a registered unit: children inherit the runner's
     unit cgroup (`setsid` does not re-parent one — `scripts/cgroup_adopt.py:82-110`,
     fix `3ce441c`), so a unit stop/restart takes them under the default
     `KillMode=control-group`.
   What is left is narrower still, and most of it is not a defect.
   **Descendants that left the session are SUPPORTED, not leaked** (operator,
   2026-09-10). The common survivor is a process the agent deliberately
   `nohup`'d or `setsid`'d — a server it started, a build it left running — and
   an agent that CANNOT leave one behind is worse than one that sometimes
   leaves too much. The harness does not reap those and should not learn to:
   they are out of its scope by intent, not by omission.
   **That withdraws the cgroup sweep this item proposed a draft ago.** "A
   starting runner kills whatever is already in its own unit cgroup" cannot
   tell an orphan from a deliberate one — `setsid` does not change cgroup
   membership (the whole point of `3ce441c`), so the process the agent meant to
   keep is sitting in exactly the same cgroup as the one nobody wants. The
   sweep would kill the capability along with the garbage.
   **Forward guard, since it is easy to break by "improving" it:** D4's expiry
   kill uses the process GROUP (`kill(-pgid)`), which a deliberately detached
   grandchild has already left. That is not an accident of the implementation,
   it is what keeps the capability intact — so the expiry kill must never be
   widened into a cgroup-wide or session-wide kill.
   The genuinely accidental residue is two narrow cases:
   - **the oneshot path**, which has no controlling terminal at all: `hostcmd`
     only wraps `exec.CommandContext` (`runner/hostcmd/hostcmd.go:44-51`), so a
     oneshot child gets no SIGHUP. It gets EPIPE on its next write to the dead
     sink and lingers if it never writes again.
   - **Windows**, where ConPTY has its own teardown rules and none of the above
     transfers.
   Both are the same before and after this change; the hold widens a window it
   did not create. No v1 work, and — the operator's read — not much observed
   pain either.
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
9. **The mux teardown wins the race against the hold** (§5 step 7). The WAL
   ends `task_held` then `task_cancelled`, so every held INTERACTIVE task comes
   back Cancelled and its child is killed at re-adoption. Presents as "the hold
   works for oneshots and does nothing for sessions", which reads like a
   feature limitation rather than a race, and would be reported that way.
10. **The screen snapshot fed in the wrong order, or left on disk.** Fed after
   the gap's buffered bytes it overwrites newer output with older; left
   undeleted it can repaint a later session with a screen from before the
   restart. Both look correct in a test where the child is idle across the
   restart, which is the test anyone writes first (§10.1a exists for this).
11. **The board registration goes through `registry.Register` instead of the
   funnel** (§5). The ticket validates, so every credential check passes and
   the agent looks healthy; `agent send` to that task returns `delivered_to=0`
   because no subscriber matches its own topic, and the inbox hook stops waking
   it. Distinguishable from §9.8 only by which number `delivered_to` shows,
   which is why §10.1b asserts the 1 rather than the absence of an error.
   The subscription patterns an agent added at runtime are lost either way
   (§2) — this item is about losing the self-topic too.
12. **A hello that outgrows its datagram** (§6b). Symptom to recognise: a runner
   registers over WebSocket and, over UDP, loops on the handshake with no error
   logged at either end. Nothing in the stack reports it, so it will not be
   found by reading logs — only by noticing that the transport is the variable.
   The `K` bound and its test are what keep this unreachable; a report that is
   truncated instead would trade it for D11 failing tasks whose children live.
13. **A re-adopted agent's runtime subscriptions are gone** (§2). Only the
   self-topic is re-seeded, so a board message addressed to a pattern the
   agent had added with `agent subscribe` matches nothing and the agent is
   never woken for it. Nothing logs a miss, and the agent has no way to notice
   its own subscription is absent — it is listed here because that combination
   is what makes an accepted limitation dangerous rather than merely
   incomplete.

## 10. Testing

**Measured 2026-09-10 on `scripts/dummy-harness.sh`.** Recorded here rather
than left as a plan, because six of the defects this change went through were
reachable only this way and none of them could have failed a unit test.

| Case | Result |
|---|---|
| 1. interactive across a deliberate restart | child alive, **same pid**; task `held` between the servers, `detached` after; `readopt: accepted kind=Interactive` → `rebind: sent` → `session rebound` |
| 1a. continuity | a counter in the session read `tick 6` before and `tick 19` after — the screen shows content produced DURING the gap, not the snapshot |
| 1b. the credential | the agent's pre-restart ticket answered `status:"ok"` against the NEW server, and its own topic came back `msgs=1 subs=1` — the subscriber only `boardRegisterTask`'s funnel creates |
| 2. oneshot mid-run | `succeeded`, log lines **1..25 with no gap** and `DONE` present |
| 3. negative control (`kill -9`) | child dead in 200 ms, nothing held — a crash recovers nothing, as designed |
| 4. deadline with no runner | `Failed err="hold_expired"` at the deadline, from the single timer |
| 5. cancel while held | refused at re-adoption (`status=Cancelled`), child reaped in ~500 ms rather than at the deadline |
| 6. runner PROCESS restart | unit-level (`TestReadoptRefusesAnotherRunnersTask`); the live run was blocked by the test harness's own argv quoting, not by the product |
| 7. the same suite over UDP | passes, at ~40 s instead of ~2 s — see below, and the one defect it alone exposed |

**The UDP pass, run 2026-09-10.** Everything above holds — child alive on the
same pid, capture written, rebind honoured, `session snapshot` showing `tick 70`
after a `tick 6` before — but the TIMING is a different animal, and one defect
showed up only here.

- **Re-adoption took ~40 s of the 90 s window**, against ~2 s over WebSocket.
  The runner did not see a `trsf.Close` at all: its relay logged the far-side
  error 40 s after the shutdown, where the WS run logs it at the instant. So
  §6a.1's fallback is not a rare case on this leg, it is the normal one — and
  the reason is that the shutdown closes the HTTP/WS listener
  (`closeListeners` is `shutdownHTTP`) while nothing tears down UDP
  connections before the process exits, so no Close is ever sent there. That
  asymmetry predates this change; the hold is what turned it into a cost.
- **Therefore the window has a floor, and it is not arbitrary**: ping interval
  (15 s) + max reconnect backoff (30 s) = 45 s before a UDP runner can even
  present its report. The 90 s default clears it twice over. Lowering it to
  "30 s, because restarts are fast" would silently strand every UDP runner
  while WS runners kept their children — the worst shape of partial failure,
  since it looks like a flaky subset of the fleet.
- Sending a Close on the UDP leg during shutdown would collapse that 40 s to
  ~2 s. Worth doing, out of scope here, and named so the 45 s floor is
  understood as a consequence of a missing teardown rather than a property of
  UDP.

**D12's stall behaviour, probed 2026-09-10 — and what the probe does NOT
settle.** Tested outside the harness, on a bare pty whose master is simply not
read, which is exactly what the relay produces and isolates the agent from
every other moving part.

- **The mechanism holds.** A real blocked writer (`yes`) filled the pty buffer
  (19,636 bytes pending — the limit here is ~20 KB, not the 64 KB one might
  assume), stopped, survived the full 20-second block, and resumed when the
  buffer was drained. That is D12's claim, measured: no loss, no death, no
  size to choose.
- **claude, codex and agy all survived**, but each had **0–44 bytes** buffered,
  which means none of them actually blocked: an agent sitting at its prompt
  with no input produces nothing, so the write never stopped. "It did not die"
  is compatible with "it was never asked to wait", and conflating the two is
  how this probe was invalid on its first two attempts — including a `bash`
  control that finished writing during the read window and reported a blocked
  buffer of zero.
- **So the residue is a CHATTY agent, and it is unmeasured.** The risk scales
  with output rate: agy repaints its whole screen at 15–57 fps, so it fills
  ~20 KB in well under a second, where claude and codex emit bursts. Measuring
  it properly needs each agent driven to produce >20 KB while blocked — a real
  model call for two of them. Worth doing before a fleet leans on holds for
  long windows; the idle case, which is the common one for a held session, is
  covered.


### The plan, as written before any of that


- Unit: the ack→`task_held` write; replay of `task_held` into `Held`; deadline
  already passed at startup → Failed; re-adoption accept/refuse across the three
  match conditions (id, `hold_id`, identity); a `Held` task absent from the
  report → Failed; capacity re-bound after re-adoption; both suppressions in §5
  step 7 (a teardown after a hold must not write `task_failed`, and a stopping
  mux must not write `task_cancelled`); an unknown WAL
  `type` is ignored, pinned so the rollback claim in §4 is measured rather than
  asserted.
- `scripts/wire-skew-check.sh` — reject-then-heal in BOTH directions, which
  means the script's new third phase (§8) has to exist before this line can be
  ticked.
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
  1a. The same, with the child writing CONTINUOUSLY across the restart (a clock,
     or `agy` at its own framerate). Two separate things must hold, and an idle
     child hides both: the reattached screen must show content produced AFTER
     the snapshot rather than the snapshot itself (the §6 replay ordering), and
     it must show content produced DURING the shutdown itself — D14's windows
     2, 3 and 4, which is what a capture taken before the drain silently drops.
     The falsifier is a monotonic counter in the child's output: no value may
     be missing between the last one seen before the restart and the first one
     seen after it.
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
     instruction is not what armed it. Runnable as written: the dummy's env
     exports `SERVER_PID` (`scripts/dummy-harness.py:363-365`), so this is
     `kill -9 $SERVER_PID` and not a `pgrep` that could match the real fleet —
     which the dummy deliberately protects against by killing only pids it
     recorded (`scripts/dummy-harness.py:320-332`).
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
