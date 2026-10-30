# Agentboard topic retention + `--exclude-self` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator (or a task holding `board_persist`) keep an agentboard topic past its TTL and a server restart, and let a sender exclude itself from delivery of its own publish.

**Architecture:** A `Store` interface in `agentboard` persists retained topics (one append log per topic under `<data-dir>/board/`); `Board` consults a per-topic `Retention` for eviction and mirrors sends / retracts / purges / delivery marks of retained topics into the store. A new operator-face kind `board_retain` (gated by the new bit `board_persist`) sets retention; `board topics` and `board read` grow the retention columns and paging. `exclude_self` is a flag bit on both send requests, stored on the entry like `no_contact`.

**Tech Stack:** Go; wire schema `runner/protocol/message.bgn` regenerated with `make protoregen ARGS='runner/protocol/message.bgn'`; CLI verb table regenerated with `go generate ./cli/verb`.

**Spec:** `docs/superpowers/specs/2026-10-30-agentboard-retention-design.md`

## Global Constraints

- Retention is refused on `chat.<id>`, `chat.<id>.*` and `chat.operator` (any name starting `chat.`), and on an empty name.
- A retention period counts from the topic's last publish (from the retain call when nothing was published since); `forever` is never evicted by time.
- Expiry, `retain --clear` and `board purge` of a whole topic delete the disk file; `board purge --seq` rewrites the log without that message (bytes gone, not tombstoned). `retract` keeps the bytes.
- Retained topics are skipped by MaxTopics eviction; if every topic is retained, the new-topic publish fails with `ErrTooManyTopics`.
- In-memory ring size stays `RingN` (`--agentboard-ring`, 64).
- Disk writes buffered, flushed every 2 s and on `Close`.
- Seq seed at start = `max(wall-clock seed, highest seq found in the logs) + 1` when logs hold anything.
- A task recorded in the WAL with the old `all` mask (`0xffffff`) must not hold `board_persist` after replay.
- Every list response sends its rows on a stream, never inline (`TestResponseRowsAreNotInline`).
- `exclude_self` changes only what the publisher itself receives; publishing without it behaves exactly as today (a send to one's own `chat.<id>` still self-pings).
- Tests that start a server or client run with `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID` (inherited task env breaks them).
- Skill text lives in `runner/agentskills/harness-cli/SKILL.md` (the `go:embed` source); `.claude/skills/harness-cli/SKILL.md` is currently byte-identical — keep it so. No repo-local paths in skill text.

## Review Focus

1. **Topic names that are not filesystem-safe** (`a/b`, `..`, 300 bytes long) — retain must work; the filename is derived, the name stored inside. Test in Task 3.
2. **Crash without `Close`** leaves a half-written last line — load must skip the torn record and start, not refuse to start. Test in Task 3.
3. **Retain a topic, then nothing is published** — it must expire `ttl` after the retain call, not immediately (its `lastPublishedAt` is zero). Test in Task 4.
4. **`board purge <topic>` (whole topic) on a retained topic** — content and file go; the retention setting stays, so a later publish lands in a retained topic again. Test in Task 4.
5. **Clock behind at boot** — a wall-clock seed below the highest persisted seq must not reuse seqs. Test in Task 4.

---

### Task 1: `--exclude-self`

**Files:**
- Modify: `runner/protocol/message.bgn` (`AgentSendRequest` and `BoardSendRequest`: `exclude_self :u1`, `reserved :u6` → `:u5`, comment states it affects only the sender's own delivery), regenerate
- Modify: `agentboard/topic.go` (`RetainedMessage.ExcludeSelf`, `takeUnshown`), `agentboard/board.go` (`sendConfig.excludeSelf`, `WithExcludeSelf`, `Send`, `Inbox`)
- Modify: `server/agent_taskcontrol.go` (`handleAgentSend`), `server/board_handler.go` (`boardPublish` gains `excludeSelf bool`, `handleBoardSend`)
- Modify: `cli/verb/table.go` (`exclude-self` flag on `agent send` and `board send`, help line), `cli/agent/send.go`, `cli/board.go` (`BoardSendParams.ExcludeSelf`), `cli/cmd_board.go`; `go generate ./cli/verb`
- Modify: both `harness-cli/SKILL.md` copies — "Only subscribe to topics you receive on" gains: on a topic you both send to and read, send with `--exclude-self`
- Test: `agentboard/exclude_self_test.go`, `cli/agent/exclude_self_e2e_test.go`

**Interfaces:**
- Produces: `func WithExcludeSelf() SendOption`; `RetainedMessage.ExcludeSelf bool` (Task 3 persists it); `boardPublish(by, topic, payload, inReplyTo, replyTo, noRetire, noWake, excludeSelf bool)`

- [ ] **Step 1: Write the failing unit tests** in `agentboard/exclude_self_test.go`:
  - `TestExcludeSelf_PublisherNotWokenNorCounted`: tasks A and B both subscribe `t`; A sends with `WithExcludeSelf()`; `deliveredTo == 1`; the `SetOnDeliver` callback fired for B only.
  - `TestExcludeSelf_HookSkipsOwnRowAndAdvancesMark`: same setup; `InboxAdvance(A)` returns no rows; then B sends without the flag; `InboxAdvance(A)` returns exactly B's message (A's own message is not offered later).
  - `TestExcludeSelf_InboxSkipsOwnRowCursorAdvances`: `Inbox(A, 0)` omits A's row, returned cursor `>=` A's seq.
  - `TestExcludeSelf_StillReadable`: `ListRetained("t")` and `Retained(A, seq)` include A's message.
  - `TestExcludeSelf_DefaultStillSelfPings`: A subscribed to its own `SelfTopic(A)`, sends there without the flag → callback fires for A, `deliveredTo == 1`.
- [ ] **Step 2: Run** `go test ./agentboard/ -run ExcludeSelf -v` — Expected: FAIL (undefined `WithExcludeSelf`).
- [ ] **Step 3: Implement.** `Send` removes from `targets` every `taskState` whose identity task id equals `fromTid` when `cfg.excludeSelf` (no-op when `fromTid` is zero). `takeUnshown(task)` computes the new mark over all rows above it, then filters out rows with `ExcludeSelf && FromTask == task`. `Inbox` filters the same rows after computing `max`.
- [ ] **Step 4: Run** the unit tests — Expected: PASS.
- [ ] **Step 5: Wire + CLI.** Edit the `.bgn`, `make protoregen ARGS='runner/protocol/message.bgn'`, read `r.ExcludeSelf()` in both handlers and pass `WithExcludeSelf()`. Operator with zero principal: accept as a no-op. Add the flags, regenerate the verb table.
- [ ] **Step 6: e2e test** `TestAgentSendExcludeSelf` in `cli/agent/exclude_self_e2e_test.go`, modelled on the existing `send_result_test.go` / `agent_e2e_test.go` setup: an agent subscribed to `grp` sends `--exclude-self`; its `agent inbox --json` has no row for that seq; `delivered_to` reported 0 with no other subscriber; `agent read <seq>` returns the body.
- [ ] **Step 7: Run** `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID go test ./agentboard/ ./cli/agent/ ./server/ ./runner/protocol/` and `scripts/wire-skew-check.sh` — Expected: PASS.
- [ ] **Step 8: Commit** `agentboard: --exclude-self skips the publisher's own delivery`.

### Task 2: `board_persist` capability bit

**Files:**
- Modify: `runner/protocol/message.bgn` (`enum Capability`: `board_persist = 0x1000000` after `runner_enroll`, comment: authorizes setting an agentboard topic's retention on topics the holder may publish to; default-off, appended; `all` → `0x1ffffff`), regenerate
- Modify: `cli/verb/caps.go` (ordering list after `BoardPeer`; `CapDescription`; `capAuditTable` entry `{tier: TierMid, scope: ScopeNone}`)
- Modify: whatever the completeness tests name (`server/cap_completeness_test.go`, `server/scope_completeness_test.go`, `server/scope_percap_completeness_test.go`, `runner/protocol/capability_test.go`, `cli/verb/caps_audit_table_test.go`) — follow commit `a8c6202a`'s handling of `runner_enroll`
- Test: `server/wal_test.go` (new case)

**Interfaces:**
- Produces: `protocol.Capability_BoardPersist`

- [ ] **Step 1: Write the failing test** `TestWALReplay_OldAllMaskLacksBoardPersist` in `server/wal_test.go`: a `task_created` record with `capabilities: 0xffffff` replays to caps where `caps & Capability_BoardPersist == 0` and `caps & Capability_RunnerEnroll != 0`.
- [ ] **Step 2: Run** `go test ./server/ -run TestWALReplay_OldAllMask -v` — Expected: FAIL (undefined).
- [ ] **Step 3: Implement** the enum change and regenerate; if replay expands a stored `0xffffff` to the new `all`, stop it (replay must keep the stored number).
- [ ] **Step 4: Walk** `.claude/skills/surface-parity-checklist` for the new bit (numbered items, verdict per number, in the commit message body).
- [ ] **Step 5: Run** `env -u … make test` (the four `-u`s above) — Expected: PASS, completeness tests included.
- [ ] **Step 6: Commit** `protocol: board_persist capability bit`.

### Task 3: `agentboard` Store + file implementation

**Files:**
- Create: `agentboard/store.go` (types + interface), `agentboard/filestore.go` (implementation)
- Test: `agentboard/filestore_test.go`

**Interfaces:**
- Consumes: `RetainedMessage` (incl. `ExcludeSelf`, retract fields)
- Produces:
  ```go
  type Retention struct {
      TTL     time.Duration // ignored when Forever
      Forever bool
      SetBy   protocol.TaskID // zero = operator
      SetAt   time.Time
  }
  type StoredTopic struct {
      Name      string
      Retention Retention
      Ring      []RetainedMessage // the newest ringN live messages, seq order
      Retracted []RetainedMessage
      Shown     map[protocol.TaskID]uint64
      MaxSeq    uint64            // highest seq in the log, live or withdrawn
  }
  type Store interface {
      Load(ringN int) ([]StoredTopic, error)
      SetRetention(topic string, r Retention) error   // creates the log if absent
      Drop(topic string) error                        // removes log and retention
      Truncate(topic string) error                    // empties the log, keeps retention
      Append(topic string, m RetainedMessage) error
      Retract(topic string, m RetainedMessage) error  // m has RetractedAt/By/ByTask set
      Remove(topic string, seq uint64) (bool, error)  // rewrite without seq
      SetShown(topic string, task protocol.TaskID, seq uint64) error
      Page(topic string, beforeSeq uint64, limit int) ([]RetainedMessage, error) // newest first, live and withdrawn; beforeSeq 0 = from the end
      Bytes(topic string) int64
      Close() error // flushes
  }
  func OpenFileStore(dir string, flushEvery time.Duration) (*FileStore, error)
  ```

Format decisions (fixed here; the spec left them to the plan): `dir/retention.json` holds `{"version":1,"topics":{"<name>":{"ttl_ms":…,"forever":…,"set_by":"<hex>","set_at_ms":…}}}`, rewritten via temp file + rename on every change. Each topic's log is `dir/<first 32 hex of sha256(name)>.log`, JSON Lines: first line `{"t":"hdr","v":1,"topic":"<name>"}`, then `{"t":"msg",…}` (every `RetainedMessage` field, enums as numbers, payload base64), `{"t":"retract","seq":…,"at_ms":…,"by":<n>,"by_task":"<hex>"}`, `{"t":"shown","task":"<hex>","seq":…}`. `Remove` flushes, rewrites to a temp file without that `msg` (and its `retract`), fsyncs, renames.

- [ ] **Step 1: Write the failing tests** in `agentboard/filestore_test.go`:
  - `TestFileStore_RoundTrip`: SetRetention + Append 100 + Retract seq 50 + SetShown(A, 90) + Close; reopen, `Load(64)` → one topic, `len(Ring)==64` ending at seq 100, seq 50 absent from `Ring` and present in `Retracted`, `Shown[A]==90`, `MaxSeq==100`, retention fields equal.
  - `TestFileStore_PageNewestFirst`: `Page(t, 0, 10)` → seqs 100..91; `Page(t, 91, 10)` → 90..81.
  - `TestFileStore_RemoveDestroysBytes`: Append a payload `SECRET-XYZ` at seq 7; `Remove(t, 7)` returns true; `os.ReadFile` of the log does not contain `SECRET-XYZ`.
  - `TestFileStore_DropAndTruncate`: `Drop` → log file gone, `Load` has no topic; `Truncate` → `Load` has the topic with empty `Ring` and the retention.
  - `TestFileStore_UnsafeTopicNames` (Review Focus 1): names `a/b`, `..`, 300 × `x` round-trip through SetRetention/Append/Load.
  - `TestFileStore_TornLastLine` (Review Focus 2): append 3, Close, append garbage `{"t":"msg","se` to the file; `Load` returns the 3 messages and no error.
  - `TestFileStore_FlushBatches`: with `flushEvery = time.Hour`, after `Append` the file size is unchanged; after `Close` it grew.
- [ ] **Step 2: Run** `go test ./agentboard/ -run FileStore -v` — Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `FileStore`; one `bufio.Writer` per open log, a ticker goroutine flushing all every `flushEvery`, stopped by `Close`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `agentboard: file store for retained topics`.

### Task 4: Board retention semantics

**Files:**
- Create: `agentboard/retention.go`
- Modify: `agentboard/board.go` (`Send`, `evictExpiredTopics`, `evictOldestTopicLocked`, `PurgeSeq`, `PurgeTopic`, retract paths, `InboxAdvance`), `agentboard/topic.go` (`retention *Retention` on `topic`)
- Test: `agentboard/retention_test.go`

**Interfaces:**
- Consumes: Task 3 `Store`, `Retention`, `StoredTopic`
- Produces:
  ```go
  var ErrNotRetainable = errors.New("agentboard: topic cannot be retained") // chat.* or empty
  func (b *Board) AttachStore(s Store) error // once, before serving: loads topics, raises seq
  func (b *Board) Retain(topic string, r Retention) error
  func (b *Board) ClearRetain(topic string) error
  func (b *Board) RetentionOf(topic string) (r Retention, diskBytes int64, ok bool)
  func (b *Board) Page(topic string, beforeSeq uint64, limit int) ([]RetainedMessage, error) // store when retained, else ring+retracted
  ```

Rules: mirror into the store only for a topic with `retention != nil`. `Retain` of a name with no topic creates it empty (subject to MaxTopics like a first publish); on an existing non-retained topic it writes the current ring and retracted list to the store first. Calling `Retain` again replaces the setting. `ClearRetain` → `store.Drop`, `retention = nil`. Expiry of a retained topic uses `max(lastPublishedAt, SetAt) + TTL` and calls `store.Drop`. `PurgeSeq` falls through to `store.Remove` when the seq is not in memory. `PurgeTopic` on a retained topic → `store.Truncate`, and the topic stays registered with its retention. `InboxAdvance` → `store.SetShown` for each retained topic whose mark moved. A store error is logged with `slog.Error` and does not fail the publish.

- [ ] **Step 1: Write the failing tests** in `agentboard/retention_test.go` (use an in-memory fake `Store` or a `FileStore` in `t.TempDir()`):
  - `TestRetain_RefusesChat`: `Retain("chat.0123abcd", …)`, `Retain("chat.0123abcd.x", …)`, `Retain("chat.operator", …)`, `Retain("", …)` → `ErrNotRetainable`.
  - `TestRetain_OutlivesTTL`: board TTL 1 ms, retain `t` 1 h, publish, run eviction after 10 ms → topic still listed.
  - `TestRetain_ExpiresFromRetainCall` (Review Focus 3): retain an unpublished `t` with TTL 50 ms; eviction at 10 ms keeps it, at 100 ms removes it and its log file.
  - `TestRetain_MaxTopicsSkipsRetained`: `MaxTopics: 2`, both retained → third topic's `Send` returns `ErrTooManyTopics`, both still present.
  - `TestRetain_RestartRestores`: FileStore; retain, publish 100, `InboxAdvance(A)` after subscribing, Close; new board with `AttachStore` → `ListRetained` has 64 ending at the last seq; A re-subscribes and `InboxAdvance(A)` is empty; `Page(t,0,200)` has 100.
  - `TestRetain_SeqAboveDisk` (Review Focus 5): logs hold seq `1<<62`; new board with `SeqSeed: 5` + `AttachStore` → next `Send` seq `> 1<<62`.
  - `TestRetain_PurgeDiskOnlySeq`: publish 100 to a retained topic, `PurgeSeq(t, 3)` → removed=true; log bytes lack seq 3's payload.
  - `TestRetain_PurgeTopicKeepsRetention` (Review Focus 4): `PurgeTopic(t)`; `RetentionOf(t)` ok; publish again → `Page` returns only the new message.
  - `TestRetain_ClearDeletesFile`.
- [ ] **Step 2: Run** `go test ./agentboard/ -run Retain -v` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./agentboard/` — Expected: PASS (existing tests untouched: a board without a store behaves as today).
- [ ] **Step 5: Commit** `agentboard: per-topic retention backed by the store`.

### Task 5: Wire + server: `board_retain`, retention columns, paged read

**Files:**
- Modify: `runner/protocol/message.bgn`:
  - `TaskControlKind`: append `board_retain` after `lifecycle_set` (comment: gated on board_persist; a task caller must also be able to publish to the topic), and its request/response union arms beside the others
  - `enum BoardRetainStatus: ok, not_retainable, not_publishable, store_error`
  - `format BoardRetainRequest: request_id :u32, topic_len :u16, topic, ttl_ms :u64, forever :u1, clear :u1, reserved :u6`
  - `format BoardRetainResponse: request_id :u32, status :BoardRetainStatus`
  - `format BoardTopicRetention: name_len :u16, name, ttl_ms :u64, forever :u1, reserved :u7, set_by :TaskID, disk_bytes :u64`
  - `BoardTopicsBody`: append `retention_len :u16, retention :[retention_len]BoardTopicRetention` (a parallel list keyed by name, **appended after `topics`** so an older client stops before it; not new fields inside `BoardTopicRow`, whose size change would misdecode every later row in an old client)
  - `BoardReadRequest`: append `before_seq :u64, limit :u16` (`limit 0` = today's ring+retracted answer)
- Modify: `server/capabilities.go` (`requiredCap`: `TaskControlKind_BoardRetain: Capability_BoardPersist`), `server/task_handler.go` (dispatch), `server/board_handler.go` (`handleBoardRetain`, topics retention list, `handleBoardRead` paging via `Board.Page`)
- Modify: `agentboard/reach.go` — add `func (b *Board) PublishableBy(tid protocol.TaskID, topic string) bool`, `Publishable`'s rule for a task identified by id (contacts from any `taskState` with that task id); `Publishable` delegates to it
- Modify: `cmd/harness-server/main.go` — `agentboard.OpenFileStore(filepath.Join(*dataDir, "board"), 2*time.Second)`, `board.AttachStore(store)` before `s.SetBoard(board)`, `store.Close()` deferred after `board.Close()`
- Test: `server/board_retain_test.go`

**Interfaces:**
- Consumes: Task 2 `Capability_BoardPersist`, Task 4 Board API
- Produces: `protocol.TaskControlKind_BoardRetain`, `BoardRetainRequest/Response`, `BoardTopicRetention`, `BoardReadRequest.BeforeSeq/Limit`

- [ ] **Step 1: Write the failing tests** in `server/board_retain_test.go`, using the existing board handler test harness (see `server/board_*_test.go` for the connection fake):
  - `TestBoardRetain_OperatorAnyTopic`: operator connection retains `free.x` `forever` → ok; topics body's retention list has `free.x`, `forever`, zero `set_by`.
  - `TestBoardRetain_TaskNeedsBit`: task without `board_persist` → permission denied naming `board_persist`.
  - `TestBoardRetain_TaskNeedsPublishable`: task with `board_persist` but no `board_peer` on free-form `free.x` → `not_publishable`; on `tree.<own id>.d` → ok with `set_by` = its id.
  - `TestBoardRetain_ChatRefused`: operator on `chat.0123abcd` → `not_retainable`.
  - `TestBoardRead_Pages`: retained topic with 100 messages; `before_seq 0, limit 10` → 10 newest; `limit 0` → today's answer (ring+retracted, oldest first).
- [ ] **Step 2: Run** `go test ./server/ -run 'BoardRetain|BoardRead_Pages' -v` — Expected: FAIL.
- [ ] **Step 3: Implement** (schema edit → `make protoregen ARGS='runner/protocol/message.bgn'` → handlers → main.go).
- [ ] **Step 4: Run** `env -u … go test ./server/ ./runner/protocol/` — Expected: PASS, `TestResponseRowsAreNotInline` and the kind-completeness tests included; `scripts/wire-skew-check.sh` clean.
- [ ] **Step 5: Commit** `server: board_retain, retention in board topics, paged board read`.

### Task 6: CLI

**Files:**
- Modify: `cli/verb/table.go` (new `board retain <topic>` with `--ttl` string — a Go duration or `forever` — and `--clear`, mutually exclusive, one required; `board read` gains `--before` uint and `--limit` uint), `go generate ./cli/verb`
- Modify: `cli/board.go` (`func (c *Client) BoardRetain(ctx context.Context, topic string, ttl time.Duration, forever, clear bool) (protocol.BoardRetainStatus, error)`; topics result carries the retention list; read takes `beforeSeq uint64, limit uint16`), `cli/cmd_board.go` (render)
- Test: `cli/board_retain_e2e_test.go`

Rendering: a retained topic's `board topics` line gains `retain=<ttl|forever> by=<8-hex short id|operator> disk=<bytes>`; non-retained lines are unchanged. `--json` gains `retain_ttl_ms`, `retain_forever`, `retain_by`, `disk_bytes` on retained topics only. `board retain` prints `ok topic=<t> retain=<…>` or the status name, exit 1 on non-ok.

- [ ] **Step 1: Write the failing e2e** `TestBoardRetainCLI`: `board retain free.x --ttl 2h` → `ok`; `board topics` line for `free.x` contains `retain=2h0m0s by=operator disk=`; `board retain free.x --clear` → the suffix is gone; `board retain chat.0123abcd --ttl 1h` exits 1 with `not_retainable`; `board retain x` with neither flag is a usage error.
- [ ] **Step 2: Run** — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `env -u … go test ./cli/...` — Expected: PASS.
- [ ] **Step 5: Commit** `cli: board retain, retention columns, paged board read`.

### Task 7: TUI and WebUI

**Files:**
- Modify: `tui/board.go`, `tui/actions.go`, `tui/overlays.go` (topic list shows `retain`/`by`/`disk`; an action to set or clear retention on the selected topic, prompting for the TTL)
- Modify: `cmd/harness-webui-wasm/main.go` and its board view in `webui/static/main.js` (same columns, same action)
- Test: `tui/board_test.go`; `webui/static/*_test.mjs` beside `board_send_test.mjs`

- [ ] **Step 1: Walk** `.claude/skills/surface-parity-checklist` items for the new verb and the new `board topics` columns; record each number's verdict in the commit body.
- [ ] **Step 2: Write the failing tests**: TUI renders `retain=forever` for a retained row and nothing for a plain one; the retain action sends a `BoardRetainRequest` with the typed TTL. WebUI: the same two assertions.
- [ ] **Step 3: Run** `go test ./tui/` and `make js-test` — Expected: FAIL.
- [ ] **Step 4: Implement.**
- [ ] **Step 5: Run** — Expected: PASS.
- [ ] **Step 6: Commit** `tui, webui: topic retention`.

### Task 8: Skill text and end-to-end verification

**Files:**
- Modify: both `harness-cli/SKILL.md` copies — a short "Keeping a topic" section: topics are deleted 30 minutes after the last publish; a task holding `board_persist` can `harness-cli board retain <topic> --ttl <duration>|forever` on a topic it may publish to; `chat.<id>` cannot be retained; retained topics survive a server restart, subscriptions do not (re-subscribe).

- [ ] **Step 1: Edit** the skill; `diff` the two copies — Expected: identical; `go test ./runner/agentskills/` — PASS.
- [ ] **Step 2: Dummy harness** (`dummy-harness` skill): retain `tree.<task>.d` from a task holding `board_persist`, publish 70, restart the server, `board read tree.<task>.d --limit 100` returns 70, the ring view returns 64. Bring the instance down afterwards.
- [ ] **Step 3: netem-lab**: `board read <retained> --limit 200` over UDP in `scripts/netem-lab` returns all rows (loopback hides a dropped datagram).
- [ ] **Step 4: Run** `env -u … make test`, `make vet`, `make build` — Expected: all pass.
- [ ] **Step 5: Commit** `skill: keeping a topic with board retain`.
