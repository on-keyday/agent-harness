# Sender-side retraction on the agentboard (`retract`)

Date: 2026-08-18

## Problem

- **The board retains, but nothing consumes.** A published message stays in
  its topic's ring until the ring overflows or the topic is evicted
  (`agentboard/topic.go:46-66`). The only "already handled" marker in the
  system is a client-side cursor file,
  `~/.cache/harness/agent-cursor-<task-id>` (`cli/agent/cursor.go:13-27`),
  advanced by the `UserPromptSubmit` hook (`runner/settings.go:38`). The
  server never sees it and the ring is not touched by it.

- **So a context reset resurfaces spent instructions.** The cursor survives
  the reset — it is on disk, keyed by task id — but the *content* it
  guarded lived only in the agent's context. Three paths hand the same
  messages back to a task that no longer remembers acting on them:

  1. `agent inbox --since-last` without `--commit` reads from the **prev**
     snapshot, deliberately re-emitting the batch the hook last delivered
     (`cli/agent/inbox.go:20-27,70-82`). To a reset agent that batch is
     indistinguishable from new work.
  2. `agent retained --self` followed by `agent read <seq>` — neither is
     capability-gated (`cli/agent/retained.go:17-23`,
     `server/agent_handler.go:610-620`).
  3. Any `inbox --since 0`.

  The observed failure is a task re-executing instructions it had already
  carried out.

- **Retention bounds do not solve it.** `--agentboard-ttl` defaults to 30
  minutes, but eviction is *whole-topic* and keyed on last publish
  (`cmd/harness-server/main.go:31`, `agentboard/board.go:345-357`), so a
  topic that is still being used never expires. `--agentboard-ring`
  defaults to 64 entries per topic (`cmd/harness-server/main.go:30`), which
  bounds the resurfacing but does not stop it, and pushing entries out by
  volume destroys unread messages just as readily as read ones.

- **The only destruction primitive is `purge`, and it cannot be narrowed.**
  `Capability_Purge` is a single bit (`runner/protocol/message.bgn` —
  `Capability.purge = 0x800`), enforced at `server/agent_handler.go:577-583`
  and `server/capabilities.go:34`. Neither axis of narrowing exists:
  - *By topic*: `TaskScope` bounds which **tasks** a capability may target;
    the agentboard kinds are recorded as `noTarget` precisely because topics
    have no owner (`server/scope_completeness_test.go:61-66`).
  - *By author*: nothing consults the stored `FromTask` before destroying.

  A task therefore either cannot clear anything, or can wipe every topic on
  the board. In practice the operator purges by hand, which is the cost this
  change is meant to remove.

- **Giving the RECIPIENT the authority does not work.** A `--self` form
  already exists client-side: `agent purge --self` derives
  `chat.<short-id>` from the caller's own task id
  (`cli/agent/purge.go:25,32-39`, `agentboard/ids.go:44-50`), and the server
  rejects it anyway for want of the cap. Honouring it would still be wrong:
  the reset recipient holds *no information* distinguishing a handled
  instruction from an unhandled one — that missing information is the bug
  itself. It would also let the container's owner destroy other tasks'
  messages, since `subscribe` has no ownership check
  (`server/agent_handler.go:368-379`) and the messages in a task's inbound
  topic were written by others. Authority would sit where knowledge does
  not.

- **Giving the SENDER unrestricted destruction trades one hazard for
  another.** The sender does know when its instruction is spent. But an
  agent retracts at agent speed — seconds — while a human reads the board
  asynchronously, so straightforward deletion shrinks the window for
  auditing what was said to nothing.

## Goal

Make "withdraw the message I published" a first-class operation, gated on
**authorship** rather than on a capability bit, which removes the message
from every agent-facing path while leaving it readable on the operator
surfaces for the topic's normal retention window.

Non-goals, each deliberate:

- **Not an undo of delivery.** A message already ingested into a
  recipient's context cannot be recalled. This addresses re-reading after a
  reset, which is where the observed damage occurs.
- **Not an audit log.** Board retention remains a bounded ring under a TTL.
  A durable ledger of agent traffic is a separate concern and is not
  smuggled in here.
- **No automatic retraction.** Reply-implies-retire and publish-side TTL
  were both considered and are left out of this change: each needs a
  publish-side schema field and a policy decision for multi-subscriber
  topics, and both would be built on the primitive defined here. Retract
  first, automate later or not at all.
  *(Superseded — see the Amendment at the end of this document, which adds
  reply-implies-retire and records why publish-side TTL was dropped
  outright rather than deferred.)*

## Design

### 1. Storage: a second list per topic

`agentboard/topic.go` gains a withdrawn-message list beside the live ring:

```go
type topic struct {
	mu              sync.Mutex
	name            string
	cap             int
	ring            []RetainedMessage   // live
	retracted       []RetainedMessage   // withdrawn, operator-visible only
	lastPublishedAt time.Time
}
```

`RetainedMessage` gains `RetractedAt time.Time` (zero value = live).

**Why a second list rather than a `retracted bool` on the entry.** Every
agent-facing read goes through the live ring: `Board.Inbox`, `Board.Wait`,
`Board.Retained`, `Board.ListRetained`, via `topic.since` / `topic.snapshot`
/ `topic.summary`. Moving the entry out of `ring` makes all of them stop
returning it with **no filter added at any call site**. A filter is
something that can be forgotten at one of six places, and forgetting one
reintroduces exactly the bug being fixed; a move cannot be forgotten. It
also matches the existing layering note at `agentboard/board.go:451-458` —
the board is storage, and who may see a message is decided one layer up.

**Why the withdrawn list has its own capacity.** The live ring is a FIFO
that drops its oldest entry on overflow (`agentboard/topic.go:51-54`). If
withdrawn entries stayed in it, a task that sends and retracts in a loop
would push *other senders'* live messages out of the window — erosion of
the operator's view at agent speed, by a different route than deletion. The
withdrawn list is capped independently at the same `cfg.RingN`, so retracts
cost nothing from live capacity. Worst-case retention per topic doubles;
against the shipped `--agentboard-max-topics × 64 × --agentboard-max-payload`
bound this is a theoretical figure that is nowhere near reached.

Eviction: the withdrawn list is FIFO at its own cap. TTL eviction and
whole-topic purge delete the topic outright, taking both lists. (Revoke —
the last-subscriber path — does not; see Amendment 2026-08-18b.)

### 2. Wire schema

`agentboard/agentboard.bgn` — new kinds, request, status, response:

```
enum AgentMessageKind:
    ...
    retract
    retract_response

format RetractRequest:
    request_id :u32
    seq :u64

enum RetractStatus:
    :u8
    ok
    not_found

format RetractResponse:
    request_id :u32
    status :RetractStatus
```

Both arms are appended to the `AgentMessage` match.

`runner/protocol/message.bgn` — the operator-facing rows:

```
format BoardTopicRow:
    ...
    msg_count :u16
    retracted_count :u16

format BoardMessageRow:
    ...
    received_at_unix_ms :u64
    size :u32
    retracted :u1
    reserved  :u7
    retracted_at_unix_ms :u64
```

`retracted_at_unix_ms` is 0 unless `retracted` is 1. There is no
`retracted_by`: only the author may retract, so the field would always
repeat `from_task`, and a redundant field is one that starts lying the day
the rule changes.
*(Superseded — Amendment 2026-08-28c adds a second withdrawal path, which is
the rule change this sentence anticipated; `retracted_by` and
`retracted_by_task` exist as of it.)*

`retracted_count` is kept out of `msg_count` because `msg_count` answers
"how much would a subscriber receive" — the number every agent-facing path
reports. A topic with an empty ring and a non-empty withdrawn log must not
read as if it still carried live traffic.

### 3. Server: the gate is authorship

```go
func (s *Server) agentHandleRetract(conn ConnHandle, ac *agentConn, r *agentboard.RetractRequest)
```

resolves the caller's task id from `ac.state.Identity()` and calls
`Board.RetractSeq(seq, callerTid)`, which scans the rings the way
`LookupSeq` already does (`agentboard/board.go:478-500`), matches on
`FromTask`, and moves the entry to that topic's withdrawn list with
`RetractedAt` stamped. **No capability is consulted.** The authority
argument is that the operation can only reach bytes the caller itself
wrote.

`Capability_Purge` is unchanged and still gates `PurgeRequest`. The two
verbs answer different needs: retract makes a message stop being acted on,
purge makes its bytes stop existing — the escape hatch for a payload that
must not survive at all, such as a leaked credential. Consequently
**`PurgeSeq` must search the withdrawn list as well as the live ring**, or
retracting would put a message beyond the reach of the operator's only
destruction tool.

Operator-facing handlers merge the two lists:

- `handleBoardRead` (`server/board_handler.go:76`) reads
  `Board.ListRetained(topic)` ∪ `Board.ListRetracted(topic)`, sorts by
  `Seq`, and sets `retracted` / `retracted_at_unix_ms` per row. Payload
  bytes for withdrawn messages ride the same stream as live ones: the
  operator's job here is to read what was said.
- `handleBoardTopics` fills `retracted_count`.

The board API stays visibility-neutral — `ListRetained` (live) and
`ListRetracted` (withdrawn) are separate calls, and the protocol layer
decides who sees which, per the layering already stated in
`agentboard/board.go:451-458`.

### 4. Agent CLI

```
harness-cli agent retract <seq>
```

A bare positional seq, mirroring `agent read <seq>` (`cli/agent/read.go:24-33`)
— the sender already holds the value from `SendResponse.seq`, and
`agent retained --topic <t>` lists `from_task` for recovering it later.

Not a flag on `purge`. The two verbs differ in what survives, not in what
they target, and a flag that silently changes which authority check applies
hides that difference at the call site.

### 5. Operator surfaces

Walk of the `surface-parity-checklist` items that apply; the rest are
recorded in the checklist walk in the implementation notes.

| Surface | Change |
| --- | --- |
| CLI `agent retract` | new verb + help text (`cmd/harness-cli/main.go`) |
| CLI `board read` | `#12 [retracted at=<rfc3339>]` prefix on withdrawn rows (`cli/cmd_board.go`) |
| CLI `board topics` | `retracted=N` column when non-zero |
| `cli.BoardMessage` / `BoardTopicRow` | `Retracted bool`, `RetractedAtMs uint64`, `RetractedCount int` |
| TUI board modal | withdrawn rows rendered dimmed with a `retracted` tag (`tui/board.go`) |
| WebUI board panel | same marker in the message list (`webui/static/main.js`) |
| wasm bridge | `retracted` / `retractedAtMs` in the `boardRead` result, `retractedCount` in `boardTopics` (`cmd/harness-webui-wasm/main.go`) |

Intentionally omitted: `submit` / `session new` flags, `ParseCaps` /
`ParseScope`, spawn-default state, task pickers and task detail views. None
of them address a board message; the feature adds no task field and no
capability.

### 6. Documentation

- `runner/agentskills/harness-cli/SKILL.md` (go:embed source of truth) gains
  a "withdrawing a message you sent" section, mirrored to `.claude/skills/`
  and `.agents/skills/` in the same commit.
- `README.md` agentboard section: the retract/purge split and which one
  needs a capability.

## Error handling

- **`not_found` merges "no such live message" with "not yours."** `seq` is
  board-global and consecutive, so a distinct "not yours" status would
  confirm the existence of any seq on any ring, including topics the caller
  can neither name nor read. `ReadSeqStatus.not_found`
  (`agentboard/agentboard.bgn` — `ReadSeqStatus`) merges its two cases for
  the same reason; this follows that precedent rather than inventing a
  second policy.
- Retracting an already-retracted seq answers `not_found`: it is no longer
  in a live ring. Idempotent from the caller's side.
- Retract after purge: `not_found`. Purge after retract: succeeds, because
  `PurgeSeq` searches both lists.
- A retract from a connection with no authenticated identity (zero task id)
  answers `not_found` — it matches no author.

## Rollout

The change appends enum members and appends fields to existing formats; no
layout shifts under an existing field. It is still a `.bgn` change, so:

1. `scripts/wire-skew-check.sh` before landing (Pitfall 10).
2. **Restart the server first**, then the runners. The server is on a
   different host and an old server cannot decode a new hello.

No migration shim: single-operator dogfood, and a message that fails to
retract during the skew window simply stays live.

## Testing

Unit (`agentboard/`):

- retract moves the entry: live reads (`Inbox`, `Wait`, `Retained`,
  `ListRetained`) stop returning it; `ListRetracted` returns it with
  `RetractedAt` set.
- authorship: a retract naming another task's message leaves the ring
  untouched.
- live capacity is unaffected by retracts — publish `cap` messages,
  retract them all, publish `cap` more, assert none of the second batch was
  evicted.
- withdrawn list is FIFO at its own cap.
- `PurgeSeq` removes a retracted entry; `PurgeTopic` takes both lists.

Server: `agentHandleRetract` needs no cap (a task with `Capability_None`
retracts its own message); `handleBoardRead` returns live and withdrawn in
seq order with the flag set.

E2E: a sibling of `cli/agent/purge_e2e_test.go` — send, retract, assert the
peer's `inbox --since 0` no longer carries it while `board read` still does.

Manual: dummy harness, two tasks, retract from the sender and confirm the
recipient's `inbox --since 0` is clean while the TUI and WebUI board views
still show the message marked as withdrawn.

## Decisions taken

Recorded so no reader has to guess which were deliberate:

1. Withdrawn messages live in a **separate list**, not behind a flag on the
   ring entry — filters can be forgotten at a call site; a move cannot.
2. The withdrawn list has **its own capacity**, equal to the ring's, so
   retracts never evict live messages.
3. Agents see **nothing** of a withdrawn message — not even metadata. A
   "there was something here" marker invites a reset agent to go looking.
4. **No `retracted_by`** field: only the author can retract, so it would
   duplicate `from_task`. *(Reversed by Amendment 2026-08-28c — a
   purge-capable caller can now withdraw somebody else's message, so the
   field no longer duplicates anything.)*
5. `RetractStatus` has **two members**; "not yours" is folded into
   `not_found` to avoid an existence oracle.
6. A **new verb**, not a flag on `purge`, because the authority check
   differs.
7. **No automatic retraction** in this change. *(Superseded by the
   Amendment below.)*

---

# Amendment 2026-08-18 — reply-implies-retire

## Why this returns so soon

Explicit `retract` still depends on the author remembering to call it, and
the author is an agent whose own context can be reset. When that happens
nobody withdraws anything and the recipient re-reads the instruction — the
original failure, one level up.

A reply is the one moment when "this instruction is spent" is known to
somebody who still has the context to know it. So the reply carries the
retraction.

## Rule

When a task publishes with `in_reply_to = P` and the publish succeeds, the
server withdraws `P` on its author's behalf if **all four** hold:

1. `P` is still live. An already-withdrawn, purged or rotated-out seq is
   nothing to do — not an error.
2. Its author did not set `no_retire_on_reply`.
3. `P` sits on the **replier's own** `chat.<short-id>` — it was addressed to
   them specifically.
4. The replier is not `P`'s author.

Condition 3 is the one that took a decision. A publish to a shared topic is
**never** auto-retired, however the flag is set: one subscriber answering
says nothing about whether the others have read it, and retiring it there
would destroy their unread copy. Broadcast senders retract explicitly. The
cost of the choice is the opposite surprise — a sender who set no flag and
published to a custom topic will find the message still live after an answer.
That is the direction the failure should point: a message that outlives its
answer is noise, a message destroyed before its other recipients read it is
lost work.

The retraction goes through the same authorship-gated `Board.RetractSeq` an
explicit retract uses, with the PARENT'S author as the actor. The author
authorised it by publishing without the opt-out; no new authority path
exists.

## Wire schema

`agentboard/agentboard.bgn`, appended to `SendRequest`:

```
    no_retire_on_reply :u1
    reserved :u7
```

The bit is **negative** so its zero value is the default behaviour — the
same reason `ScopeBase.subtree` is 0. A caller that sets no bits, and a
struct nobody filled in, must both mean "retire on reply".

`Board.Send` grows a variadic `...SendOption` rather than an eighth
positional parameter: it already takes seven and is called from about fifty
places, nearly all tests with no opinion about any of this.

## Agent CLI

```
harness-cli agent send --topic T --data D --no-retire-on-reply
```

Set it for a message that must survive being answered — a standing
instruction, or one whose reply is an acknowledgement rather than a
completion.

## Deliberately not surfaced to the operator

`no_retire_on_reply` is **not** carried on `BoardMessageRow`. A live
message that has been replied to can be live for three different reasons —
the flag, the point-to-point condition, or the replier being the author —
and a field naming only the first would read as the whole story. The
operator sees outcomes (`RETRACTED`), and the rule is documented.

## Publish-side TTL: dropped, not deferred

The original Non-goals deferred it alongside reply-retire. On inspection it
is not an independent axis. A per-message TTL that deletes is the existing
`--agentboard-ttl` at a different granularity, and it deletes from the
operator's view too — the opposite of what this design is for. A per-message
TTL that *retracts* is this amendment with a timer instead of a reply as its
trigger. Since explicit retract covers "no reply is coming", the remaining
value did not justify a second publish-side field, so it is dropped rather
than left open.

Worth recording because it is the actual reason the existing TTL does not
solve the original problem either: `evictExpiredTopics` keys on the topic's
LAST PUBLISH, so a topic still in use never expires and a stale instruction
inside it never ages out.

## Decisions taken (amendment)

1. Default **on**, opt-out per message. The failure mode of forgetting to
   opt in is the bug this exists to fix.
2. The bit is **negative**, so zero means the default.
3. Point-to-point only (condition 3). Shared topics are never auto-retired.
4. A self-reply never retires (condition 4).
5. The flag is **not** shown on operator surfaces.
6. Publish-side TTL is **dropped**, not deferred.

## Testing (amendment)

Unit (`server/retire_on_reply_test.go`): the point-to-point path fires and
stamps `RetractedAt`; the opt-out survives; a shared topic never fires even
so; a self-reply never fires; an unknown / already-retired parent is a
no-op, and a zero replier id matches nobody.

---

# Amendment 2026-08-18b — a topic holding withdrawn messages outlives its last subscriber

## The hole this closes

Found while verifying the base design on a live harness, twice.

`Board.Revoke` deletes topics that only the finishing task subscribed
(`agentboard/board.go`). A worker's `chat.<short-id>` is subscribed by that
one task, so the moment the worker's task ended, the topic went — **and the
withdrawn list with it**. Every instruction the worker had retracted
disappeared at exactly the point an operator has most reason to read them:
after the worker is done.

The base spec's wording ("stays visible… until it ages out with the topic")
was literally accurate and practically misleading. For the very case the
design targets — a supervisor's instruction to a worker, retired by the
worker's reply — the audit trail lasted only as long as the worker.

## Rule

In `Revoke`, a topic whose last subscriber is leaving is dropped **only if
its withdrawn list is empty**. Otherwise it stays and ages out under the
normal TTL, which is the bound an operator already lives with.

Deliberately NOT applied to `evictOldestTopicLocked` (the `MaxTopics`
pressure path): that runs when something has to go, and a rule able to
refuse every candidate would turn a full board into a permanent publish
failure.

## Decision

Narrow exemption, keyed on "is there anything to audit", not on the topic's
shape or age. A topic with nothing withdrawn in it still follows its last
subscriber out exactly as before — tested both ways, since an exemption
that quietly stopped collecting garbage would be its own defect.

## Testing (amendment b)

`agentboard/retract_test.go`: a retracted message survives its topic's last
subscriber being revoked; a topic with no withdrawn messages is still
dropped by the same Revoke.

---

# Amendment 2026-08-28c — the operator withdraws (`board retract`)

## The gap

The operator's only way to take a message out of the agents' reach was
`board purge`, which destroys the bytes — the operator's own copy included.
So "keep the history for now, but this message must stop being acted on"
had no verb: the choice was leave it live or lose it entirely. Named by the
operator, 2026-08-28: 「履歴はしばらく保持しときたいけど不都合だからこっちで
消したいって時に purge しかないの不便だな」.

The base design gave that shape to the author alone. Nothing about the shape
is author-specific — what was author-specific was the *authority argument*
(§3: the operation can only reach bytes the caller wrote), and that argument
does not have to be the only one.

## Rule

```
harness-cli board retract <topic> --seq N
```

Withdraws one message with **no authorship check**, gated on
`Capability_Purge`. Same move as an author retract — out of the live ring,
into the withdrawn list, stamped — so it leaves every agent-facing path at
once and stays readable on the operator surfaces until the topic ages out.

`--seq` is **required and must be non-zero**. Purge's "seq 0 means the whole
topic" shorthand is deliberately not inherited: this verb should not be able
to take a conversation on a mistyped flag, and there is no operator need for
a topic-wide soft withdrawal that `purge <topic>` does not already answer
(asked and answered: the operator chose seq-only, 2026-08-28).

Unlike `RetractSeq`, it takes a topic NAME instead of scanning every ring.
The caller reached the seq through `board_read`, which is per-topic, so the
scan would be work spent rediscovering something the caller already knows.

A **zero caller id is accepted here and refused by `RetractSeq`**, and the
asymmetry is the point: on the authorship path a zero id is a match against
nobody, while here it is the honest identity of an operator client, which
holds capabilities directly and has no principal task.

## Why `purge` and not a new bit

Containment, not convenience. A caller holding `purge` can already reach the
same message through `board purge --seq N` and destroy it outright; this
verb reaches nothing further and leaves more behind. A new grantable bit
would add a name to the catalog without adding a reachable state.

The cost, stated because it is real: **"may withdraw but may not destroy" is
not a grantable shape.** If that separation is ever wanted, the bit is the
change — not a flag on this verb.

Note what this means for the reader of a withdrawn row: `purge_cap` is not a
synonym for "the operator". Any task granted `purge` can withdraw another
task's message, which is why the recorded value names the *check that
passed* rather than a kind of caller.

## Reversal: `retracted_by` now exists

Base spec §2 and Decision 4 said there must be no `retracted_by` because
"only the author may retract, so the field would always repeat `from_task`".
That premise is exactly what this amendment removes, and it was written with
the failure mode in view — *"a redundant field is one that starts lying the
day the rule changes"*. This is that day, so the field arrives with the rule
rather than after it.

`BoardMessageRow` gains two fields, both meaningful only under the existing
`retracted` bit:

```
    retracted_by :RetractedBy        # author | purge_cap
    retracted_by_task :TaskID        # == from_task on the author path;
                                     # all-zero for an operator client
```

`RetractedBy` names the **authority check that passed**, not a kind of
caller, for the reason in the section above. The task id is carried as well
as the enum — the operator asked for it explicitly — because a `purge_cap`
withdrawal by another task is otherwise unattributable: nothing else on the
row identifies who took it.

The all-zero id is a real state (an operator client has no principal task),
not missing data, so it is rendered as `purge_cap:operator` rather than as
32 zeros or as a blank. Reading the id alone is wrong in both directions:
zero under `purge_cap` means an operator client, zero under `author` is a
live row's padding.

One spelling of that rendering exists — `cli.RetractedByLabel` — and the
wasm bridge ships its RESULT to the browser rather than re-deriving it in
JS, the same rule `cli.ShownTo` follows.

## Operator surfaces

| Surface | Change |
| --- | --- |
| CLI | `board retract <topic> --seq N`, JSON-line result mirroring `board purge` (`cli/cmd_board.go`); usage in `cmd/harness-cli/main.go` |
| CLI `board read` | `RETRACTED at=<t> by=<author\|purge_cap:…>` — `by=` prints on every withdrawn row, author case included |
| `cli.BoardMessage` | `RetractedBy protocol.RetractedBy`, `RetractedByTaskHex string` (empty = operator client) |
| TUI board modal | `w` on a message (`modalKeys.BoardRetractMsg`), footer hint, `by=` on the list row and the content header |
| WebUI board panel | ⊘ button per live message card, `by=` in the RETRACTED badge, `.board-msg-retract` styling incl. the ≤390px rule |
| wasm bridge | `harness.boardRetract(topic, seq)`; `retractedBy` on each `boardRead` row |
| `caps` catalog | the `purge` description now names both verbs |

Deliberately omitted, and not a silent default: the **TUI cmdline** and the
**WebUI command input** get no `board retract`, because neither has a
`board` verb family at all — `topics` / `read` / `purge` / `subscribers` are
equally absent from both. Adding that family is a separate change; retract is
at parity with its siblings where it is absent.

## Testing (amendment c)

- `agentboard/retract_test.go`: force-retract takes another task's message;
  the author path and the purge_cap path each stamp their own provenance; a
  zero caller id is accepted and still records `purge_cap`; unknown topic /
  unknown seq / seq 0 / already-withdrawn all answer no; `PurgeSeq` still
  reaches a force-retracted message.
- `server/board_handler_test.go`: the message leaves the live ring and comes
  back on `board read` with payload, stamp, `retracted_by` and
  `retracted_by_task`; the four not-found shapes; an operator client's zero
  id still reads `purge_cap`; the required cap is `purge`.
- `cli/board_e2e_test.go`: the whole round trip over a live server,
  including `RetractedByLabel` == `purge_cap:operator` and purge-after-
  retract.
- `tui/board_test.go`: the list row and content header name who withdrew it,
  in all three shapes (author / purge_cap with a task / purge_cap with none),
  and the footer advertises the key.

## Decisions taken (amendment c)

1. Gated on **`purge`**, no new capability bit — it is a subset of what that
   bit already reaches. Consequence recorded: withdraw and destroy cannot be
   granted separately.
2. **Per-seq only.** No whole-topic form; seq 0 is an error, not a wider
   operation.
3. **Topic-addressed**, not a board-wide scan, because the caller already
   knows the topic.
4. `retracted_by` **exists now**, reversing base Decision 4, and carries the
   caller's task id alongside the enum.
5. The enum names the **check that passed** (`author` / `purge_cap`), not a
   kind of caller, because a task granted `purge` is not an operator.
6. A zero caller id is **accepted** on this path and still refused on the
   authorship path.
