# Agentboard reach: which topics a task may publish to and read — design

Status: DECIDED (operator, 2026-10-16) except the rows marked **this spec**.
D10 was conditional on the benchmark in §6, and the benchmark met it
(2026-10-16, §6 Results). §1–§8 and the amendments are built.

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
| D3 | The base set includes **contacts**: reply routes of messages handed to the task **that were addressed to it** (amended 2026-10-16, §3), kept in `taskState`, not in the ring | operator |
| D4 | The publish check is applied to the topic `resolveReplyTarget` returns, not to the topic the caller named | this spec |
| D5 | Reading needs `board_observe` only for another task's `chat.<id>` / `chat.<id>.*`, for `chat.operator`, and for `tree.<id>.*` when the caller is not a member. Every other topic stays open | operator |
| D6 | A confidential side channel is named `chat.<self>.<suffix>`; `dispatch`'s default (its own `chat.<short-id>`) is unchanged | operator |
| D7 | `tree.<id>.<suffix>`, with `<id>` the full 32-hex task id: `<id>` and its subtree read and write it; a non-member reads it only with `board_observe` and never writes it, `board_peer` included | operator |
| D8 | Membership is decided by walking the CALLER's `CreatorTaskID` chain upward, never by building the child index | this spec |
| D9 | A chain broken by a pruned task ends membership; the WAL is not consulted | this spec |
| D10 | Authorization is evaluated on every read, inside one predicate, so `caps set` and `caps set-parent` take effect without touching subscriptions. Conditional on §6, which met it: 0.010% of the reference at the acceptance scale | operator |
| D11 | A message retained while the reader was authorized is not readable once it is not | operator (follows from D10) |
| D12 | `board_peer = 0x80000`; `all` widens to `0xfffff` | this spec |
| D13 | Operator spawn surfaces that prefill caps include `board_peer`. Live tasks are not migrated: after the upgrade they are re-granted with `caps set`, when one turns out to need to publish beyond its base set | operator |
| D14 | ~~A refusal is answered as `not_permitted`, a status of its own on every refused verb~~ — **reversed** (see Amendment): a refusal is the ordinary `PermissionDenied` response naming `board_peer` or `board_observe`, the one every capability denial already uses | operator (reversal, 2026-10-16) |
| D15 | `list_retained` on a topic the caller cannot read is not refused: it lists only the rows the caller published, and the response says so (`own_only`). A sender checking whether its message still sits on a peer's topic needs no bit | operator |

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

When a read path hands a message M to task T **and M was addressed to T** —
M sits on T's own `chat.<id>` / `chat.<id>.*`, or M replies to a message T
published — the server adds M's reply route to T's contacts:
`M.ReplyToTopic` if it is set, otherwise `SelfTopic(M.FromTask)` when
`M.SenderKind` is agent. Nothing is added for an operator or server message
without a `ReplyToTopic` (such a message is already `no_reply_route`). The
"addressed to T" condition is an operator amendment of D3 (2026-10-16): as
first written, any delivered message counted, so reading a group topic — or
another task's topic with `board_observe` — let T publish to everyone who had
posted there, turning a read power into a publish power, and a busy group
topic pushed T's real correspondents out of the 64-entry bound.

- **Where:** at the single point every agent read path goes through:
  `deliveredBody` (`server/agent_taskcontrol.go`), which builds the rows wait,
  inbox, inbox_advance and read_seq return.
- **Lifetime:** `taskState`'s lifetime — it goes at `Revoke`. It does not live
  in the ring, so a message evicted by the TTL (`--agentboard-ttl`, 30 minutes
  after the topic's last publish) or by ring overflow (`--agentboard-ring`, 64)
  does not take the right to answer it with it.
- **Bound:** 64 entries, least recently added dropped first. A sender chooses
  `ReplyToTopic` freely, so without a bound one peer could grow another task's
  state without limit. An entry added again counts as just added (operator,
  2026-10-16): as first built, a re-add left it where it was, so the
  correspondent who kept writing was the one dropped first. A publish through a
  contact does not refresh it; only a message addressed to T does.
- **Visible (operator, 2026-10-16):** the task reads its own list with
  `agent contacts` (newest first, no bit), and the operator reads every task's
  on `board subscribers` (`board_observe`) in the CLI, TUI and WebUI. Before
  this nothing showed it, so an operator deciding whether a task needs
  `board_peer` could not see what it already reached.
- **Forgetting (operator, 2026-10-16):** `board clear-contacts <task>
  [--topic T]` drops one contact, or every contact the task holds. Without
  it, nothing but a server restart removed one: `caps set` taking
  `board_peer` away left every route a peer had opened by writing first, and
  a restart costs every task its board. It is gated on `purge` — it removes
  another task's state, the class of `board retract` — and, because it names a
  task, on that bit's scope: a task outside it answers `not_found`, the same
  answer as one the board holds no state for. A forgotten route opens again
  the next time someone addresses the task from it.
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
| `Subscribe` (`:209`) | adds the pattern | yes, so the caller is denied (`board_observe`) instead of getting a silent empty subscription |
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

### Results (2026-10-16)

Host: Intel N100, 4 cores, load average 1.6–2.6 during the run. Commands:

```
go test ./agentboard/ -run '^$' -bench 'BenchmarkReachBase' -benchmem -count 6 -benchtime 300ms
go test ./server/     -run '^$' -bench 'BenchmarkReach'     -benchmem -count 6 -benchtime 200ms
```

Medians of six. The benchmarks are `agentboard/reach_bench_test.go` and
`server/reach_bench_test.go`.

**How it was measured, and where that differs from the list above.** The
predicate does not exist yet, so the server benchmark carries a prototype of
§5's read check and §2's publish base set. It reads the real `TaskStore`: its
read lock, its hex-keyed map, and one hex encoding per hop. It is not wired
into `Board.Send`. Instead the cost with it in place is composed: the board
baseline, plus one check per subscriber on a publish, plus one per subscribed
topic on a read. That composition leaves out the candidate slice that `Send`
would gain (§5, Lock order). The read baseline is `Inbox`, not `InboxAdvance`:
it runs the same pattern iteration, and it is idempotent, so it needs no
republishing between iterations. The publish base-set check was not in the list
above and was added, because it runs once on every publish.

| Path | Measured |
| --- | --- |
| `Send` fan-out today, S = 1 / 10 / 100 | 0.86 / 2.52 / 21.2 µs |
| `Inbox` today, R = 1 / 64 | 0.62 / 18.4 µs |
| read check, free-form topic | 7–9 ns, any N, d |
| read check, `chat.*` (needs a caps lookup when not self) | 0.13–0.32 µs |
| read check, `tree.` member at depth d = 1 / 4 / 16 | 0.09–0.12 / 0.37–0.49 / 1.6–2.2 µs |
| read check, `tree.` non-member at d = 1 / 4 / 16 | 0.23–0.32 / 0.56–0.70 / 1.9–2.1 µs |
| the same, 4 goroutines (`RunParallel`), N = 100 / 10000, d = 4 | 0.51 / 0.51 µs |
| membership via `childIndex`, N = 100 / 1000 / 10000 | 43 µs / 342 µs / 3.78 ms |
| publish base set, destination is the creator, N = 100 – 10000 | 0.30–0.46 µs |
| publish base set, destination is a child, N = 100 / 1000 / 10000 | 1.5 / 15 / 144 µs |
| publish base set, miss (scans every task), N = 100 / 1000 / 10000 | 2.1 / 27 / 269 µs |

**Reference.** Thirty `harness-cli agent send` calls from a bash task on a
dummy harness on the same host, each timed with `date +%s%N` around the call:
median **88.6 ms** (minimum 74, p90 120). That covers process start, dial, PSK
handshake and the publish.

**Verdict: met.** The worst case at the acceptance scale (N = 100, d = 4,
S = 10) is a publish whose base-set check misses (2.1 µs), fanned out to ten
subscribers on a `tree.` topic none of them belongs to (10 × 0.70 µs). That is
9.1 µs, **0.010%** of the reference. The largest cell swept (N = 10000, d = 16,
S = 100: 269 µs + 100 × 2.1 µs) is 0.48 ms, 0.54%. So **10% is not reached
inside the swept range**. Extrapolated, not measured: 8.9 ms takes about
3 × 10⁵ tasks through the base-set scan, or about 4 × 10⁴ subscriber-hops of
tree walks per publish.

What the numbers also say:

- **D8 holds by three orders of magnitude.** The child index costs
  43 µs–3.8 ms per check, against 0.4 µs for the walk at the same depth, and it
  grows with N where the walk does not.
- **The one term that grows with N is the base set's child lookup** (§2, "every
  live task C whose CreatorTaskID is T"), which scans the store: about 27 ns per
  task. It is far inside the budget at every size measured. If it ever matters,
  a creator → children map kept by `TaskStore` makes it a lookup, at the price
  of a second structure that `SetParent` and prune must keep in step.
- **Relative to the server's own work the cost is not small.** At S = 10 the
  check adds 9.1 µs to a 2.5 µs fan-out. It is negligible only against the
  end-to-end cost, which is the criterion this section set.
- **No lock contention showed.** Four goroutines on one store ran each check in
  0.51 µs against 0.37–0.49 µs single-threaded. Concurrent writers to the store
  were not part of the run.

## 7. Wire

- `Capability.board_peer = 0x80000`; `all = 0xfffff` (D12). The `all` literal
  has to widen for the same reason as for `forward_tap`: operator connections
  are handed `Capability_All`.
- `board_observe`'s comment in `message.bgn` says it is "not needed to send, to
  subscribe, or to read your own inbox". It is rewritten: it is now needed to
  read another task's direct messages, by any verb.
- Refusals are the existing `PermissionDenied` response (D14, as reversed):
  no status is added to any format. This item used to add `not_permitted` to
  `SendStatus`, `SubscribeStatus` and `BoardStatus`, and a `WaitStatus` to
  `AgentWaitResponse`. `list_retained` filters to the caller's own rows instead
  of refusing (D15), and `AgentListRetainedResponse` gains `own_only :u1` to
  say so. That changes the response's length, and how an old client decodes a
  longer response is not relied on: the order is server first, then clients,
  as for every response change in this repo.
- Inbox and inbox_advance get no status. They return the authorized rows.
- Contacts (§3, Visible): `TaskControlKind.agent_list_contacts` (appended, no
  bit) answers with a `ContactsBody` on a stream, and `BoardSubscriberRow`
  gains `contacts :[contacts_len :u8]BoardContact`. The row change lengthens
  the `board subscribers` body, so the order is again server first.
- Forgetting (§3): `TaskControlKind.board_contacts_clear` (appended), request
  `{task, topic}` with an empty topic meaning all, response `{status, removed
  :u8}`. `requiredCap` maps it to `purge`, and `kindTargetClass` marks it
  `targetGated`.
- `scripts/wire-skew-check.sh` is run.

**Interaction with the delivery-row size defect.** Wait, inbox, inbox_advance
and read_seq put every `DeliveredMessage` inline in one task-control response,
with no row limit (`server/agent_taskcontrol.go:226-299`). Measured by encoding
`TaskControlResponse{AgentInboxAdvance}` with N rows (topic `chat.70fbad4a`,
hostname 7 bytes, profile `claude`, no reply route): 89 bytes a row, **1169
bytes at 13 rows** — the UDP budget is about 1170. With `tree.<full-id>.build`
as both topic and reply route: 162 bytes a row, over budget at 8 rows. Agents
use whichever transport their runner connected on (`runner/servercandidates.go:28`),
UDP included. That was an existing defect. It was reproduced in `scripts/netem-lab`
and fixed in its own change, landed as `33d2d885`
(`2026-10-16-board-row-responses-streamed-design.md`). The rows now travel
on a stream, so D7's longer topics no longer move any datagram limit.

## 8. Surfaces

- `board_peer` is parsed, printed and offered wherever capabilities are:
  `cli/verb/caps.go`, the CLI, TUI and WebUI caps editors and spawn forms, and
  the wasm bridge. The plan walks `surface-parity-checklist` item by item.
- D13 (as amended below): no surface prefills caps, so there is no prefill
  to change. A task, new or already running, reaches only its base set until
  `caps set <id> --caps …` adds `board_peer`. A top-level task's base set has
  no creator entry, so peers in other trees stop reaching each other until
  then.
- The `harness-cli` skill (source: `runner/agentskills`, plus its mirrors) gains
  the namespaces of §1, the confidential-channel convention (D6), and what a
  `board_peer` / `board_observe` denial means.
- Contacts are printed on each `board subscribers` row: `contacts=a,b` in the
  CLI (`[]` in `--json`), a wrapped line under the row in the TUI and WebUI
  modals, `-` when there are none. The TUI and WebUI reach a task's row
  through the subscribers of its `chat.<id>`. The task detail views do not
  show contacts: they render the `ls` snapshot, which comes from the task
  store, and contacts live on the board.
- Forgetting is on the same view. The TUI subscribers view lists one contact
  per line under its task and moves a cursor over both kinds of line; `x`
  forgets the highlighted contact, or every contact on a task line. The list
  is windowed to the modal's height. The WebUI puts × on each contact and
  「全消去」 on each row, behind `confirm` as its other destructive buttons are;
  the TUI asks nothing, as its `w` / `X` do not. All three print
  `cli.ClearContactsLabel`.

## Amendment 2026-10-16 — found while planning

**D13's first half described a surface that does not exist.** It said
"operator spawn surfaces that prefill caps include `board_peer`". None
prefills anything: the TUI's `sessionCaps` starts at `Capability_None`
(`tui/app.go:370`), the WebUI's `spawnCaps` at 0 (`webui/static/main.js:322`,
reset at `:3682`), and `ParseCaps("")` is `none` (`cli/verb/caps.go:275`), so
every spawn on every surface starts with no bits. What D13 decided still holds,
and with no prefill it means the following. A task — top-level or not — holds
`board_peer` only when the operator names it, at spawn or with `caps set`, and
the operator decided to do exactly that when a task turns out to need it
(2026-10-16). §8's sentence about prefills is replaced by this one.

**Decisions the spec left open, taken by the plan** (`this spec` provenance):

- A `Board` with no reach source set enforces nothing. The server always sets
  one in `SetBoard`, and a test asserts it does. The unit tests of the ring and
  delivery mechanics construct bare boards and stay valid.
- With the source set, a task the store does not know is treated as holding no
  bits and having no creator, so it fails closed. A helloed agent always has a
  record (its ticket is registered at assignment), so this only bites test
  fixtures that register a ticket without creating the task. Those are moved
  to real records.
- **D15, operator (2026-10-16): `list_retained` on an unreadable topic returns
  the caller's own rows.** The `harness-cli` skill relies on a sender looking
  at a peer's topic with `agent retained` to see whether its own message is
  still there: a purge-cap retract makes it vanish from "your `agent retained`
  and the peer's inbox", and the reply-retire section warns not to read its
  absence as an ack. Refusing the read would end that. Rows whose `from_task`
  is the caller are a subset of what the caller itself published, so returning
  them exposes nothing. `AgentListRetainedResponse` gains `own_only :u1` so a
  filtered list is never read as the topic's whole ring, and `list_retained`
  therefore never answers `not_permitted`. D14's `BoardStatus.not_permitted`
  is dropped from §7.
- **D14 reversed, operator (2026-10-16): refusals use the unified denial.**
  D14 gave each refused verb a `not_permitted` status of its own. Every other
  capability refusal in the harness is one response: `denyTaskControl`
  (`server/task_handler.go:320`) sends `PermissionDenied{requested_kind,
  required_cap}`, and the client turns it into `*cli.CapabilityDeniedError` in
  `RoundTripTaskControl` and `TaskControlWithPayload` (`cli/client.go:306-318`,
  `:384-393`). Every surface therefore renders it one way, and the
  `harness-cli` skill states that there is one denial wording. Refusals that
  depend on the request rather than on its kind already go this way:
  `await-idle --notify` needs `notify` while `await-idle` itself needs nothing
  (`server/await_idle_handler.go:87`), and file transfer picks its cap by
  direction (`task_handler.go:479`, `:645`). A reach refusal has the same
  shape. D14 was written without checking that sibling, which planning should
  have done. So: send answers `PermissionDenied` with `board_peer`, subscribe
  and wait answer it with `board_observe`. `SendStatus`, `SubscribeStatus`
  and `AgentWaitResponse` keep their pre-reach shape, and there is no
  `WaitStatus`. The cost, accepted as it is for `await-idle`: the line reads
  "AgentSend requires capability board_peer", which is true of this topic and
  not of every send. `agent send` appends which topic was out of reach and what
  is reachable. `own_only` (D15) stays: it is a filter, not a refusal.
- `read_seq` keeps answering `not_found` for an unauthorized topic, not a
  denial.

**Two publish paths the first implementation left open, found in review
(this spec, 2026-10-16):**

- **A declared `reply_to` must be publishable by its sender.** §3 makes a
  delivered message's reply route a contact of whoever reads it, the sender
  included. So a task with no bits could send its own `chat.<id>` a message
  with `--reply-to chat.<victim>` (or any free-form name), read it, and hold
  the route as a contact: `board_peer` bypassed by one task alone. `agent send`
  now refuses a `reply_to` the sender could not publish to, with the same
  `board_peer` denial, so a contact is never wider than what its asker could
  reach. The consequence a confined task meets is that `--reply-to rr.dec-019`
  needs `board_peer`, while `chat.<self>.<suffix>` needs nothing. That is D6's
  convention.
- **`await-idle --topic` publishes, so it obeys reach.** Its board sink posts
  a `session_idle` row stamped with the requester and wakes the topic's
  subscribers. It was gated only by the target's visibility, which a task
  always passes for itself. It now refuses, at arm time, a topic its
  requester could not publish to (`board_peer`). An operator connection, which
  has no principal task, is exempt like the rest of the operator face. Its schema comment explains why: a distinguishable refusal
  answers "does seq N exist?" for every seq. D14's list (send, subscribe,
  list_retained, wait) never named it.
- `HasChild` scans the store, as §6 measured. No children index is added.

## Out of scope

- Topic ownership or per-topic access lists — groups that span spawn trees
  stay on free-form names, readable by everyone and writable with `board_peer`.
- Confining group topics outside `tree.` (Rd). If wanted, it is a separate read
  bit, not `board_observe`: `board_observe` also reads direct messages.
- Fixing the delivery-row size (§7) — its own change, landed.
- The operator face (`board read` / `board send` / …), whose gates are unchanged.
