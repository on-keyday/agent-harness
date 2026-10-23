# The delivery mark survives a resume that keeps the conversation

Status: DECIDED (operator, 2026-10-24) — rows marked `this spec` in the
decision table were chosen while writing and are the ones worth a second look.

## Problem

A task resumed with its conversation is handed again, by its first
`UserPromptSubmit` hook, every message its previous agent was already given
that is still retained on its subscribed topics.

Observed 2026-10-24 on a Windows claude session: a message was injected at
01:06; the runner restarted at 01:31 and ended the session; the operator
resumed the task with its conversation at 01:31; the next wake at 01:32
injected the 01:06 message again beside the new one. The agent, holding the
earlier turn in its context, reported the duplicate itself. A message that asks
for work is the one this costs: the resumed agent may do it twice.

Mechanism. The delivery mark — the highest seq per topic the automatic
injection path has handed a task — lives in `taskState.shown`, keyed with the
task by `ticketKey{runner, task}`. `Board.Revoke` (TaskFinished, runner
disconnect) destroys the `taskState`, and a resume registers a fresh, empty one
— under a different key, too, when the runner's identity changed (it did here).
`InboxAdvance` then reports everything retained as unshown.

Two decisions in the 2026-08-23 delivery-mark spec made this safe at the time,
and one later change removed the ground under both:

- *the mark lives in `taskState`, not in `topic`* (operator) — because a mark
  in `topic` "would leave one entry per finished task in every topic, needing a
  sweep at `Revoke`";
- *`taskState` keeps its `ticketKey{runner, task}` keying* (author) — because
  finishing a task "also destroys the topic and its ring, so a resumed task …
  has an empty ring and nothing for a carried-over mark to filter".

Since Amendment 2026-09-18d, `Revoke` keeps any topic that still holds
something, so operators do not lose unread messages. The ring now outlives the
`taskState`, and the mark is the half that does not survive.

## Design

### 1. The mark lives on the topic

`topic` gains `shown map[protocol.TaskID]uint64` — the same per-(task, topic)
mark, held by the other half of the pair. `taskState.shown`, `takeUnshown` and
`shownSnapshot` go.

- `topic.takeUnshown(task)` returns the retained messages above the task's mark
  and raises it, under the topic's lock: two concurrent advances still cannot
  both return a message.
- `InboxAdvance` calls it per readable subscribed topic. `ListSubscribers`
  reads `topic.shownMark(task)` for its `Shown` / `Pending` columns.
- Keyed by task id only: a resume on another runner, or with another runner
  identity, finds the same marks.

The lifetime is the topic's. TTL eviction, `MaxTopics` eviction, `PurgeTopic`
and `Revoke` deleting an empty topic all take the marks with it, so there is no
sweep. The entries a finished task leaves behind — the cost the 2026-08-23
decision named — are now what is wanted, and they last no longer than the ring
they index. That is the property the old client-side cursor file lacked.

### 2. Only a new conversation forgets

`agentboard.Conversation` says what the agent being registered is:

- `ContinuedConversation` — it carries the previous agent's conversation
  (a resume with `--resume-conversation`), or it IS the previous agent
  (re-adoption after a server restart);
- `NewConversation` — it starts with an empty context: a new task, or a
  resume without the conversation.

`Board.ForgetShown(task)` deletes the task's mark from every topic.
`boardRegisterTask` — the server's one funnel into `RegisterTask` — takes a
`Conversation` and calls `ForgetShown` for `NewConversation`. A required
parameter rather than a separate call, so no registration site can omit the
decision:

| Site | Conversation |
| --- | --- |
| `dispatch.go` (oneshot dispatch) | from `task.ResumeConversation` |
| `server.go` `sendAssign` | from `task.ResumeConversation` |
| `task_handler.go` `handleOpenInteractive` | from `req.ResumeConversation()` |
| `readopt.go` (server restart) | `ContinuedConversation` — the same child |

A new conversation is handed what is retained again, as today: it has never
seen those messages, and they may be requests it should act on.

## Decisions

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | A resume that keeps the conversation is not handed again what its previous agent was given | operator |
| D2 | A resume without the conversation is handed everything retained, as today | operator |
| D3 | The mark moves from `taskState` to `topic`, keyed by task id — REVERSES the 2026-08-23 operator decision "the mark lives in `taskState` … not in `topic`", whose cost (entries outliving the task) became the requirement once Amendment 2026-09-18d let topics outlive tasks | operator, 2026-10-24 |
| D4 | The choice is a required `Conversation` argument of `boardRegisterTask`, not a separate call | this spec |
| D5 | Re-adoption registers `ContinuedConversation` | this spec |

## Not in this change

- A server restart still forgets every mark; it forgets the topics too, so
  nothing is re-delivered.
- Topic eviction that ignores unread subscribers (`--agentboard-ttl`), noted in
  `2026-10-24-pty-wake-bracketed-paste-design.md`.

## Verification

- Unit (agentboard): a message advanced, the task revoked and re-registered
  under a different runner id, is not advanced again; after `ForgetShown` it
  is. Two concurrent advances on one topic return a message once.
  `ListSubscribers` reports the mark from the topic.
- Unit (server): `boardRegisterTask` with `NewConversation` forgets, with
  `ContinuedConversation` keeps.
- Dummy harness: a session reads a message through
  `agent inbox --json --user-prompt-submit-hook`, is cancelled, and is resumed;
  with the conversation the same read returns nothing old, without it the old
  message again.
