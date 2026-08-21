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

## 9. Correction: `agentexec` is not PTY-only, and it already does the framing

Entry 5 and the comment at the top of `runner/streamtask.go` both say the
adapter cannot use `agentexec` because "that package exists to splice a PTY and
there is no PTY here". **I had not read it.** The operator asked whether I had.

`ExecuteCommandWithOption(ctx, stream, logger, command, args, cwd, ptyEnabled
bool, extraEnv, opt)` takes `ptyEnabled` as a parameter, and the `false` branch
is:

```go
cmd := exec.CommandContext(grCtx, command, args...)
cmd.Stdin  = pipeOut   // fed by handleInput from FrameType_Stdin frames
cmd.Stdout = stdout    // outStreamWrapper → FrameType_Stdout
cmd.Stderr = stderr    // outStreamWrapper → FrameType_Stderr
```

That is what `runner/streamtask.go` does by hand. The spike reimplemented
framing that already exists, and — worse — invented a framing **the server and
the client do not speak**. `SessionMux` pumps `frame` frames; so does the
client's `CommandExecutionStream`. Neutral NDJSON riding inside
`FrameType_Stdout` needs no new frame type at all, which also satisfies the
standing rule that `exec/frame` is legacy and not to be extended.

So the wiring is smaller than the spike suggests:

```
runner: ExecuteCommandWithOption(ctx, stream, log, adapterPath, adapterArgv, dir,
                                 /*ptyEnabled=*/false, env, opt)
```

with the adapter as the command. Its stdout NDJSON becomes Stdout frames, a
client's Stdin frames become its stdin. What the spike genuinely adds and
`agentexec` cannot is the **pending table and the log rendering**, which need a
tap on the frames going past — a wrapper around the stream, not a parallel
implementation of it.

Things I got for free by reading and would otherwise have rebuilt: separate
stderr framing, Signal control frames, the audit hook, `OnStdinWriter` (the
agentboard wake path), and a teardown that calls `stream.Cancel()` to terminate
the input handler — which is the cancellable read entry 6 said did not exist.

### And it answers entry 5's open question

`handleInput` has this:

```go
if hdr.Header.Type == frame.FrameType_Stdin {
    if hdr.Header.Len == 0 { // close stdin
        pipeIn.Close()
        continue
    }
```

**A zero-length Stdin frame closes the child's stdin without killing it.** That
is exactly the `finish` semantic entry 5 called for and assumed did not exist:
a client can end an event-stream agent cleanly — stdin closes, the agent
finishes its turn and exits on its own — as distinct from `kill`. The mechanism
is already on the wire and already understood by both ends.

### What this costs

`runner/streamtask.go` as committed is the wrong vehicle for the framing half.
It stays useful as the pending-table + rendering + adapter-argv logic, and its
tests keep their value, but the transport must be replaced with `agentexec`
before any of it is wired for real. Recorded rather than quietly rewritten,
because the reason it exists in the wrong shape — an unread assumption stated
with the same confidence as a read fact — is the thing worth keeping.

## 10. Interactive tasks have never reported an exit code

Asked while reviewing entry 9: if `ExecuteCommandWithOption` always returns
nil, where did the exit code come from until now?

**Nowhere.** Read, then measured on a dummy harness: a `bash` session driven to
`exit 3` reports

```
status = succeeded | exit_code = 0
```

The path: `waitFn()`'s `*exec.ExitError` goes into the errgroup, `gr.Wait()`
returns it, `executeCommandImpl` logs it and returns `nil` unconditionally. So
`handleOpenExec`'s

```go
if runErr != nil { tf.ExitCode = -1; tf.ErrorMessage = ...("interactive_error: ") }
```

can only fire when the process failed to START (pty.New, cmd.Start). Every
interactive task that runs at all reports 0, and `TaskStore.Finish` maps
`exit == 0` to **Succeeded**. The oneshot path is unaffected and correct —
`Process.Run` reads `cmd.ProcessState.ExitCode()` / `*exec.ExitError` directly.

**Two readings, and I do not think this one is mine to settle:**

1. A bug. The dead `if runErr != nil` branch and its `interactive_error:`
   message say the author expected a real error there.
2. Deliberate. A detach sends SIGHUP and the agent exits non-zero; surfacing
   that would mark every ordinary detach **Failed**, which is worse than
   reporting 0. If that is the reason, the swallow is in the wrong layer —
   it sits in objtrsf's generic exec rather than in the harness's policy — and
   it is undocumented either way.

Fixing it properly means changing `objtrsf/exec` to surface the wait error
(return it, or add it to `Auditor.Exit`), which is a shared-module publish plus
a go.mod bump, not a local edit.

**What it means for this kind:** nothing to fix, by accident of design. The
adapter reports its own exit in-band as a neutral `exit` message, so
`StreamTask.Exit()` has a real code where the PTY kind has none — and the
distinction the PTY kind cannot make (agent exited 3 vs client detached) is one
this kind gets for free, because a detach is not an exit here at all.

## 11. Fixing the exit code took three attempts, two of them wrong

The operator decided to fix objtrsf rather than leave entry 10 recorded. What
that cost is worth writing down, because two of the three attempts looked right
and shipped a regression.

**Attempt 1 — return `gr.Wait()`'s error.** Obvious, and wrong. On Linux the
pty master returns EIO the instant the slave closes, so the stdout copy fails
BEFORE the exit status is collected and the errgroup hands back the EIO.
Measured: a clean `exit 0` session came back
`interactive_error: read /dev/ptmx: input/output error` — a session that
succeeded, reported Failed. **The original `return nil` was load-bearing after
all**, for exactly this, and nothing said so.

**Attempt 2 — keep the child's error in its own variable** so it does not race
the group. Correct as far as it went, and still made the caller type-assert
through `*exec.ExitError` to recover an int.

**Attempt 3 (the operator's suggestion) — read `ProcessState`.** After
`waitFn` returns, both branches have one; go-pty copies the `exec.Cmd`'s
across. It cannot lose the race, because the wait itself sets it. So
`ExecuteOption.OnProcessExit(state, err)` hands over an `ExitCode()` int and
the caller never sniffs an error type — which is what the ONESHOT path in this
repo has always done (`cmd.ProcessState.ExitCode()`), one file away.

Measured after, all three endings:

| ending | before | after |
|---|---|---|
| `exit 0` | succeeded / 0 | succeeded / 0 |
| `exit 3` | **succeeded / 0** | **failed / 3** |
| cancel | succeeded / 0 | succeeded / 0 |

A third hang turned up on the way, unrelated to the exit code and worth its own
line: **a non-PTY child that exits could not complete its own `Wait`.** Handing
`cmd.Stdin` a non-`*os.File` makes os/exec own the stdin copier, and that
goroutine parks in `Read` on a pipe only the stream closes — so `Cmd.Wait`
waited for it while the child's status sat there unread. Owning the copy fixes
it. The PTY path never had this, a pty being an `*os.File`.

One thing left as it was: a CANCELLED task still reports 0 and lands on
Succeeded, because `TaskStore.Finish` deliberately overwrites Cancelled. That
is unchanged behaviour rather than something this touched, and whether
"cancelled" should survive is a separate decision.

## 12. Correction: the ring is already an event ring

Entry 4 listed as an open problem that "`RingBuffer` replay for a byte stream
is the last N bytes… reusing the ring verbatim would truncate mid-JSON-line",
and entry 8 repeated it as a thing the server side would have to solve. Asked
whether I had checked. I had not.

`server/ring_buffer.go` stores `frames [][]byte` — **one entry per complete
frame** — and its own doc says why:

> Each Append() must receive exactly one complete wire-encoded frame (header +
> payload). Truncation at arbitrary byte offsets would corrupt the consumer's
> frame parser when the ring later wraps mid-frame; the API is shaped to make
> that mistake impossible at the type level.

Eviction drops whole frames. `appendCount` gives each a stable identity so
`SnapshotFrom` can replay a suffix. And `outStreamWrapper` chunks at
`math.MaxUint32`, so one Write is one frame in any real case, while the
adapter's `Writer` does exactly one Write per neutral message.

So **one neutral message = one frame = one ring entry**, and replay is already
event-granular. The concern was not merely wrong, it was backwards: I wrote
that an event stream needs a different container than the byte ring, and the
ring had been that container the whole time.

Sixth instance today of the same shape — a claim about code I had not opened,
written in the same register as the ones I had. Every one of them was one grep
away, and every one was caught by the operator asking rather than by anything
in the process.

**What this removes from the remaining work:** the server-side "SessionMux
sibling that buffers events" from entry 8 is not needed. The mux is frame-aware
but not PTY-aware — it moves complete frames, stamps activity on
Stdout/Stderr, and special-cases only TerminalWindowSize, which an event-stream
session simply never sends.

## 13. Correction: the mux DOES carry PTY semantics — and they are inert here

Entry 12 said "the mux is frame-aware but not PTY-aware". Asked whether there
was something PTY-assuming we had already seen. There is, and I had read its
doc comment before writing the opposite:

`SessionMux` runs a **VT mode parser over every Stdout/Stderr frame**
(`m.modes.feed(frameBytes[frameHeaderSize:])`, `server/mode_tracker.go`). It
tracks private DEC modes to know when a full-screen app leaves the alt screen,
stamps `mainMark` at that frame, and replays from there instead of the ring
head so a reattach does not repaint a finished alt-screen episode. It also
replays `lastWinSize` and a mode `preamble()` to a new viewer.

That is terminal semantics living in the mux, not around it.

**For an event-stream session it is inert, and that is now checked rather than
assumed.** `encoding/json` escapes every control byte below 0x20 in a string,
so a raw 0x1B cannot appear in well-formed NDJSON — not in `Event.Text`, and
not in the vendor `Input` passed through as `json.RawMessage`. The parser stays
in its normal state, `onAltScreen()` stays false, `mainMark` stays 0 (full
replay, which is what an event stream wants), `preamble()` is empty and no
winsize frame is ever sent.

The cost is real but small: every event frame is walked by an ANSI parser that
will never match. Worth knowing before someone measures it and calls it a bug.

## 14. The client side needs no new transport either

`CommandExecutionStream` already exposes `Stdout() io.Reader`,
`Stdin() io.Writer` and `Stderr() io.Reader`. The only PTY-assuming entry point
is `RemoteShell()`, which puts the LOCAL terminal into raw mode
(`term.MakeRaw(os.Stdin)`) — an event-stream client simply does not call it.

And `Stdin().Close()` is the zero-length Stdin frame, i.e. the clean `finish`
of entry 9, already reachable from the client API.

So the §3 verbs are not new plumbing. `attach` for this kind is "read
`Stdout()`, render events, write responses to `Stdin()`" instead of "hand the
terminal to `RemoteShell`".

### Remaining work, after entries 12–14

Three of the four items entry 8 listed as "not started" turn out to already
exist:

| entry 8 said | actually |
|---|---|
| `.bgn` additions | done, and needed **no layout change** — an enum value plus two spare reserved bits |
| a `SessionMux` sibling buffering events | not needed; the ring is frame-granular and the mux's PTY logic is inert |
| client transport | not needed; `Stdout()`/`Stdin()` are already exposed |
| `pending=N` on `TaskInfo`, and the verbs | still real work |

## 15. Correction: `-p` is not required, and the docs do not cover the input side

Two claims checked on a prompt, both mine, both from reading rather than
running:

**"`-p` is not a choice."** Wrong. `--input-format` is *documented* as "only
works with `--print`", and I wrote the spec section on that sentence. It is not
enforced: with and without `-p`, the same framed input produced the same
answer, one `result` line and exit 0. `--output-format stream-json` is what
suppresses the TUI. Corrected in the spec, with the limit of the test stated —
one turn, piped stdin, non-TTY stdout.

**"There is no session-end message."** This one held, and now has a better
basis than my six guessed subtypes. The TypeScript SDK reference documents the
control requests — `setPermissionMode`, `setModel`, `setMaxThinkingTokens`,
`applyFlagSettings`, `interrupt`, `reconnectMcpServer`, `toggleMcpServer`,
`setMcpServers`, `stopTask` — and none ends a session; the docs say closure is
`close()` / abort on the Query object. The headless page documents the input
side not at all.

Two things that search turned up which change the design rather than confirm
it:

- **`setPermissionMode` is a documented control request.** So the
  `permission_suggestions` entry `{"type":"setMode","mode":"acceptEdits",
  "destination":"session"}` is not an opaque blob — accepting it means issuing
  that control request. §3's decision to let `exec_cowrite` cover it now has a
  concrete mechanism attached to it rather than a shape.
- **SIGTERM is the documented way an SDK host closes a session**, and it runs
  `SessionEnd` hooks (headless page: "aborts the in-progress turn, terminates
  the process tree of any running Bash command, runs SessionEnd hooks, and
  exits with code 143"). agentexec already carries `ControlType_Signal` and
  `CommandExecutionStream.SendSignal`.

That splits `finish` in a way entry 9 did not see: **stdin close is gentler**
(the current turn completes) but **SIGTERM is what runs SessionEnd hooks**. The
runner injects `.claude/settings.json`, so choosing stdin close means this kind
is the one where a configured SessionEnd hook never fires. Left for the
operator to decide rather than settled here.

## 16. `session send` writes RAW BYTES, and that broke two of my claims

Asked whether `approve` is special, then whether `send` could forge one by
writing NDJSON, then whether `send` can put non-NDJSON on the stream. Each
answer was worse than the last.

**`approve` is not a separate channel.** `KindResponse` and `KindUser` are two
arms of one `switch` in the adapter's input pump — same stream, same direction.
The "damming" is the AGENT, which blocks synchronously until its
`control_response` arrives; the transport keeps flowing. So the case for giving
`approve` its own verb is not the transport. It is that a pending request is
STATE (a user turn leaves none), and that §4's notify-when-unattended default
means someone must be able to answer without attaching.

**And `send` can forge one.** `Client.SessionSend` does
`stream.Stdin().Write(data)` — raw bytes. So
`session send <id> '{"kind":"response",...}'` is a valid approval. That is not
a privilege hole: `send` and `approve` are the same capability under the same
scope, deliberately (§3). What it does mean is that `approve`'s id check is
ADVISORY, and that "may send turns but may not approve" cannot be built by
splitting verbs — it would need the capability bit §3 declined.

One thing survives that unharmed: the runner's tap reads the STREAM, not the
verb, so a forged response still clears `pending` correctly. State derived from
the wire rather than from the caller is why.

**Then the real defect.** Raw bytes mean an operator can write anything, and
the adapter's reader treated a malformed line as fatal — I had written a comment
justifying it ("on the adapter's own input a malformed line is a protocol
violation worth failing on"). But the far side of this stream is a person as
often as a program: `session send <id> "hello"`, which is exactly what that verb
MEANS for the PTY kind, would have killed an event-stream task.

The test for it deadlocked before it failed, which is the defect showing itself
from the other side: the follow-up write blocked forever because the adapter had
stopped reading its input entirely. Rewritten so the writes happen off the read
loop, it fails as an assertion.

Fixed: `ErrBadLine` is a sentinel the pump recognises and continues past, after
reporting it as a warning event. Negative-controlled by restoring the fatal
path, which reproduces "one line of non-protocol input ended the session".

**Still to decide, and now clearly a decision rather than a detail:** §3 says
`send` for this kind means "a user turn", which implies the CLIENT wraps the
text rather than writing raw bytes. If it does, the forging path above closes
on its own and this becomes moot; if it does not, `send` stays a raw pipe into
the adapter and `approve` stays a convenience over it.


## 17. The hello advertised a capability the protocol could not reach

Asked what inbound kinds exist besides the two. There were exactly two —
`response` and `user` — and the adapter's hello advertises three capabilities:
`approvals`, `user_turns`, **`interrupt`**.

`CapInterrupt` appeared in exactly two places: the constant, and the hello that
names it. No inbound kind invoked it, and the input pump had no arm for it. The
mechanism had been MEASURED to work (a turn abandoned, the process alive, a
fresh session-start after) and then not connected. A hello that names a
capability the far side cannot reach is a lie the far side has no way to
detect.

Added `KindInterrupt`, and with it the half I would otherwise have missed: the
receipt. The vendor answers an interrupt with a `control_response`, and without
somewhere to consume it that line falls through to the log decoder and surfaces
as a raw event **carrying vendor JSON** — the exact leak the seam exists to
prevent, and the one a test already asserts against in the other direction.
Both halves are pinned and both negative-controlled.

### What this says about the verbs

The verb question ("`turn` and `approve`, or a polymorphic `send`?") was being
argued a level above the thing that decides it. Verbs map onto inbound kinds,
and the inbound side was incomplete:

| operation | measured | neutral kind |
|---|---|---|
| user turn | yes | `user` |
| answer an approval | yes | `response` |
| abandon a turn | yes | `interrupt` — added here |
| finish (close stdin) | yes | none; transport-level zero-length frame |
| `setPermissionMode` | documented | none; only reachable inside a `response` |

So two gaps remain before the verb list can be settled rather than guessed:
whether `finish` becomes a kind or stays transport-level, and whether a
standing permission-mode change is reachable at all outside an approval.

## 18. "Fetch a snapshot to learn the kind" reinstates the rule we deleted

I proposed that the CLI learn a task's kind by pulling a snapshot, so
`session attach` could refuse a stream task politely. The operator pointed out
what that breaks: **a caller whose action scope is wider than its visibility.**

`exec_view: global` with visibility `none` — the agentboard-driven observer
that may attach to ids handed to it and must not enumerate the server — sees
NOTHING in `ls`. A snapshot-based check would either refuse a caller that is
allowed to attach, or fall through and attach blind.

That is the rank rule again, in a new place. It was dropped from the
per-capability scope design **today** for exactly this use case, and I designed
the check on the assumption it deleted: that `ls` bounds what a caller can
reach. Building the counterexample does not stop you re-deriving the rule
somewhere else.

Two sound routes, neither depending on visibility:

| route | cost |
|---|---|
| put the kind in the attach RESPONSE | `AttachSessionResponse` has no spare bits — appending a field is a layout change, so a real skew, server-first |
| let the STREAM identify itself | the first line on a stream task is the adapter's `hello`; no wire change at all |

The second has one hole: a REATTACH replays from the ring, and a long-running
task will have evicted the hello. The existing machinery already answers that
— the mux replays `lastWinSize` and a mode preamble to every new viewer,
because a late joiner needs the preamble it missed. Keeping the `hello` and
replaying it is the same mechanism on the same problem: winsize for terminals,
hello for streams.

**And the justification improves once visibility is out of it.** The kind does
not belong in the attach path so a verb can decline politely; it belongs there
because a client cannot interpret the stream without knowing whether to expect
terminal bytes or NDJSON. That reason holds no matter who is asking.

Not built here — it is the work `session stream attach` needs, not what
starting a task needs.

## 19. Decision: the kind is a FIELD on the attach response — and entry 18 priced the alternative wrong

Entry 18 left two routes open. Across a compaction I then reported it as
"decided: mux replays the hello, no wire change", and when the operator
challenged that, I searched the session record and reported that no conclusion
existed at all. **Both answers were wrong, and entry 18 is why.**

What the record actually holds, in order:

1. The operator makes the visibility point. My reply names the field route
   FIRST — "attach 応答に kind を載せる: サーバは知っていて、呼び出し側は
   attach を許されている以上それを知る権利がある" — and then adds the
   justification entry 18 presents as its own late improvement: the kind is
   needed because a client cannot decide whether to expect terminal bytes or
   NDJSON, not so a verb can decline politely.
2. One tool call later I grep the schema, find no spare bits, and **switch to
   the hello route on that cost alone**.
3. Entry 18 records the state after the switch. The reasoning that produced the
   field route survives in it; the fact that the field route had been the
   answer does not.

So the conclusion was reached and then overturned by a cost estimate — the
same estimate this entry corrects below. Entry 18 recorded a reversal as if it
were an opening.

**Two process failures, both mine, both already named in this log:**

- **A preference written under a neutral cost table becomes a decision one
  reader later.** Nothing between the table and the paragraph said which was
  binding.
- **I claimed absence from a filtered view.** I grepped the transcript for
  `AttachSessionResponse`; the message that mattered says `attach 応答`, and my
  dump truncated at 1500 characters besides. Entry 12's shape exactly: grep is
  a locator, and a miss in a locator is not evidence of absence.

The decision, restored: **`AttachSessionResponse` gets a `kind` field.** Entry
18's cost column was wrong twice, both times in the direction that made the
schema route look unaffordable:

- **"a real skew, server-first" overstates it and misstates it.** There are no
  third-party clients on this deployment; the fleet restarts with one script
  and the WebUI is a hard reload. More precisely, *server-first stages
  nothing here*: a field appended to a RESPONSE breaks old clients the moment
  the server is new, because `DecodeExactCopy` rejects trailing bytes. It is a
  coordinated restart, not a staged migration — a different, smaller thing than
  the request-side skews that made "server-first" a rule on this project.

**And "no spare bits" was never a criterion — it is not even an input.** This
is the sharper correction, because the sentence above still argues about a
price, and the thing that actually flipped the decision was not a price at all.

`reserved` in this schema is **alignment padding, not an extension budget**.
`OpenInteractiveRequest` carries five `u1` flags and `reserved :u3`; 5 + 3 = 8.
The field exists because a bit run has to reach a byte boundary — it is not a
balance somebody deposited for later use. It follows that a format holding no
bit flags has no `reserved` **by construction**: `AttachSessionResponse` is a
`u8` enum and two `u64`s, so there is nothing to pad. Its lack of spare bits
reports that it contains no flags. It says nothing whatever about whether a
field belongs in it.

The converse makes it plain: had `OpenInteractiveRequest`'s flags already
filled their byte, `event_stream` would have cost one more byte — a layout
change — and would still have been the right place to put it.

What decides a field is whether it belongs there semantically and what the
rollout costs. Neither question is answered by how a neighbouring format's bit
run happens to align.

So the reversal in entry 18 did not run on a bad estimate. It ran on a grep
result with **zero information content**, which flipped a decision because it
was concrete and looked like a constraint. That is the shape memory already
records for invented labels becoming implicit constraints, one layer down: here
the label was not invented, it was the schema's own padding, read as a budget
with a balance.

## 20. The skew I "tested" was the wrong axis, my failure model was backwards, and the real failure was a hang

Asked whether the wire skew was tested. It had been "tested" — and the answer
fell apart in three layers, each found only by going one level more concrete.

**Layer 1: the check that PASSED tests a different skew.** `wire-skew-check`
runs NEW runner × OLD server and asserts the hello rejection is recoverable.
The kind field lives in `TaskControlResponse`, which the RUNNER NEVER DECODES.
For this change the PASS was true and nearly vacuous — the second vacuous
wire-skew run in two days (the first ran with an uncommitted `.bgn`, diffing a
ref against itself). A green check for axis A reads as coverage for axis B
until someone asks which axis it measures.

**Layer 2: the failure model in the record was backwards.** Entry 19, the
spec, the `.bgn` comment and the landed commit message all said "old clients
break the moment the server is new — `DecodeExactCopy` rejects trailing
bytes". `DecodeExactCopy` exists and does that; **nothing on this path calls
it**. `dispatchControl` uses the tolerant `Decode`, which returns trailing
bytes unread, so an OLD client on a NEW server ignores the field and works.
The claim was written from the generated API's strictest member, not from the
call site — the same class as entry 9 (agentexec "is PTY-only") and entry 12
(the ring "truncates mid-line"): a property of the artifact asserted without
reading which part of the artifact is actually used.

**Layer 3: the direction that DOES break was a silent hang, measured.** NEW
client × OLD server (built `61f5a4a`'s server, drove it with the new CLI):
`session snapshot` printed one slog line — `not enough data to read for field
"AttachSessionResponse::Kind"` — and then sat forever; `timeout` killed it at
10s. The mechanism: `dispatchControl` logged the decode error and DROPPED the
response, and `RoundTripTaskControl`'s only other exit is its context, which
most CLI paths pass as `context.Background()`. And this is not a lab shape:
`make build` had already put the new CLI on the live host while the LIVE
server stays old until the operator restarts it — the hang was the deployed
state of every attach-family verb at the moment the question was asked.

Fixed at the correlator, not the caller: a response that fails to decode is
now ROUTED to the waiting request as `ErrResponseUndecodable` (RequestId is
the second field, parsed before any arm can fail), with a guard for payloads
too short to carry an id — request 0 is real, `nextReq` starts there, and
failing it on unroutable garbage would break an unrelated caller. Measured
after: the same command errors immediately, naming the version-skewed server
and the fix. Pinned by tests, negative-controlled (restoring the drop reddens
the strand test) — and the negative control cost a lesson of its own:
`git checkout -- cli/client.go` to undo it also destroyed the uncommitted fix,
which had to be reapplied. Revert a negative control by reapplying the edit,
never by checking out a file that holds other uncommitted work.

Deploy order comes out where the fleet rule already was — server first — but
for the measured reason (new client × old server is the failing direction),
not the recorded one.

## 21. Handoff (2026-08-21): where this stands, and the next increment

Written for the session that picks this up. Decision status is a FIELD here,
not emphasis — that rule is what entry 19 cost.

**Shipped and E2E-verified on a dummy harness** (through `758f62a`, landed,
fleet restarted — server AND runners, so no version-mix exists):
the adapter + runner wiring, `TaskKind.stream`, `AttachSessionResponse.kind`,
kind gates on every attach-based verb, `session stream attach` (CLI renders
via the shared `streamagent.RenderText`; TUI maps it to the logs pane),
`session new --stream` open-then-follow, and the correlator fix
(`ErrResponseUndecodable` instead of a silent hang on a version-skewed
server).

**Next increments, in dependency order:**

1. **The `session stream` write verbs** — `turn`, `approve
   --allow|--deny|--modify`, `interrupt`, `finish`. Each is a cowrite attach
   writing ONE NDJSON line, and it must APPEND THE NEWLINE itself: measured,
   a line without `\n` sits invisibly in the adapter's line buffer until the
   next newline flushes it (the PTY "text sits on the prompt" semantic).
   `session send` stays the raw escape hatch and appends nothing. A deny
   message reaches the agent verbatim (operator-authored text entering a
   model's context). The verbs are 1:1 with inbound kinds; the CLI dispatch
   already exists and answers "not built yet".
2. **`pending=N` on `TaskInfo`** — needs a NEW runner→server message (only
   accepted/started/finished/heartbeat exist); then the display walk (`ls`
   zeros printed, TUI, WebUI, checklist 11–23). `session stream requests`
   reads this state, so it depends on this, not just on verb plumbing:
   the pending table currently lives runner-side only (`StreamTask.Pending`).
3. **Request-id nonce** — `req-N` repeats after a resume; the id is the
   staleness guard (spec §3), so it needs a per-run nonce before `approve`
   ships, or a stale `approve req-1` answers a different request.
4. **`session stream snapshot`** — the FORMATTED last-N-events read
   (`session snapshot --raw` already yields the raw NDJSON, kind-agnostic
   by design).
5. **WebUI** — the `session` command family does not exist in `runCmd` at
   all; the stream verbs land with it. The approval modal (§3: `tool_name` +
   structured input diff + suggestions as the button row) and a `--stream`
   spawn control (checklist 34a: build from existing controls, not a
   textarea) come with the write verbs. TUI session popup stream toggle:
   same batch (recorded omission in the firing log).

**DECIDED — NOT BUILT (2026-08-21, operator):** the `RunnerHello`
per-profile stream declaration stays unbuilt. The motivating case (a
version-mixed fleet silently PTY-ing a stream task via the ignored reserved
bit) does not occur under wholesale-restart operation, and this deployment
restarts wholesale. The spec records the mechanism for the day mixed
operation is real.

**NOT DECIDED / untested:** `mcp_message` (no MCP server was probed),
subagent-concurrent approvals (pending stays a count for this reason), the
`defer` hook decision (§3), and how the pending-approval notification arms
(§4 amendment decides the GATE — notify cap, degrade honestly — but nothing
is wired).
- **It did not price the hello route's real defect.** The server already holds
  the kind in the task record. Replaying a `hello` so the client can infer it
  means the server declines to state what it knows and the client re-derives it
  from payload bytes — and the mux would consult that same `TaskKind` anyway,
  to decide which tasks get a pinned frame 0. It is a convention standing in
  for a protocol field, which is the thing this project keeps saying not to do.

**Considered and rejected on the way:** appending `ok_event_stream` to
`AttachSessionStatus`. It is layout-neutral and decode-safe — the generated
decoder is a raw cast, `a.Status = AttachSessionStatus(tmp8760)`
(`runner/protocol/message.go:20510`), so an old client sees an unknown value,
falls out of its `ok` branch and declines to attach, which is the correct
behaviour for a client that cannot render NDJSON. Rejected because it overloads
a RESULT enum with a task PROPERTY: the third kind turns it into `ok_×N`.

**What the field also removes:** the hello route needed the mux to retain and
replay a stream task's first frame — new mux state, a new eviction exception,
and a rule keyed off the kind it was trying to avoid asking for. The field
deletes that work rather than deferring it.

## 22. The kind shipped with no way to launch a runner that serves it

Asked how the adapter binary is actually specified. Reading the config path
turned up that everything wired in entries 1–21 was reachable only by hand.

`--agent-stream-adapter` exists (`cmd/agent-runner/main.go`) and defaults to
empty; `--agent-profiles` accepts a `streamAdapter` key
(`runner/agent_profile.go`) — but the flag's own help text listed every OTHER
key of that JSON, so a caller reading `-h` had no way to learn it exists. And
`KNOWN_AGENT_PRESETS` (`scripts/agent_presets.py`) had no such key at all,
which means **every runner spawned the documented way** —
`scripts/runner.sh up --as <tag> --agents claude`, the route
`feedback_use_runner_scripts` says to use — refused every event-stream task.
The E2E runs in entry 21 passed the flag by hand and so never met this.

Fixed: the claude preset supplies `bin/harness-stream-adapter` (derived from
the module's own checkout, so the adapter's `protocol_version` matches the
`agent-runner` beside it), `--agents` emits `--agent-stream-adapter` for the
default profile and the `streamAdapter` key for the rest, and the flag joins
`_CONFLICTING_FLAGS` — the list's invariant is "every flag `--agents` sets",
so an explicit one alongside is refused rather than silently overridden.

Three things worth not rediscovering:

- **The other presets get `""`, and that is a fact about the ADAPTER.**
  `harness-stream-adapter` speaks claude's protocol specifically — it appends
  `--input-format stream-json` / `--permission-prompt-tool stdio` and refuses
  an argv that already names one. Pointing it at codex would append claude's
  flags to a CLI without them. An empty value makes the profile REFUSE the
  kind, which is the honest outcome, not a gap to fill by aiming the flag
  somewhere.
- **The sandbox twins inherit the HOST path, unchanged, and must.** The
  adapter is a host process that execs the wrapper as its agent argv; the
  container never sees that path, and the wrapper's `podman run -i` carries
  the adapter's stdio through. The derivation rule (only `bin` changes) gets
  this right by construction — a hand-written container path would have been
  the drift S1 warns about.
- **`bin/harness-stream-adapter` is a BUILD ARTIFACT**, unlike the checked-in
  sandbox wrappers, so a checkout that has not run `make build` now has a
  preset naming a path that does not exist. The runner LookPaths it per task
  by design (an adapter can be replaced under a live runner), so the task-level
  failure was already loud; added `ProfileSet.UnresolvableStreamAdapters()`
  warned once at startup, the same shape and the same reason as
  `UnrecognisedLogFormats` — a config value consumed later in the task path,
  reported where the operator is still watching. WARN not error, matching
  `ResolveBinPaths`: a binary built after startup still works.

**Status:** shipped. Verified by unit tests on both sides (negative-controlled
— blanking the preset value reddens them), and by `runner.sh up --dry-run`
showing the concrete argv. NOT verified: an event-stream task running inside
the podman sandbox. The launch wiring is inherited correctly; whether an
approval round-trips through the container is unmeasured, and the plain claude
path is what entry 21's E2E covered.

**Unrelated, noticed while reading:** the three paragraphs above this entry
(`It did not price the hello route's real defect` onward) belong to entry 19's
argument, not to entry 21's handoff list — they read as part of the "NOT
DECIDED" section they follow. Left as-is: this log appends and does not edit
away, and moving them is an edit.

## 23. The write verbs and the TUI chat, and the gate that is disarmed anyway

Built to Amendment 2026-08-21b: the request-id nonce, `cli.EncodeStreamMsg`
plus the four write helpers, `session stream turn/approve/interrupt/finish`,
`cli.StreamSession`, and a TUI chat screen on `r`. `requests`, `snapshot`,
`--modify` and the WebUI are the remainder.

**The finding that changes what this feature IS, on this deployment.** Driving
a real stream session, a `Write` ran with no approval request at all — the
whole §4 gate silent. The cause is not in the harness: the operator's own
`~/.claude/settings.json` sets `defaultMode: auto`, and the agent runs as that
user, so ordinary tools are pre-approved before the permission channel is ever
consulted. The harness's injected settings allow exactly one thing
(`Bash(harness-cli *)`) and are not involved. Re-run with `--agent-arg
--permission-mode --agent-arg default` and the request fires immediately,
carrying its full input.

So an approval UI is real code for a path that, as this fleet is configured, is
dormant. Operator's call, recorded as a decision rather than a discovery:
**build it anyway** — the mode is per-task and one flag away, and a gate that
exists only once someone needs it is not a gate.

**Two things measured that correct earlier claims in this log and its spec:**

- **`updatedInput: {}` does NOT break an allow.** `originalInput` has returned
  `{}` since the first adapter commit, and the code comment beside it says the
  vendor reads a missing input as an empty one — which reads as "a plain allow
  runs the tool with no arguments". Measured: the Write ran with its ORIGINAL
  arguments and wrote the right file. The placeholder is still a placeholder,
  but it is not a blocker, and `--modify` is where it starts to matter.
- **The task log is not as blind as Amendment 2026-08-21b says.** `RenderText`
  does drop a request's `Input` entirely — that part holds — but `tool_start`
  is emitted BEFORE the request and renders `truncate(Args)`, so a SMALL tool
  call's arguments do appear in `logs`. The honest claim is narrower and worse:
  the operator sees the payload right up until it exceeds 200 bytes, which is
  exactly the case where reviewing it matters. The chat still has to read the
  stream; the reason is the large input, not every input.

**Three defects this increment made and caught, all by writing the thing down
or driving it rather than by review:**

1. The plan's own correction was half wrong. It said every new CLI verb must
   use `parsePermuted` for order-free flags — but that helper's doc says it is
   only for positionals that can never begin with `-`, and `turn`'s tail is
   free-form text. `turn` follows `session send`'s flags-first shape instead;
   `approve` keeps the permuted parse by making the deny reason a `--message`
   flag rather than a trailing positional, which is what buys the freedom.
2. A late `chatAttachedMsg` could assign a session to a CLOSED chat, leaking
   it with nothing left to close it. The id check alone was not enough; the
   guard is `!m.open || id mismatch`.
3. The chat rendered CENTRED, because its View branch was copied from the
   grid's, which centres fixed-width panes deliberately. Full-width prose
   centred puts every transcript line at its own indent. Only visible by
   driving a real turn through it — the code read fine.

**Verified live**, on a preset-launched runner with the real fleet: `turn`
reached the agent; an approval fired with its input; `approve --allow` ran the
tool and the file appeared; in the TUI, `r` opened the chat, a typed turn was
sent, the approval block showed the input pretty-printed, `a` allowed it, and
reopening the chat replayed the whole exchange from the mux ring.

**For the next increment:** `pending=N` still needs its runner→server message,
and `stream requests` still wants it for the count even though the chat no
longer needs `requests` to see a payload. `--modify` needs `originalInput` to
actually retain the input. The WebUI has no `session` command family at all,
which is where its half of this starts.
