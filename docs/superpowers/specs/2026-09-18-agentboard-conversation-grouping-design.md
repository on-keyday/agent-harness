# Conversations, above chains, on the agentboard chain view

`board thread` groups messages into chains by `in_reply_to`. A chain is not a
conversation, and at more than two participants the difference stops being
academic: chains from unrelated exchanges interleave in one flat list with
nothing saying which belong together.

This adds one layer above the chain — the conversation — and leaves the chain
assembly as it is.

## Decisions taken

| # | Decision | Decided by |
|---|----------|------------|
| E1 | A conversation is identified by its **participant set**, not by its reply links. | operator, 2026-09-18 |
| E2 | A chain that touches a topic which is not a `chat.<short-id>` belongs to **that topic** instead; the topic is the room. | author, from the `--reply-to` per-subject convention |
| E3 | Conversations render oldest-activity first, like the view they extend and like every other terminal transcript. | author |
| E4 | `--json` carries the conversation key per row. A consumer must not have to re-derive the grouping. | author, from `feedback_ssot_means_consumers_cannot_restate` |
| E5 | **NOT DECIDED — whether the fork gutter survives at all.** See "Open decision" below; it is the reason this spec exists before any more rendering work. | — |

## Problem

**A chain is not a conversation, and the gap is not small.** Measured on the
live board, 2026-09-18, on a two-party exchange five minutes long:

```
rows: 8
CHAINS (connected components): 4
  chain of 2: speakers=[claude/70fbad4a, pi/c96af19d]
  chain of 1: speakers=[pi/c96af19d]
  chain of 4: speakers=[claude/70fbad4a, pi/c96af19d]
  chain of 1: speakers=[pi/c96af19d]
CONVERSATIONS (grouped by participant set): 1
```

Two people, one exchange, **four chains**. Two things fragment it, and both are
ordinary:

- **An unanswered message is its own root.** Either agent sending a status
  update nobody replies to starts a new chain. Two of the four above are
  exactly that.
- **A message sent with `--topic` rather than `--in-reply-to` is a root**, even
  when it continues the same discussion. The third chain above is one.

**At N participants and M subjects those fragments interleave.** Roots are
ordered by seq across the whole board, so with several conversations running,
their chains alternate in the output and nothing marks the boundary. `--task`
filters, but filtering is for a reader who already knows which conversation
they want; this view's job is to show what is happening.

### What is NOT the problem

- **The chain assembly.** `BuildThreads` / `SelectThreads` are correct about
  reply links and stay as they are. This adds a grouping above them.
- **The board's retention.** Unchanged, and out of scope exactly as in the
  chain-view spec's D2.
- **Filtering.** `--task` already answers "show me this one". This is about
  what the unfiltered view looks like.

## The unit

```
participants(chain) = { m.from.task_id           for m in chain }
                    ∪ { m.topic[len("chat."):]   for m in chain if m.topic starts with "chat." }
```

A message on `chat.<id>` has two parties: whoever sent it, and whoever owns
that inbox. Both belong to the conversation, and the second is the half a
sender-only rule would miss — the whole reason a topic-keyed view shows one
side of an exchange.

**Room identity:**

- every topic in the chain is a `chat.<short-id>` → the conversation key is the
  sorted participant set: `70fbad4a+c96af19d`.
- any topic in the chain is NOT a `chat.<short-id>` → that topic is the key:
  `rr.dec-019`. A sender who declared a subject with `--reply-to` said the
  subject is the unit, and this takes them at their word. When a chain touches
  more than one such topic, the lowest-sorting name wins, so the key is stable
  under message order.

Consequences worth stating, because both are visible immediately:

- A supervisor talking to five workers produces **five** conversations, one per
  pair. That is the wanted answer.
- Two agents discussing three unrelated subjects over their `chat.<id>` topics
  produce **one** conversation. That is the cost of E1: the participant set
  cannot see a subject change. The answer for a reader who needs them apart is
  the per-subject reply topic the harness already has (`--reply-to`), which E2
  then groups separately — the mechanism exists and this spec does not invent
  a second one.

## Ordering

Conversations by their most recent message, **ascending** — the freshest
conversation last, nearest the prompt. A chat app's channel list is
newest-first because it is a list of rooms to enter; this is a transcript, and
the view it extends already reads forward in time.

Within a conversation: chains by their root's seq, then the chain's own
pre-order, both unchanged.

## Rendering

Each conversation is a section with a header naming its participants and what
it holds. The rows under it render as they do today, minus whatever the open
decision below removes.

```
claude/70fbad4a ↔ pi/c96af19d    8 messages, 4 chains    14:32:39–14:34:46
  <rows>

claude/70fbad4a ↔ worker/3f2a9c1b    2 messages, 1 chain    14:40:02–14:40:05
  <rows>
```

A conversation's header names the agent profile beside each task id, because
`70fbad4a` is not an identity a reader holds in their head and `claude` alone
is not unique. Where the key is a named topic, the header is that topic and the
participants are listed after it.

## Open decision — does the fork gutter survive?

**Not decided. Deliberately left for the operator, because deciding it silently
is what produced the rework this spec follows.**

The chain view indents a reply that shares a parent with another reply
(`├─` / `└─`), and draws linear runs flat. What has been learned since:

- **Forks are structurally impossible in the dominant case.** Replying to a
  message addressed to you withdraws it (the retract design's
  reply-implies-retire), so a second reply to the same parent is rejected with
  `unknown_in_reply_to`. Observed directly, 2026-09-18: a worker that replied
  and then tried to reply again to the same seq was refused. A fork requires a
  shared topic, a self-reply, or `--no-retire-on-reply`.
- **Measured: zero forks.** 31 messages in the supervisor/worker exchange that
  produced the view, and 8 in a later one — no message in either had more than
  one reply. The single fork ever rendered was manufactured on purpose with
  the opt-out flag.
- **Group chats converged the other way.** Slack puts replies in a thread panel
  and shows one line in the channel; Discord shows a one-line quote of the
  parent and does not indent; Gmail flattens entirely. The clients that DO
  indent (mail readers, forum trees) all carry a depth-collapsing feature,
  which is the same admission.

The three options:

1. **Remove indentation; show a back-reference instead** — the parent's speaker
   and a snippet, where `re=<seq>` is today. 19 digits is not a reference a
   human can follow, which is the actual reason the view reads as a wall.
2. **Keep it** — forks are rare, and when one happens the structure is the one
   thing a back-reference cannot convey.
3. **Both** — back-reference always, indentation only for the rare fork.

Two defects are **held pending this**, because fixing them is wasted work if
option 1 wins:

- A continuation inherits its parent's `IsLast` whole, so it redraws the
  parent's branch glyph and is indistinguishable from a third sibling.
- A row's body is printed flush-left, so at depth it does not sit under its own
  header. (Whatever fixes this must apply only when the destination is a
  terminal: indenting a redirected body would break the byte-exactness the
  chain-view spec's obligation 2 requires.)

Both are legibility, not data loss: every row carries `re=<parent seq>`, which
names the parent exactly.

## Surfaces

| Surface | Change |
|---------|--------|
| CLI | section headers in `board thread` / `agent thread`; `--json` gains `conversation` per row |
| CLI (shared) | the grouping in `cli/boardthread.go`, beside `SelectThreads`, which already computes the components this builds on |
| TUI | the same sections in the board modal's chain view — it draws `cli.RenderThreads`, so it inherits them |
| WebUI | a section per conversation in the Board tab's Chains view; the browser groups nothing itself |
| wasm bridge | `conversation` on each row, beside the existing fields |
| server | none |

The `surface-parity-checklist` is walked item by item before this is called
done, and item 39 against this spec's own matrix.

## Testing

On the grouping:

- two chains between the same pair collapse into one conversation; a third
  chain with a different peer does not join them
- a chain whose messages are all on `chat.<id>` topics keys on the participant
  set; one touching `rr.foo` keys on `rr.foo` regardless of who spoke
- a message on `chat.<id>` contributes BOTH its sender and the topic's owner,
  asserted by a chain where one party never sent anything (it was only written
  to) and must still appear in the header
- conversation order follows the most recent message, not the first
- an orphan-rooted fragment joins the conversation its participants name, not a
  section of its own
- `--json` carries the same key the text header shows, for every row

The fixture for the first of those is the measured shape above: 8 messages, 4
chains, 1 conversation.

## Risks

**E1 cannot see a subject change.** Two agents discussing three things over
their own topics are one conversation. Stated in the unit section with the
existing answer (`--reply-to`); the alternative — inferring a subject from a
gap in time — was considered and rejected as a threshold nobody can defend.

**A conversation's identity changes when a new participant joins a chain.** A
third agent replying into an exchange re-keys that chain's conversation, which
moves rows between sections between one refresh and the next. That is correct
— it IS a different conversation once three people are in it — but it will look
like the view lost something. The section header naming its participants is
what makes the change legible rather than surprising.

**A task id is the identity, and a one-shot task is a new identity every time.**
Found by running it, 2026-09-18: a "supervisor" that sends each message from a
fresh `submit --agent bash` task appears as a different participant per message,
so one stream of instructions to the same worker split into two conversations.
Nothing is wrong with the grouping — those really were different tasks — but the
mental model "a supervisor" and the mechanical fact "a task id" come apart
exactly there.

It is bounded: a long-lived session keeps its id, a `--resume`d task keeps its
id, and the case this view is for — agents talking over their `chat.<short-id>`
topics — is long-lived on both sides. The pattern it degrades on is a script
spawning a throwaway task per message, which is not a conversation so much as a
sequence of announcements.

Deliberately not fixed here. A stable "who" above the task id would be a new
identity concept on the wire, and inventing one to tidy a view is the wrong
order; if it is ever wanted, it wants its own spec.

## Completion

1. The grouping + its tests green, including the measured 8/4/1 fixture.
2. The open decision resolved, recorded here as an amendment naming who decided
   it, and the two held defects either fixed or deleted with the gutter.
3. Section headers on all three surfaces; `conversation` in `--json` and across
   the wasm bridge.
4. `surface-parity-checklist` walked, verdict per number.
5. `make check` green.
