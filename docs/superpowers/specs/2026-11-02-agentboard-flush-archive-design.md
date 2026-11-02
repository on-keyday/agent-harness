# Agentboard flush and archive — Design

Date: 2026-11-02

Two operations on a topic's history, independent of each other:

- `harness-cli board flush <topic>`: a retained topic's in-memory ring is
  emptied. Its disk log keeps everything; later publishes fill the ring again.
- `harness-cli board archive <topic>`: the topic's history up to now is copied
  into an archive that is never appended to and is kept until it is deleted.

Builds on the retention design
(`docs/superpowers/specs/2026-10-30-agentboard-retention-design.md`, "the
retention spec" below) and on `agent read --topic`
(`docs/superpowers/specs/2026-10-31-agent-read-history-design.md`).

---

## 1. Problem

A retained topic always holds its newest `RingN` (64) messages in memory, and
a restart refills the ring from the log (`FileStore.Load`). The ring is what
every agent read path sees: the hook (`Board.InboxAdvance`), `agent inbox`
(`Board.Inbox`) and `agent wait` (`Board.Wait`) read `topic.ring` only. On a
long-lived topic there is no way to say "what came before is history now":
the last 64 stay in every agent's view until 64 newer messages push them out.

Separately, there is no way to keep a topic's history as a fixed record. A
retained topic's log changes on every publish, retract and purge, and ends
with the retention (expiry or `--clear` deletes it). A non-retained topic's
history exists only in memory.

## 2. Goals, non-goals

Goals:

- Empty a retained topic's ring on demand, without losing anything from disk
  and without the ring coming back on restart.
- Fix a topic's history up to now into an archive that nothing appends to,
  kept until it is deleted.
- Agents and the operator can list archives, read them, see who made them
  and how much disk each uses, and delete them or single messages in them,
  each within the gates of §3.

Non-goals:

- Changing what the ring is: it stays the source for every agent delivery
  path. Flush is a one-shot operation, not a ring size.
- Widening the by-seq operations (`agent read <seq>`, `in_reply_to`
  resolution, retract, reply-retire, thread verbs) to the disk log or to
  archives. They stay ring-only (retention spec D9).
- Archiving `chat.<id>` / `chat.<id>.*` / `chat.operator`.

## 3. Decisions

`Decided-by` is provenance; `author` rows were proposed by this task and not
explicitly ruled on.

| # | Decision | Decided-by |
|---|---|---|
| 1 | Both operations are built; archive does not replace flush | operator, 2026-11-02 |
| 2 | Flush is gated by `board_persist`, plus `Publishable(topic)` when the caller has a principal task — the same gate as `board retain` | operator, 2026-11-02 |
| 3 | Flushed messages leave the ring, so the by-seq operations no longer find them (D9 unchanged). `agent read --topic` reaches them | author (operator: either way is fine, 2026-11-02) |
| 4 | Flush is refused on a topic that is not retained: it has no disk copy, so a flush would be a purge | author |
| 5 | Flush takes no account of delivery marks. `topic.shown` moves only for agents running the `--user-prompt-submit-hook` inbox hook; for every other agent it never moves, so no mark says what was read. An agent that had not read a flushed message reaches it with `agent read --topic` | operator, 2026-11-02 |
| 6 | Flush empties the withdrawn list as well as the live ring | author |
| 7 | Flush is persisted: a restart does not refill the ring with what was flushed | author |
| 8 | Flush writes the store's buffered records out and syncs the log before it answers ok | author |
| 9 | When a flushed topic's retention ends (expiry or `--clear`), the ring is refilled from the log tail before the log is deleted, so the retention spec §3.2 rule — expiry never deletes messages itself — still holds | author |
| 10 | An archive is a separate file that nothing appends to | operator, 2026-11-02 |
| 11 | An archive is kept until it is explicitly deleted; it has no TTL | operator, 2026-11-02 |
| 12 | Archiving does not change the topic. Emptying the ring is flush; destroying bytes is purge | author |
| 13 | Archive creation is gated like flush (D2). Refused on `chat.*` (retention spec D3) and on an empty name | author |
| 14 | Agents may read archives | operator, 2026-11-02 |
| 15 | Nothing in this spec is operator-only. Every gate is a capability a task can be granted or a reach rule; operator-only is kept for changing authority itself (re-grant) | operator, 2026-11-02 |
| 16 | Reading an archive by id: on the agent face, no capability on the kind; a per-request check that the caller may read the source topic (reach `Readable`), with the same merged not_found as `agent read`. On the operator face, `board_observe`, like `board read` | author |
| 17 | Listing archives: keyed by a topic the caller names, the same per-request `Readable` check and no capability. Unkeyed, it lists topic names the caller did not name — discovery, gated by `board_observe` as `agent topics` is | author |
| 18 | Deleting an archive or a message in it is gated by the purge capability, on both faces, as `purge` / `board purge` are | author |
| 19 | Archive seqs count toward the seq high-water the board seeds from | author |

## 4. Flush

### 4.1 Interface

```
harness-cli board flush <topic>
```

Statuses: ok (with the flushed-through seq and the count dropped from
memory), not_found, not_retained (D4), not_permitted (D2), store_error.

### 4.2 Semantics

- X is the highest seq in `ring` ∪ `retracted`. The store syncs the log and
  records X (D8), and only then do entries with seq ≤ X leave both lists. A
  publish appended in between has a seq above X and stays in the ring.
- A flush of a topic whose ring and withdrawn list are both empty changes
  nothing and answers the current floor: writing a floor of 0 would undo an
  earlier flush on the next restart.
- The topic records X as its ring floor (`topic.ringFloor`, guarded like
  `retention` by the Board's `mu`). Nothing below the floor re-enters the ring
  while the topic stays retained.
- D8: the store flushes its `bufio.Writer` for the topic and `fsync`s the log
  before it writes the floor record, and syncs again after. The batched-write
  trade-off (retention spec D8) is otherwise unchanged.
- An empty ring is not a reason to evict: retained topics are never evicted
  by time or by `MaxTopics` (retention spec §3.2).
- D9: on expiry or `--clear`, the board refills the ring with the newest
  `RingN` live and `RingN` withdrawn messages of the log (what `Load` would
  pick ignoring the floor), merged with what the ring holds, and only then
  drops the log. Both paths call `store.Drop` today with `b.mu` released;
  the refill reads the log on the same side of the lock.
- `board purge <topic>` (`Truncate`) and `board purge <topic> --seq` work
  as before. Truncate drops the floor record with everything else.

### 4.3 Storage

A new log record, `{"t":"ring_floor","seq":X}`. `Load` keeps for the ring
only seqs above the newest `ring_floor`. `Remove` copies the record through
like any other line.

An older server binary skips unknown record types (`scanMeta`'s switch), so a
downgrade refills the ring on its next start and loses nothing.

### 4.4 Display

- `BoardTopicRetention` (`board topics`) gains the floor seq; every surface
  prints it next to `retain` / `by` / `disk` / `until`, as `flushed≤<seq>`.
- `board read <topic>` without `--limit` reads the ring and withdrawn list
  only (`server/board_handler.go`, `handleBoardRead`). On a flushed topic the
  CLI prints, after the rows, that older messages are on disk and the
  `--before`/`--limit` form to page them.
- TUI (`DoBoardReadOlder`) and WebUI (`webui/static/main.js`, covered by
  `board_older_test.mjs`) already page older messages. Check that the control
  is offered when the ring is **empty**, where there is no oldest ring seq to
  page from: `Page` with `beforeSeq` 0 returns the newest on disk, which are
  the flushed messages.
- Flush is offered only on a retained topic, in all three UIs.

## 5. Archive

### 5.1 Interface

```
harness-cli board archive <topic>                                  # create
harness-cli board archives [--topic <topic>]                       # list
harness-cli board read --archive <id> [--before <seq>] [--limit <n>]  # in place of <topic>
harness-cli board purge --archive <id> [--seq <seq>]               # in place of <topic>; delete

harness-cli agent archives --topic <topic>                         # D17, keyed
harness-cli agent read --archive <id> [--before <seq>] [--limit <n>]  # D16
harness-cli agent purge --archive <id> [--seq <seq>]               # D18
```

`agent archives` takes a topic always. The unkeyed list (D17's discovery
half) is `board archives`, which a task holding `board_observe` can call, as
`agent topics` and `board topics` share a gate. `board read --archive` pages
64 at a time unless `--limit` says otherwise.

`board archive` and `board flush` are callable from a task that holds
`board_persist`, as `board retain` is (retention spec D7).

D16 checks `Readable`, not `Board.Subscribes` as `agent read --topic` does.
An archive outlives its topic, and requiring a subscription would make a task
subscribe — and be woken by every later publish — to read history. `Readable`
is exactly what a subscribe would be checked against, so it reaches nothing a
subscribe + `agent read --topic` could not have.

- The list shows, per archive: id, source topic, through-seq, message count,
  created at, by (task short id or `operator`), bytes on disk. As with
  retention (retention spec D5), this display is how misuse is found.
- Create statuses: ok (with the id), not_found, not_archivable (D13),
  not_permitted, empty (the topic holds no messages), store_error.
- Rows go on a stream, never inline (`TestResponseRowsAreNotInline`).

### 5.2 Semantics

- Content: every message of the topic with seq ≤ X, live and withdrawn, with
  each withdrawal as it stood at the call. X is the topic's highest seq at the
  call. Delivery marks and memberships are not copied: they describe the live
  topic's readers, not its content.
- From a retained topic, the source is the log: the store flushes it, then
  streams the `msg` and `retract` records with seq ≤ X into the archive.
  From a topic that is not retained, the source is a snapshot of `ring` and
  `retracted`.
- After creation nothing writes the archive except a purge, which rewrites
  it without the purged message, or deletes the file.
- Expiry, `--clear`, purge and flush of the source topic do not touch an
  archive (D12). A secret posted to a topic and archived must be purged in
  both places; `board archives --topic` finds the archives to check.

### 5.3 Storage

`<data-dir>/board/archive/<id>.log`, one file per archive, written to a
temporary file and renamed. The log format of `FileStore` (JSON Lines, enums
as numbers, ids as hex, a version marker). The header record carries the
source topic name, the id, X, the creation time and the creator, so the
directory is the index and there is no second file to keep in step.

The archive directory must stay a subdirectory: `sweepOrphans` deletes every
`*.log` directly under `board/` that `retention.json` does not name, and
skips directories.

The id is 12 lowercase hex characters from `crypto/rand`, drawn again if a
file by that name exists. Not derived from the topic name, which may hold
characters a filesystem rejects; checked against that form before any path
is built from a request.

D19: `AttachStore` raises the seq above the highest seq of every archive too,
so a boot whose clock seed is behind cannot reuse a seq an archive holds.

## 6. Wire

Seven `TaskControlKind`s, appended: `board_flush`, `board_archive`,
`board_archives`, `board_archive_read`, `board_archive_purge`,
`agent_archives`, `agent_archive_read`. New kinds rather than an archive id
on `BoardReadRequest` / `AgentReadPageRequest`, so no existing request changes
shape. The reads answer with the existing `BoardReadResponse` /
`AgentReadPageResponse` and their bodies; `agent purge --archive` uses
`board_archive_purge`, as `agent purge` uses `board_purge`.
`BoardTopicRetention` gains `ring_floor`. Every new kind is listed in
`server/cap_completeness_test.go` with the gate D15–D18 give it. Client→server
requests the runner does not decode, so no runner-first restart concern; run
`scripts/wire-skew-check.sh` anyway.

Walk `.claude/skills/surface-parity-checklist` for every new verb and the
new `board topics` column.

## 7. Security notes

- A `board_persist` holder can fill the disk with archives at up to
  `--agentboard-max-payload` per message times the topic's history, and they
  outlive the topic. Accepted with the same reasoning as retention (retention
  spec §4): the bit is default-off, the list shows creator and bytes, and
  `board_purge` deletes.
- Purge must be tested against the archive's file bytes, not a listing.
- Reading an archive does not widen reach: an archive of `tree.<X>.*` is
  readable by X's lineage and by `board_observe`, as the topic is. Reach is
  evaluated at read time against the source topic's name, so a task that
  loses reach loses the archive too.

## 8. Testing

Flush:

- Retain, publish 100, flush: the ring and withdrawn list are empty; `Page`
  returns all 100; `agent read --topic` returns them; `agent read <seq>` of a
  flushed seq is not found; a reply to it is refused.
- Publish after a flush lands in the ring and is delivered as usual.
- Flush, restart: the ring holds only messages published after the flush.
- Flush, then `--clear` (and separately, expiry): the ring holds the log's
  newest `RingN` and the log file is gone.
- Flush of a topic that is not retained is refused; a task without
  `board_persist`, or without `Publishable`, is refused.
- Flush answers ok only after the log holds every message published before it
  (assert on the file, with a flush interval long enough that the batch has not
  run).
- An older binary's `Load` of a log with `ring_floor` loads the ring as before.
- TUI and WebUI: an empty-ring flushed topic offers older paging and shows the
  flushed messages (Playwright for WebUI, a dummy PTY for TUI).

Archive:

- Archive a retained topic of 100 with 3 withdrawn: the archive holds 100,
  the 3 marked withdrawn; the topic is unchanged.
- Archive a topic that is not retained: the archive holds its ring and
  withdrawn list.
- A publish racing the archive is either in it (seq ≤ X) or not, never half.
- After the source topic's `--clear`, expiry, purge and flush, the archive
  still reads the same.
- `purge --archive <id> --seq` rewrites the file without the message (assert
  on the bytes); `purge --archive <id>` removes the file.
- `chat.*` and an empty name are refused; a task without `board_persist` is
  refused.
- Agent face, from a task with no capabilities: `agent read --archive` and
  `agent archives --topic` succeed for an archive of a topic it may read and
  answer not_found for one of a `tree.<Y>.*` outside its lineage;
  `agent archives` without `--topic` is refused (the unkeyed list is
  `board archives`, `board_observe`);
  `agent purge --archive` is refused without the purge capability.
- Restart: archives are listed and readable; the seq seeds above the highest
  archived seq.
- The archive list and read over UDP with enough rows to exceed one datagram,
  in `scripts/netem-lab`.
- A dummy-harness run (`dummy-harness` skill) across a server restart.
