# Agentboard reach: which topics a task may publish to and read — design

Status: DECIDED (operator, 2026-10-16) except the rows marked **this spec**.
D10 is conditional on the benchmark in §6; nothing past §6 is built until it
has a result.

## Problem

An agent's board access is gated on nothing but a completed hello. On the agent
face, `send`, `subscribe`, `wait` and `list_retained` take no capability, and
`read_seq` checks only that the caller subscribes to the message's topic
(`server/agent_taskcontrol.go:310`) — which it can do for any topic. A task
spawned with every other bit withheld can therefore:

- **S2** publish to any topic, waking every subscriber of it;
- **S3** put instructions in front of any agent, including one holding far more
  authority than itself — the defence today is the recipient's own judgement;
- **S4** hand data to any agent that has the network egress or file access it
  was denied. `forward_*` / `file_*` confine the task itself, not what it can
  ask a peer to do;
- **R1** read any other task's `chat.<short-id>` by subscribing or waiting, and
  `chat.operator` by waiting (`agentboard/board.go:215-218` refuses only the
  subscription).

S1 (impersonation) is not open: the server stamps `from_task`, and publishing
as the operator needs `board_send`. R2 (taking a delivery away from its
recipient) is not open: the delivery mark is per (task, topic).

**S3 and S4 are what this design is for** (operator). R1 is closed only for
direct messages (D5); group topics stay readable.

**Why the earlier deferral no longer holds.** `2026-06-20-harness-cli-capabilities-design.md`
deferred both lineage-scoped send and subscribe gating because "topic↔task
binding is convention, not protocol". The server now owns that binding itself:
it computes `SelfTopic(tid)` (`agentboard/ids.go:31`), seeds that subscription
when a task registers (`agentboard/board.go:100`), and routes replies to
`SelfTopic(parent.FromTask)` (`server/agent_handler.go:131`).

**What this does not reopen.** `2026-08-23-agentboard-delivery-mark-design.md`
decided that topic ownership is not added. Nothing here stores an owner: every
check below is computed from the topic NAME and facts the server already holds
(task ids, `CreatorTaskID`, capabilities).

## Decisions taken

**operator** = chosen in conversation (2026-10-16); **this spec** = chosen by
the author while writing — those are the rows worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Confine publishing (S2–S4); confine reading only for direct messages (R1 partially) | operator |
| D2 | A task publishes without any bit to its **base set** (§2); anything else needs the new bit `board_peer` | operator |
| D3 | The base set includes **contacts**: reply routes of messages handed to the task, kept in `taskState`, not in the ring | operator |
| D4 | The publish check is applied to the topic `resolveReplyTarget` returns, not to the topic the caller named | this spec |
| D5 | Reading needs `board_observe` only for another task's `chat.<id>` / `chat.<id>.*`, for `chat.operator`, and for `tree.<id>.*` when the caller is not a member. Every other topic stays open | operator |
| D6 | A confidential side channel is named `chat.<self>.<suffix>`; `dispatch`'s default (its own `chat.<short-id>`) is unchanged | operator |
| D7 | `tree.<id>.<suffix>`, with `<id>` the full 32-hex task id: `<id>` and its subtree read and write it; a non-member reads it only with `board_observe` and never writes it, `board_peer` included | operator |
| D8 | Membership is decided by walking the CALLER's `CreatorTaskID` chain upward, never by building the child index | this spec |
| D9 | A chain broken by a pruned task ends membership; the WAL is not consulted | this spec |
| D10 | Authorization is evaluated on every read, inside one predicate, so `caps set` and `caps set-parent` take effect without touching subscriptions — **conditional on §6** | operator |
| D11 | A message retained while the reader was authorized is not readable once it is not | operator (follows from D10) |
| D12 | `board_peer = 0x80000`; `all` widens to `0xfffff` | this spec |
| D13 | Operator spawn surfaces that prefill caps include `board_peer`. Live tasks are not migrated: after the upgrade they are re-granted with `caps set` | this spec |
| D14 | A refusal is answered as `not_permitted`, a status of its own on every refused verb | this spec |

## 1. Topic namespaces

| Namespace | Who reads it | Who publishes to it |
| --- | --- | --- |
| `chat.<self>` (own short id) | self | base set or `board_peer` (§2) |
| `chat.<id>.<suffix>` | `<id>` (by short id) | base set or `board_peer` |
| `chat.operator` | `board_observe` holders (wait); subscribe stays refused | everyone (it is in every base set) |
| `tree.<full-id>.<suffix>` | `<full-id>` and its subtree; others with `board_observe` | `<full-id>` and its subtree only |
| anything else | everyone | `board_peer` holders |

`chat.<id>.*` keeps the 8-hex short id so that a task's side channels sit
beside its `chat.<id>`. A short-id collision between two live tasks would give
each the other's namespace. The board already accepts that for `chat.<id>`
itself; at n live tasks the chance is about n²/2³³.

`tree.` uses the full id (D7) because it authorizes a whole subtree. Topic
lengths are `:u16` on every format in `runner/protocol/message.bgn`, so the
length fits. What it costs on the wire is in §7.

## 2. Publishing

The **base set** of task T, needing no bit:

- `chat.<T>` and `chat.<T>.*`
- `SelfTopic(T.CreatorTaskID)`, when T has a creator
- `SelfTopic(C)` for every live task C whose `CreatorTaskID` is T
- `chat.operator`
- T's contacts (§3)
- `tree.<X>.*` for every X that T is a member of (§4)

Every other topic needs `board_peer`, and no bit opens a `tree.` namespace T is
not a member of (D7).

The check runs on the topic the send will actually be written to, AFTER
`resolveReplyTarget` (D4). That is what makes a reply to a guessed seq
harmless: seqs are monotonic and easy to guess, `resolveReplyTarget` does not
ask whether the replier ever saw the parent (`server/agent_handler.go:117`),
and the route it resolves is not in the replier's contacts unless the parent
was handed to the replier. No separate "did you see the parent" check is
added.

The spawn-tree entries are evaluated at send time, so `caps set-parent`
(operator-only, `message.bgn` `set_parent`) needs no handling on this side.

## 3. Contacts

When a read path hands a message M to task T, the server adds M's reply route to
T's contacts: `M.ReplyToTopic` if it is set, otherwise `SelfTopic(M.FromTask)`
when `M.SenderKind` is agent. Nothing is added for an operator or server
message without a `ReplyToTopic` (such a message is already `no_reply_route`).

- **Where:** at the single point every agent read path goes through:
  `deliveredRows` (`server/agent_taskcontrol.go:190`), used by wait, inbox,
  inbox_advance and read_seq.
- **Lifetime:** `taskState`'s lifetime — it goes at `Revoke`. It does not live
  in the ring, so a message evicted by the TTL (`--agentboard-ttl`, 30 minutes
  after the topic's last publish) or by ring overflow (`--agentboard-ring`, 64)
  does not take the right to answer it with it.
- **Bound:** 64 entries, least recently added dropped first. A sender chooses
  `ReplyToTopic` freely, so without a bound one peer could grow another task's
  state without limit.
- **Limit, stated because it is real:** after eviction, `--in-reply-to` still
  fails with `unknown_in_reply_to`, because the parent is gone. Contacts make
  the explicit `--topic <route>` fallback pass the check; they do not make the
  reply a reply. A `dispatch` waiting for that answer will not match it.
- **Server restart:** contacts do not survive one, and neither does anything
  else in `taskState`. `agentboard` writes nothing to disk — rings,
  subscriptions and delivery marks are in memory, and seq is seeded from the
  wall clock precisely so that it keeps rising across a restart that loses
  them (`agentboard/board.go:18-28`). A re-adopted task gets its `chat.<id>`
  back from `RegisterTask` and nothing else. Persisting contacts alone would
  keep a reply right to a message the ring no longer has.

## 4. `tree.` membership

T is a member of `tree.<X>.*` when X is T, or X appears on T's ancestor chain:
start at T, follow `CreatorTaskID` until X is found, the root (zero id) is
reached, or a record is missing (D8, D9).

- **Ancestor walk, not child index:** `childIndex` (`server/capabilities.go:127`)
  lists every task on each call. The walk costs one store lookup per level.
- **Prune:** the scope code repairs prune holes from the WAL (`walChildIndex`,
  `server/capabilities.go:139-143`) because a restore has to be scope-gated.
  Board membership is a relation between live tasks, so a hole ends it instead.
- **`caps set-parent` changes membership.** Swapping worker W with its
  supervisor S makes S W's child: W leaves `tree.S.*` and S joins `tree.W.*`.
  This is the reason for D10.
- **Cycles:** `set_parent` refuses them (`would_cycle`), and the walk still
  stops after as many steps as there are tasks in the store.

## 5. Reading

Reading means subscribe, wait, inbox, inbox_advance, read_seq and
list_retained. A read of topic P by T is authorized unless P is one of the
following and T holds no `board_observe`:

- `chat.<id>` or `chat.<id>.*` where `<id>` is not T's short id
- `chat.operator` (subscription stays refused for everyone, as today)
- `tree.<X>.*` where T is not a member

**One predicate, evaluated on every read (D10):** `Board.readable(ts, topic)`.
There is no single choke point to put it in today. The board reaches a task's
subscriptions in three ways, and every entry that hands a task a message or a
wake must call the predicate:

| Entry | How it reaches the topic | Gets the predicate |
| --- | --- | --- |
| `Send` fan-out (`agentboard/board.go:283`) | `taskState.matches` | yes |
| `Wake` (`:343`) | `taskState.matches` | yes |
| `Subscribes` (`:755`), used by read_seq (`server/agent_taskcontrol.go:310`) | `taskState.matches` | yes |
| `Inbox` (`:358`), `InboxAdvance` (`:393`) | iterates `snapshotPatterns()` | yes, per pattern |
| `Wait` (`:434`), `list_retained` | reads the named topic's ring directly | yes |
| `Subscribe` (`:209`) | adds the pattern | yes, so the caller is told `not_permitted` instead of a silent empty subscription |
| `ListSubscribers` (`:906`, operator face) | `taskState.matches` | yes: its help says "only the tasks a publish there reaches", which is the authorized set |
| `anyTaskMatchesLocked` (`:200`, cleanup at `Revoke`) | `taskState.matches` | no: a topic is deleted only when empty, so the raw set can only keep more |
| `ListSubscriptions` (`:853`), and the per-task pattern list inside `ListSubscribers` (`:911`) | iterates `snapshotPatterns()` | no: they report the subscription set itself |

A test fails when a `Board` method calls `taskState.matches` or
`snapshotPatterns()` without being on this table. Without it, the next reader
is the gap. `ConnState.matches` (`agentboard/conn.go:47`) has no caller outside
tests and is deleted.

- **Subscriptions are not pruned.** A pattern T is no longer authorized for
  stays in T's set and yields nothing. Granting the bit back restores it.
  `list_subscriptions` shows the pattern unchanged. That is a known limit:
  a row there does not prove delivery.
- **Already-retained messages (D11)** fall under the same predicate. Losing
  `board_observe` or membership hides what was retained before.
- **Layering:** `agentboard` does not know the task store. The server injects
  the predicate's two inputs: a function from task id to (creator, ok) and a
  function from task id to capabilities.
- **Lock order:** the predicate takes the task store's lock, so it runs with
  neither `b.mu` nor `taskState.mu` held. `Send` and `Wake` call `matches`
  inside `b.mu` today (`:281-286`, `:341-346`). They collect candidates under
  the lock and filter them with the predicate after `b.mu.Unlock()`.

## 6. Benchmark that gates D10

D10 adds work to every publish fan-out and every read. It is adopted only if
this measurement says the cost is negligible at the scale the harness runs.

**Benchmarks** (committed as `_test.go` so the numbers can be produced again;
nothing measured in a scratch directory counts):

- `Board.Send` fan-out, with and without the predicate.
- `Board.InboxAdvance` returning R rows, with and without the predicate.
- Both again under `b.RunParallel`, to see contention on the task store's lock.
- Ancestor walk against `childIndex`-based membership, to put a number on D8.

Swept over: tasks in the store N ∈ {10, 100, 1000, 10000}, tree depth
d ∈ {1, 4, 16}, subscribers per topic S ∈ {1, 10, 100}, rows R ∈ {1, 64}.
Reported as ns/op and allocs/op, with the `go test -bench` command line that
produced them.

**Reference:** one `harness-cli agent send` against `scripts/dummy-harness.sh`,
end to end. That is the cost an agent turn pays for a publish today.

**Acceptance:** at N≈100, d≤4, S≤10, the predicate adds under 1% of the
reference. The spec records the scale at which it reaches 10%; that is the
documented limit of D10. If acceptance fails, D10 is reopened with the numbers,
and the alternative — re-evaluating subscriptions when `caps set`,
`caps set-parent` and prune change their inputs — comes back for decision.

## 7. Wire

- `Capability.board_peer = 0x80000`; `all = 0xfffff` (D12). The `all` literal
  has to widen for the same reason as for `forward_tap`: operator connections
  are handed `Capability_All`.
- `board_observe`'s comment in `message.bgn` says it is "not needed to send, to
  subscribe, or to read your own inbox". It is rewritten: it is now needed to
  read another task's direct messages, by any verb.
- `not_permitted` is appended to `SendStatus`, `SubscribeStatus` and
  `BoardStatus` (list_retained) (D14). `AgentWaitResponse` has no status, and
  `timed_out` must not carry a refusal, so it gains `status :WaitStatus`
  (`ok`, `not_permitted`) at its end. That changes the response's length, and
  how an old client decodes a longer response is not relied on: the order is
  server first, then clients, as for every response change in this repo.
- Inbox and inbox_advance get no status. They return the authorized rows.
- `scripts/wire-skew-check.sh` is run.

**Interaction with the delivery-row size defect.** Wait, inbox, inbox_advance
and read_seq put every `DeliveredMessage` inline in one task-control response,
with no row limit (`server/agent_taskcontrol.go:226-299`). Measured by encoding
`TaskControlResponse{AgentInboxAdvance}` with N rows (topic `chat.70fbad4a`,
hostname 7 bytes, profile `claude`, no reply route): 89 bytes a row, **1169
bytes at 13 rows** — the UDP budget is about 1170. With `tree.<full-id>.build`
as both topic and reply route: 162 bytes a row, over budget at 8 rows. Agents
use whichever transport their runner connected on (`runner/servercandidates.go:28`),
UDP included. Not yet seen failing live, and a dummy harness will not show it:
loopback's MTU is 65536 (the same reason a 2000-byte exec line passed there on
2026-10-10, `2026-10-10-exec-request-streamed-design.md`). This is
an existing defect, fixed in its own change
(`2026-10-16-board-row-responses-streamed-design.md`). This design does not depend on the
fix, but D7's full id brings the failure closer, so the fix should land first.

## 8. Surfaces

- `board_peer` is parsed, printed and offered wherever capabilities are:
  `cli/verb/caps.go`, the CLI, TUI and WebUI caps editors and spawn forms, and
  the wasm bridge. The plan walks `surface-parity-checklist` item by item.
- D13: wherever an operator surface prefills a spawn's caps, the prefill gains
  `board_peer`, so a newly spawned top-level task keeps today's reach. A task
  already running holds a persisted number without the bit. After the upgrade
  it reaches only its base set until `caps set <id> --caps …` adds it. A
  top-level task's base set has no creator entry, so peers in other trees stop
  reaching each other until then.
- The `harness-cli` skill (source: `runner/agentskills`, plus its mirrors) gains
  the namespaces of §1, the confidential-channel convention (D6), and what
  `not_permitted` means.

## Out of scope

- Topic ownership or per-topic access lists — groups that span spawn trees
  stay on free-form names, readable by everyone and writable with `board_peer`.
- Confining group topics outside `tree.` (Rd). If wanted, it is a separate read
  bit, not `board_observe`: `board_observe` also reads direct messages.
- Fixing the delivery-row size (§7) — its own change.
- The operator face (`board read` / `board send` / …), whose gates are unchanged.
