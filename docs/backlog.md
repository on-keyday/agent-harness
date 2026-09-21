# Backlog

Things found while doing something else, deliberately not done then, and worth
doing. One entry per item: what it is, the evidence it rests on, and why it was
deferred rather than folded into the work that found it.

An entry earns its place by being **actionable and measured**. "X could be
nicer" is not an entry; "X is split N ways, here is the count and the command
that produced it" is. Delete an entry when it is done, or when it turns out to
be wrong — a backlog that only grows stops being read.

---

## Aggregate the server package's logging

**What.** `server/` logs two ways. `Server` methods use `s.cfg.Logger`;
everything else calls package-level `slog` directly. Neither is wrong, but the
split means a caller cannot redirect the package's output by configuring the
server, and a struct without a `*Server` (`Dispatcher`, `TaskHandler`) has no
choice but the package logger.

**Evidence** (2026-09-22):

```
$ grep -c 'slog\.\(Warn\|Error\|Info\|Debug\)(' server/*.go   # non-test
task_handler.go:62  runner_handler.go:25  taskstore.go:15
port_forward.go:10  exec_run.go:10  psk.go:9  dispatch.go:9   # …and more
$ grep -rn 'cfg\.Logger\.' server/*.go | grep -v _test | wc -l
25
```

So direct `slog` outnumbers the configured logger by roughly six to one, and
`Dispatcher` has no logger field at all.

**Why deferred.** Found while adding one line to `server/dispatch.go`. Changing
how ~200 call sites log is not a change to make inside a board migration, and
the right shape is a decision (a logger field on each handler struct? a package
`var log` the server sets? accept the split and say so?) rather than a
mechanical sweep.

---

## `board_send`: a capability for agentboard publishing

**What.** `agent send` takes no capability, so a task spawned `--caps none`
can publish to any topic it can name — including any task's
`chat.<short-id>`. Two ranked bits, `board_send ⊇ board_reply`, let an
operator either silence a task entirely (drop both) or leave it able to answer
what it was asked without initiating (`board_reply` alone).

**Evidence.** `requiredCap` (`server/capabilities.go`) has no entry for the
send path, and `readAgentPayloadStream`'s own comment says "agent send is
reachable with no capability". The wire already distinguishes the two cases —
`AgentSendRequest.in_reply_to` — which is this schema's stated test for when a
capability may be split (`message.bgn`, the `exec_resize` comment).

**Why deferred.** Sequenced after the task-control unification on purpose
(U10 of `2026-09-22-agentboard-task-control-unification-design.md`): with
`PermissionDenied` now answering every refusal, the bits cost an enum entry and
a gate, and no new status values. Its own spec is still to be written.

**Known cost to state when it lands.** `Capability.all` is a literal, and the
WAL persists the number, so a task whose mask was persisted before the change
holds the old `all` and loses board sending until re-granted. This is the first
bit to gate something that was previously ungated, so unlike every earlier
addition the default-off direction is not the safe one.

---

## A dial per inbox hook

**What.** Every `harness-cli agent inbox` invocation opens a connection and
exits. The wake path makes this worse rather than better: a wake fires
`UserPromptSubmit`, which runs `agent inbox`, which dials.

**Evidence.** `cli/agent/client.go`'s `connectClient` → `cli.Dial` per
invocation; the cost was named in the 2026-04-28 agent-comms design (§13) and
is unchanged.

**Why deferred, and why the obvious fix is not one.** That design reserved
`AgentMessageKind.deliver` for a long-lived connection carrying a push, and
that reservation is now deleted — the push exists as
`RunnerRequestType.TaskWake`, terminating at the runner, and no push can remove
this cost anyway: the dial happens because the inbox path is a process per
turn, not because the transport lacks one. Removing it means something resident
on the agent side, which is a different design and has never been scoped.
