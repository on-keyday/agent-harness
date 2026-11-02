# Agentboard flush and archive Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `board flush` empties a retained topic's ring without touching its disk log; `board archive` freezes a topic's history into a file nothing appends to; both readable and deletable by agents within reach and caps.

**Architecture:** The store (`agentboard/filestore.go`) gains a `ring_floor` log record and an `archive/` subdirectory of frozen logs in the same JSON Lines format. The board (`agentboard/`) gains `Flush`, `Archive*` and the ring refill on retention end. Seven appended `TaskControlKind`s carry them; the CLI verb table, TUI board modal and WebUI board pane expose them.

**Tech Stack:** Go, `.bgn` → `ebm2go` (`make protoregen`), `cli/verb` table + `go generate`, bubbletea TUI, WebUI JS + wasm bridge.

**Spec:** `docs/superpowers/specs/2026-11-02-agentboard-flush-archive-design.md`

## Global Constraints

- Store format: numeric enums, ids as hex, `"v":1` header; a new record type an older binary skips must lose nothing on downgrade.
- Archives live in `<data-dir>/board/archive/`, never directly in `board/` (`sweepOrphans` deletes unnamed `*.log` there).
- Rows go on a stream, never inline (`TestResponseRowsAreNotInline`).
- Every new kind is in `server/cap_completeness_test.go`.
- Agent paths never return withdrawn messages.
- Verification is `make check` / `make test` / `make vet`, run as `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test` inside a task.
- No `go build ./cmd/<x>` that drops a binary in the worktree.

## Decisions this plan takes (the spec's builder's calls)

- **Archive id**: 12 lowercase hex characters from `crypto/rand` (6 bytes), regenerated on the rare collision with an existing file.
- **Kinds** (appended in this order): `board_flush`, `board_archive`, `board_archives`, `board_archive_read`, `board_archive_purge`, `agent_archives`, `agent_archive_read`. New kinds rather than fields on `BoardReadRequest` / `AgentReadPageRequest`, so no existing request changes shape. `agent purge --archive` uses `board_archive_purge`, as `agent purge` uses `board_purge`.
- **Gates**: `board_flush`, `board_archive` → `board_persist` + `PublishableBy` for a task caller. `board_archives`, `board_archive_read` → `board_observe`. `board_archive_purge` → `board_purge`. `agent_archives` (topic required), `agent_archive_read` → no cap, `Board.Readable(st, sourceTopic)`, not_found merged.
- **Archive header carries the count**, so listing reads one line per file. The log-copy path scans the source log first (it needs the withdrawals anyway), so the count is known before the header is written.
- **`through` for a retained topic is taken from the log under the store's lock**, not from the ring: after a flush the ring may be empty while the log is not.
- **CLI shapes**: `board read [<topic>] --archive <id>` and `board purge [<topic>] --archive <id>` (ExactlyOne topic/archive, as `agent read` does with seq/topic/self). With `--archive`, `board read`'s `--limit` defaults to 64.
- **TUI keys** in the board modal: `F` flush, `A` archive, `v` archives view. None is a viewport scroll key (`git_keys_test.go`).

## Review Focus

1. A flushed topic in the TUI/WebUI message view: `O` / the older button must page from the newest on disk when the ring is empty (`OldestSeq()==0` currently returns nil in `tui/overlays.go:444`; WebUI `boardMinSeq([])` is `""`). Tests in Tasks 5 and 6.
2. Flush, then restart, then `--clear`: the refill must read the log ignoring the floor, and the floor must not survive into a later re-retain. Test in Task 2.
3. A publish racing `board archive` on a retained topic: either inside the archive (seq ≤ through) or outside, never partially. Test in Task 1 (store-level, under `s.mu`).
4. `board purge --archive <id>` with `--seq` omitted deletes the whole archive (`WidensIfUnset`, same trap as `board purge`); the verb test must pin both spellings. Test in Task 4.
5. An agent reading an archive of `tree.<Y>.*` outside its lineage gets not_found, and the same answer as a nonexistent id. Test in Task 3.

---

### Task 1: Store — ring floor, refill tail, archives

**Files:**
- Modify: `agentboard/store.go`, `agentboard/filestore.go`
- Create: `agentboard/archive_store.go` (archive half of `FileStore`)
- Test: `agentboard/filestore_test.go`, `agentboard/archive_store_test.go`

**Interfaces:**
- Produces (on `Store` and `*FileStore`):
  - `FlushRing(topic string, through uint64) error` — flush the topic's `bufio.Writer`, `fsync`, append `{"t":"ring_floor","seq":through}`, flush, `fsync`.
  - `Tail(topic string, ringN int) (live, withdrawn []RetainedMessage, err error)` — what `Load` picks, ignoring `ring_floor`.
  - `StoredTopic.RingFloor uint64`; `Load` keeps only seqs `> RingFloor` for `Ring` and `Retracted`.
  - `ArchiveLog(topic string, by protocol.TaskID, at time.Time) (ArchiveInfo, error)` — retained topic: under `s.mu`, flush, `scanMeta`, through = max msg seq, copy `msg`/`retract` records with seq ≤ through. `ErrArchiveEmpty` when the log holds no message.
  - `ArchiveMessages(topic string, msgs []RetainedMessage, by protocol.TaskID, at time.Time) (ArchiveInfo, error)` — snapshot path; `msgs` carry `RetractedAt` etc.
  - `Archives() ([]ArchiveInfo, error)`, `ArchiveOf(id string) (ArchiveInfo, bool, error)`
  - `ArchivePage(id string, beforeSeq uint64, limit int) ([]RetainedMessage, error)` — newest first, withdrawn included.
  - `ArchiveRemove(id string, seq uint64) (removed bool, err error)` — seq 0 deletes the file; otherwise rewrite without it, header count − 1.
  - `type ArchiveInfo struct { ID, Topic string; Through uint64; Count int; CreatedAt time.Time; CreatedBy protocol.TaskID; Bytes int64 }`
  - `var ErrArchiveEmpty = errors.New("agentboard: nothing to archive")`
- Header record: `{"t":"hdr","v":1,"topic":T,"archive":ID,"seq":through,"at_ms":ms,"by_task":hex,"count":n}` — add `Archive string` and `Count int` to `storeRecord`.

- [ ] **Step 1: Write failing tests**
  - `TestFileStore_RingFloorHidesFromLoad`: retain, append seq 1..5, `FlushRing(t,5)`, append 6, close, reopen, `Load(64)` → `Ring` seqs `[6]`, `RingFloor==5`.
  - `TestFileStore_RingFloorSyncsBufferedRecords`: store opened with flush interval 1h, append 3, `FlushRing`, read the file bytes directly → 3 `msg` lines present before `Close`.
  - `TestFileStore_TailIgnoresFloor`: after the above, `Tail(t,64)` live seqs `[1..6]`.
  - `TestFileStore_RingFloorAddsNoMessage`: a `ring_floor` record changes `maxSeq` at most and adds no entry to `meta.msgs` — the property an older binary's switch (no `ring_floor` case) relies on.
  - `TestArchive_FromLogCopiesThrough`: 100 msgs, 3 retracted, `ArchiveLog` → `Count==100`, `Through==seq100`; `ArchivePage(id,0,200)` 100 rows, 3 with non-zero `RetractedAt`; file is under `dir/archive/`.
  - `TestArchive_RacingAppendIsAllOrNothing`: goroutine appending 200 while `ArchiveLog` runs; every archived seq ≤ `Through`, and every log seq ≤ `Through` is in the archive.
  - `TestArchive_SurvivesSweepAndDrop`: `Drop(topic)` then reopen + `Load` (runs `sweepOrphans`) → `Archives()` still lists it.
  - `TestArchive_RemoveSeqRewritesBytes`: remove one seq → payload bytes absent from the file (`bytes.Contains`), header count 99. `ArchiveRemove(id,0)` → file gone.
  - `TestArchive_Empty`: `ArchiveLog` on an empty log → `ErrArchiveEmpty`, no file created.
- [ ] **Step 2: Run** `go test ./agentboard -run 'RingFloor|Tail|Archive' -v` → FAIL (undefined).
- [ ] **Step 3: Implement** the interface above. `scanMeta` gains a `ring_floor` case setting `m.ringFloor` (last one wins); `Truncate` drops it with everything else; `Remove` copies it through. Archive files are written with `rewriteFile` (tmp + rename + fsync).
- [ ] **Step 4: Run** the same command → PASS; `go test ./agentboard` → PASS.
- [ ] **Step 5: Commit** `agentboard: store ring floor, log tail and archives`.

### Task 2: Board — Flush, refill on retention end, Archive*

**Files:**
- Modify: `agentboard/topic.go` (field `ringFloor uint64`, guarded by `Board.mu`), `agentboard/retention.go`, `agentboard/board.go` (`evictExpiredTopics`)
- Create: `agentboard/archive.go`
- Test: `agentboard/flush_test.go`, `agentboard/archive_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces:
  - `var ErrNoTopic = errors.New("agentboard: no such topic")`, `var ErrNotRetained = errors.New("agentboard: topic is not retained")`
  - `func (b *Board) Flush(topic string) (through uint64, dropped int, err error)` — `ErrNoTopic` / `ErrNotRetained`; under `t.mu`: through = max seq of ring ∪ retracted (0 when both empty → still writes nothing and returns ok with dropped 0), remove entries ≤ through from both; then `store.FlushRing(topic, through)` with `b.mu` released; set `t.ringFloor`.
  - `func (b *Board) RingFloor(topic string) uint64`
  - `AttachStore` sets `t.ringFloor = st.RingFloor`, and raises the seq above `max(ArchiveInfo.Through)` of `Archives()` (spec D19).
  - Refill: `ClearRetain` and the expiry branch of `evictExpiredTopics` call `b.refill(name, t, store)` with `b.mu` released, before `store.Drop`, when the floor was non-zero: `Tail(name, RingN)`, merge by seq into `ring` / `retracted`, keep the newest `RingN` of each, `ringFloor = 0`.
  - `func (b *Board) Archive(topic string, by protocol.TaskID) (ArchiveInfo, error)` — `ErrNotRetainable` (reuse `Retainable`), `ErrNoStore`, `ErrNoTopic`, `ErrArchiveEmpty`; retained → `store.ArchiveLog`, else snapshot ring + retracted → `store.ArchiveMessages`.
  - `func (b *Board) Archives(topic string) ([]ArchiveInfo, error)` — `""` = all; sorted by `CreatedAt` desc.
  - `func (b *Board) ArchiveOf(id string) (ArchiveInfo, bool, error)`
  - `func (b *Board) ArchivePage(id string, beforeSeq uint64, limit int) ([]RetainedMessage, error)`
  - `func (b *Board) ArchivePageLive(id string, beforeSeq uint64, limit int) ([]RetainedMessage, error)` — `PageLive`'s refill loop over `ArchivePage`.
  - `func (b *Board) ArchivePurge(id string, seq uint64) (removed, found bool, err error)`

- [ ] **Step 1: Write failing tests** (use `newRetainBoard` / `publish` from `retention_test.go`)
  - `TestFlush_EmptiesRingKeepsLog`: retain, publish 100, retract 1 → `Flush` returns through = last seq, dropped 64+1; `ListRetained` empty, `ListRetracted` empty; `Page(t,0,200)` 100 rows; `PageLive` 99.
  - `TestFlush_NextPublishDelivered`: subscribed task; flush; publish → `InboxAdvance` returns exactly the new one.
  - `TestFlush_RefusesUnretained`: `ErrNotRetained`; unknown → `ErrNoTopic`.
  - `TestFlush_SurvivesRestart`: flush, publish 1, close store, new board + `AttachStore` → ring holds only the new one, `RingFloor` = through.
  - `TestFlush_ClearRefillsRing`: flush then `ClearRetain` → `ListRetained` holds the newest 64 of the log, file gone, `RingFloor`==0.
  - `TestFlush_ExpiryRefillsRing`: TTL 1ms retention, flush, run `evictExpiredTopics` after the period → same as above; topic still present (board TTL counts from last publish).
  - `TestFlush_RestartThenClear`: flush, restart, clear → refill works from the reloaded floor.
  - `TestArchive_RetainedAndPlain`: retained topic → archive count = log count; plain topic → count = ring + retracted; topic unchanged in both (`ListRetained` same before/after).
  - `TestArchive_RefusesChatAndMissing`: `chat.x` → `ErrNotRetainable`; missing → `ErrNoTopic`; empty plain topic → `ErrArchiveEmpty`.
  - `TestArchive_OutlivesSourceDestruction`: archive, then `PurgeTopic`, `ClearRetain` → `ArchivePage` unchanged.
  - `TestArchive_SeqSeedAboveArchive`: archive with through = X on a board seeded at X+0; new board with `SeqSeed` 1 + `AttachStore` → next publish seq > X.
  - `TestArchivePageLive_SkipsWithdrawn`.
- [ ] **Step 2: Run** `go test ./agentboard -run 'Flush|Archive' -v` → FAIL.
- [ ] **Step 3: Implement.** Lock order stays `b.mu → t.mu`; store calls happen with `b.mu` released, as `ClearRetain` already does.
- [ ] **Step 4: Run** `go test -race ./agentboard` → PASS.
- [ ] **Step 5: Commit** `agentboard: flush a retained ring; archive a topic`.

### Task 3: Wire + server handlers

**Files:**
- Modify: `runner/protocol/message.bgn` (all formats below, in ONE edit), regenerate with `make protoregen`
- Modify: `server/capabilities.go`, `server/task_handler.go` (dispatch), `server/board_handler.go` (`handleBoardTopics`: `RingFloor`)
- Create: `server/board_archive_handler.go`
- Modify: `server/agent_taskcontrol.go` (agent handlers), `server/cap_completeness_test.go`
- Modify: spec §5.1 / §6 to name the kinds and CLI shapes this plan decided (one paragraph each)
- Test: `server/board_archive_test.go`

**Wire (append after `agent_read_page`; every format in this one task):**

```
board_flush / board_archive / board_archives / board_archive_read /
board_archive_purge / agent_archives / agent_archive_read

format BoardFlushRequest:      topic_len :u16, topic
enum   BoardFlushStatus:       ok=0 not_found=1 not_retained=2 not_publishable=3 store_error=4
format BoardFlushResponse:     status :BoardFlushStatus, through_seq :u64, dropped :u16

format BoardArchiveRequest:    topic_len :u16, topic
enum   BoardArchiveStatus:     ok=0 not_found=1 not_archivable=2 not_publishable=3 empty=4 store_error=5
format BoardArchiveResponse:   status, id_len :u8, id, through_seq :u64, count :u32

format BoardArchivesRequest:   request_id :u32, topic_len :u16, topic   # empty = all
format BoardArchiveRow:        id_len :u8, id, topic_len :u16, topic, through_seq :u64,
                               count :u32, created_at_unix_ms :u64, created_by :TaskID, disk_bytes :u64
format BoardArchivesResponse:  request_id :u32, status :BoardStatus, stream_id :u64
format BoardArchivesBody:      rows_len :u16, rows :[rows_len]BoardArchiveRow

format BoardArchiveReadRequest:  request_id :u32, id_len :u8, id, before_seq :u64, limit :u16
  → response arm board_archive_read :BoardReadResponse (body BoardReadBody)
format BoardArchivePurgeRequest: request_id :u32, id_len :u8, id, seq :u64
  → response arm board_archive_purge :BoardPurgeResponse

format AgentArchivesRequest:   request_id :u32, topic_len :u16, topic   # required
  → response arm agent_archives :BoardArchivesResponse
format AgentArchiveReadRequest: request_id :u32, id_len :u8, id, before_seq :u64, limit :u16
  → response arm agent_archive_read :AgentReadPageResponse (body DeliveredBody)

BoardTopicRetention: append ring_floor :u64   # 0 = never flushed
```

Each kind gets an enum comment in the house style (gate + spec path), as `board_retain` has.

**Interfaces:**
- Consumes: Task 2.
- Produces: `protocol.TaskControlKind_Board{Flush,Archive,Archives,ArchiveRead,ArchivePurge}`, `protocol.TaskControlKind_Agent{Archives,ArchiveRead}` and the request/response types above (generated names).

- [ ] **Step 1: Edit `message.bgn`, run `make protoregen`, `go build ./...`** → builds (handlers absent).
- [ ] **Step 2: Write failing tests** in `server/board_archive_test.go` (follow `server/board_retain_test.go`'s board + store setup):
  - `TestBoardFlush_TaskNeedsPublishable`: task caller without reach to `tree.<Y>.x` → `not_publishable`; operator → ok with through/dropped.
  - `TestBoardFlush_NotRetained`.
  - `TestBoardArchive_StatusMapping`: chat → not_archivable, missing → not_found, empty → empty, ok → id of 12 hex chars.
  - `TestBoardArchives_RowsOnStream` and `TestBoardArchiveRead_Paging` (newest first, withdrawn included).
  - `TestAgentArchiveRead_ReachMergedNotFound`: an archive of `tree.<Y>.d` read by a task outside Y's lineage → not_found; a nonexistent id → the same status, no stream; a task in the lineage → ok, withdrawn absent.
  - `TestAgentArchives_RequiresTopic`: empty topic → not_found.
  - `TestBoardArchivePurge_SeqAndWhole`.
  - `TestBoardTopics_RingFloor`: after flush the retention row's `RingFloor` = through.
  - `cap_completeness_test.go`: add the 7 kinds (`capInMap` for the five board kinds, `capNone` for the two agent kinds) and move the loop bound to `TaskControlKind_AgentArchiveRead`.
- [ ] **Step 3: Run** `go test ./server -run 'BoardFlush|BoardArchive|AgentArchive|RingFloor|CapCompleteness' -v` → FAIL.
- [ ] **Step 4: Implement** handlers. Flush/archive take `by := h.lookupPrincipal(cid)` as `board_retain` does; the agent handlers take `st := h.boardState(conn)` and resolve the source topic with `Board.ArchiveOf`, then `Board.Readable(st, info.Topic)`. Bodies go through `respondWithBody`.
- [ ] **Step 5: Run** step 3's command, then `go test -race ./server ./agentboard` → PASS; `scripts/wire-skew-check.sh` → exit 0.
- [ ] **Step 6: Commit** `wire, server: board flush and archive kinds`.

### Task 4: CLI client + verbs (CLI surface, agent face)

**Files:**
- Create: `cli/board_archive.go` (client methods + `BoardArchiveRow` + its `Summary()`), `cli/agent/archives.go`
- Modify: `cli/board.go` (`TopicRetention.RingFloor`, `Summary()` appends ` flushed≤<seq>` when non-zero, decode `ring_floor`), `cli/verb/table.go`, regenerate (`go generate ./cli/verb`), `cli/cmd_board.go`, `cli/agent/read.go`, `cli/agent/purge.go`, `cmd/harness-cli/dispatch.go` if the new agent sub needs routing
- Modify: `runner/agentskills/harness-cli/` (the `go:embed` source): one paragraph under the history/read section on `agent read --archive` and `agent archives --topic`
- Test: `cli/board_archive_e2e_test.go` (in-process server, as `cli/board_retain_e2e_test.go`)

**Interfaces:**
- Consumes: Task 3.
- Produces (used by TUI/WebUI/wasm):
  - `func (c *Client) BoardFlush(ctx context.Context, topic string) (through uint64, dropped int, err error)`
  - `func (c *Client) BoardArchive(ctx context.Context, topic string) (BoardArchiveRow, error)`
  - `func (c *Client) BoardArchives(ctx context.Context, topic string) ([]BoardArchiveRow, error)`
  - `func (c *Client) BoardArchiveRead(ctx context.Context, id string, beforeSeq uint64, limit int) ([]BoardMessage, bool, error)` — bool = found
  - `func (c *Client) BoardArchivePurge(ctx context.Context, id string, seq uint64) (purged int, found bool, err error)`
  - `type BoardArchiveRow struct { ID, Topic string; Through uint64; Count int; CreatedAt time.Time; CreatedBy protocol.TaskID; Bytes int64 }`, `func (r BoardArchiveRow) Summary() string` — `"<id> topic=<t> through=<seq> msgs=<n> by=<setter> disk=<bytes> at=<RFC3339>"`, one spelling for all surfaces; `by` via the same short-id/"operator" rule as `TopicRetention.SetByLabel`.

Verbs (`cli/verb/table.go`), each with `ModalSurfaces` naming the TUI key and WebUI element from Tasks 5/6:
- `board flush <topic>`; `board archive <topic>`; `board archives [--topic T] [--json]`
- `board read`: `topic` becomes `Optional`; add `--archive` (FlagString); `ExactlyOne{topic, archive}`; `--in-reply-to` refused with `--archive`.
- `board purge`: `topic` `Optional`; add `--archive`; `ExactlyOne{topic, archive}`; `--seq` keeps `WidensIfUnset`.
- `agent archives --topic T [--json]` (`--topic` required); `agent read`: add `--archive` to the `ExactlyOne`; `agent purge`: add `--archive` to the `Exclusive` set.

- [ ] **Step 1: Write failing e2e tests:**
  - `TestBoardFlushCLI`: `board flush <t>` prints `flushed <t> through=<seq> dropped=<n>`; `board topics` line contains `flushed≤<seq>`; `board read <t>` prints the hint `older messages are on disk: board read <t> --limit 64` after the rows.
  - `TestBoardArchiveCLI`: `board archive` prints the id; `board archives` lists it with `Summary()`; `board read --archive <id>` prints the messages; `board purge --archive <id> --seq <s>` purges 1; `board purge --archive <id>` removes it.
  - `TestBoardPurgeArchiveFlagOrder`: `board purge --archive <id> --seq 5` and `board purge --seq 5 --archive <id>` both purge exactly one (Review Focus 4).
  - `TestAgentArchiveCLI`: from an agent context with no caps: `agent archives --topic T` and `agent read --archive <id>` succeed on a readable topic; `agent purge --archive <id>` → permission denied.
- [ ] **Step 2: Run** `go test ./cli -run 'Archive|Flush' -v` → FAIL.
- [ ] **Step 3: Implement**; `go generate ./cli/verb`.
- [ ] **Step 4: Run** `go test ./cli/... ./cmd/...` → PASS (the verb wiring/help/surface-notes tests included).
- [ ] **Step 5: Commit** `cli: board flush / archive verbs, agent archive reads`.

### Task 5: TUI

**Files:**
- Modify: `tui/keys.go` (`BoardFlush: "F"`, `BoardArchive: "A"`, `BoardArchives: "v"`, each with its reason comment), `tui/overlays.go`, `tui/app.go`, `tui/board.go`
- Create: `tui/board_archive.go` (`DoBoardFlush`, `DoBoardArchive`, `DoBoardArchives`, `DoBoardArchiveRead`, `DoBoardArchivePurge` over `a.client`, as `DoBoardRetain`; the archives view)
- Test: `tui/board_archive_test.go`, `tui/git_keys_test.go` stays green

Behaviour:
- `F` on a retained topic (list row or open topic) asks y/n, then flushes; on an unretained one the status says why it is not offered. The footer offers `F` only on a retained topic.
- `A` archives the highlighted/open topic; status `archived <topic> → <id> (<n> msgs)`.
- `v` opens the archives list (all, or the open topic's): rows from `Summary()`; Enter reads one (message view, `O` pages older via `DoBoardArchiveRead`); `x` deletes the archive (y/n); `X` deletes the selected message (y/n).
- `O` fix: `if seq := OldestSeq(); seq != 0 || len(msgs) == 0` → page with `seq` (0 = newest on disk).
- The Retain cell / detail line shows `flushed≤<seq>` through `TopicRetention.Summary()`.

- [ ] **Step 1: Write failing tests:** `TestBoardOlder_EmptyRingPagesFromNewest`, `TestBoardFlushKey_OnlyOnRetained`, `TestBoardArchivesView_EnterReadsAndXDeletes` (feed `tea.KeyMsg`s to the App with a fake client as the existing board tests do).
- [ ] **Step 2: Run** `go test ./tui -run 'BoardOlder|BoardFlush|BoardArchive' -v` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./tui` → PASS.
- [ ] **Step 5: Commit** `tui: flush, archive and the archives view`.

### Task 6: WebUI + wasm bridge

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (`boardFlush(topic)`, `boardArchive(topic)`, `boardArchives(topic)`, `boardArchiveRead(id, beforeSeq, limit)`, `boardArchivePurge(id, seq)`; `boardTopics` rows gain `retainFlushed` (decimal string))
- Modify: `webui/index.html` (`#board-flush-btn` beside the retain controls, hidden unless retained; `#board-archive-btn`; `#board-archives-btn` opening `#board-archives-view`), `webui/static/main.js`
- Test: `webui/static/board_archive_test.mjs` (add to the `js-test` list in `Makefile`), Playwright run

Behaviour mirrors Task 5. Older-button fix: when `r.msgs` is empty, `oldest` is `"0"`, not `""`.

- [ ] **Step 1: Write failing tests:** `boardArchiveRead refuses a cursor that is not a seq`, `the older button pages from 0 on an empty ring` (`boardMinSeq` + the click path), `boardArchiveSummary` matches the CLI spelling for one fixture row.
- [ ] **Step 2: Run** `make js-test` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make js-test` → PASS; Playwright (`webui/INDEX.md` procedure) at 1280 and 390: flush a retained topic, page older, archive, open the archive, delete a message. Screenshots kept as deliverables.
- [ ] **Step 5: Commit** `webui: flush, archive and the archives view`.

### Task 7: Live checks, then land

- [ ] `surface-parity-checklist`: walk items for the 7 kinds, the `ring_floor` column and the new verbs; record verdicts in the commit message of any fix it produces.
- [ ] `scripts/dummy-harness.sh`: retain a topic, publish 80, `board flush`, restart the server, `board topics` (flushed≤), `agent read --topic` from a caps=none task, `board archive`, `agent read --archive`, `board retain --clear` (ring refilled), `board archives` still lists it. TUI in a dummy PTY: `F`, `A`, `v`, `O`.
- [ ] `scripts/netem-lab`: `board archives` and `board read --archive --limit 500` over UDP return every row.
- [ ] `make check`, `make vet`, `make test` (env-unset form above) → green.
- [ ] Land: FF-push the task branch to `origin/main`, `merge --ff-only` local main, `make build` in the main checkout.
