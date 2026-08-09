# Reply linkage on the agentboard (`in_reply_to`)

Date: 2026-08-09

## Problem

- **The board has message identity but no way to reference it.** Every
  published message gets a board-global monotonic `seq`
  (`agentboard/board.go:231`), returned to the publisher in
  `SendResponse.seq` (`agentboard/agentboard.bgn:62-65`) and delivered to
  the receiver in `DeliveredMessage.seq` (`agentboard/agentboard.bgn:86`).
  Both ends already hold the identifier. No field anywhere on the wire
  connects a reply back to the message it answers.

- **So correlation falls to the payload, which an LLM writes.** The
  documented practice is a `reply_topic` key inside the JSON body plus a
  free-form `kind` discriminator (`.claude/skills/harness-cli/SKILL.md`,
  "Agent-to-agent communication conventions"). Neither is inspected by the
  server; both are prose the sending model chooses to emit or not.

- **This fails in practice, not just in theory.** A peer task running a
  review-collection workflow reported three different envelopes from the
  *same* responder for the same instruction ("prefix the reply with
  `[viewer-review] decision=DEC-019`"): raw text carrying the marker, a
  JSON object with the marker nested in a `message` field, and a JSON
  object where the marker was gone entirely and the decision had moved to
  a `decision` field. The third shape made the collector conclude "no
  review arrived". Their parser grew a case per shape; the space of
  envelopes an LLM can invent has no bound, so the parser cannot converge.

- **The server-attested fields are insufficient for this.** What a sender
  cannot forge today is `seq`, `topic`, `from_runner_id`, `from_task_id`,
  `from_hostname`, and `from_agent_profile`
  (`agentboard/agentboard.bgn:86-113`, surfaced at
  `cli/agent/json_emit.go:24-41`). `from_task_id` identifies *who* replied
  but not *what they replied to*: two questions sent to one peer produce
  two replies indistinguishable by sender.

- **The existing workaround costs more than it should.** Splitting replies
  onto a per-subject topic (`rr.dec-019`) does move the discriminator out
  of the payload and into the envelope, and it works today — subscriptions
  accept arbitrary names, validated only against the empty string
  (`server/agent_handler.go:237-249`). But `taskState.matches` is an exact
  map lookup with no wildcards (`agentboard/taskstate.go:45-49`), so N
  subjects means N explicit `subscribe`/`unsubscribe` calls; a quiet
  subject topic is the first evicted when the board passes
  `--agentboard-max-topics` (`agentboard/board.go:350-360`); and its ring
  is dropped 30 minutes after the reply lands
  (`--agentboard-ttl`, `agentboard/board.go:335-348`) whether or not
  anything read it. It also still depends on the responder addressing the
  right topic — one string instead of an unbounded envelope, but the same
  class of dependency.

## Goal

Make "which message is this a reply to" a value the transport carries and
the server vouches for, so a program collecting replies never parses model
prose to correlate them.

Non-goal: threads. Only the direct parent link is modeled. A caller that
wants a tree walks the links itself.

## Design

### 1. Wire schema

`agentboard/agentboard.bgn`:

```
format SendRequest:
    request_id :u32
    in_reply_to :u64
    topic_len :u16
    topic :[topic_len]u8
    topic_len != 0 || in_reply_to != 0
    payload_stream_id :u64
```

`in_reply_to` is the parent message's `seq`; `0` means "not a reply".

The bare expression is a brgen assertion, the same construct
`RunnerID` uses for `ip_addr_len == 4 || ip_addr_len == 16`
(`agentboard/agentboard.bgn:7`). Generated code enforces assertions on
encode as well as decode, so an invalid `SendRequest` cannot be
constructed — "an empty topic is only meaningful on a reply" is a schema
constraint, not a convention a caller may forget.

```
enum SendStatus:
    :u8
    ok
    payload_too_large
    too_many_topics
    bad_frame
    unknown_in_reply_to

format DeliveredMessage:
    seq :u64
    in_reply_to :u64
    ...

format RetainedMeta:
    seq :u64
    in_reply_to :u64
    ...
```

`unknown_in_reply_to` is appended, leaving existing enum values unchanged.
`in_reply_to` sits directly after `seq` in both delivery formats: the two
u64s are the message's own id and its parent's id, and keeping them
adjacent makes the pairing legible in the schema.

`runner/protocol/message.bgn`, `BoardMessageRow` (`:673`) gains
`in_reply_to :u64` so the operator read path carries it too.

**Why `seq` is a sufficient identifier.** It is board-global and
monotonic, and `Config.SeqSeed` starts each boot in a strictly higher
range than any prior boot's cursors (`agentboard/board.go:18-29`). One
`seq` therefore never denotes two different messages across the board's
lifetime, and no new identifier space is needed.

### 2. Server validation and destination derivation

Add to `agentboard`:

```go
// LookupSeq resolves a published seq to the topic that retains it and the
// task that published it.
func (b *Board) LookupSeq(seq uint64) (topic string, fromTid protocol.TaskID, ok bool)
```

`agentHandleSend` (`server/agent_handler.go:145`) gains, before publishing:

1. If `in_reply_to != 0`, call `LookupSeq`. On `!ok`, reply
   `SendStatus_UnknownInReplyTo` and publish nothing.
2. If `topic_len == 0`, set the destination to
   `agentboard.SelfTopic(fromTid)` (`agentboard/ids.go:44-50`) — the
   inbound topic of the parent's *authenticated* sender, taken from the
   retained entry, never from the requester.
3. Carry `in_reply_to` into `RetainedMessage` (`agentboard/topic.go:11-25`)
   and out through `DeliveredMessage`, `RetainedMeta`, and
   `BoardMessageRow`.

Publishing rejects rather than accepting an unresolvable link: a link the
server cannot vouch for is worse than no link, because a collector would
have to distrust every link to handle it.

**Resolution is a scan, not an index.** `LookupSeq` snapshots the topic
pointers under `b.mu`, releases it, and scans each ring under its own
`t.mu`.

- An index (`map[seq]→topic`) is O(1) but must be invalidated in six
  places: ring overflow inside `topic.append` (`agentboard/topic.go:41-62`),
  `topic.removeSeq` (`:64-75`), `Board.PurgeTopic` (`board.go:371`),
  `Board.PurgeSeq` (`board.go:388`), `Board.evictExpiredTopics`
  (`board.go:335`) / `evictOldestTopicLocked` (`board.go:350`), and the
  topic deletion inside `Board.Revoke` (`board.go:148-167`). Missing one
  desynchronizes the index from the rings and produces either "rejected
  though it exists" or "accepted though it is gone", intermittently.
- The scan holds no derived state, so it cannot desynchronize. Its bound
  is `--agentboard-max-topics` × `--agentboard-ring` = 1024 × 64 ≈ 65k
  comparisons worst case (`cmd/harness-server/main.go:30-32`), against a
  publish rate driven by agent turns.
- A message evicted mid-scan can be missed, yielding a spurious rejection.
  This is the same approximate-read tradeoff already documented for
  `lastPublishedAt` in `evictExpiredTopics` (`agentboard/board.go:340-343`).

`LookupSeq` carries a comment recording the bound and stating that raising
`--agentboard-max-topics` or `--agentboard-ring` by an order of magnitude
is the trigger to replace the scan with an index.

**Rejection window equals collection window.** A parent that has fallen
out of its ring is also unreadable by any collector, so a link to it could
not have been resolved downstream either. Rejecting costs nothing that was
recoverable.

### 3. Agent CLI

`cli/agent/send.go`:

```bash
# Reply. No topic needed — the server routes to the parent's sender.
harness-cli agent send --in-reply-to <seq> "review result ..."

# Explicit destination still available (e.g. a third-party collector topic).
harness-cli agent send --topic rr.dec-019 --in-reply-to <seq> "..."
```

`--topic` becomes optional exactly when `--in-reply-to` is given; with
neither, the existing "`--topic` required" error stands (`send.go:26-28`).

This shape is deliberate. The peer report shows instructions about message
*form* are not reliably followed, and `--in-reply-to` is another such
instruction. It has a chance of being followed only because the linked
reply is **less** work than the unlinked one: the responder passes one
number and needs neither the reply topic nor a `reply_topic` key parsed
out of the request body.

`cli/agent/json_emit.go:24` emits `in_reply_to` on every record, `0` when
absent — matching the reason the `from` block is unconditional there
(a consumer can address the field without probing for it).

`harness-cli agent inbox --in-reply-to <seq>` filters client-side.
`agent wait` and `agent retained` (`cli/agent/wait.go`,
`cli/agent/retained.go`) surface the field through the same emit paths.

`agent dispatch` is left unchanged. It blocks on `wait` and so is unusable
from an agent turn, and its `--reply-topic` performs no server-side
correlation — it merely waits on a topic, from `since=0`, so it also
returns messages retained before the send. `in_reply_to` supersedes what
that flag was reached for; the subcommand is not extended.

No cross-topic "find replies to N" RPC. Replies land on the collector's
own `chat.<short-id>`, so `inbox` plus a one-line filter covers the case;
a board-wide search would be a new capability-gated surface for no
additional reach.

### 4. Operator surfaces

`in_reply_to` reaches all three operator UIs, per the repo rule that a
feature is reachable from CLI, TUI and WebUI unless functionally
impossible:

- `cli.BoardMessage` (`cli/board.go:23-35`) gains `InReplyTo`, populated
  in `Client.BoardRead` (`cli/board.go:69`) and the standalone
  `BoardRead` (`cli/board.go:171`); `board read` prints it and accepts
  `--in-reply-to` as a filter.
- TUI board message view (`tui/board.go:27`, `:179`) shows the parent seq.
- WebUI board view (`webui/static/main.js:3429`) shows the parent seq.

`server/board_handler.go:46` fills the new row field. The gate is
unchanged: `BoardRead` already requires `Capability_InfoGlobal`
(`server/capabilities.go:33`), and no new gate is introduced — reply
linkage is metadata about messages the caller can already read.

### 5. Skill documentation

Edited in the embed source under `runner/agentskills` and synced to the
`.claude/skills` copy, then rebuilt:

- Replying uses `agent send --in-reply-to <seq>`; the reply topic does not
  need to be known.
- `reply_topic` in payloads remains as the interoperability fallback for
  peers that do not set `in_reply_to` (`agent=bash`, peers without skill
  injection).
- Per-subject reply topics remain documented as the fallback for the same
  peers, with their limits stated: exact-match subscribe only, per-topic
  ring of 64, 30-minute TTL from last publish, and board-wide eviction of
  the least recently published topic past 1024.

## Error handling

`unknown_in_reply_to` surfaces from `agent send` as an error naming the
cause and the recovery: the parent seq is not on the board (evicted past
the 64-entry ring or the 30-minute TTL, or purged), and dropping
`--in-reply-to` sends the same body as an ordinary message.

Both current `Board.Send` callers pass a real task id — agent publishes
(`server/agent_handler.go:161`) and `await-idle` publishes, which use the
requester's task id with a placeholder RunnerID
(`server/await_idle_handler.go:168`, `server/task_handler.go:1744-1752`).
Derivation therefore always has a task to route to; a reply to an
await-idle notification routes back to the requester, which is correct.

## Rollout

The schema change is not backward compatible in either direction: a
mismatched peer fails to decode `SendRequest`, and
`handleAgentMessage` logs a warning and drops the message
(`server/agent_handler.go:64-68`). Publishes fail silently from the
agent's perspective during a mismatch.

Restart the server and run `make build` to refresh `bin/harness-cli` in
the same maintenance step. Because `harness-cli` is a short-lived process
per subcommand, refreshing the binary is sufficient — running agent
sessions do not need to be restarted.

## Testing

- `agentboard`: `LookupSeq` resolves across topics; resolution fails after
  ring overflow, `PurgeSeq`, `PurgeTopic`, `Revoke`, and TTL eviction;
  `in_reply_to` survives into `RetainedMessage` and out through delivery.
- `server`: `unknown_in_reply_to` on an unresolvable parent with no
  publish side effect; derivation targets the parent sender's
  `SelfTopic`; an explicit topic overrides derivation.
- `cli/agent`: `--in-reply-to` without `--topic`; `--topic` alone; neither
  (error); rejection message text; `in_reply_to` present in every
  `emitMessageLine` record; `inbox --in-reply-to` filtering.
- `cli`/`tui`/`webui`: the parent seq reaches each board view.
- End to end: two agents, request → reply carrying `in_reply_to` → the
  collector selects replies by parent seq with no payload parsing.

Verification runs through the existing `make` targets rather than ad-hoc
`go build ./...`, and the feature is driven against a locally started
dummy server and runner before it is called done.
