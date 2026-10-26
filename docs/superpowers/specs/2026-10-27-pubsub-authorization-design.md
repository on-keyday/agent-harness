# Pubsub authorization — design

## Problem

A pubsub JOIN is authorized by nothing beyond the PSK handshake, and whatever a
subscriber writes on its joined stream is re-published to the topic. The backlog
entry "Pubsub topics: anyone authenticated can read and write them"
(`docs/backlog.md`, 2026-10-14) recorded this from reading; re-read on
2026-10-27, the state is:

- **P1 — the task log is readable by JOIN alone.** `server/server.go`
  dispatches `AppKind_Pubsub` straight to `subscriber.HandleMessage` with no
  check. The `logs` snapshot of the same bytes needs `exec_view` plus scope
  (`handleGetTaskLog`, `server/task_handler.go`), so a `--caps none` task reads
  any task's log by skipping the snapshot and joining `task.<id>.log`.
- **P2 — `runners.status` reaches every subscriber.** `publishRunnerEvent`
  uses `Publish` (unfiltered), while `handleList` gives runner rows only to a
  caller for which `listVisibleToCaller` answers `all` ("a confined caller …
  sees zero runners, so it cannot enumerate the operator's runner topology").
- **P3 — `notifications` reaches every subscriber**, including the replay of
  the last 64 events on JOIN (`replayNotifications`). A `NotifyEvent` from a
  worker carries that task's id, runner id, repo and hostname (`WorkerInfo`).
- **P4 — any subscriber can publish to any topic it joins.** The per-stream
  reader goroutine in `pubsub.Subscribe` calls `ps.Publish(nickName, topic,
  data)` for every read. So an agent can inject frames into `tasks.status`,
  `runners.status`, `notifications` or any task's log, and the log tap
  (`server/logtap.go`) persists what is injected into a log, because taps
  receive every published message regardless of filters.

**Corrected from the backlog entry.** `tasks.status` and `task.<id>.status`
are no longer unfiltered: since `18d28190` (2026-10-19) every event except
`task_pruned` goes through `PublishSelect` with `taskEventAudience`, which is
`ls`'s predicate. `task_pruned` still goes to everyone, by a written decision
in that code (it carries only id and kind, and the entry is gone when it is
published). `conns.status`, `forwards.status` and `execs.status` were already
filtered. This spec does not change any of those.

**The one legitimate writer.** The backlog left "does any client rely on
writing to a joined stream" unread. One does: the runner publishes a oneshot
task's output to `task.<id>.log` (`runner/session.go` `handleAssign` →
`runner/connect.go` `peerSender.Publish` → `peer/publish.go` `Conn.Publish`,
which JOINs once per topic and `AppendData`s on the stream). Every other
`JoinAndGetStream` caller (`cli/watch.go`, `cli/conns.go`,
`cli/notify_watch.go`, `cli/logs.go`, `tui/events.go`) only reads.

**A latent ordering bug this design depends on.** `Registry.ownsTask`
(`server/registry.go`) says "The reservation exists before AssignTask is sent".
`Dispatcher.TryDispatch` and the interactive open honour that (`BindTask`, then
send). `Scheduler.Tick` does not: it calls `s.assign` (→ `Server.sendAssign`,
which sends AssignTask and binds nothing) and only then `s.reg.BindTask`. A
runner's `TaskAccepted` handled between the two is refused by
`acceptTaskReport` as "task report from non-owner". Read, not exercised. This
spec makes `ownsTask` gate the runner's log JOIN (D3), which widens the window
from one control message to the first log write, so it is fixed here (D9).

## Decisions taken

**operator** = chosen by the human in conversation (2026-10-27), including the
design presented there before "どうぞ"; **this spec** = chosen by the author
while writing — those rows are the ones worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | This item is done first among the hardening items, before sandbox-by-default, runner enrollment and tenancy, because it is worth doing under either threat model | operator |
| D2 | Readers are gated with the predicate the matching snapshot verb already uses: one predicate for the snapshot and the stream | operator |
| D3 | Writing is allowed only on `task.<id>.log`, and only for the runner connection that owns the task (`Registry.ownsTask`) | operator |
| D4 | The write permission is decided once, at JOIN, not per write | operator |
| D5 | A refused JOIN is logged by the server at WARN, because `peer.Conn.Publish` caches a JOIN failure for the life of the connection and the runner would otherwise lose the task's log silently | operator |
| D6 | Read permission is re-evaluated on every publish, so `caps set` narrowing a grant takes effect on the next message, as board reach does | this spec |
| D7 | `runners.status` and `notifications` are delivered only to callers for which `listVisibleToCaller` answers `all` (operators and global-visibility callers). `runners.status` JOIN is accepted for everyone (a confined `watch` gets no runner events, as its `ls` gets no runner rows); `notifications` JOIN is refused for the rest | this spec |
| D8 | A new pubsub status `forbidden` answers a refused JOIN, for every reason (missing cap, out of scope, unknown task), so the answer is not an existence oracle | this spec |
| D9 | `Scheduler.Tick` binds before it sends, and unbinds on a send error, as `TryDispatch` does | this spec |
| D10 | A write on a stream without write permission is dropped, with one WARN per stream; the stream stays open | this spec |
| D11 | The authorization hook runs before `PubSub` takes its lock | this spec |
| D12 | The JOIN decision depends on the role the connection authenticated as: a runner-role connection may JOIN only the log of a task it owns, whatever the topic. The hook is per subscriber, not per broker (Amendment below) | this spec, during implementation |

## Rules per topic

| Topic | JOIN | Delivery (per publish) | Write |
| --- | --- | --- | --- |
| `task.<id>.log` | owning runner, or `exec_view` held AND `<id>` in its scope; else `forbidden` | same predicate | owning runner only (D3, D4) |
| `tasks.status`, `task.<id>.status` | anyone | unchanged (`taskEventAudience`) | nobody |
| `runners.status` | anyone | `all` only (D7) | nobody |
| `notifications` | `all` only; else `forbidden` (D7) | `all` only | nobody |
| `conns.status`, `forwards.status`, `execs.status` | anyone | unchanged (`visibleToCaller`) | nobody |
| any other name | anyone | (no server publisher) | nobody |

"Owning runner" is `Registry.ownsTask(cid, id)`: the connection is the current
one for a runner identity that has `<id>` in `ActiveTasks`. "`exec_view` AND in
scope" is `authorize(cid, exec_view, id)` (`server/capabilities.go`), i.e. the
`hasCap` + `inScope` pair `handleGetTaskLog` checks in two steps. An operator
connection passes both. So would a runner connection — it has no principal
either — which is why a runner's JOIN is decided by ownership alone (D12).

Why `forbidden` for every task-log refusal (D8): `handleGetTaskLog` answers a
missing cap with `PermissionDenied` and an out-of-scope id with "not found", so
that the out-of-scope answer looks like absence. A JOIN to a task that does not
exist succeeds today, so answering "not found" to an out-of-scope JOIN would
distinguish it from a nonexistent task. Refusing all three alike (missing cap,
out of scope, nonexistent within a confined scope) leaves nothing to tell
apart. A caller whose `exec_view` scope is global may still JOIN a nonexistent
task's log, as today.

Why `notifications` refuses rather than delivering nothing (D7): it has no
snapshot verb whose empty answer the stream would mirror, and `notify-watch`
printing nothing forever is indistinguishable from "no notifications". No
bundled skill tells an agent to run `notify-watch`
(`grep -rn notify-watch runner/agentskills` returns nothing).
`runners.status` is different: `harness-cli watch` joins it together with
`tasks.status`, and `supervising-workers` tells agents to run `watch`, so
refusing it would break `watch` for a confined agent.

## Mechanism

### `pubsub`

- Each `Subscriber` gains an authorizer, installed by the connection that owns
  it: `SetAuthorizer(func(topic string) JoinGrant)`, where `JoinGrant` is
  `{Allow, Publish bool}`. None installed = allow and publish, so `pubsub`'s
  own tests keep today's behaviour. Per subscriber because the answer depends
  on the authenticated role, which only the connection knows (D12).
- `Subscribe` calls it **before** `ps.m` is taken (D11). The authorizer reaches
  into the registry and the task handler; `PublishFiltered`'s comment already
  explains why such callbacks must not run under `ps.m`.
- `!Allow` → `PubSubResponse{status: forbidden}`, no subscription, and a WARN
  naming the cid and topic (D5).
- `Subscribe` stores `Publish` on the `topicJoinInfo`. The per-stream reader
  publishes a read only when it is set; otherwise it drops the read and logs one
  WARN for that stream (D10).
- `already_subscribed` keeps its meaning; the hook runs first, so a refused
  caller never learns whether it was already subscribed.

`pubsub/protocol/message.bgn` appends `forbidden` to `enum Status`. Nothing
persists a `PubSubResponse`, so there is no disk axis.

### Server

- `handleConnection` installs the authorizer on its subscriber, reading the
  PSK gate's role at each JOIN (pubsub only flows once the gate has fixed it).
  `authorizeJoin(role, cid, topic)` implements the JOIN column of the table,
  parsing the task id out of `task.<id>.log` (`topics.ParseTaskLog`).
- The task log's delivery predicate: the per-stream reader currently calls
  `ps.Publish`. It calls `ps.PublishFiltered` instead, with a filter the server
  supplies per topic (a broker hook, `DeliverFilter func(topic string)
  func(objproto.ConnectionID) bool`, nil = everyone). Taps still receive every
  published message, so the log store is unchanged.
- `publishRunnerEvent` and `OnNotify` switch from `Publish` to
  `PublishFiltered` with the `all` predicate.
- The notification replay runs under `ps.m` inside `OnSubscribe`; it needs no
  filter, because only an `all` caller gets past `authorizeJoin` for that topic.
- `Scheduler.Tick`: `BindTask` → `assign` → on error `UnbindTask` (D9). The
  doc comment's "Order of operations" list is updated to match.

### Clients

No client change is needed for the operator: every rule admits an operator
connection. What a confined caller sees changes:

| Surface | Before | After |
| --- | --- | --- |
| `harness-cli logs -f <id>` | snapshot denied; JOIN bypass possible | unchanged — `Logs` asks `GetTaskLog` first and stops at its denial |
| raw JOIN to `task.<id>.log` | full log | `forbidden` |
| `harness-cli watch` | every runner event | no runner events; task events unchanged |
| `harness-cli notify-watch` | every notification | JOIN error naming `forbidden` |
| TUI / WebUI | operator surfaces | unchanged |

`peer.Conn.JoinAndGetStream` already turns a non-`ok` status into an error that
prints the status name. `tui/events.go` `resubscribeLoop` would retry a
`forbidden` JOIN with backoff; it is not changed, because the TUI is an
operator surface (it proves the operator secret, README "Quick start") and
every rule above admits an operator connection.

`runner/agentskills/supervising-workers/SKILL.md` describes `watch` as "(all
tasks)"; that has been false for a confined caller since `18d28190` and becomes
false for runners too. It is reworded to "tasks and runners you can see"
(the repo's `.claude/skills` copy follows, per the agentskills embed rule).

## Amendment 2026-10-27 (during implementation): the runner role

**Replaces** the broker-wide `AuthorizeJoin(cid, topic)` hook this spec first
specified, and the rule table's silence about runners on topics other than a
task log.

**The fact that moved it.** `callerCaps`, `scopeSet` and `visibleToCaller`
(`server/capabilities.go`) read a connection with no principal task as an
operator. A runner connection has no principal task. Implemented as first
written, every predicate in the table would have admitted a runner connection
to every topic — and any connect-PSK holder can authenticate as a runner (the
backlog entry "Any connect-PSK holder can register as a runner"), so P1–P3
would have stayed open to an agent willing to connect with the runner role.
Measured: the E2E test `TestPubsubAuthz_RunnerRoleJoinsOnlyOwnedLogs` joins
`task.<id>.log`, `notifications`, `runners.status` and `tasks.status` as a
runner that owns nothing; before the change all four succeed.

**What changed.** The role is known only to the connection's PSK gate, so the
decision moved onto the subscriber (D12). A runner connection may JOIN only
`task.<id>.log` of a task it owns, and is the only connection whose writes are
published. The per-publish delivery filter still reads the runner as an
operator would; that is harmless because the JOIN gate is the only way a
runner becomes a subscriber.

## Version skew and rollout

- Only the server enforces anything. Restart the server; runners and clients
  need no restart.
- A new runner and an old server, or the reverse: the runner's JOIN to its own
  task's log is answered `ok` by both, so `scripts/wire-skew-check.sh` has no
  new phase. Run it anyway, since a `.bgn` changed.
- An old client receiving `forbidden` decodes an unknown enum value and reports
  the JOIN as rejected — the same path as any non-`ok` status.
- A runner whose hello predates `runner_id` has a zero identity, and
  `ownsTask` refuses a zero identity, so such a runner's oneshot logs are
  refused. Every runner built since that field landed sends one.

## Testing

- **Negative control first**, through a real server, written and seen failing
  before the change: a `--caps none` agent connection (1) JOINs another task's
  `task.<id>.log` and receives a line the runner publishes; (2) writes a frame
  on its `tasks.status` stream and an operator subscriber receives it; (3)
  writes on a `task.<id>.log` stream and the line lands in the log store.
- After the change, the same three fail closed: (1) `forbidden`; (2) and (3)
  nothing delivered, nothing persisted, one WARN.
- Positive paths: the owning runner's writes reach an operator `logs -f` and
  the log store; an agent holding `exec_view` scoped to the task JOINs and
  reads; an operator receives `runners.status` and `notifications` with the
  replay.
- Narrowing: a reader admitted with `exec_view`, then `caps set` without it,
  receives no further log chunks (D6).
- Scheduler: a test `AssignFunc` that asserts `ownsTask` is already true when it
  is called; it fails on today's order.
- `pubsub` unit tests: nil hooks keep today's behaviour; `!Allow` answers
  `forbidden` and creates no subscription; `!Publish` drops writes, taps
  included.
- Live, on a dummy harness: `submit --agent bash --task 'echo hi'` and
  `logs -f` show the output end to end (the runner write path); `harness-cli
  watch` from inside a `--caps none` task runs and shows its own task's events;
  `notify-watch` from that task reports `forbidden`.

## Out of scope

- `task_pruned` delivery to everyone (an existing, written decision).
- Topic names outside the `topics` package: JOIN stays allowed. With writes
  restricted, nothing is ever published to them.
- Bounding the number of topics a connection can create by joining.
- The agentboard, runner enrollment, sandbox defaults and tenancy (the other
  hardening items; D1).

## When this lands

Delete the backlog entry "Pubsub topics: anyone authenticated can read and
write them", and in "Multi-tenancy" drop the "pubsub status topics (previous
entry)" row from the outside-the-tree column.
