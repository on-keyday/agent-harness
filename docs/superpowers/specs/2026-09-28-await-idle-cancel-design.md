# Listing and killing armed await-idle watchers

`session await-idle` arms a one-shot watcher that ends only by firing or by the
session stopping. Nothing lists what is armed and nothing disarms it, so an
armed watcher is invisible and irrevocable from the moment the request returns.

## Decisions taken

| Decision | Decided by |
|---|---|
| Manual list + kill only; no automatic disarm when the watched task replies on the topic ("an extra agentboard wake is not much harm") | operator, 2026-09-28 |
| An agent sees and kills only the watchers it armed; the operator sees and kills all | operator, 2026-09-28 |
| Approach A: a watcher registry with ids, `session await-idle ls` / `kill`, shaped after `exec ls` / `exec kill` | claude, approved by operator 2026-09-28 |
| A reply-sink watcher is dropped when its requester's connection closes | claude, approved by operator 2026-09-28 |
| The sub-verb is `kill`, not `cancel`, to match `exec kill` / `forward kill` | claude, approved by operator 2026-09-28 |
| The 🔔 button does not show a live "armed" state; the list panel answers "did I arm it" | claude, approved by operator 2026-09-28 |
| An agent's list/kill also requires the target task to be visible to it (found reading the scope-completeness table, after the design was approved) | claude, 2026-09-28 — **not yet seen by the operator** |

## Problem

1. **An insurance watcher cannot be taken back.** A supervisor arms
   `--topic chat.<self>` on a worker in case no reply comes; the reply comes;
   the watcher still fires ~3s after the worker's PTY goes quiet and wakes the
   supervisor a second time for the same event. `supervising-workers`'
   SKILL.md records this as a rule ("an armed watcher cannot be disarmed")
   rather than as a gap.
2. **Nobody can tell what is armed.** "Did I press 🔔 on that task?" has no
   answer on any surface. `AwaitIdleResponse` carries `status` and
   `last_output_at` only (`runner/protocol/message.bgn`, `format
   AwaitIdleResponse`), so even the arming caller holds no handle to it.
3. **A reply-sink watcher outlives its caller.** `SessionMux.ArmIdleWatcher`
   (`server/session_mux.go`) starts a goroutine that returns only on fire or
   session stop. Ctrl-C on a blocking `harness-cli session await-idle` leaves
   it running until the fire, which then sends to a dead connection
   (`handleAwaitIdle`'s doc comment calls this harmless, and it is — but once
   watchers are listed it becomes a row that looks armed and delivers nowhere).

### What is NOT the problem

- **Persistence.** Watchers are in memory and a server restart drops them.
  That stays; nothing here writes a WAL record.
- **The cost of the extra wake.** The operator judged a redundant
  agentboard wake to be low-harm; this is about control and visibility, not
  about suppressing that wake automatically.
- **Arming authority.** Arming needs no capability except `--notify`, which
  needs `notify` (`handleAwaitIdle`). Unchanged.

## Scope

**In:** a server-side watcher registry; `watcher_id` on the arm response; a
`cancelled` status; two TaskControl kinds (`await_idle_list`,
`await_idle_kill`); dropping reply-sink watchers on connection teardown;
`session await-idle ls` / `kill` on CLI, TUI and WebUI; a TUI watcher modal; a
WebUI list panel; the wasm bridge; README; the `supervising-workers` skill.

**Out:** automatic disarm on reply; a live armed indicator on the 🔔 button;
persistence across server restart; any change to when a watcher fires.

## Server

### Registry

Shaped after the exec registry (`server/exec_run.go`, `removeExec` in
`server/forward_events.go`). One entry per armed watcher:

| Field | Source |
|---|---|
| `id` u64 | monotonic, never 0 |
| `taskIDHex` | `AwaitIdleRequest.task_id` |
| `sink`, `topic`, `threshold` | the request (threshold after the 2500ms default is applied) |
| `armedAt` | server clock |
| `requester` protocol.TaskID | `lookupPrincipal(connID)` at arm time — zero for the operator, which `handleAwaitIdle` already resolves |
| `clientKind`, `clientCID` | the arming connection, as `execRun` records them |
| `stop` | a channel closed by kill / conn teardown |

**Exactly one of fire, session stop, kill, or teardown ends a watcher.** Each
path calls `removeIdleWatcher(id) (*idleWatcher, bool)`; only the caller that
gets `true` acts. The watcher goroutine removes itself before invoking the fire
callback, so a kill that loses the race reports `not_found` and a fire that
loses it invokes nothing.

`ArmIdleWatcher` gains the stop channel as a parameter and selects on it
beside `m.ctx.Done()` and the tick. Returning on `stop` invokes nothing: the
kill/teardown path owns what (if anything) is sent.

### Authorization

One predicate, used by both the list and the kill:

```go
func (h *TaskHandler) idleWatcherVisibleTo(connID string, w *idleWatcher) bool {
	all, allowed := h.visibleToCaller(connID)
	if all {
		return true // the operator: principal zero
	}
	return h.lookupPrincipal(connID) == w.requester && allowed[w.taskIDHex]
}
```

Two conjuncts. **Armed by the caller** is the operator's decision and the
reason this differs from the exec and forward siblings, which bound by
visibility alone: a worker that can see its supervisor's task must not be able
to strip the supervisor's insurance on it. **Target still visible** is the
sibling rule kept: arming already required the target to be in scope
(`handleAwaitIdle`'s `inScope`), and a later `caps set` can narrow that, after
which the repo reports the task as absent everywhere else. Without it the
watcher would be the one surface still naming a task the caller can no longer
see. A kill the predicate refuses returns `not_found`, as `handleExecRunKill`
does, so the reply does not confirm that someone else's watcher exists.

Completeness tables: `await_idle_list` is `infoScoped` /
`capNone`; `await_idle_kill` is `targetGated` / `capNone` (its target is found
through the registry, as `exec_run_kill`'s is, and no capability bit is read).

No capability is added. Listing reveals only watchers the caller armed itself
(or, for the operator, everything the operator can already observe); killing
one of your own undoes an act that needed no capability.

### Connection teardown

`DropIdleWatchersForConn(connID)` sits beside `DropExecRunsForConn` and
`DropPortForwardsForConn` in `handleConnection`'s teardown. It removes
**reply-sink watchers only** whose `clientCID` is the closing connection.
`--topic` and `--notify` watchers are designed to outlive the request
(`handleAwaitIdle`: "sink=notify/board deliberately outlive the request") and
are left alone.

### Kill outcome per sink

| Sink | On kill |
|---|---|
| reply | the deferred `TaskControlResponse{AwaitIdle}` is sent now with `status=cancelled` — the blocked caller returns |
| board | nothing is published. Not waking the recipient is the purpose |
| notify | no notification is sent |

## Wire (`runner/protocol/message.bgn`)

```
enum AwaitIdleStatus:
    ...
    cancelled = 5             # killed via await_idle_kill before it fired

format AwaitIdleResponse:
    status :AwaitIdleStatus
    last_output_at :u64
    watcher_id :u64           # 0 for not_found / bad_request

format AwaitIdleListRequest:
    task_id :TaskID           # all-zero = every watcher visible to the caller

format AwaitIdleListResponse:
    stream_id :u64            # rows on their own send stream; 0 on failure

format AwaitIdleWatcherInfo:
    watcher_id     :u64
    task_id        :TaskID
    sink           :AwaitIdleSink
    topic_len      :u16
    topic          :[topic_len]u8
    threshold_ms   :u32
    armed_unix_ms  :u64
    requester      :TaskID    # zero = operator
    origin_kind    :ClientKind
    origin_cid_len :u8
    origin_cid     :[origin_cid_len]u8

format AwaitIdleKillRequest:
    watcher_id :u64

format AwaitIdleKillResponse:
    status :AwaitIdleKillStatus   # ok | not_found
```

plus `TaskControlKind.await_idle_list` / `await_idle_kill` and their `match`
arms on request and response. Rows go on a send stream, as `handleExecRunList`
does, because a row carries a topic of up to 64KiB and a control message must
fit one datagram (~1170B over UDP).

**Skew.** `AwaitIdleResponse` grows by 8 bytes, so a client and a server built
on opposite sides of this change fail to decode each other's arm response. The
format is client↔server only; the runner does not decode TaskControl.
`scripts/wire-skew-check.sh` is run anyway before landing and its output
recorded.

## Surfaces

| Surface | Arm | List | Kill |
|---|---|---|---|
| CLI | `session await-idle <task>` prints `watcher_id` in its JSON | `session await-idle ls [--task T] [--json]` | `session await-idle kill <id>...` |
| TUI | `w` / `W` result line names the id | `I` opens the watcher modal (shape of `tui/execsmodal.go`) | `x` on the selected row in that modal |
| TUI cmdline | unchanged | `session await-idle ls` | `session await-idle kill <id>...` |
| WebUI | 🔔 result names the id | panel `#await-idle-list` beside `#exec-list` | per-row kill button |
| WebUI cmdline | unchanged | `session await-idle ls` | `session await-idle kill <id>...` |
| wasm bridge | `harness.awaitIdle` returns `watcherId` | `harness.awaitIdleList(taskFilterHex?)` | `harness.awaitIdleKill(id)` |

**Freshness.** Watchers get no push subscription, like forwards and unlike
execs (`execs.status`). The WebUI panel rides the snapshot poll, as
`#exec-list` does ("Execs ride the same poll as forwards" in
`cmd/harness-webui-wasm/main.go`): one more list call per poll, the snapshot
key `idle_watchers`. The TUI modal is `ForwardsModal`-shaped — fetched on open
and after a kill, no `ApplyEvent`.

**CLI output.**

- `ls` human rows: `<id>  <task8>  sink=<reply|notify|board>[ topic=<t>]  threshold=<ms>ms  armed=<age>  by=<operator|task8>`.
  An empty list prints `no armed watchers`, not nothing.
- `kill`: the `ExecKill` shape in `cmd/harness-cli/dispatch.go` —
  every id is tried even after one fails; `killed await-idle watcher <id>` on
  stdout per success, `await-idle kill <id>: not found` on stderr per failure,
  non-zero exit if any failed.
- A blocking arm that is killed prints `{"status":"cancelled",...}` and exits
  **4**, distinct from `fired` (0) and `session_stopped` (3) so scripts can
  branch.

**Verb shape.** `session await-idle` stays a verb with a task-id positional
and gains `ls` / `kill` children. The table already has five such
verb-and-parent paths (`exec`, `forward`, `caps`, `skill`, `ssh-gateway`), all
at depth 1; this is the first at depth 2, so routing a two-word parent with
children is checked in `cli/verb` rather than assumed.

## Testing

**Server** (`server/`):

1. Fire vs kill: with the watcher at the threshold edge, exactly one of the
   fire callback or the kill's `ok` happens, over many iterations under
   `-race`.
2. Authorization: operator lists/kills an agent's watcher; agent A lists
   neither B's nor the operator's, and a kill of B's returns `not_found` with
   B's watcher still armed afterwards. An agent whose scope no longer covers
   the target does not list its own watcher on it.
3. Teardown: closing the arming connection removes its reply-sink watcher
   and leaves its board/notify watchers in `ls`.
4. Reply-sink kill: the blocked caller receives `cancelled` and its
   `watcher_id`.
5. Board-sink kill: no publish reaches the topic after the kill, including
   when the session goes idle afterwards.

**Grammar** (`cli/verb`): the existing invariant suite once the rows exist,
plus a routing test that `session await-idle ls` reaches `ls` and
`session await-idle <32-hex>` still reaches the arm.

**Surfaces:** `tui` modal test for `x` acting on the selected row only;
`make js-test` for the panel's kill button passing that row's id; a
dummy-harness run arming, listing and killing on all three UIs, including the
teardown case with a real Ctrl-C.

Verification: `make check`, `make test`, `make vet`, `make test-integration`.

## Risks

- **Two hand-written `awaitIdleStatusStr`.** One in
  `cmd/harness-cli/session.go`, one in `cmd/harness-webui-wasm/main.go`.
  `cancelled` must reach both; consolidating them onto one function is in
  scope if it is a mechanical move, because a third status added to one copy
  is exactly the item-32 failure shape.
- **An id copied out of `ls` can already be gone** (fired in between). Kill
  reports `not_found`, which is the truth; no retry semantics are added.
- **The operator's list carries every topic name any agent armed on.** This
  is the approved scope (operator sees all). An agent's list never carries
  another principal's topic.

## Completion

- [ ] Registry + `removeIdleWatcher` + stop channel on `ArmIdleWatcher`
- [ ] `idleWatcherVisibleTo`; list + kill handlers; `DropIdleWatchersForConn` in teardown
- [ ] `.bgn` changes, regenerated; `scripts/wire-skew-check.sh` output recorded
- [ ] `cli.Client` `AwaitIdleList` / `AwaitIdleKill`; CLI verbs + output + exit 4
- [ ] TUI `I` modal, cmdline, id in the `w`/`W` result line
- [ ] WebUI `#await-idle-list` panel; bridge `awaitIdleList` / `awaitIdleKill`; `watcherId` from `awaitIdle`
- [ ] README; `runner/agentskills/supervising-workers/SKILL.md` + both mirrors
- [ ] Tests above; make targets green; dummy-harness run on three UIs
- [ ] **Item 39 walked against the Surfaces table above, row by row**, and a `firing-log.md` entry written

## Surface-parity walk (1–39)

Walked 2026-09-28, before implementation.

**Input surfaces**

1. **done** — two new `VerbSpec` rows (`session await-idle ls`,
   `session await-idle kill`) in `cli/verb/table.go`, `CmdlineSurfaces:
   CLI | TUI | WebUI`, `Action: "SessionAction"`. `kill` takes a variadic
   `ArgUint` with `MinArgs: 1`, as `exec kill`.
2. **done** — no cross-flag rule is added; `ls --task` and `--json` are
   independent.
3. **done** — pointer; keys and buttons are items 4 and 6.
4. **done** — `I` in `mainKeyMap` + a `mainKeyBindings` row (`keys_test`
   enforces the pair). `I` is unbound at the main level today; the modal's own
   `x` lives in the modal key handling, as `ExecsModal`'s does.
5. **n/a** — no picker; the modal is a list with a cursor.
6. **done** — `#await-idle-list` in `webui/index.html` beside `#exec-list`,
   rendered in `main.js` the way the exec list is.
7. **done** — both rows declare `WebUIDispatch{Fn: "awaitIdleList"}` /
   `{Fn: "awaitIdleKill"}` in `cli/verb/table.go` (the dispatch map is now
   declared there, not in `main.js`, which this checklist item still names),
   and `main.js`'s `session` case routes `b.path[2]` to them.
8. **done** — `Build` does not interpret positionals beyond `ArgUint`; the
   generic `Bound` crossing suffices for the cmdline. The panel calls the two
   bridge functions directly.
9. **n/a** — not a spawn.
10. **done** — pointer; conventions are 29–31.

**Display surfaces**

11–14, 16–18, 18a, 19–23. **n/a** — no `TaskInfo` / `RunnerInfo` / snapshot
field changes. Watchers are their own listing, not a task-row field (approach
C, embedding them in `session ls`, was rejected).

15. **n/a** — no capability gates the new verbs, so `CapDescription` names
    nothing new. Checked: `cli/verb/caps.go` does not mention await-idle
    today either, and `notify`'s description is unaffected because arming
    with `--notify` is unchanged.

**Semantics axes**

24. **done** — `--task` on `ls` filters the list; it is unrelated to the arm
    verb's task-id positional. `--json` means the listing-JSON convention as on
    every other `ls`. The arm verb's `--topic` / `--notify` /
    `--threshold-ms` are not reachable from `ls` / `kill`.
25. **done** — `watcher_id` 0 is never a valid id (monotonic from 1), so
    zero means "none" without a presence bit. `AwaitIdleListRequest.task_id`
    all-zero = no filter, as `PortForwardListQuery`.
26. **n/a** — not a spawn, no resume.
27. **done** — CLI, TUI and wasm reach one `cli.Client.AwaitIdleList` /
    `AwaitIdleKill`.
28. **n/a** — in memory only; no `WALEvent`.
28a. **done** — `AwaitIdleKillRequest` / `AwaitIdleListRequest` are built in
    those two client methods only; grep the literals once implemented.

**Conventions**

29. **done** — kill results name the id and the outcome; the arm result names
    the id.
30. **done** — WebUI `appendCmdOutput`, TUI `a.cmdresult`.
31. **done** — an empty list prints `no armed watchers`; a zero threshold is
    never shown because the stored value is post-default.
32. **done** — see Risks: `awaitIdleStatusStr` exists twice and gains
    `cancelled`; consolidated if mechanical.
33. **done** — `kill` of an unknown or foreign id reports `not_found`; it
    never succeeds silently.
34. **n/a** — the TUI modal has a fixed column set, as `ExecsModal`.
34a. **done** — the WebUI control is a per-row button, as the exec list's.

**Live surfaces and the spec's own table**

38. **n/a** — no screen-rendering change.
39. **pending** — end-of-feature check; last box in Completion.

**Documentation surfaces**

35. **done** — README: the session verbs section and the TUI cmdline verb
    list gain `session await-idle ls` / `kill`; the TUI key list gains `I`.
36. **done** — `runner/agentskills/supervising-workers/SKILL.md`: replace
    "an armed watcher cannot be disarmed" with the kill procedure (the `armed`
    reply carries `watcher_id`); keep "do not arm it for a peer you asked to
    report back" as the default but state that an insurance watcher can now be
    killed when the reply arrives. Mirror to `.claude/skills/` and
    `.agents/skills/` (`TestMirrorsMatchEmbeddedSkills`).
37. **done** — this document.

**S1–S6** — **n/a**, trigger did not fire: no agent, launch env or server
addressing changes.
