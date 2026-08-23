# The sender declares where a reply goes (`reply_to_topic`)

Date: 2026-08-24

## Problem

- **A sender cannot say where it wants the answer.** `resolveReplyTarget`
  (`server/agent_handler.go:156-168`) routes a reply carrying `--in-reply-to`
  and no `--topic` to `SelfTopic(parentTid)` — the parent sender's own
  `chat.<short-id>` — and `SendRequest` has no field for anything else
  (`agentboard/agentboard.bgn:47-85`). The only lever is `--topic` on the
  REPLY, which is the replier's choice, not the asker's.

- **So a script driving a peer on an agent's behalf cannot keep the answer out
  of that agent's inbox.** A script in a task shell runs with the task's
  `HARNESS_*`, so `dispatch` waits on `chat.<agent-id>` — which is also where
  the agent's `UserPromptSubmit` hook reads from. The reply therefore lands in
  the agent's context whether or not the agent should see it, and the script
  has no way to summarise or to report only a failure. That is the case this
  change exists for:

  ```
  dispatch --topic chat.<target> --reply-to rr.task-1     # answer goes to rr.task-1
  ...on timeout: send --topic chat.<agent-id> "it failed" # only THIS reaches the agent
  ```

- **The workaround in the skill is a payload convention that nothing
  implements.** `reply_topic` is a JSON field the sender writes and the replier
  is trusted to honour. Measured: `grep -rn reply_topic` over every `.go`,
  `.py`, `.sh` and `.js` in the repo returns TWO hits, both comments; no code
  reads it. It exists in two SKILL.md files and two historical design docs, and
  nowhere else.

  The audience it names is also the one least able to use it. The skill keeps
  it "for peers that never set `in_reply_to` (`agent=bash`, or any peer without
  skill injection)", and the only description of the convention is in that same
  skill — so honouring it takes either skill injection, or a human writing the
  parse by hand. What is measured is the first part: no code in this repo reads
  the field. Whether some hand-written script elsewhere does is not something
  this repo can see, and does not change that the harness itself provides no
  mechanism.

- **`agent dispatch --reply-topic` was removed on 2026-08-23 (`99c84cb`) and
  would not have helped anyway.** It changed where dispatch WAITED and told the
  peer nothing, so a peer replying with `--in-reply-to` alone still had its
  reply routed to the asker's own chat topic — the inbox the caller was trying
  to keep clean — while the caller waited out its timeout somewhere else. The
  removal rationale ("a caller-chosen reply topic can only disagree with where
  the reply lands") was correct about the old flag and is what this change
  fixes: it makes the caller's choice the thing that DECIDES where the reply
  lands.

- **The repo's own rule names this shape.**
  `feedback_protocol_explicit_over_convention`: "If a field is needed, extend
  the schema. Don't write code that by convention puts X in this position of
  the payload. LLMs lose convention context across sessions; explicit schema
  fields survive."

## Goal

1. A sender can declare, on the wire, where a reply to its message is routed.
   The server records it and enforces it.
2. The replier needs no change and no knowledge: `--in-reply-to <seq>` alone
   reaches the declared destination.
3. `agent send` and `agent dispatch` both expose it; `dispatch` additionally
   waits there.
4. Today's behaviour is the default. A message that declares nothing routes its
   replies to the sender's `chat.<short-id>` exactly as now.

Out of scope, decided rather than deferred:

- **No restriction on which topic may be named.** Publishing takes no
  capability and any task can already publish anywhere, so naming a
  destination grants no reach a sender did not have. What it does add is that
  the PEER's message lands there, attributed to the peer — recorded here as the
  one new property, not gated.
- **A destination nobody subscribes to is accepted, not refused.** It is the
  same state a publish to a quiet topic already produces, and `delivered_to`
  already reports it as 0. `dispatch` subscribes its wait topic for the
  duration anyway.
- **The `reply_topic` payload convention is not removed in this change.** Its
  two SKILL.md sections are rewritten to point at the field (see §6), but
  nothing parses the payload today, so there is no code path to delete.

## Design

### 1. Wire — one field on `SendRequest`

`agentboard/agentboard.bgn`, appended at the END of the format:

```
format SendRequest:
    request_id :u32
    in_reply_to :u64
    topic_len :u16
    topic :[topic_len]u8
    topic_len != 0 || in_reply_to != 0
    payload_stream_id :u64
    no_retire_on_reply :u1
    reserved :u7
    # reply_to_topic is where a reply to THIS message is routed. Empty = the
    # sender's own chat.<short-id>, which is what every message did before this
    # field existed and what one still does when it says nothing.
    #
    # It is the SENDER declaring a destination, not the replier choosing one:
    # a peer answers with --in-reply-to alone and never has to know the topic
    # exists. That is the whole point — the alternative was a `reply_topic`
    # field in the payload, which only works on a peer that reads the
    # convention, and the convention is documented in a file such a peer has
    # by definition not read.
    reply_to_topic_len :u16
    reply_to_topic :[reply_to_topic_len]u8
```

Appended rather than inserted: `SendRequest` is a REQUEST, so the skew is a new
client against an old server — the old server stops before the new bytes and
the field is silently ignored, which degrades to today's routing rather than to
a decode failure. `scripts/wire-skew-check.sh` asserts the skew stays
recoverable either way.

### 2. Board — recorded per retained message

`agentboard.RetainedMessage` gains:

```go
// ReplyToTopic is where the SENDER asked replies to this message to go.
// Empty = the sender's own chat.<short-id>. Frozen here with the message for
// the same reason FromAgentProfile is: the ring outlives the connection that
// produced the entry, and a reply may arrive long after.
ReplyToTopic string
```

It reaches `Board.Send` as a `SendOption`, matching `NoRetireOnReply`:

```go
// WithReplyTo records where replies to this message should be routed.
func WithReplyTo(topic string) SendOption {
    return func(c *sendConfig) { c.replyToTopic = topic }
}
```

`Send` already takes seven positional parameters and its own comment says
options exist because of that; a new one belongs here rather than as an eighth.

### 3. Server — one branch in `resolveReplyTarget`

```go
func resolveReplyTarget(b *agentboard.Board, topic string, inReplyTo uint64) (string, bool) {
	if inReplyTo == 0 {
		return topic, true
	}
	parent, ok := b.Retained(inReplyTo)
	if !ok {
		return "", false
	}
	// An explicit --topic on the REPLY still wins: the replier is answering and
	// may know something the asker did not.
	if topic != "" {
		return topic, true
	}
	if parent.ReplyToTopic != "" {
		return parent.ReplyToTopic, true
	}
	return agentboard.SelfTopic(parent.FromTask), true
}
```

Priority is **explicit `--topic` > the parent's `reply_to_topic` > the parent
sender's `chat.<short-id>`**. The third arm is today's behaviour, unchanged and
still the default.

The lookup moves from `LookupSeq` (topic + sender) to `Retained` (the whole
entry), because the destination now comes off the parent message. Both are full
ring scans with the same cost; `LookupSeq` has no remaining caller in this
function.

### 4. Agent CLI

```
agent send     --topic T [--reply-to R] --data D    # declare, end the turn
agent dispatch --topic T [--reply-to R] --data D    # declare, and wait on R
```

`dispatch` puts `--reply-to` on the wire AND waits on that topic; with no
`--reply-to` it waits on its own `chat.<short-id>` as it does now. The
correlation is unchanged on both paths: `Since` and `InReplyTo` are the seq the
call just published.

**The flag is `--reply-to`, not `--reply-topic`.** The removed
`--reply-topic` meant "wait here" and told the peer nothing; reusing the
spelling for the opposite mechanism would give a stale script new behaviour
silently.

`send --reply-to` exists because the field is on `SendRequest` and a wire field
no CLI can set is a field that rots. It is also directly useful to an agent
inside a turn: declare a destination, end the turn, and the answer arrives
somewhere the inbox hook will not splice into the next prompt.

### 5. What the correlation costs — nothing extra

A reply reaches a declared destination by the server resolving `in_reply_to`
against the parent. A reply that carries no `in_reply_to` therefore cannot take
that route at all; it can only land on R by naming `--topic R` itself, which
requires knowing R — the knowledge this design removes the need for. So
requiring `in_reply_to` is what the routing already requires, not a restriction
layered on top.

The one shape excluded is a peer that both knows R (from the payload
convention) and does not set `in_reply_to`. Nothing implements that convention
(§Problem), so this is recorded for completeness rather than as a limitation
anyone is expected to hit. A caller who does hit it uses
`agent wait --topic R --since N`, which applies no correlation.

### 6. Documentation

`runner/agentskills/harness-cli/SKILL.md` (the `go:embed` source, mirrored to
`.claude/` and `.agents/` in the same commit):

- **"Naming inbound channels"** currently tells every agent to announce
  `reply_topic` in every message and calls it "the fallback, not the
  mechanism". It becomes: your inbound topic is `chat.<short-id>` and is
  server-seeded; a sender that wants replies elsewhere says so with
  `--reply-to`, which the server records and enforces. The payload field is
  named as legacy, with the note that no code reads it.
- **"Per-subject reply topics (fallback)"** is the pattern this replaces. It
  becomes a short section on `--reply-to` with the per-subject example, keeping
  the limits that still hold (exact-match topics, ring size, TTL, the
  1024-topic cap).

### 7. The residual: dispatch's wait subscribes its reply topic

`Board.Wait` subscribes the topic for the duration of the call, because
`Send` only pings connections whose taskState matches — without the
subscription the waiter is never woken. So while `dispatch --reply-to R` is
waiting, R IS subscribed by the calling task, and a turn that starts in that
window has R in its `snapshotPatterns()`: the inbox hook would inject the
reply into the agent's context after all.

Accepted rather than solved. The window is only as long as the script's own
call, and the content is the answer to something that script asked for on the
agent's behalf. Solving it means waking a waiter without a subscription, which
is a second delivery path in the board for one edge case.

Stated here because the goal in §Problem is keeping the agent's inbox clean,
and this is the one gap in it.

## Error handling

- `--reply-to` naming a topic that does not exist yet: accepted. The reply
  creates it, as any publish does.
- `--reply-to` on a message that is itself a reply: allowed and independent.
  Each message carries its own destination for ITS replies; there is no
  inheritance.
- A parent that has left its ring (evicted, TTL, purged) already fails the
  whole send with `unknown_in_reply_to` before the destination is consulted.
  Unchanged.
- `reply_to_topic` longer than the topic limit: refused by the same
  length-prefix bound as `topic`, at decode.

## Rollout

1. `scripts/wire-skew-check.sh` — `.bgn` changed, so it runs for real.
2. **Restart the server first**, then `make build` and restart runners. Old
   client against new server is unaffected (the field defaults empty); new
   client against old server loses the field silently and gets today's routing,
   which is the degrade this ordering avoids anyway.
3. `make build` in the main checkout, per the landing rule.

## Testing

Unit, `agentboard/`:

- `Send` with `WithReplyTo` stores it on the retained entry; without it the
  field is empty.
- A retained entry's `ReplyToTopic` survives being read back after other
  publishes (it is frozen with the message).

Unit, `server/`:

- `resolveReplyTarget` priority: explicit `--topic` beats the parent's
  `reply_to_topic`; `reply_to_topic` beats `SelfTopic`; neither present falls
  through to `SelfTopic` exactly as before.

E2E, `cli/agent/`:

- `send --reply-to R`, then a peer replies with `--in-reply-to` ONLY: the reply
  lands on R and NOT on the sender's `chat.<short-id>`. This is the property
  the whole change exists for, and asserting the negative half matters as much
  as the positive.
- `dispatch --topic T --reply-to R` returns the peer's `--in-reply-to`-only
  reply, and the sender's own chat topic stays empty.
- `dispatch` with no `--reply-to` still waits on its own chat topic (the
  existing tests cover this and must keep passing unchanged).

Live check after rollout, in the shape the feature was asked for: a script
dispatches to a worker with `--reply-to rr.task-1`, the worker replies with
`--in-reply-to` alone, the script receives it on `rr.task-1`, and
`board subscribers` shows the asker's `chat.<short-id>` with `pending` unchanged
— the answer never touched the agent's inbox.

## Decisions taken

- **DECIDED (operator, 2026-08-24)** — the destination is declared by the
  SENDER and enforced by the server, not carried in the payload for the
  replier to honour.
- **DECIDED (operator, 2026-08-24)** — exposed on `agent send` as well as
  `agent dispatch`.
- **DECIDED (operator, 2026-08-24)** — `dispatch` keeps its correlation
  (`Since` + `InReplyTo` = the published seq) on the `--reply-to` path.
- **DECIDED (author)** — the flag is `--reply-to`; the removed `--reply-topic`
  spelling is not reused for the new meaning.
- **DECIDED (author)** — no restriction on which topic may be named, and no
  refusal for an unsubscribed one.
- **DECIDED (author)** — the wait-side subscription window (§7) is accepted and
  documented rather than closed.
