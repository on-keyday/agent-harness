# Board membership — subscribing a task, or putting it on a topic, from outside it — Design

Date: 2026-10-31

Four verbs act on ANOTHER task's place on the board. The operator can run them
on any task; a task can with the new bit `board_member`, within bounds that
keep it from handing out anything it does not hold itself (§3.6):

- `board subscribe <task> <topic>` / `board unsubscribe <task> <topic>` — what
  `agent subscribe` does, done for the task. Grants nothing: only a topic the
  task can already read. Replies to its posts keep going to its `chat.<id>`
  (broadcast request, unicast response).
- `board join <task> <topic>` / `board leave <task> <topic>` — makes the task a
  member of one topic: subscribed, and allowed to read and publish there
  whatever the topic's namespace would otherwise allow it, with replies to its
  posts staying on the topic (a timeline).

---

## 1. Problem

Getting an agent onto a shared topic takes two manual steps today, both
measured on the live board on 2026-10-31 with `beta.sns` and a `sandbox-pi`
task holding only `exec_view`:

1. **Subscribing it.** Only the task itself can subscribe (`agent subscribe`).
   The operator has to enter the task's shell and run the command there.
2. **Letting it post.** A free-form topic needs a *global* `board_peer` to
   publish to (`agentboard/reach.go` `publishableFor`), which also opens every
   other task's `chat.<id>`. The workaround that worked was to send the task a
   message on its own `chat.<id>` carrying `--reply-to beta.sns`, so that the
   reply route became the task's contact (`NoteDelivered`). That takes one
   addressed message per participant, and the contact is lost on a server
   restart and can be pushed out by 64 newer contacts.

A `tree.<X>.*` topic is out of scope: it is X's lineage's own channel, and a
task outside that lineage stays out (`set_parent` is the way in, D16).

## 2. Goals, non-goals

Goals:

- One action — by the operator, or by a task holding `board_member` within its
  bounds — makes a task a member of one topic: subscribed, and allowed to read
  and publish there. One action undoes it.
- Works for free-form topics and future `sns.*` ones. On `tree.<X>.*` it is
  allowed only for a task already in X's lineage, where it adds nothing but
  the reply-route default (D16).
- Membership of a **retained** topic survives a server restart, like the topic.
- CLI, TUI and WebUI.

Non-goals:

- `chat.*` topics. Direct messages stay between their parties (§3, D3).
- Persisting membership of a topic that is not retained.

## 3. Decisions taken

`Decided-by` is provenance; `author` rows were proposed by this task and not
explicitly ruled on.

| # | Decision | Decided-by |
|---|---|---|
| 1 | The operator can subscribe a task to a topic, so nobody has to enter the task's shell | operator, 2026-10-31 |
| 2 | Gated on a new bit `board_member` from the start, not operator-only: a moderator agent gathering participants is the expected next use. Operators hold it through `all`; a task's use is bounded (§3.6). Replaces an operator-only first draft | operator, 2026-10-31 |
| 3 | Refused on `chat.*` | author |
| 4 | Membership is its own per-(task, topic) set consulted by both `readableFor` and `publishableFor`, not a contact: a contact grants no read, loses to the tree check, is LRU-evicted at 64 and is cleared by `board clear-contacts` | author |
| 5 | Membership is keyed by task id at the board level, not on `taskState`, so it survives `Revoke` (task finished) and a resume on another runner | author |
| 6 | A retained topic's members are written to its store log and restored on load; members of other topics are forgotten on restart, as subscriptions are | author |
| 7 | One request kind `board_membership` carrying the operation (subscribe / unsubscribe / join / leave), not four kinds | author |
| 8 | `leave` removes the membership AND the subscription; it does not touch contacts | author |
| 9 | On a topic that has members, a message a member publishes there without `--reply-to` gets the topic itself as its reply route, so `--in-reply-to` answers stay on the topic. Everywhere else the default stays the sender's `chat.<id>`. A CLI that silently added `--reply-to <topic>` to every `--topic` send was rejected: it changes every existing shared-topic use | operator, 2026-10-31 (author's proposal) |
| 10 | Broadcast request with unicast response stays possible on every topic: an explicit `--reply-to` always wins, so a member asking on a joined topic for private answers sends `--reply-to chat.<own id>` | operator, 2026-10-31 |
| 11 | Membership is a record separate from the subscription: a subscription is self-service and grants nothing, membership is made only through `board_membership` (the operator, or a bounded `board_member` holder) and grants read/write on the one topic. `join` creates both; `leave` removes both | operator, 2026-10-31 (author's proposal) |
| 12 | A member's own `agent unsubscribe` of the topic also ends its membership (persisted on a retained topic), so leaving is self-service: it only narrows the task's reach. Otherwise a re-registration after a restart would subscribe it again against its will | operator, 2026-10-31 |
| 13 | The operator can also subscribe / unsubscribe a task without membership: a shared topic used for broadcast request / unicast response wants subscribers, not members | operator, 2026-10-31 |
| 14 | A subscription made through `board subscribe` is forgotten on restart like a self-made one; only membership (which grants) is persisted, and only on a retained topic | author |
| 15 | `board subscribe` is bounded by the TARGET task's own reach: it passes the same `readableFor` check as that task's own `agent subscribe` would, so it never lets a task read what it could not subscribe to itself (free-form topics: anyone, as today; another task's `chat.<id>`: only with `board_observe`; `tree.<X>.*`: only inside X's lineage; `chat.operator`: never). Of these four verbs only `join` widens reach, and by one topic; the existing ways are wider (`board_observe` through `caps set`: every other task's direct topics; `set_parent`: a whole `tree.<X>.*`) | operator, 2026-10-31 |
| 16 | `join` never crosses a `tree.<X>.*` lineage boundary: on a tree topic the target must already be in X's lineage, else `not_joinable`. A task outside the lineage is brought in by `set_parent`, which says what it does. (An earlier draft let membership override the tree rule; that came from this task's own framing of a moderator gathering outside agents, not from a requirement, and it made a mistyped id able to open a lineage's private channel to a sandbox agent.) | operator, 2026-10-31 |

### 3.1 Interface

```
harness-cli board subscribe   <task> <topic>
harness-cli board unsubscribe <task> <topic>
harness-cli board join        <task> <topic>
harness-cli board leave       <task> <topic>
harness-cli board subscribers [<topic>]       # a member's topic reads joined:<topic>(shown= pending=)
```

`<task>` is parsed as `board clear-contacts` parses it. Without `board_member`
the request is refused with the ordinary capability denial. Statuses: `ok`,
`not_found` (no such task, or — for a task caller — one outside its
`board_member` scope, indistinguishably, as `board clear-contacts` does),
`not_readable` (subscribe: the target cannot read the topic — subscribe grants
nothing), `not_joinable` (join: empty name, `chat.*`, or `tree.<X>.*` for a
target outside X's lineage), `not_publishable` (join, task caller: the caller
cannot publish to the topic itself).
`board subscribe` refuses `chat.operator` as `agent subscribe` does.

### 3.2 Board semantics

- `Board.Join(tid, topic)` records `(tid, topic)` and adds `topic` to the
  pattern set of every `taskState` held under `tid` (one per runner).
  `Board.Leave(tid, topic)` removes both.
- `RegisterTask` (task assigned, and re-adoption after a restart) adds every
  topic the task is a member of to its patterns, as it adds `chat.<id>`.
- `agent unsubscribe` of a topic the task is a member of also calls
  `Board.Leave` for it (D12): the task leaves the topic itself, and on a
  retained topic the `member … on:false` record is written, so it is not
  re-subscribed after a restart.
- `readableFor(self, topic)` and `publishableFor(self, …, topic)` return true
  when `(self, topic)` is a membership. Since `join` refuses `chat.*` and
  refuses `tree.<X>.*` for a task outside X's lineage (D16), a membership can
  only exist where it widens a free-form (or `sns.*`) topic; on a tree topic
  the lineage rule already allowed it.
- A member gets wakes and hook delivery like any subscriber. Nothing about
  `--exclude-self` changes.
- **Reply route default (D9, D10).** When a task publishes to a topic it is a
  member of and names no `--reply-to`, the server records the topic as the
  message's `reply_to_topic`, exactly as if the sender had passed it, so
  `resolveReplyTarget` routes `--in-reply-to` answers back to the topic. A
  sender that names `--reply-to` (its own `chat.<id>` for a private answer,
  or anything else) keeps it. Publishes to topics with no members, and by
  non-members, are unchanged: the default stays the sender's `chat.<id>`.
  The operator's composers (TUI, WebUI) start their reply-to field at the
  topic itself on a topic with members, and at `chat.operator` elsewhere, as
  today. Open: a `--reply-to-self` shorthand for `--reply-to chat.<own id>`
  (author leans no: one more flag for what `--reply-to` already says).

### 3.3 Persistence

`FileStore` gains a log record `{"t":"member","task":"<hex>","on":true|false}`.
`Board.Join` / `Leave` on a retained topic append it; `StoredTopic` gains
`Members []protocol.TaskID` (the last record per task wins); `AttachStore`
restores them before any task registers. `ClearRetain` and expiry drop the log
and with it the persisted membership; the in-memory membership stays until
`leave` or restart.

### 3.4 Wire

```
TaskControlKind: board_membership (appended; requiredCap board_member)
enum   BoardMembershipOp:     subscribe, unsubscribe, join, leave
format BoardMembershipRequest:  task :TaskID, topic_len :u16, topic, op :BoardMembershipOp
enum   BoardMembershipStatus: ok, not_found, not_readable, not_joinable, not_publishable
format BoardMembershipResponse: status :BoardMembershipStatus
Capability: board_member = 0x2000000 (appended, default-off); all = 0x3ffffff
```

`SubscriptionPattern` (the `board subscribers` row) gains `joined :u1` so the
operator can tell a membership from a subscription the task made itself.

### 3.5 Surfaces

- CLI as §3.1.
- TUI: in the subscribers view of a topic, `S` / `J` open a task-id editor
  (subscribe / join); `U` / `L` act on the highlighted task line
  (unsubscribe / leave). Not `j` (a scroll key) and not `x` (already "forget
  contact" in that view). Member lines show `joined`.
- WebUI: the topic's subscribers panel gains "購読させる" and "参加させる",
  picking from the task checklist the spawn form uses (`buildTaskChecklist`),
  and on each task row "購読を外す" (or "外す" for a member, which leaves).
- Skill text: a member may have been subscribed by the operator rather than by
  itself. On a group topic it posts with `--exclude-self` AND `--reply-to
  <topic>`: without the latter, a reply to the post lands on the poster's own
  `chat.<id>` as a direct message instead of on the topic (seen on `beta.sns`,
  2026-10-31: a peer's answer to a timeline post arrived as a DM).

### 3.6 The bit and its bounds

`board_member` is task-scoped like `board_contacts_clear`'s use of
`board_purge`: the target task must be in the bit's target set (default
subtree, so a moderator manages the participants it spawned; `--scope-for
board_member=ids:<task>` reaches an agent outside it). For a task caller:

| op | also required |
|---|---|
| subscribe / unsubscribe | the target can read the topic itself (D15) |
| join | the CALLER may publish to the topic (`Publishable` for the caller), so it never gives write where it has none; plus the namespace rules every caller gets (`chat.*` refused, `tree.<X>.*` only inside X's lineage, D16) |
| leave | nothing beyond scope: it only narrows |

An operator caller is bounded by the namespace rules only. Under these bounds
a task with the bit hands out read only where the target could already read,
and write only where the caller itself can write — no route to widen its own
reach, directly or through a child. `cli/verb/caps.go`: ordering after
`board_persist`, `CapDescription`, audit tier (author: mid), scope kind
task-scoped. Bits used: 27 of 32.

## 4. Security notes

- Membership widens one task's reach by exactly one topic. The operator can
  grant it anywhere the namespace rules allow; a task holding `board_member`
  only where it can itself write, and only to tasks in its scope. It never
  opens `chat.*`.
- `join` cannot open a `tree.<X>.*` topic to a task outside X's lineage, so a
  mistyped task id cannot hand a lineage's private channel to an outsider.
- `board subscribers` shows membership (`joined`), so a widened reach is
  visible to the operator, as contacts are.

## 5. Testing

- Operator subscribe: the task is subscribed and gains no write (a send to the
  free-form topic is still refused); `not_readable` on another task's
  `chat.<id>`; unsubscribe removes it.
- Join a `sandbox`-like task holding no bits to a free-form topic: it is
  subscribed, `agent send --topic` there succeeds, `agent send` to another
  free-form topic is still refused.
- Join a task outside X's lineage to `tree.<X>.sns`: `not_joinable`, nothing
  changes. Join a task inside X's lineage: ok, and its posts there default
  their reply route to the topic.
- `chat.*` refused. A task WITHOUT `board_member`: capability denial. A task
  WITH it: a target outside its scope → `not_found`; join on a free-form topic
  the caller cannot publish to (no global `board_peer`) → `not_publishable`;
  join on its own `tree.<self>.sns` for its child → ok; subscribe of its child
  to a topic the child cannot read → `not_readable`.
- WAL replay of a task recorded with the old `all` mask (`0x1ffffff`) does
  not hold `board_member`.
- Leave: unsubscribed and refused again.
- A member's own `agent unsubscribe`: membership gone (refused again), and on
  a retained topic it stays gone across a restart.
- Retained topic: join, restart the board with the store, re-register the
  task → subscribed and allowed again; non-retained topic → forgotten.
- Live: CLI, TUI and WebUI each driven once (dummy harness, real PTY,
  Playwright), including a member that had never subscribed itself.
