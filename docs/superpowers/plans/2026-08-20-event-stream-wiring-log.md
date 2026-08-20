# Wiring the event-stream adapter into the runner — working log

Date: 2026-08-20. Companion to
[`../specs/2026-08-20-event-stream-agent-design.md`](../specs/2026-08-20-event-stream-agent-design.md).

This is a **log of an attempt**, not a plan. It records what the codebase
turned out to look like, what resisted, and which of my assumptions were wrong
— including the ones corrected mid-attempt. Entries are appended in the order
things were found, so a later entry may overturn an earlier one; nothing is
edited away.

---

## 1. First guess: `handleAssign`. Wrong.

I started reading toward `runner/session.go:handleAssign` — the oneshot path:

```
AssignTask → worktree → settings → env → Process.Run(prompt, logSink) → TaskFinished
```

It is the shortest place to add a branch, and that is the whole of its appeal.
The operator asked whether it was right before I wrote anything, which is what
sent me to check.

**It cannot work.** The assign path has no per-task inbound channel. Its only
data plane is `s.Sender.Publish(topic, data)` onto the task log topic, which is
publish-only. An approval needs a route from a client back to the blocked
agent, and there is none here. The event-stream kind would run, block on its
first tool, and have no way to be answered.

Wiring approvals onto the assign path would mean inventing a second inbound
channel keyed by task id — next to one that already exists.

## 2. `handleOpenExec` is the right analogue, and the reason is structural

`handleOpenExec` (`runner/session.go:588`) receives a **server-allocated bidi
stream** (`oer.StreamId`), waits for it with
`peer.WaitForBidirectionalStream`, and hands it to
`agentexec.ExecuteCommandWithOption`, which splices it to the PTY.

That is the shape the event-stream kind wants: one stream per task, both
directions, allocated by the server. Events go out on it, responses and user
turns come back on it.

## 3. The detach machinery is on the SERVER, and it already fits

The thing that made this obvious: `handleOpenExec` looks like it would end the
task when the client goes away — it returns when the stream ends and then sends
`TaskFinished`. That would contradict the recorded behaviour that a Detached
task stays alive.

It does not, because **the runner never sees a detach**. `server/session_mux.go`
owns the runner-side stream:

> SessionMux owns the runner-side bidi stream for a detachable interactive
> session. It pumps runner output into a RingBuffer, forwards to whatever
> tuiStream is currently attached, and accepts new client tuiStreams that take
> over from any existing attach.

So the server holds the runner stream open for the task's whole life and
detach is purely a client-side event. The runner's job is "speak on this
stream until the process exits".

**This is the finding that changes the size of the work.** The mux already
provides, for the PTY kind, everything §3 asks for:

| §3 wants | SessionMux already has |
|---|---|
| `exec_view` = read the stream | `viewers map[*viewerConn]struct{}` |
| `exec_control` = the seat | `tui` + `tuiCancel`, the take-over path |
| `exec_cowrite` = write without the seat | multi-writer forwarding under `runnerWriteMu` |
| a task that outlives its client | the mux owning the runner stream |
| replay on attach | `RingBuffer` |
| observer counts | `OnObservers` |
| busy/idle | `lastOutput` + `OnActivity` |

The event-stream kind is therefore **not a new subsystem**. It is the same
session machinery carrying a different frame type. That is a much smaller and
much better-tested starting point than a parallel path, and it means the
per-kind verdicts in §3 mostly fall out of what the mux already enforces rather
than being re-implemented.

Two things do NOT fall out and have to be built:

- **The pending table.** The mux buffers bytes for replay; a pending approval
  is not a byte to replay, it is state with an identity that must survive a
  detach and be answerable by a client that was not attached when it was
  raised. `pending=N` on the task comes from this, not from the ring.
- **Frame typing.** The PTY stream carries `exec/frame`, which is pre-trsf
  legacy generated from `.bgn` and kept as-is; the established rule on this
  project is that sideband goes in a separate trsf stream rather than being
  added to that frame. So the neutral NDJSON needs its own stream or its own
  frame kind, decided before code.

## 4. Open, before writing anything

- Does the event-stream task need `agentexec` at all, or does the runner
  frame NDJSON onto the stream directly? `agentexec` exists to splice a PTY;
  there is no PTY here.
- `RingBuffer` replay for a byte stream is "the last N bytes". For an event
  stream the useful replay is "the last N EVENTS", which is a different
  container. Reusing the ring verbatim would truncate mid-JSON-line.

---

*(appended as the attempt continues)*

## 5. First run: deadlock. An event-stream task has no natural end.

Wrote `runner/streamtask.go` (adapter spawn + NDJSON both ways + pending
table), wired a test over an `io.Pipe` pair, ran it. It hung until the 300s
timeout.

The cycle:

1. the runner calls `cmd.Wait()` and only closes the adapter's stdin afterwards
2. the adapter blocks on its own stdin until EOF
3. the agent blocks waiting for another user turn until ITS stdin closes, which
   the adapter does only when its own stdin closes

Nobody can start the teardown. The immediate cause is my ordering — close
before wait, not after — but fixing the ordering only moves the question, and
the question is the finding:

**What ends an event-stream task?** The PTY kind has an answer: claude exits
when the user leaves the TUI, and `ExecuteCommandWithOption` returns. This kind
has none. The agent is a multi-turn process that waits for input forever, by
design. And the obvious candidate is wrong: **a client detaching must NOT end
the task** (§3 keeps a Detached task alive with `pending=N`), so "the stream
closed" cannot mean "we are done".

So an event-stream task, once started, runs until something explicitly ends it:
a cancel, or a `finish` the design does not currently have. That is a real
semantic difference from both existing kinds and it was not in the spec.

Consequences the plan has to carry:

- The runner closes the adapter's stdin on ctx cancellation (the task being
  killed) or on the stream ending, and waits for the exit after that, never
  before.
- `session kill` becomes the ordinary way an event-stream task ends, not an
  exceptional one. A user who "finished" with an agent must cancel it, or the
  worktree and the process stay live indefinitely.
- Worth deciding in the plan, not here: whether the kind gets an explicit
  `finish` verb that closes stdin and lets the agent exit cleanly, which reads
  very differently from `kill` in `ls` and in the status a task ends in.

## 6. Second hang, different cause: the inbound pump cannot be cancelled

Fixing the ordering did not fix the hang. The remaining wait was on the
stream→adapter pump: a plain `io.Reader` has no cancellable read, so that
goroutine ends only when the STREAM ends, and the task's own teardown cannot
make that happen.

Resolved by not waiting on it. In the real system the server closes the runner
stream as the task finishes, so the goroutine does end — just never on the
runner's timeline. Written down at the call site because "why is this the one
pump we do not wait for" is otherwise a question a reader has to re-derive.

The real trsf streams DO have `ReadContext`, so the wired version can do
better than this; the spike takes an `io.ReadWriter` deliberately, so it can be
tested over a pipe.

## 7. There is no acknowledgement that an answer was delivered

Found by a test race, and it survives the test being fixed.

The runner drops a request from `pending` when it **forwards** the answer, not
when the agent acts on it. Nothing in the protocol says "delivered". So:

- `pending=N` going back to zero means "the runner sent it downstream", which
  is not the same as "the agent is unblocked".
- If the adapter or agent dies between the forward and the vendor write, the
  request is gone from the table and the operator sees a task that is no longer
  pending and also not progressing. Nothing reports that.

The obvious repair is an adapter→runner ack carrying the request id, which
costs one message kind. Not added in the spike; recorded so the plan decides it
rather than inheriting it.

It also made the first version of the test wrong in an instructive way: the
fake agent used `cat > file`, which holds the file open until its stdin closes,
so "the answer arrived" was only observable during teardown — the moment the
test wanted to stop. `head -n 1` gives the agent a way to acknowledge by
finishing a turn, which is what a real agent does anyway.

## 8. Where the spike got to

`runner/streamtask.go` + `runner/streamtask_test.go`. Over a pipe pair standing
in for the server-allocated stream, with the REAL adapter binary and a fake
agent:

- the adapter's `hello`, its events and an approval request all reach the
  stream
- an answer written on the stream reaches the agent as a vendor
  `control_response` with the vendor's own request id
- `pending` goes 1 → 0 and `Pending()` exposes the blocked request
- events render into the task log through the SAME `agentlog.Render` the
  oneshot path uses, and neutral JSON never leaks into the log
- a missing adapter fails the task at start rather than warning
- `agentlog.Event → neutral → agentlog.Event` round-trips without changing what
  `Render` prints, which is what keeps this kind's logs from quietly saying
  less than the oneshot kind's

**Not done, and each is a decision rather than typing:** the `.bgn` additions
(a `RunnerRequestType` for opening a stream-agent task, the `TaskKind` value),
the server-side owner (a `SessionMux` sibling that buffers EVENTS rather than
bytes — the ring truncates mid-line), `pending=N` reaching `TaskInfo`, and the
§3 verbs.
