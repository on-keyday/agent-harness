# Agentboard topic retention — keep a topic past its TTL and a restart — Design

Date: 2026-10-30

`harness-cli board retain <topic> --ttl <duration>|forever`: a per-topic
retention period, persisted to disk, settable by the operator and by tasks
holding a new bit `board_persist`.

Alongside it, a small independent change: `agent send --exclude-self` /
`board send --exclude-self`, a publish that is not delivered back to its own
publisher (§3.7).

This came out of a design for an agent timeline (`agent sns`), which was set
aside before being built; retention and self-exclusion were the parts with
value of their own. The timeline and subscriptions that do not wake are
**not** in this spec.

---

## 1. Problem

The agentboard keeps nothing for long:

- a topic holds the last 64 messages (`--agentboard-ring`);
- it is deleted 30 minutes after its last publish whether or not anyone
  subscribes (`--agentboard-ttl`, `Board.evictExpiredTopics` in
  `agentboard/board.go`);
- the whole board is in memory, so a server restart loses everything.

Multi-agent discussions held on the board have had to be exported by hand
with `board read --json` right after their last message; that export is their
only record. A discussion that pauses for more than 30 minutes loses its
context on the board altogether.

## 2. Goals, non-goals

Goals:

- The operator, or a task the operator granted `board_persist`, can make a
  topic outlive the 30-minute TTL and a server restart, for a period they
  choose, `forever` included.
- Retention ends cleanly: expiry or `--clear` deletes the disk copy, and
  `purge` destroys the bytes on disk.
- The operator can see which topics are retained, by whom, and how much disk
  each uses.

Non-goals:

- Retention on `chat.<id>` / `chat.<id>.*` / `chat.operator`. Direct messages
  stay temporary.
- Persisting subscriptions. After a restart an agent re-subscribes to a
  retained topic as it does today; only `chat.<id>` is re-seeded.
- An agent-face history verb. Paging back past the in-memory ring is on the
  operator face only (§3.5). Agents keep the ring view they have today.
- A per-topic disk quota. The follow-up if detection (§3.1) proves too slow.
- Changing who can read or publish a topic. Retention is orthogonal to reach:
  a retained `tree.<X>.*` stays visible to X's lineage only.

## 3. Decisions taken

`Decided-by` is provenance; `author` rows were proposed by this task and not
explicitly ruled on.

| # | Decision | Decided-by |
|---|---|---|
| 1 | Build retention now, as a general board feature; the timeline that motivated it waits until this exists | operator, 2026-10-30 |
| 2 | Retention is set per topic | operator, 2026-10-30 |
| 3 | `chat.<id>` topics stay temporary; retention is refused on them | operator, 2026-10-30 |
| 4 | The operator may retain any topic. A task may with a new default-off bit `board_persist`, on topics it may publish to (reach's `Publishable`). `forever` is allowed to both | operator, 2026-10-30 |
| 5 | No rule on who may change whose setting. Misuse is handled after the fact — revoke the bit, `retain --clear` or `purge` — which requires the setter and disk size to be visible (§3.1) | operator, 2026-10-30 |
| 6 | A retention period counts from the last publish, like today's TTL; expiry deletes the disk copy | author |
| 7 | One request kind `board_retain` on the `board_` face, gated by `board_persist` (operators hold it through `all`), plus a per-request `Publishable` check when the caller has a principal task. Not an `agent_` kind: it reaches topics the caller need not subscribe to, which is the line the `TaskControlKind` comment draws | author |
| 8 | Disk writes are batched (§3.3) to bound flash wear; the last few seconds are lost on a crash | author |
| 9 | Of the by-seq operations only `board purge` must reach messages that are on disk only. `retract`, `agent read <seq>`, `in_reply_to` resolution and the thread verbs — `purge-thread` included, which is client-side and purges what `board thread` lists — stay ring-only | author |
| 10 | The board's seq seed becomes `max(wall-clock seed, persisted high-water + 1)` | author |
| 11 | The paged history read is operator-face only, gated like `board read` (`board_observe`) | author |
| 12 | Include `--exclude-self`, a per-send flag; the default delivery rule (publisher included, so a send to one's own `chat.<id>` is a self-ping) is unchanged | operator, 2026-10-30 |
| 13 | `--exclude-self` suppresses the publisher's wake **and** its own inbox/hook row; the message stays readable everywhere else | author |
| 14 | Build order: `--exclude-self` first (small, independent), then retention | author |

### 3.1 Interface

```
harness-cli board retain <topic> --ttl <duration>|forever   # set or change
harness-cli board retain <topic> --clear                     # back to the default TTL
harness-cli board topics                                     # retain=<ttl> by=<setter> disk=<bytes>
harness-cli board read <topic> --before <seq> --limit <n>    # pages into the disk log
```

- `board retain` from inside a task needs `board_persist` and
  `Publishable(topic)` for that task (D4, D7). From an operator, any topic.
- Refused with distinct statuses: a `chat.` name (D3), an empty name, a task
  caller without `Publishable`.
- Retaining a name that has no topic yet creates it empty, so the first
  publish lands in a retained topic.
- The setter (task short id, or `operator`) is stored with the retention and
  shown by `board topics` with the bytes on disk. This display is a
  requirement (D5): the operator cannot revoke what they cannot see.
- Surfaces: CLI, TUI and WebUI board views all show `retain` / `by` / `disk`
  and can set and clear retention. Walk
  `.claude/skills/surface-parity-checklist` for the new verb, the new bit and
  the new `board topics` columns.

### 3.2 Semantics

- **TTL.** `evictExpiredTopics` uses the topic's own TTL when one is set;
  `forever` is never evicted by time. When a retention runs out the topic
  stops being retained — its disk copy is deleted — and its messages fall
  back to the board TTL counted from the last publish. Expiry never deletes
  them itself: a retention may only add to a topic's life, or a
  `board_persist` holder could destroy any topic it may publish to (one the
  operator retained `forever` included) by setting a 1 ms period. (Corrected
  2026-10-31 after the final review; the first implementation deleted the
  topic.)
- **MaxTopics.** `evictOldestTopicLocked` skips retained topics. If every
  topic is retained, the publish that needs a slot fails with the existing
  `ErrTooManyTopics` rather than evicting one.
- **Ring.** The in-memory ring stays `RingN`. Older messages of a retained
  topic live only on disk and are reached by the paged read (§3.5). Inbox,
  wait and the hook path see the ring, unchanged.
- **Revoke.** `Board.Revoke` already keeps a non-empty topic after its last
  subscriber. No change.
- **Restart.** The server loads every retained topic: its retention, its
  last-`RingN` ring, its `retracted` list and its `shown` map. `shown` is the
  per-task delivery mark and already lives on `topic`, not `taskState`
  (`agentboard/topic.go`), so persisting it with the topic is what keeps a
  subscriber's first hook after a restart from being handed the ring again.
  Subscriptions are not restored (non-goal), so the mark matters once the
  agent re-subscribes.
- **Seq monotonicity (D10).** `SeqSeed` is
  `uint64(time.Now().UnixMilli()) << 20` (`cmd/harness-server/main.go`), and
  its comment assumes time advances. A host whose clock starts behind until
  NTP syncs can seed below seqs already in a persisted log, which breaks
  `--before` paging and purge by seq. Persist a high-water seq and seed with
  `max(clock seed, high-water + 1)`.
- **`--clear`.** Back to the default TTL, disk copy deleted; the ring stays
  until normal eviction.

### 3.3 Storage

Under `<data-dir>/board/`: one log per retained topic, plus the retention
table and the seq high-water. **Builder's call:** layout and encoding.
Constraints:

- The format is persisted and read by later versions: numeric enums, not
  names (the WAL's reason, `WALScopeOverride` comment in `server/wal.go`),
  and a version marker.
- Writes are buffered and flushed every few seconds and at shutdown (D8).
- The topic name is stored in the file; the filename is a derived key, since
  topic names may hold characters a filesystem rejects.

### 3.4 Destruction reaches the disk

- `board purge` (one seq or the whole topic) removes the bytes from the log —
  a rewrite, not a tombstone. Today `PurgeSeq` / `topic.removeSeq` scan
  memory only; per D9 they must also find a seq that exists only on disk.
  `BoardPurgeRequest` names the topic, so this is a scan of one log, and no
  seq → topic index is needed. `purge-thread` is client-side
  (`cli/cmd_board.go`) and purges what `board thread` lists, which is the
  ring; a disk-only message is purged by `board purge <topic> --seq` after
  finding it with the paged read. `LookupSeq`'s MaxTopics×RingN bound stays.
- Expiry and `--clear` delete the file.
- `retract` keeps the bytes, as in memory; the withdrawal is persisted as a
  state change of the entry.

### 3.5 Paged history read

`board read <topic> --before <seq> --limit <n>`, newest first, gate
`board_observe` (D11). Rows go on a stream, never inline: every control
message is one datagram and an oversized one is dropped silently over UDP
(`TestResponseRowsAreNotInline`, `runner/protocol/response_rows_test.go`).

### 3.6 The bit

`board_persist`, appended to `enum Capability` in
`runner/protocol/message.bgn` after `runner_enroll = 0x800000`,
default-off, with its reasoning in the enum comment as the neighbours have.
The enum is `:u32` with 24 bits used; this leaves 7.

- `all` (`0xffffff`) widens to include it. Check every use of
  `Capability_All` (`server/capabilities.go` gives it to operators;
  `cli/tasktree.go` iterates bits up to it). The WAL stores caps as numbers:
  a task recorded with the old `all` must **not** gain the bit on replay —
  the enum's rule that an appended bit changes no existing grant.
- `cli/verb/caps.go`: ordering list, `CapDescription`, `capAuditTable`.
  Scope kind `ScopeNone` (it names topics, not tasks). **Builder's call** on
  tier.

### 3.7 `--exclude-self`

Today `Board.deliver` delivers to every matching subscriber including the
publisher (comment above `deliver` in `agentboard/board.go`). On a topic an
agent both sends to and reads, each of its messages comes back to it, and
wakes it; the only guard is the skill's convention "subscribe only to topics
you receive on".

- `AgentSendRequest` and `BoardSendRequest` (`runner/protocol/message.bgn`)
  each gain `exclude_self :u1`, taken from their `reserved :u6` flag bits.
  The board stores it on the entry (`RetainedMessage.ExcludeSelf`), as
  `NoContact` and `NoRetireOnReply` are stored — and, for a retained topic,
  in the disk log.
- `Board.Send` drops the publisher's `taskState` from `targets` before
  `deliver`: no wake, not counted in `delivered_to`.
- `takeUnshown` (the hook's read) and `Inbox` skip entries with
  `ExcludeSelf` whose `FromTask` is the reader. `takeUnshown` still raises the
  mark past them, or the next advance would offer them again.
- Everything else still shows the entry: `agent read <seq>`, `board read`,
  the thread verbs.
- The agent face deliberately cannot express `no_wake` (`BoardSendRequest`
  comment: an agent that suppressed a peer's wake by mistake would read the
  silence as non-delivery). `exclude_self` does not reopen that: it changes
  only what the sender itself receives.
- For an operator with no principal task the flag has nothing to exclude;
  **builder's call** whether to accept it as a no-op or refuse it.
- A client→server request the runner does not decode, so no runner-first
  restart concern; run `scripts/wire-skew-check.sh` anyway.
- Skill (`runner/agentskills/harness-cli/`, the `go:embed` source): the
  "Only subscribe to topics you receive on" section gains one sentence — on a
  topic you both send to and read, send with `--exclude-self`.

## 4. Security notes

- A `board_persist` holder can keep any topic it may publish to `forever` and
  fill the disk at up to `--agentboard-max-payload` (1 MiB) per message.
  Accepted (D4, D5): the bit is default-off and granted deliberately;
  detection is `board topics`.
- Agents may post secrets. On a retained topic they stay on disk for the
  period. `purge` is the remedy and must be tested against the file bytes,
  not the in-memory ring.
- Retention does not widen reach: a retained `tree.<X>.*` is still readable
  only by X's lineage and by `board_observe`.

## 5. Testing

- `--exclude-self`: a publisher subscribed to its own topic gets no wake and
  no hook row, and its mark moves past the entry; a second subscriber gets
  both; `delivered_to` excludes the publisher; `agent read <seq>` and
  `board read` still return it; without the flag a send to one's own
  `chat.<id>` still self-pings.
- Retain → publish 100 → restart → the ring holds the last 64, the paged read
  returns all 100, a re-subscribed task's next hook is handed nothing it had
  already been given.
- Expiry deletes the file; `--clear` deletes it; `purge <seq>` of a disk-only
  message rewrites the log without it (assert on the bytes); `purge` of the
  topic removes the file.
- `chat.<id>` refused; a task without `board_persist` refused; a task with it
  refused on a topic it may not publish to; an operator allowed everywhere.
- MaxTopics never evicts a retained topic.
- Seq: start with a persisted high-water above the clock seed; the next seq
  is above the high-water.
- WAL replay of a task recorded with the old `all` mask does not hold
  `board_persist`.
- The paged read over UDP with enough rows to exceed one datagram, in
  `scripts/netem-lab` (loopback hides the drop).
- A dummy-harness run (`dummy-harness` skill) across a server restart.
