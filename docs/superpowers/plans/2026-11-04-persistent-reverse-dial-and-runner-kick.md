# Persistent reverse dial and runner kick — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server keeps reverse-dialed runners connected across
disconnects and its own restarts, one runner can be kicked on its own, and
every runner row says how that runner is connected.

**Architecture:**
- The server stores dial records in `<data-dir>/runner-dials.json`. A
  `dialManager` runs one dial loop per record, started after WAL replay.
- Every server-initiated connection carries a `DialOrigin` to the
  runner-hello handler. That origin checks the bound key (D12) and sets the
  runner's `link` (D21).
- A new `RunnerRequest{leave}` makes a runner exit 0. A listen-mode runner
  keeps one server connection, refuses to start without a server key, and
  drains before it exits.

**Tech Stack:** Go; brgen `.bgn` schema (`make protoregen`); verb table
codegen (`go generate ./cli/verb`); bubbletea TUI; wasm + JS WebUI.

**Spec:** `docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md`
(decisions D1–D22, problems P1–P6). Read its Problem section before each
task, not only the section the task implements (implementation-pitfalls
Pitfall 1).

## Global Constraints

- Before writing code, read `.claude/skills/implementation-pitfalls/SKILL.md`
  in full. Walk `.claude/skills/surface-parity-checklist/SKILL.md` items 1–39
  in Task 8 and Task 9.
- Work in this worktree on branch `harness/70fbad4a6eb6f1e992be8a669f1bcefd`.
  Use paths under the worktree, never the parent checkout.
- All `.bgn` changes go in Task 1, and only there (`feedback_no_split_schemas`).
  Every new enum value and field is **appended**; existing ordinals do not move.
- Limits, verbatim from the spec:
  - at most 8 candidates per record
  - each candidate at most 255 bytes, all candidates at most 512 bytes in total
  - a label at most 64 bytes
  - `by` at most 64 bytes
  - a key prefix and a runner-identity prefix of at least 8 hex characters
  - a control message at most 1170 bytes
- Timeouts and backoff, verbatim from the spec:
  - per-candidate dial timeout 10 s
  - dial backoff 500 ms, doubling, capped at 30 s, reset after a successful attempt
  - kick waits 10 s for the connection to end
  - listen-mode drain waits at most 5 s
- Capabilities (D11): `runner dial add/ls/rm` → `runner_admin`; `runner kick`
  → `runner_enroll`.
- Compile-check with `go build ./...` or `go vet`. Never use a bare
  `go build ./cmd/x`, which drops a binary in the worktree.
- Full test run, from inside a task:
  `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`.
  Without the `env -u`, two UDP-forward tests fail on the inherited key.
- Every commit message ends with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A host-name candidate that does not resolve yet.**
   - Expected: `runner dial add` accepts it, `runner dial ls` shows
     `waiting` with the resolution error, and the record connects once DNS
     answers.
   - Test: `TestDialerUnresolvableCandidateWaitsThenConnects` (Task 4).
2. **A candidate on a transport the server has no leg for** (`udp:` against
   a ws-only server).
   - Expected: `invalid_candidates` at add. It must not be accepted and then
     fail forever.
   - Test: `TestRunnerDialAddRefusesTransportWithoutLeg` (Task 4).
3. **Two slots on one host, one key, two records on different ports.**
   - Expected: both stay connected. D20's duplicate rule fires only when one
     identity is displaced, never just because two records share a key.
   - Test: `TestTwoSlotsOneKeyAreNotDuplicates` (Task 4).
4. **A record loaded at startup whose key is no longer enrolled**
   (`runner-keys.json` edited by hand).
   - Expected: the record is kept, and every attempt fails with
     `key not enrolled` as its last error. It is never deleted silently.
   - Test: `TestLoadedRecordWithUnknownKeyStaysAndReportsIt` (Task 4).
5. **A kick after the listen-mode runner restarted** (new identity, same
   port), while the record is in `waiting`.
   - Expected: the record is removed by its address.
   - Test: `TestKickRemovesRecordByAddressAfterRunnerRestart` (Task 5).

---

### Task 1: Wire schema

**Files:**
- Modify: `runner/protocol/message.bgn`; regenerate its Go with `make protoregen`
- Test: `runner/protocol/dial_kick_size_test.go` (new)

**Interfaces — Produces** (all appended):
- `enum TaskControlKind`: `runner_dial_add`, `runner_dial_list`,
  `runner_dial_remove`, `runner_kick`, after the current last value.
- `enum RunnerRequestType`: `leave`, after `rebind_session`.
- `enum RunnerDialStatus :u8 { ok, invalid_candidates, key_not_found,
  ambiguous_key, not_found, duplicate_candidate }`
- `enum RunnerDialState :u8 { dialing, connected, waiting, duplicate }`
- `enum RunnerKickStatus :u8 { left, closed_without_leaving, not_found,
  ambiguous_prefix, in_use }`
- `enum RunnerLink :u8 { dialed_in, dialed_out, dial_record, via }`
- `format RunnerDialCandidate { text_len :u8, text :[text_len]u8 }`
- `format RunnerDialAddRequest { key_prefix_len :u8, key_prefix, label_len
  :u8, label, candidates_len :u8, candidates :[..]RunnerDialCandidate }`
- `format RunnerDialAddResponse { request_id :u32, status :RunnerDialStatus,
  id :u32 }`
- `format RunnerDialListRequest { reserved :u8 }`
- `format RunnerDialListResponse { request_id :u32, stream_id :u64 }`. Rows
  travel on the stream.
- `format RunnerDialRow { id :u32, label, key :[32]u8, candidates,
  state :RunnerDialState, runner :RunnerID, answered :u8 (index into
  candidates, 0xff = none), next_attempt_unix_ms :u64, last_error (u16 len),
  added_at_unix_ms :u64, added_by (u8 len), duplicate_of :u32 }`
- `format RunnerDialListBody { rows_len :u16, rows }`
- `format RunnerDialRemoveRequest { id :u32 }` →
  `RunnerDialRemoveResponse { request_id :u32, status :RunnerDialStatus }`
- `format RunnerKickRequest { prefix_len :u8, prefix, force :u1, reserved :u7 }`
  → `RunnerKickResponse { request_id :u32, status :RunnerKickStatus,
  stream_id :u64 }`
- `format RunnerKickBody { runner :RunnerKeyUser, tasks_len :u16,
  tasks :[..]TaskID, removed_dials_len :u8, removed_dials :[..]u32 }`
- `RunnerUnenrollBody`: append `removed_dials_len :u8, removed_dials :[..]u32`.
- `format LeaveRequest { by_len :u8, by }`; `RunnerRequest` gets the match
  arm `leave => leave :LeaveRequest`.
- `RunnerInfo`: append `link :RunnerLink, link_record :u32, link_via :RunnerID`.
- The four new `TaskControlKind` values each get a request arm and a response
  arm, in the same two `match` blocks where `runner_enroll` sits
  (`message.bgn:2768`, `:2861`).

Put each format next to its `RunnerEnroll*` / `RunnerUnenroll*` sibling, and
keep the sibling's comment style: one line on what the message is for and
which cap gates it.

- [ ] **Step 1: Write the failing size test**

  `TestDialKickControlMessagesFitOneDatagram`:
  - builds `RunnerDialAddRequest` with eight candidates totalling 512 bytes, a
    64-byte prefix and a 64-byte label;
  - builds `RunnerKickRequest` with a 64-byte prefix, and `LeaveRequest` with
    a 64-byte `by`;
  - wraps each in its `TaskControlRequest` / `RunnerRequest` and asserts
    `len(encoded)+1 <= 1170`.
  - Negative control: one more 255-byte candidate must exceed 1170. That shows
    the test can fail.
- [ ] **Step 2: Run it.** `go test ./runner/protocol -run TestDialKick`.
  Expected: build failure (the types do not exist).
- [ ] **Step 3: Edit `message.bgn` as above, then run `make protoregen`.** The
  regen churn is expected (`feedback_bgn_is_defined_over_handwritten_switch`).
  Check build, vet, the new symbols, and nothing else.
- [ ] **Step 4: Run it.** `go test ./runner/protocol -run TestDialKick` → PASS.
  `go build ./... && go vet ./runner/protocol` → clean.
  `go test ./server -run TestResponseRowsAreNotInline` → PASS (the new list
  responses carry rows on a stream).
- [ ] **Step 5: Commit** `protocol: dial records, runner kick, leave, RunnerInfo.link`.

### Task 2: Server dial-record store

**Files:**
- Create: `server/runnerdials.go`, `server/runnerdials_test.go`
- Modify: `cmd/harness-server/main.go`. Load `<data-dir>/runner-dials.json`
  next to `runner-keys.json` and pass it as `server.Config.RunnerDials`.

**Interfaces — Produces:**
```go
type RunnerDialRecord struct {
    ID uint32; Candidates []string; Key ed25519.PublicKey
    Label string; AddedAt time.Time; AddedBy string
}
func LoadRunnerDialStore(path string) (*RunnerDialStore, error) // "" = memory only
func (st *RunnerDialStore) Add(rec RunnerDialRecord) (RunnerDialRecord, error) // assigns a random non-zero unique ID
func (st *RunnerDialStore) Remove(id uint32) (RunnerDialRecord, error)         // ErrDialNotFound
func (st *RunnerDialStore) RemoveByKey(key ed25519.PublicKey) []RunnerDialRecord
func (st *RunnerDialStore) List() []RunnerDialRecord                            // oldest first
func (st *RunnerDialStore) OwnerOfCandidate(text string) (uint32, bool)         // exact text match, D20
```
Mirror `server/runnerkeys.go`:
- JSON array of `{id, candidates, key (hex), label, added_at, added_by}`;
- written to a temporary file then renamed, mode 0600;
- an absent file means an empty store, and a file that does not parse is an
  error.

Validation of candidates is not done here; Task 4 does it.

- [ ] **Step 1: Write the failing tests:**
  - `TestRunnerDialStoreRoundTrip`: add two, reload from the same path,
    `List` equal and in order.
  - `TestRunnerDialStoreUnparsableFails`: `LoadRunnerDialStore` on `"{"` returns
    an error.
  - `TestRunnerDialStoreRemoveByKey`: two records on key A, one on B;
    `RemoveByKey(A)` returns two and leaves one.
  - `TestRunnerDialStoreOwnerOfCandidate`.
- [ ] **Step 2: Run them.** `go test ./server -run TestRunnerDialStore` → FAIL
  (undefined).
- [ ] **Step 3: Implement** `server/runnerdials.go`, and the `Config.RunnerDials`
  field. `New` defaults it to a memory-only store, as it does for `RunnerKeys`
  (`server/server.go:237`). Load it in main.
- [ ] **Step 4: Run them.** `go test ./server -run TestRunnerDialStore` → PASS.
- [ ] **Step 5: Commit** `server: runner-dials.json store`.

### Task 3: Dial origin — bound-key check before registration, and `RunnerInfo.link`

**Files:**
- Modify: `server/dial_runner_handler.go`, `server/server.go` (the
  `OnDialed` wiring at :762-776; `pendingViaInfo` becomes `pendingDialOrigin`),
  `server/runner_handler.go` (:160-195), `server/registry.go` (`RunnerEntry`),
  `server/task_handler.go` (`toRunnerInfo`, :2504)
- Test: `server/dial_origin_test.go` (new)

**Interfaces — Produces:**
```go
type DialOrigin struct {
    Link      protocol.RunnerLink   // dialed_out, dial_record or via; never dialed_in
    RecordID  uint32                // dial_record only
    ExpectKey ed25519.PublicKey     // dial_record only (D12)
    Via       *ViaRegistrationInfo  // via only
    OnRegistered func(identity protocol.RunnerID) // dialer's success signal; may be nil
    OnRefused    func(reason string)              // e.g. "key mismatch: <prefix16>"; may be nil
}
// OnDialed's third parameter becomes *DialOrigin (was *ViaRegistrationInfo).
// RunnerHandler.TakePendingViaInfo is replaced by:
TakePendingDialOrigin func(cid objproto.ConnectionID) *DialOrigin
// Factored out of Handle so the dialer and the one-shot verb share it:
func (h *DialRunnerHandler) DialCandidate(ctx context.Context, cid objproto.ConnectionID, origin *DialOrigin) error
// RunnerEntry gains: Link protocol.RunnerLink; LinkRecord uint32; LinkVia protocol.RunnerID
// Server gains:      func (s *Server) setRunnerLink(identity protocol.RunnerID, link protocol.RunnerLink, record uint32) // adoption, D20
```
Behaviour:
- An accept-loop connection has no origin → `dialed_in`.
- `Handle` passes `{Link: dialed_out}`; `HandleWithVia` passes `{Link: via, Via: …}`.
- In the hello handler, check **before** `Registry.Add`:
  - `origin.ExpectKey != nil && !entry.Key.Equal(origin.ExpectKey)` →
  - call `OnRefused("key mismatch: "+keyPrefix(entry.Key))`, then `KickConn`;
  - no `RunnerHelloResponse` is sent, and nothing is added to the registry.
  - This is the same shape as the `ErrIdentityKeyMismatch` branch at
    `runner_handler.go:177-189`.
- On a successful `Add`, call `OnRegistered(identity)`.
- `toRunnerInfo` copies `Link`, `LinkRecord`, `LinkVia`.

- [ ] **Step 1: Write the failing tests.** Use the in-process server and listen-runner
  harness of `integration/server_dial_runner_e2e_test.go`, or the
  `server/dial_runner_handler_test.go` fakes when those suffice:
  - `TestAcceptedRunnerLinkIsDialedIn`
  - `TestOneShotDialLinkIsDialedOut`
  - `TestViaDialLinkIsVia` (reuse the existing via fixture)
  - `TestRecordOriginWrongKeyRefusedBeforeRegistry`. Assert all four:
    - no registry entry;
    - `OnRefused` got `"key mismatch: "`;
    - no `RunnerHelloResponse` reached the runner;
    - a runner that was already registered on another connection keeps its
      entry and its task.
  - `TestRecordOriginRightKeyCallsOnRegistered`
- [ ] **Step 2: Run them.** `go test ./server -run 'Link|RecordOrigin'` → FAIL.
- [ ] **Step 3: Implement.** Rename every `TakePendingViaInfo` /
  `pendingViaInfo` call site; `grep -rn PendingViaInfo` must return nothing
  afterwards.
- [ ] **Step 4: Run them, then the package.** The named tests PASS, and
  `go test ./server ./integration` passes. The server package has a known
  flake rate of ~1/6 per run (`project_flaky_test_open_interactive_sessionmux`).
  If a failure is in a test you did not touch, re-run it before
  investigating.
- [ ] **Step 5: Commit** `server: dial origin carries the bound key and the runner's link`.

### Task 4: Dial manager and the `runner dial add / ls / rm` handlers

**Files:**
- Create: `server/runner_dialer.go`, `server/runner_dialer_test.go`,
  `server/runner_dial_handler.go`, `server/runner_dial_handler_test.go`
- Create: `cli/candidate.go`. Move `runner.ResolveServerCandidate` here as
  `cli.ResolveCandidate`, and add `cli.ParseCandidate` (parses without
  resolving). `runner/servercandidates.go` then calls these.
- Modify:
  - `server/server.go`: start the manager inside `serve`, after `ReplayEvents`
    (:868), `SetWAL` (:889) and `go s.watchHeldDeadlines` (:972); cancel it
    before the connection drain on shutdown.
  - `server/task_handler.go`: the dispatch switch at :1125-1136.
  - `server/capabilities.go`: the `requiredCap` rows.
  - `server/runner_keys_handler.go`: the unenroll cascade.

**Interfaces — Consumes:** Task 2's store; Task 3's `DialCandidate`,
`DialOrigin`, `setRunnerLink`.

**Interfaces — Produces:**
```go
type dialManager struct{ /* per-record loops */ }
func newDialManager(st *RunnerDialStore, d *DialRunnerHandler, reg *Registry, keys *RunnerKeyStore, legs func(transport string) bool) *dialManager
func (m *dialManager) Start(ctx context.Context)                  // starts every stored record
func (m *dialManager) Add(rec RunnerDialRecord)                    // starts one loop
func (m *dialManager) Stop(id uint32)                              // ends one loop, leaves its connection alone (D17)
func (m *dialManager) States() []protocol.RunnerDialRow
func (m *dialManager) RecordsPointingAt(e RunnerEntry) []uint32    // used by kick, Task 5
func (m *dialManager) onDisplaced(displacedCID, byCID objproto.ConnectionID) // wired from OnRunnerDisplaced, D20
func (h *TaskHandler) handleRunnerDialAdd(conn ConnHandle, requestID uint32, r *protocol.RunnerDialAddRequest)
func (h *TaskHandler) handleRunnerDialList(conn ConnHandle, requestID uint32)
func (h *TaskHandler) handleRunnerDialRemove(conn ConnHandle, requestID uint32, r *protocol.RunnerDialRemoveRequest)
```

The per-record loop (spec §1.2) runs these steps:
1. **Adopt (D20).** If the registry has a runner on `rec.Key` whose
   ConnectionID matches a candidate's resolved transport and address
   (compare those two only), the record takes that connection as its live
   one: `setRunnerLink(identity, dial_record, rec.ID)`. Skip to step 4.
2. **Walk the candidates in order.** For each: `cli.ResolveCandidate`, then
   `DialCandidate` with the 10 s timeout and the origin `{dial_record,
   rec.ID, rec.Key, OnRegistered, OnRefused}`. The first that completes ECDH
   ends the walk.
3. **Success.** `OnRegistered` means success, and resets the backoff. A
   connection that ends without registering is a failure, with the refusal
   reason or `"closed before registering"`.
4. **While connected,** wait for the connection to end. Hook this onto
   `handleConnection`'s return in the `OnDialed` goroutine, or onto the
   registry's `OnRemove` for an adopted connection. Then back off: 500 ms
   doubling to 30 s.
5. **If the record's key is not enrolled** at attempt time, the attempt fails
   with `key not enrolled`.

Handler rules:
- **add:**
  - resolve `key_prefix` with `RunnerKeys.Resolve` (`key_not_found` /
    `ambiguous_key`);
  - apply the limits from Global Constraints;
  - every candidate must pass `cli.ParseCandidate` and name a transport the
    endpoint has a leg for; otherwise `invalid_candidates`;
  - a candidate equal to an existing record's text → `duplicate_candidate`;
  - `AddedBy` comes from `h.enrolledBy`; then `store.Add` + `manager.Add`.
- **list:** `respondWithBody` with `RunnerDialListBody{Rows: manager.States()}`.
- **rm:** `store.Remove` + `manager.Stop`; the connection stays (D17).
- **unenroll:** after `RunnerKeys.Remove`, call `RunnerDials.RemoveByKey`,
  stop each removed loop, and put the ids in `RunnerUnenrollBody.removed_dials`.
  The `--force` guard still counts live runners only.

- [ ] **Step 1: Write the failing tests:**
  - `TestRunnerDialAddValidates`: one case per status.
  - `TestRunnerDialAddRefusesTransportWithoutLeg`
  - `TestRunnerDialCapabilities`: add/ls/rm need `runner_admin`, and
    `runner_enroll` alone is refused.
  - `TestDialerConnectsAndRedialsAfterDrop`: kill the connection; it is back
    within the backoff.
  - `TestDialerBackoffResetsAfterSuccess`
  - `TestDialerWalksCandidatesInOrder`: the first candidate is dead and the
    second answers.
  - `TestDialerUnresolvableCandidateWaitsThenConnects`
  - `TestDialerAdoptsOneShotConnectionWithoutTouchingTasks`: the runner's
    task is still Running and no second connection is made.
  - `TestRuntimeDuplicateStopsDialing`: two records naming one runner by
    different spellings; after one switch the displaced record is
    `duplicate`, and no further switch happens within 3× the backoff cap.
  - `TestTwoSlotsOneKeyAreNotDuplicates`
  - `TestLoadedRecordWithUnknownKeyStaysAndReportsIt`
  - `TestRemoveLeavesConnectionUp`
  - `TestUnenrollRemovesBoundRecords`: answer lists them, store empty, loops
    stopped.
  - `TestDialManagerStartsAfterReplayAndReadoptsHeldTask`: a held task in the
    WAL plus a stored record; the restarted server re-adopts the task, and
    the task is not Failed.
- [ ] **Step 2: Run them.** `go test ./server -run 'Dial|Unenroll'` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them, then the server, integration, cli and runner packages.**
  `go test ./server ./integration ./cli ./runner` → PASS. Apply the flake note
  from Task 3.
- [ ] **Step 5: Commit** `server: persistent reverse dial records`.

### Task 5: Kick (server)

**Files:**
- Create: `server/runner_kick_handler.go`, `server/runner_kick_handler_test.go`
- Modify: `server/task_handler.go` (dispatch), `server/capabilities.go`
  (`runner_kick` → `runner_enroll`)

**Interfaces — Consumes:** `dialManager.RecordsPointingAt`, `Stop`; `Server.kickConn`.

**Interfaces — Produces:**
```go
func (h *TaskHandler) handleRunnerKick(conn ConnHandle, requestID uint32, r *protocol.RunnerKickRequest)
```
Steps (spec §2):
1. Resolve the prefix against the live registry identities, with at least 8
   hex characters (`not_found` / `ambiguous_prefix`).
2. If the runner has non-terminal tasks and `force` is not set: `in_use`, with
   those tasks in the body.
3. For each `RecordsPointingAt(entry)`, remove it from the store and `Stop`
   its loop. That covers the live connection, the last registered identity,
   and the candidate address equal to the runner's ConnectionID.
4. Send `RunnerRequest{leave, by: h.enrolledBy(caller)}`.
5. Wait up to 10 s for the connection to leave the registry → `left`.
   Otherwise `kickConn` → `closed_without_leaving`.

The response body carries the runner identity and hostname, the tasks, and
the removed record ids.

- [ ] **Step 1: Write the failing tests:**
  - `TestKickStatuses`: `not_found`, `ambiguous_prefix`, `in_use` listing
    tasks.
  - `TestKickNeedsRunnerEnroll`
  - `TestKickRemovesLiveRecord`
  - `TestKickRemovesRecordByAddressAfterRunnerRestart`
  - `TestKickOldRunnerFallsBackToClose`: a fake runner that ignores `leave`;
    after 10 s the status is `closed_without_leaving` and the connection is
    gone. Inject the wait as a field so the test uses a short one.
- [ ] **Step 2: Run them.** `go test ./server -run Kick` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them.** `go test ./server -run Kick` → PASS.
- [ ] **Step 5: Commit** `server: runner kick`.

### Task 6: Runner — `leave`, listen-mode key requirement, one server connection, drain

**Files:**
- Modify:
  - `runner/connect.go`: the dispatch switch at :701, the `RunnerHelloResponse`
    case at :740.
  - `runner/listen.go`: `ListenAndServe`, `handleServerConn`.
  - `runner/session.go`: hook fields.
  - `cmd/agent-runner/main.go`: after `cli.ResolveServerPubKey` (~:449);
    `OnLeave`; the help text of `--listen` / `--udp-listen`.
- Test: `runner/leave_test.go`, `runner/listen_single_conn_test.go`,
  `cmd/agent-runner/listen_flag_test.go` (extend)

**Interfaces — Produces:**
```go
// runner.Config
OnLeave func(by string)            // main: cancel the process context; exit stays 0
// runner.Session
OnHelloAccepted func()             // listen mode: retire the previous server connection, synchronously
```

Behaviour:
- **`leave`.** Log `leaving on the server's request` with `by`, then call
  `cfg.OnLeave(by)`. Dial mode: `PersistLoop` returns nil on ctx cancel, so
  `main` returns and the exit status is 0. Listen mode: `ListenAndServe`
  returns `context.Canceled`, which `main` already treats as clean.
- **`RunnerHelloResponse`.** Call `session.OnHelloAccepted` *before*
  `killHeldExcept` and before any later message is dispatched (D13). Listen
  mode's implementation does the following, then lets dispatch continue:
  - `prev.pc.Close()` (the `peer.Conn` Close, which sends the wire Close);
  - waits for `prev`'s `OnConnect` to return.
- **Drain.** On `ctx.Done`, `ListenAndServe` waits for the current server
  connection's done channel, for at most 5 s, then returns.
- **D10.** `isListenMode() && serverPubKey == nil` → print
  `agent-runner: --listen/--udp-listen needs the server's key (--server-pubkey or --server-pubkey-file): without it any dialer can act as the server`
  and exit 1.

- [ ] **Step 1: Write the failing tests:**
  - `TestLeaveCallsOnLeave`
  - `TestDialModeLeaveDoesNotReconnect`: `PersistLoop` with an OnLeave that
    cancels; there is no second dial.
  - `TestListenSecondConnRetiresFirstBeforeAssign`. Write it red first:
    - the first connection has a running task;
    - a second verified connection gets `RunnerHelloResponse` and then
      assigns task B;
    - assert that the first connection's teardown happened before B was
      accepted, and that B is still running after the first connection's
      `OnConnect` returned.
  - `TestServerSendsHelloResponseBeforeAssign`, in `server/`: the order D13
    relies on.
  - `TestListenDrainSendsCloseOverUDP`: a UDP-only listener, cancel, and the
    peer sees the close within 1 s.
  - `TestListenWithoutServerKeyRefuses`, in `cmd/agent-runner`: run the flag
    path, expect exit 1 and the message.
- [ ] **Step 2: Run them.**
  `go test ./runner ./cmd/agent-runner ./server -run 'Leave|Listen|HelloResponseBefore'`
  → FAIL. The retire test must fail on the current code, by B being
  cancelled.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them, then the packages.**
  `go test ./runner ./cmd/agent-runner ./integration` → PASS.
- [ ] **Step 5: Commit** `runner: leave on request; listen mode needs the server key, keeps one server connection, drains`.

### Task 7: Client functions and output formats

**Files:**
- Create: `cli/runner_dials.go`, `cli/runner_dials_test.go`
- Modify: `cli/runner_keys.go`. `RunnerUnenrollResult` gains
  `RemovedDials []uint32`, and `FormatRunnerUnenroll` prints them.

**Interfaces — Produces.** These mirror `cli/runner_keys.go`: the `*With`
forms take the long-lived client, the short forms dial and close, and
`Format*` is the only text producer.
```go
type RunnerDialRow struct { ID uint32; Label string; Key ed25519.PublicKey; Candidates []string
    State protocol.RunnerDialState; Runner protocol.RunnerID; Answered int // -1 none
    NextAttempt time.Time; LastError string; AddedAt time.Time; AddedBy string; DuplicateOf uint32 }
type RunnerKickResult struct { Runner RunnerKeyUser; Tasks []protocol.TaskID; RemovedDials []uint32 }
func RunnerDialAddWith(ctx context.Context, c taskControlClient, candidates []string, keyPrefix, label string) (protocol.RunnerDialStatus, uint32, error)
func RunnerDialListWith(ctx context.Context, c *Client) ([]RunnerDialRow, error)
func RunnerDialRemoveWith(ctx context.Context, c taskControlClient, id uint32) (protocol.RunnerDialStatus, error)
func RunnerKickWith(ctx context.Context, c *Client, prefix string, force bool) (protocol.RunnerKickStatus, RunnerKickResult, error)
// + RunnerDialAdd / RunnerDialList / RunnerDialRemove / RunnerKick (server objproto.ConnectionID) short forms
func FormatRunnerDialAdd(st protocol.RunnerDialStatus, id uint32, candidates []string) (string, bool)
func FormatRunnerDialList(rows []RunnerDialRow) string
func FormatRunnerDialListJSON(rows []RunnerDialRow) string
func FormatRunnerDialRemove(st protocol.RunnerDialStatus, id uint32) (string, bool)
func FormatRunnerKick(st protocol.RunnerKickStatus, res RunnerKickResult) (string, bool)
func RunnerLinkStr(r protocol.RunnerInfo) string // "in" | "out" | "dial:<8 hex>" | "via:<8 hex>"
func FormatDialID(id uint32) string              // 8 lower hex
```

Output rules (checklist 29 and 31):
- Every message names its target, for example
  `dial 3fa9c012 added: ws:runner.example:8540-*`.
- `closed_without_leaving` says
  `the runner did not leave (too old to know the request?); a dial-mode runner reconnects`.
- A list row prints every field, including `runner=-` when there is no
  runner. A zero is printed, never elided.

- [ ] **Step 1: Write the failing tests:**
  - `TestFormatRunnerDialListPrintsEveryField`
  - `TestFormatRunnerKickEveryStatus`
  - `TestRunnerLinkStr`: all four values.
  - `TestDialIDRoundTrip`: `FormatDialID` and the parse used by `rm`.
- [ ] **Step 2: Run them.** `go test ./cli -run 'RunnerDial|RunnerKick|RunnerLink|DialID'` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them.** The same command → PASS.
- [ ] **Step 5: Commit** `cli: runner dial / kick client and formats`.

### Task 8: Verbs on every surface

**Files:**
- Modify:
  - `cli/verb/table.go`: four rows after `runner unenroll`, then run
    `go generate ./cli/verb`.
  - `cli/verb/caps.go`: `CapDescription` for `runner_admin` and
    `runner_enroll`.
  - `cmd/harness-cli/dispatch.go`, `tui/dispatch.go`, `tui/runner_keys.go`
    (add `DoRunnerDialAdd/List/Remove`, `DoRunnerKick`).
  - `cmd/harness-webui-wasm/main.go`: four bridge functions next to
    `harnessRunnerUnenroll` (:3160).
  - `webui/static/main.js`: the `case "runner"` switch at :9298. A `dial`
    sub-path routes on `b.path[2]`.
  - `README.md`: the reverse-dial section.

**Interfaces — Consumes:** Task 7.

Rows (copy the `runner unenroll` row's shape, `CmdlineSurfaces: CLI | TUI | WebUI`,
`NoModalSurface` with the D19 reason):

| Path | Args | Flags | WebUIDispatch | Notes end with |
| --- | --- | --- | --- | --- |
| `runner dial add` | `candidates` (one comma-separated list, as `--server-cid`) | `--key` (required), `--label` | `runnerDialAdd` | `needs runner_admin` |
| `runner dial ls` | — | `--json` (CLI only, same `SurfaceReason` as `runner keys`) | `runnerDialList` | `needs runner_admin` |
| `runner dial rm` | `id` (8 hex) | — | `runnerDialRemove` | `stops re-dialing; the live connection stays — runner kick to disconnect` / `needs runner_admin` |
| `runner kick` | `runner-id` (≥8 hex of the id= column) | `--force` | `runnerKick` | `the runner exits; it returns after its host restarts unless its key is unenrolled` / `needs runner_enroll` |

The `--key` requirement is enforced in the action's `Build`, not in a surface
(checklist 2). Every TUI `Do*` takes `h.a.client` (Pitfall 3).

- [ ] **Step 1: Write the failing tests:**
  - `TestRunnerDialAddNeedsKey`, in `cli/verb`.
  - `TestCapDescriptionNamesDialAndKick`.
  - The existing invariants must stay green: `TestEveryWebUIVerbDeclaresDispatch`,
    `TestNoHandWrittenVerbFlagSetsRemain`, and the `modal_surface_test.go` check.
- [ ] **Step 2: Run them.** `go test ./cli/verb` → FAIL.
- [ ] **Step 3: Implement**, then `go generate ./cli/verb` and `make webui-build`.
- [ ] **Step 4: Run them.** `go test ./cli/... ./tui/... ./cmd/...` → PASS;
  `make wasm-check` → clean.
- [ ] **Step 5: Walk the surface-parity checklist items 1–39** and record a
  verdict per number in `.claude/skills/surface-parity-checklist/firing-log.md`.
- [ ] **Step 6: Commit** `runner dial add/ls/rm and runner kick on every surface`.

### Task 9: `link` on runner rows

**Files:**
- Modify:
  - `cli/list.go`: the runner JSON struct near :408 and the row renderer.
  - `tui/runners.go`: `runnerColsFor`, plus `linkColumnMinWidth = 120`.
  - `tui/detail.go`: `formatRunnerDetail`, near :129.
  - `cmd/harness-webui-wasm/main.go`: the runner conversion near :1725.
  - `webui/static/main.js`: the runner row meta near :8770.

**Interfaces — Consumes:** `cli.RunnerLinkStr` (Task 7), the only formatter
of the value (checklist 32).

Placement:
- **`ls` row:** `link=<RunnerLinkStr>` after `key=`.
- **`ls --json`:** `link` (the enum name), `link_record` (8 hex or `""`),
  `link_via` (32 hex or `""`).
- **TUI table:** a `Link` column, 12 wide, before `Roots`, only when width
  ≥ 120. It goes through `applyColumns`, since the column set varies with
  width (checklist 34).
- **TUI detail:** `link:` at every width, with the full id.
- **wasm:** `link`, `linkRecord`, `linkVia`.
- **WebUI row meta:** `link` next to `key`.

- [ ] **Step 1: Write the failing tests:**
  - `TestListRunnerRowShowsLink`
  - `TestListRunnerJSONHasLinkFields`
  - `TestRunnerTableLinkColumnFrom120`: present at 120, absent at 119.
    `TestViewFitsTerminalWidth` must still pass at both widths.
  - `TestRunnerDetailShowsLink`
- [ ] **Step 2: Run them.** `go test ./cli ./tui -run 'Link|ViewFits'` → FAIL.
- [ ] **Step 3: Implement**, then `make webui-build`.
- [ ] **Step 4: Run them.** The same command → PASS.
- [ ] **Step 5: Re-walk the checklist items 11–23, 31, 32 and 34 for the new
  field** (a continuation is a new walk), and append to `firing-log.md`.
- [ ] **Step 6: Commit** `runner rows show how the runner is connected`.

### Task 10: End-to-end and landing

- [ ] **Step 1: Run the whole suite.** `env -u HARNESS_SERVER_PUBKEY -u
  HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`
  → PASS.
- [ ] **Step 2: Run the wire-skew check.** `scripts/wire-skew-check.sh` →
  PASS. `leave` is a runner-decoded format; an old runner must log the
  decode error and keep running.
- [ ] **Step 3: Dummy-harness E2E** (`dummy-harness` skill; `make build` in
  this worktree first; run each command exactly as the help text prints it).
  Bring up the dummy server and a listen-mode runner started with
  `--server-pubkey-file <dummy data-dir>/server-key.pub`. Then:
  1. `runner dial add ws:127.0.0.1:<port>-* --key <8 hex>` → `runner dial
     ls` shows `connected`, and `ls` shows `link=dial:<id>`.
  2. Start an interactive task, then restart the dummy server
     **deliberately** → the task is re-adopted with no manual dial.
  3. Kill the dummy server and start it again → the runner re-registers by
     itself, and the task is Failed.
  4. `runner kick <8 hex>` (with `--force` if a task runs) → the runner
     process exits 0, the record is gone, and the runner does not return.
  5. Start a listen-mode runner without a key → exit 1 with the message.
  6. Open the TUI in a 130-column PTY and in a 100-column PTY. The `Link`
     column is shown at 130 and absent at 100, and the detail popup shows it
     at both (`feedback_live_check_every_surface_not_one`).
  7. Check the WebUI runner row through Playwright.
  8. Keep the screenshots and report their paths.

  Bring the dummy instance down afterwards.
- [ ] **Step 4: Walk checklist item 39.** Check spec §5's Surfaces table row
  by row against the code.
- [ ] **Step 5: Land.** Use the `landing-to-main` skill (Mode A,
  local-trunk FF-push), then run `make build` in the main checkout.
