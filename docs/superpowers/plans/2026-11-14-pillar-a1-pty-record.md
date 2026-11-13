# Pillar A1 — PTY record — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An interactive session's PTY output can be recorded on the server
for a whole run — `session snapshot --raw` kept on disk — and listed, read
(raw or asciicast v2), kept and deleted from the CLI, TUI and WebUI.

**Architecture:**
- **Run numbers:** a per-task `Run` counter on `TaskEntry`, persisted on
  `task_created`/`task_resumed`.
- **Capture:** `SessionMux.recordFrame` and `applyWinSizeFrame` hand each
  output or size frame to a per-run recorder. The recorder queues them to a
  writer goroutine, so disk I/O never runs on the runner pump. A segment
  rotation is decided on the pump side, where the screen model matches the
  stream position, and carries its Synth head with it.
- **Storage:** a `RecordStore` owns `<data-dir>/records/`. A run is a
  directory of segments, each a sequence of `RecordEntry` (a `.bgn` format
  shared by disk and wire). Segments are gzipped on close. Per-run and
  server-wide caps drop the oldest segment. A retention loop expires records.
- **Access:** five task-control verbs. Get and list answer with a stream, as
  `GetTaskLog` does. The client renders raw bytes or asciicast.

**Tech Stack:** Go (server, runner/protocol, cli, tui, wasm), `.bgn` →
`make protoregen`, JS (webui), stdlib `compress/gzip`.

**Spec:** `docs/superpowers/specs/2026-11-13-pillar-a-run-modes-design.md`
(§A1: facts F22–F28, decisions A-D20–A-D28, Wire/Persistence (A1), Testing
(A1)), inside `docs/superpowers/specs/2026-11-12-run-modes-and-records-grand-design.md`.

## Global Constraints

- **Before starting:** read `.claude/skills/implementation-pitfalls/SKILL.md`.
- **Where to work:** this worktree, its `harness/<dir>` branch.
- **Wire:** every `.bgn` change is in Task 1 and only there.
  - Flag bits go at the end of a reserved run. Enum values are appended.
  - Regenerate with `make protoregen`.
  - The runner decodes none of the new formats. Still run
    `scripts/wire-skew-check.sh` before landing. Restart order: server only.
- **Datagram size:** every new request and response is fixed-size. Anything
  of variable length (a record list, a record's entries) goes on a stream
  (memory: one control message is one datagram, ~1170 B over UDP).
- **WAL:** every new field is a JSON key whose zero value is today's
  behaviour. Each one is added to `WALEvent`, `walEventJSON`, `MarshalJSON`
  and `UnmarshalJSON` (`server/wal.go`).
  `TestWALEventJSONRoundTripCopiesEveryField` stays green.
- **Capabilities:**
  - `record_delete = 0x4000000`, `record_persist = 0x8000000`,
    `all = 0xfffffff`.
  - Every place the `board_member` commit (`ae109101`) touched is updated the
    same way: `runner/protocol/capability_test.go`, `cli/verb/caps.go`
    (ordered list, `CapDescription`, tier table),
    `cli/verb/caps_audit_table_test.go`, `server/capabilities.go`,
    `server/cap_completeness_test.go`, `server/scope_completeness_test.go`,
    `server/scope_percap_completeness_test.go`, `server/wal_test.go`, README.
- **Gates (A-D22, A-D26):**
  - Read (list, get): `exec_view`.
  - Switch on, open with on, and keep later or forever: `record_persist`.
  - Switch off, delete, and keep earlier: `record_delete`.
  - Every gate is checked within the task's scope, through `authorize` /
    `inScope` (`server/capabilities.go:326-337`), so `exclude_self` on an
    override is honoured with no extra code.
  - An open that says nothing or off is not gated.
- **Defaults:**
  - `--record` false;
  - `--record-run-max` 64 MiB of entries;
  - segment = run max / 8;
  - `--record-total-max` 1 GiB on disk;
  - `--record-retention` 0 = forever.
- **No new dependencies.** gzip is `compress/gzip`.
- **Never recorded:** input (Stdin frames), the stream kind.
- **Verification:**
  - `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`
  - `make build`
  - `go generate ./cli/verb` after a verb-table change
  - `make js-test` after a WebUI change
- **Repo is public:** no local paths, hostnames or task ids in committed
  files.

## Review Focus

1. **A run that crosses a deliberate restart** must stay one record. The new
   server reopens the run's directory and appends: no second record, no
   repeated bytes, no lost head. Owned by Task 5.
2. **A crash mid-segment** leaves a `.seg` whose last entry is cut off. The
   startup pass and any reader stop at the last complete entry and never
   error on the tail. Owned by Task 3.
3. **The server-wide cap firing while a run is writing** never deletes an
   open segment, and never deletes the only segment of a live run. Owned by
   Task 3.
4. **asciicast with a multi-byte UTF-8 character split across two frames**
   must not become two U+FFFD. An incomplete tail is carried into the next
   event; only bytes that are invalid on their own are replaced. Owned by
   Task 7.
5. **A slow disk** must not stall the runner pump indefinitely, and must not
   reorder frames. The recorder queue is bounded and blocks when full: order
   wins over latency, and the bound caps memory. A test pins that the order is
   kept under a slow writer. Owned by Task 4.

---

### Task 1: Wire — formats, verbs, capability bits

**Files:**
- Modify: `runner/protocol/message.bgn` (regenerate `runner/protocol/message.go`)
- Modify: the capability touch-list in Global Constraints
- Test: `runner/protocol/record_wire_test.go` (new), the capability tests

**Interfaces — Produces (exact `.bgn` names; Go names follow protoregen):**
- `enum TaskControlKind` appended: `record_list`, `record_get`,
  `record_delete`, `record_keep`, `record_switch`, with request and response
  arms in both unions.
- `enum RecordStatus :u8`: `ok`, `not_found`, `not_recordable` (stream kind,
  or a run with no session to switch), `internal_error`.
- `format RecordRef`: `task_id :TaskID`, `run :u32` (0 = latest).
- `format RecordEntry`: `ts_ns :u64`, `frame_type :u8`, `payload_len :u32`,
  `payload :[payload_len]u8`. This is the disk format of a segment and the
  body of a record-get stream. A comment says why both share it.
- `format RecordInfo`:
  - `task_id`, `run :u32`;
  - `started_ns :u64`, `ended_ns :u64` (0 = still recording);
  - `end_reason :EndReason`;
  - `truncated_from_ns :u64` (0 = whole);
  - `entry_bytes :u64`, `disk_bytes :u64`;
  - `forever :u1`, `recording :u1`, `reserved :u6`;
  - `expires_ns :u64` (0 when forever).
- `format RecordListBody`: `count :u32`, `records :[count]RecordInfo`. It goes
  on a stream.
- Requests and responses:
  - `RecordListRequest`: `task_present :u1`, `reserved :u7`, then
    `task_id` if present.
  - `RecordListResponse`: `status`, `stream_id :u64`.
  - `RecordGetRequest`: `ref :RecordRef`.
  - `RecordGetResponse`: `status`, `stream_id :u64`.
  - `RecordDeleteRequest`: `ref`, `all_runs :u1`, `reserved :u7`.
  - `RecordKeepRequest`: `ref`, `forever :u1`, `reserved :u7`, `ttl_ms :u64`.
  - `RecordSwitchRequest`: `task_id`, `on :u1`, `reserved :u7`.
  - The delete, keep and switch responses are `status`.
- `OpenInteractiveRequest`:
  - `reserved2` becomes `ext_present :u1`.
  - After the `prompt_present` conditional comes
    `if ext_present == 1: ext :OpenInteractiveExt`.
  - `format OpenInteractiveExt`: `record_set :u1`, `record :u1`,
    `reserved :u6`.
- `OpenInteractiveStatus` appended: `record_not_permitted`.
- `TaskInfo`:
  - one bit of the `reserved :u3` byte becomes `recording :u1`;
  - `run :u32` is appended after `conditions`.
- `TaskStatusEvent`:
  - one bit of `reserved :u7` becomes `recording :u1`;
  - `run :u32` is appended last.
- `Capability`: `record_delete = 0x4000000`, `record_persist = 0x8000000`,
  `all = 0xfffffff`. The widening list in the `all` comment is extended.

- [ ] **Step 1: Write the failing tests** in `runner/protocol/record_wire_test.go`:
  - `TestRecordEntryRoundTrip`: a `RecordEntry{TsNs: 1, FrameType: 1}` with a
    3-byte payload encodes and decodes equal.
  - `TestOpenInteractiveExtRoundTrip`: an open request with `ExtPresent=1`,
    `Ext.RecordSet=1`, `Ext.Record=0` decodes with both bits as sent. A request
    with `ExtPresent=0` has no ext byte on the wire: its length is unchanged
    from today's encoding of the same request.
  - `TestRecordRequestsFitOneDatagram`: each of the five requests and four
    fixed responses encodes in ≤ 1170 bytes.
  - Update `capability_test.go:10` to expect `Capability_All == 0xfffffff`,
    with both new bits in the OR list.
- [ ] **Step 2: Run** `go test ./runner/protocol/`. Expected: FAIL (undefined
  types).
- [ ] **Step 3: Edit `message.bgn`** as in Produces, then run `make protoregen`.
- [ ] **Step 4: Update the capability touch-list.**
  - `server/capabilities.go` `requiredCap`:
    - `RecordList` and `RecordGet` → `exec_view`;
    - `RecordDelete` → `record_delete`;
    - `RecordKeep` and `RecordSwitch` are classified `capInHandler` in
      `cap_completeness_test.go`, because their bit depends on direction.
  - Move the upper bound of `TestEveryTaskControlKindHasACapVerdict` to
    `TaskControlKind_RecordSwitch`.
  - Add the two `CapDescription` texts:
    - `record_delete`: "delete PTY records, switch recording off, shorten a
      record's retention";
    - `record_persist`: "switch recording on, open a session recorded, keep a
      record longer".
  - Put both bits in the same tier as `board_persist` / `board_purge`.
  - Handlers do not exist yet: the dispatch switch gets the five cases routed
    to `h.handleRecord*` stubs that answer `internal_error`. Task 6 replaces
    them.
- [ ] **Step 5: Run** `make test`. Expected: PASS. Then run
  `scripts/wire-skew-check.sh`. Expected: PASS.
- [ ] **Step 6: Commit** (stage by explicit path).

### Task 2: Run numbers

**Files:**
- Modify: `server/taskstore.go` (`TaskEntry` :19, `createLocked` :274,
  `Resume` :690, `ReplayEvents` :1347)
- Modify: `server/wal.go`, `server/task_handler.go` (`toTaskInfo` :2666),
  `server/server.go` (the `publishTaskEvent` literal :483)
- Test: `server/run_number_test.go` (new)

**Interfaces — Produces:**
- `TaskEntry.Run uint32`.
- WAL field `Run uint32 json:"run,omitempty"` on `task_created` and
  `task_resumed`.
- `TaskInfo.Run`, `TaskStatusEvent.Run`.

- [ ] **Step 1: Write the failing tests:**
  - `TestRunNumberCountsCreateAndResumes`: create → `Run == 1`; resume twice
    → `3`.
  - `TestRunNumberReplaysFromRecordsWithoutIt`: events with no `run` field
    (one `task_created`, two `task_resumed`) replay to `Run == 3`.
  - `TestRunNumberSurvivesReplay`: events carrying `run` replay to the same
    value.
  - `TestRunNumberKeptAcrossHoldAndReadopt`: hold and readopt leave `Run`
    unchanged.
- [ ] **Step 2: Run** `go test ./server -run RunNumber`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `createLocked` sets `Run = 1`. `Resume` increments `Run` before the
    `task_resumed` write.
  - Replay takes `ev.Run` when non-zero, else 1 on created and previous+1 on
    resumed.
  - Add the field to the four WAL places and to both mappers.
- [ ] **Step 4: Run** `make test`. Expected: PASS, including
  `TestToTaskInfoMapsEveryField` and the WAL round-trip test.
- [ ] **Step 5: Commit.**

### Task 3: RecordStore — segments, caps, retention, deletion

**Files:**
- Create: `server/recordstore.go`, `server/recordstore_test.go`

**Interfaces — Produces:**
```go
type RecordConfig struct {
    Dir          string        // <data-dir>/records
    RunMax       int64         // entry bytes per run; segment = RunMax/8
    TotalMax     int64         // on-disk bytes, whole store
    Retention    time.Duration // 0 = forever; the default for new records
}
func NewRecordStore(cfg RecordConfig) (*RecordStore, error) // scans Dir, builds the index
func (s *RecordStore) OpenRun(taskID string, run uint32, now time.Time) (*RunWriter, error) // creates or reopens
func (w *RunWriter) Append(e protocol.RecordEntry) error
func (w *RunWriter) Rotate(head []protocol.RecordEntry) error // close current segment, start one beginning with head
func (w *RunWriter) Close(now time.Time) error              // switch-off: close + gzip the segment, stamp recording end
func (w *RunWriter) Detach() error                          // mux stopped: flush, release the file, leave the .seg open for a re-adopting server
func (s *RecordStore) List(taskID string) []protocol.RecordInfo // taskID "" = all
func (s *RecordStore) Read(taskID string, run uint32, fn func(protocol.RecordEntry) error) error
func (s *RecordStore) Delete(taskID string, run uint32) (bool, error) // run 0 = every run
func (s *RecordStore) SetRetention(taskID string, run uint32, forever bool, ttl time.Duration) error
func (s *RecordStore) Expiry(taskID string, run uint32) (time.Time, bool) // for the direction check
func (s *RecordStore) FinishRun(taskID string, run uint32, ended time.Time, reason protocol.EndReason) error
func (s *RecordStore) CompressLeftovers(isHeld func(taskID string) bool) error
func (s *RecordStore) RunRetention(ctx context.Context) // ticker, as agentboard evictLoop (board.go:594)
```
- **Layout:** `Dir/<task>/<run>/NNNNNNNN.seg` while open,
  `NNNNNNNN.seg.gz` once closed, and `meta.json` (A-D24's fields, plus
  `retention {forever, ttl, set_at}`).
- **Rotation:** `Append` itself never rotates; the caller rotates (Task 4).
  The writer reports `Full() bool` once the open segment's entry bytes reach
  `RunMax/8`.
- **Caps:**
  - After a segment closes, if the run's entry bytes exceed `RunMax`, delete
    the run's oldest closed segment and set `truncated_from_ns` to the
    `ts_ns` of the new oldest segment's first entry.
  - Then, while the store's disk bytes exceed `TotalMax`, delete the oldest
    closed segment in the store, by first-entry time, from any run. Never
    delete an open segment, and never delete a live run's last remaining
    segment.
- **Retention:** expiry = the run's recording end + ttl (F28's shape). A run
  still recording never expires.

- [ ] **Step 1: Write the failing tests:**
  - `TestRunWriterRoundTrip`: entries appended, closed, and read back equal,
    in order.
  - `TestSegmentGzippedOnClose`: after `Rotate` the old segment is `.seg.gz`
    and readable. The open one is `.seg`.
  - `TestRunCapDropsOldestAndMarksTruncated`: with `RunMax` = 8 × a small
    segment, ten rotations leave eight segments, and `truncated_from_ns`
    equals the first ts of the oldest kept segment.
  - `TestTotalCapEvictsOldestAcrossTasks`: two tasks; exceeding `TotalMax`
    removes the oldest segment of the older task. The open segment and a live
    run's last segment survive.
  - `TestReadStopsAtTruncatedTail` (Review Focus 2): a `.seg` with half an
    entry at its end reads every complete entry and returns nil.
    `CompressLeftovers` gzips it without the partial entry.
  - `TestCompressLeftoversSkipsHeld`: a held task's `.seg` stays open.
  - `TestReopenRunAppends` (Review Focus 1, store half): `OpenRun` on an
    existing run continues in its open segment, and the entries read back are
    one sequence.
  - `TestRetentionExpiresRecordNotRun`: ttl 1 s and the end 2 s ago → gone
    after a sweep. Forever → kept. A run with no end → kept.
  - `TestDeleteAllRuns`: `Delete(task, 0)` removes the task directory.
- [ ] **Step 2: Run** `go test ./server -run 'RunWriter|Segment|RunCap|TotalCap|ReadStops|CompressLeftovers|ReopenRun|Retention|DeleteAllRuns'`.
  Expected: FAIL.
- [ ] **Step 3: Implement** `server/recordstore.go`.
  - One mutex guards the index; file I/O for eviction runs outside it, as
    `evictExpiredTopics` does.
  - `meta.json` is written via a temp file and rename.
- [ ] **Step 4: Run** the same tests, then `make test`. Expected: PASS.
- [ ] **Step 5: Commit.**

### Task 4: Capture in the mux

**Files:**
- Modify: `server/session_mux.go` (`recordFrame` :375, `applyWinSizeFrame`
  :967, `Stop` :1338)
- Create: `server/session_record.go` (the recorder), `server/session_record_test.go`

**Interfaces:**
- Consumes: `RunWriter` (Task 3).
- Produces:
```go
type recordStart uint8
const (
    recordFresh    recordStart = iota // the run's first record, from the PTY's first byte: no head
    recordFromRing                    // switch-on with no record yet: replaySnapshot() frames first, no head
    recordAgain                       // switch-on with a record already: Rotate(segmentHead())
    recordContinue                    // re-adoption: append to the open segment as it stands
)
func (m *SessionMux) StartRecording(w *RunWriter, how recordStart) error // a second call while recording is a no-op
func (m *SessionMux) StopRecording() // switch-off: w.Close; the record stays
func (m *SessionMux) Recording() bool
func (m *SessionMux) segmentHead() []protocol.RecordEntry // lastWinSize (if any), Synth(modes.Preamble()), Synth(screenRepaint())
```
- **Recorder:** a bounded channel (1024 items) drained by one writer
  goroutine. An item is either an entry or a rotate carrying its head.
  `recordFrame` calls it after the screen write, for Stdout and Stderr only.
  `applyWinSizeFrame` calls it after releasing `m.mu`, with the size frame.
  - When the bytes enqueued in the current segment reach `RunMax/8`,
    `recordFrame` builds `segmentHead()` right then, so it describes the
    screen just after this frame, and enqueues a rotate.
  - `recordStart` decides how a record begins, so only the record's first
    segment lacks a head (A-D23):
    - `recordAgain` enqueues a rotate with the head;
    - `recordFromRing` stamps the `replaySnapshot()` frames with the start
      time (A-D22);
    - `recordContinue` appends where the previous server stopped.
  - `Stop` calls `w.Detach`, **not** `Close`. A mux also stops at a
    deliberate shutdown, and the held run must be continued by the next
    server (Review Focus 1). The open segment is closed only by a rotation, a
    switch-off, `FinishRun` at the run's end, or the startup pass when the
    run was not held.
- **Order:** the queue blocks when full. It never drops (Review Focus 5).

- [ ] **Step 1: Write the failing tests:**
  - `TestRecordingCapturesOutputAndSizeNotInput`: feed Stdout, Stdin and a
    size frame through a mux. The record holds the Stdout and the size frame
    and no Stdin.
  - `TestRotatedSegmentHeadRebuildsTheScreen`: after enough output to rotate,
    feeding only the new segment's entries to a fresh `vtgrid` screen gives
    the same text as the live mux's screen at the rotation point.
  - `TestStartRecordingSeedsFromRing`: output written before
    `StartRecording(w, recordFromRing)` appears in the record, stamped with
    the start time.
  - `TestFreshRecordHasNoHead`: the first entry of a `recordFresh` record is
    the PTY's first frame, not a Synth.
  - `TestStopThenStartMakesNewSegment`: `StopRecording` then
    `StartRecording(w, recordAgain)` → a new segment beginning with the head.
  - `TestMuxStopDetachesWithoutClosing`: after `Stop`, the run's segment is
    still `.seg` and its meta has no recording end.
  - `TestRecorderKeepsOrderUnderSlowWriter`: a writer that sleeps per entry,
    with 5,000 frames fed, reads back in order with none missing.
- [ ] **Step 2: Run** `go test ./server -run 'Recording|Rotated|StartRecording|StopThenStart|Recorder'`.
  Expected: FAIL.
- [ ] **Step 3: Implement** `session_record.go` and the three mux hooks.
- [ ] **Step 4: Run** `make test`. Expected: PASS.
- [ ] **Step 5: Commit.**

### Task 5: Server wiring — switch, open, restart, prune, flags

**Files:**
- Modify: `server/taskstore.go`, `server/wal.go`, `server/session_start.go`
  (:39, mux at :173), `server/readopt.go` (:177, mux at :210),
  `server/task_handler.go` (open path, `toTaskInfo`), `server/server.go`
  (`Config` :39, `New` :234, `serve` :862, the `OnPrune` chain :939),
  `cmd/harness-server/main.go` (flags :33-58, `Config` :340)

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces:
  - `TaskEntry.Record bool` (this run's switch).
  - WAL `Record bool json:"record,omitempty"` on `task_created` and
    `task_resumed`, and a new `task_record_changed` record (`Record`, `Ts`).
  - `func (s *TaskStore) SetRecord(id string, on bool) error` writes
    `task_record_changed`.
  - `TaskHandler.Records *RecordStore`, `TaskHandler.RecordDefault bool`.
  - Server flags `--record`, `--record-run-max`, `--record-total-max`,
    `--record-retention` → `Config.Record{Default, RunMax, TotalMax, Retention}`.

- [ ] **Step 1: Write the failing tests:**
  - `TestOpenRecordFollowsRequestThenServerDefault`:
    - ext record on → recorded;
    - ext off → not recorded;
    - no ext with `RecordDefault` true → recorded;
    - a resume with no ext after a recorded run → takes the default, not the
      previous run's setting.
  - `TestOpenWithRecordOnNeedsRecordPersist`: a caller without the bit gets
    `record_not_permitted`. No ext, or off, needs nothing.
  - `TestStreamKindIsNeverRecorded`: a stream open with record on is
    accepted and not recorded.
  - `TestRecordSwitchPersistsAcrossReplay`: `task_record_changed` replays.
  - `TestReadoptContinuesTheSameRecord` (Review Focus 1, server half): a held
    recorded task re-adopted with `rebindHeldSessions` reopens the same run's
    writer. One `RecordInfo` for the run.
  - `TestPruneDeletesRecords`: prune removes `records/<task>`; a restore
    does not recreate it.
  - `TestRunEndStampsTheRecord`: after a run ends and the task is resumed,
    `List` shows the first run with its `end_reason` and `ended_ns`.
- [ ] **Step 2: Run** these tests. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Open path: read `ext`, gate `record_persist` on an explicit on, store
    `Record`.
  - `startSession` and `rebindHeldSessions`: when `Record` is set and the kind
    is interactive, `OpenRun`, then `StartRecording` with `recordFresh` at a
    session start and `recordContinue` at re-adoption.
  - The run end: `FinishRun` closes and gzips the open segment and stamps the
    end. It is called from the `OnFinish` and `OnCancel` hooks, and from the
    failure path that stamps `EndedAt`. `List` fills the latest run's
    end reason from `TaskEntry` when its meta has none.
  - Prune: `Records.Delete(id, 0)` in the `OnPrune` chain.
  - Startup, after replay in `serve`: `CompressLeftovers(isHeld)`, then
    `go Records.RunRetention(ctx)`.
  - `TaskInfo.Recording` and `TaskStatusEvent.Recording` come from
    `mux.Recording()`.
- [ ] **Step 4: Run** `make test`. Expected: PASS.
- [ ] **Step 5: Commit.**

### Task 6: Server handlers for the five verbs

**Files:**
- Create: `server/record_handlers.go`, `server/record_handlers_test.go`
- Modify: `server/task_handler.go` (replace the Task 1 stubs)

**Interfaces — Produces:** the five `handleRecord*` functions.
- **List and get:**
  - Gate `exec_view`. A missing bit → `denyTaskControl`. Out of scope →
    `not_found`, the `handleGetTaskLog` pattern (`task_handler.go:2550-2557`).
  - List filters by `scopeSet(conn, ExecView)`. Its body goes out through
    `respondWithBody` (`server/respond_body.go:27`).
  - Get answers `ok` with a stream id first, then writes the encoded entries
    and EOF, as `handleGetTaskLog` does.
  - A run still recording is read up to the request: entries the writer has
    not flushed may be missing, never cut.
- **Delete:** `record_delete` within scope.
- **Keep:**
  - Compare the requested expiry with `Expiry`. Later or forever →
    `record_persist`; earlier → `record_delete`. Both within scope.
  - A miss → `denyTaskControl` with the bit that was needed.
- **Switch:**
  - On → `record_persist`, off → `record_delete`, within scope.
  - `not_recordable` when there is no live interactive mux.
  - On calls `OpenRun`, then `StartRecording` with `recordFromRing` when the
    run has no record yet and `recordAgain` when it has, plus `SetRecord`.
    Off calls `StopRecording` and `SetRecord`.

- [ ] **Step 1: Write the failing tests.** Each runs through `Handle` with a
  task caller:
  - `TestRecordGetNeedsExecViewInScope`;
  - `TestRecordDeleteNeedsRecordDelete`;
  - `TestRecordKeepGatedByDirection`: extend with `exec_view` only → denied;
    with `record_persist` → ok; shorten with `record_persist` only → denied;
    with `record_delete` → ok;
  - `TestRecordSwitchGates`;
  - `TestExcludeSelfProtectsOwnRecord`: a task whose `record_delete` override
    has `exclude_self` cannot delete, shorten or switch off its own record,
    and can do all three to a sibling in scope;
  - `TestRecordGetStreamsEntriesInOrder`;
  - `TestRecordListOverStreamHoldsManyRecords`: 100 records, more than one
    datagram's worth, all listed.
- [ ] **Step 2: Run** these tests. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make test`. Expected: PASS.
- [ ] **Step 5: Commit.**

### Task 7: CLI — client, renderers, verbs

**Files:**
- Create: `cli/record.go` (client methods), `cli/record_render.go`,
  `cli/record_render_test.go`
- Modify:
  - `cli/session_opts.go` (`SessionOpts` :26, `buildOpenInteractiveRequest`
    :116);
  - `cli/verb/spawn.go` (:117-139), `cli/verb/table.go`;
  - `cmd/harness-cli/dispatch.go`, `cmd/harness-cli/session.go`;
  - `cli/list.go` (an `ls` fragment `rec` while recording; the run number in
    `ls --json`).

**Interfaces — Produces:**
```go
func (c *Client) RecordList(ctx context.Context, taskIDHex string) ([]protocol.RecordInfo, error) // "" = all in scope
func (c *Client) RecordGet(ctx context.Context, ref string, fn func(protocol.RecordEntry) error) error // ref "<id>" or "<id>#<n>"
func (c *Client) RecordDelete(ctx context.Context, ref string, allRuns bool) error
func (c *Client) RecordKeep(ctx context.Context, ref string, forever bool, ttl time.Duration) error
func (c *Client) RecordSwitch(ctx context.Context, taskIDHex string, on bool) error
func ParseRecordRef(s string) (taskIDHex string, run uint32, err error)
func RenderRaw(entries []protocol.RecordEntry, withSynth bool) []byte
func RenderAsciicast(entries []protocol.RecordEntry) ([]byte, error)
```
- **`SessionOpts`:** `RecordSet, Record bool` → the `ext` byte.
  `RunModeFlags` is not touched: recording is not a run-mode flag.
- **Verbs:**
  - `session new` gains `--record` / `--no-record` (mutually exclusive).
  - `record ls [task-id]`.
  - `record get <ref> [--format raw|asciicast] [--without-synth] [-o FILE]`.
  - `record rm <ref> | <task-id> --all-runs`. A bare id without `--all-runs`
    is refused, so no run goes by accident.
  - `record keep <ref> (--forever | --for D)`.
  - `record on|off <task-id>`.
  - Surfaces: CLI and TUI command line. The WebUI modal points at
    `buildTaskSheet`.
- **asciicast v2:**
  - Header: `{"version":2,"width":W,"height":H,"timestamp":<start s>}`, with
    the size from the first size entry, or 80×24 if there is none.
  - Events:
    - `[t,"o",text]` for Stdout, Stderr and Synth;
    - `[t,"r","WxH"]` for a size entry.
  - A size entry is the mux's `TerminalWindowSize` frame (F22). Decode it with
    the same code `winSizeOf` uses in `server/session_mux.go`, moved to a
    package `cli` can import. A second hand-written decoder would be a second
    reading of one format.
  - `t` is seconds from the first entry, clamped non-decreasing.
  - A UTF-8 sequence split across entries is carried into the next event
    (Review Focus 4). Only bytes invalid on their own become U+FFFD.

- [ ] **Step 1: Write the failing tests:**
  - `TestRenderRawConcatenatesPayloads`, with and without Synth.
  - `TestAsciicastHeaderAndEvents`.
  - `TestAsciicastCarriesSplitUTF8`: "あ" split 1+2 bytes across two entries
    → one event holding "あ", no U+FFFD.
  - `TestAsciicastReplacesInvalidByte`: `0xff` → U+FFFD.
  - `TestAsciicastTimeNeverGoesBack`.
  - `TestParseRecordRef`: `"<id>#3"`, `"<id>"` (run 0), `"<id>#0"` refused,
    `"<id>#x"` refused.
  - `TestSessionNewRecordFlagsSetExt`: `--record` sets `RecordSet` and
    `Record`; neither flag leaves the ext byte off the wire.
- [ ] **Step 2: Run** `go test ./cli/...`. Expected: FAIL.
- [ ] **Step 3: Implement.** Then run `go generate ./cli/verb`. Walk
  `surface-parity-checklist` for the new verbs and the `rec` field, and ledger
  a verdict per item.
- [ ] **Step 4: Run** `make test`. Expected: PASS.
- [ ] **Step 5: Commit.**

### Task 8: TUI

**Files:**
- Modify:
  - `tui/bgform.go` (a three-state record field: default / on / off);
  - `tui/detail.go` (`formatTaskDetail` :195);
  - `tui/actions.go` (`onDetail` :389);
  - `tui/dispatch.go` (the generated `TUIDispatch` methods for the record
    verbs);
  - `tui/client.go`.
- Test: `tui/record_test.go`

- **The form:** `BgFormModel` gains a record field cycling
  default → on → off. `Values()` returns it beside the run-mode flags and
  sends no ext byte on default.
- **The detail popup:** `onDetail` also fires `RecordList(task)`. A
  `RecordListMsg{TaskID string; Records []protocol.RecordInfo; Err error}`
  fills a `records:` block in the popup: one line per run with run, size on
  disk, expiry or forever, `truncated`, and `recording`. "loading…" shows
  until the message arrives.
- **The command line** takes every `record` verb through the generated
  dispatch. `record get` from the TUI writes to the `-o` file only, and is
  refused without `-o`, since a raw byte dump in the result pane would draw
  escape sequences into the TUI.

- [ ] **Step 1: Write the failing tests:**
  - `TestBgFormRecordCycles`;
  - `TestBgFormDefaultSendsNoExt`;
  - `TestDetailShowsRecordsWithSizeAndExpiry`;
  - `TestTuiRecordGetNeedsOutputFile`.
- [ ] **Step 2: Run** `go test ./tui`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make test`. Expected: PASS. Then drive `harness-tui`
  on a dummy harness (Task 10) to check the form and the popup live.
- [ ] **Step 5: Commit.**

### Task 9: WebUI

**Files:**
- Modify:
  - `cmd/harness-webui-wasm/main.go` (exports :95-127; `harnessList` :1651
    gains `run` and `recording`; `sessionReq` option `record`:
    `"default"|"on"|"off"`);
  - `webui/index.html` (a Compose select);
  - `webui/static/main.js` (`sessionReq` :372, Compose, `buildTaskSheet`
    :6306).
- Test: `webui/static/*.test.js` per `make js-test`

- **wasm exports:**
  - `recordList(taskId)` → array of `{run, startedNs, endedNs, endReason,
    truncatedFromNs, diskBytes, entryBytes, forever, expiresNs, recording}`;
  - `recordGet(ref, format, withSynth)` → `Uint8Array`, rendered in Go by
    Task 7's functions;
  - `recordDelete(ref, allRuns)`, `recordKeep(ref, forever, ttlMs)`,
    `recordSwitch(taskId, on)`.
- **Compose:** a `record` select (server default / on / off).
- **Task sheet:**
  - A "Records" item opens a sub-sheet listing each run with its size and
    expiry, and buttons for download raw, download asciicast and delete.
    Downloads go through `triggerDownload` (`webui/static/files.js:334`) as
    `<id8>-run<n>.raw` / `.cast`.
  - A "Record on" / "Record off" item for a live interactive session.

- [ ] **Step 1: Write the failing JS tests:**
  - the Compose select maps to the `record` option, and default sends none;
  - the sheet lists a record's size and expiry;
  - the download names.
- [ ] **Step 2: Run** `make js-test`. Expected: FAIL.
- [ ] **Step 3: Implement.** Check it in Playwright on a dummy harness:
  Compose with record on, the sheet's records list, a download. Keep the
  screenshots and report their path.
- [ ] **Step 4: Run** `make js-test` and `make test`. Expected: PASS.
- [ ] **Step 5: Commit.**

### Task 10: Docs and live verification

**Files:**
- Modify:
  - `README.md` (a "5d. PTY records" block next to 5c, plus the capability
    list);
  - `runner/agentskills/session-debugging/SKILL.md` (a short "keeping a
    session: `record`" note beside `snapshot --raw`) and its mirrors under
    `.claude/skills/` and `.agents/skills/` (`TestMirrorsMatchEmbeddedSkills`).

- [ ] **Step 1: Write the docs.** Cover:
  - the defaults and the four server flags;
  - the gates, including how `exclude_self` protects a task's own record;
  - that input and the stream kind are never recorded;
  - the caps keeping the most recent part;
  - `--format asciicast` playing in `asciinema play`.
- [ ] **Step 2: Dummy harness, live** (`dummy-harness` skill; the bash
  profile; `down` afterwards):
  - `session new -d --agent bash --record`. Type a few commands, including
    `printf 'あい\n'`.
  - `record get --without-synth` contains what
    `session snapshot --raw --without-synth` returns.
  - `record get --format asciicast`: every line parses as JSON
    (`python3 -c` over the lines; asciinema is not installed).
  - Restart the server deliberately (SIGTERM, relaunch with the same flags)
    mid-session. Then type again: `record ls` shows one record for the run,
    and its bytes hold both halves once.
  - `record off` without `record_delete` from a bash-profile task: refused.
  - Start the server with `--record-run-max 1MiB` and print about 3 MiB.
    `record ls` shows `truncated`, and the record plays from a correct screen.
  - The TUI `d` popup and the WebUI sheet show size and expiry.
- [ ] **Step 3: Run** `make test`, `make js-test`, `make build`.
  Expected: PASS.
- [ ] **Step 4: Commit.**
