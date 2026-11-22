# Request → capability (`authority.RequestNeed`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every capability bit the server checks or scopes by is decided in package `authority`, from the request (or a value the handler just looked up), with no server-side check moving or answering differently.

**Architecture:** A new `authority/need.go` holds the decision: `RequestNeed(req)` (clauses + later requirements), `ScopeBit(req, held)`, `Possible(kind)`, `CentralCap(kind)` (the former `requiredCap`), and the lookup-value helpers. Server sites keep their position and order and swap their bit expression for a call. A go/ast guard over `server/` keeps new bits from being written inline.

**Tech Stack:** Go; `runner/protocol` (generated from `.bgn`, unchanged); `go/ast` for the guard.

**Spec:** `docs/superpowers/specs/2026-11-22-request-cap-need-design.md`

## Global Constraints

- No request is accepted, refused, or answered differently than before: same PermissionDenied `RequiredCap`, same status, same order of checks (spec §2 anti-goals, §3.2).
- The §3.4 reported-vs-checked quirks are preserved exactly (ListFiles reports file_read; RecordKeep pre-gate reports record_persist; reach refusals report board_peer / board_observe).
- `server/` and `cli/` test files are not edited to make a refactor commit pass. New pinning tests are added BEFORE the site they pin is changed.
- No `.bgn` / wire change. `scripts/wire-skew-check.sh` is not needed but `scripts/land.py` runs it anyway.
- Verify with `make test` (never bare `go test` inside a task: inherited `HARNESS_*` breaks signature tests).
- Every commit leaves `make test` green.

## Review Focus

1. **A clause's `Report` that differs from its `AnyOf`** (ListFiles, RecordKeep pre-gate, reach) — a refactor that "tidies" it into `AnyOf` changes the denial text. Pinned in Task 3.
2. **ListFiles caller holding only file_write** — scope must resolve through file_write, not file_read. Pinned in Task 3 (scope) and Task 2 (`ScopeBit`).
3. **OpenInteractive check order** — schedule_edit only after `resumeTargetAllowed` (oracle). A clause loop that checks all clauses up front would reorder it. Pinned in Task 3.
4. **ExecRunKill / KillPortForward on an invisible target** — must stay NotFound / NoSuchForward, never PermissionDenied. Pinned in Task 3.
5. **Zero-bodied request vs `Possible`** — `Possible(OpenFileTransfer)` must include file_read even though a zero request's direction is Push. Pinned in Task 2.

---

### Task 1: `authority/need.go` — types, central map, exhaustiveness

**Files:**
- Create: `authority/need.go`
- Create: `authority/need_test.go`

**Interfaces:**
- Produces:
  ```go
  type Clause struct{ AnyOf, Report protocol.Capability }
  type Later struct{ What string; Possible, Report protocol.Capability }
  type NeedClass uint8 // NeedBits, NeedNone, NeedIdentity, NeedRetired, NeedNotRequest (0 = unclassified)
  type Need struct{ Class NeedClass; Clauses []Clause; Later []Later }
  func CentralCap(kind protocol.TaskControlKind) (protocol.Capability, bool) // the former server.requiredCap, verbatim
  func RequestNeed(req *protocol.TaskControlRequest) Need
  ```

- [ ] **Step 1: Write the failing tests** in `authority/need_test.go`:
  - `TestEveryKindIsClassified`: for `k := 0; k <= 88; k++`, a `&protocol.TaskControlRequest{Header: {Kind: k}}` gives `RequestNeed(...).Class != 0`; also asserts `protocol.TaskControlKind(89).String()` is the protocol's unknown spelling, so a new kind 89 makes the loop bound stale and fails.
  - `TestCentralCapIsTheFormerMap`: a literal table of the 43 entries in `server/capabilities.go:44-124` today (kind → bit), asserting `CentralCap` returns each and `false` for every other kind.
  - `TestMapKindsNeedTheirCentralBit`: for each kind with `CentralCap(k)` ok and no other branch (the MAP rows of spec §9), `RequestNeed` = `{Class: NeedBits, Clauses: [{AnyOf: bit, Report: bit}]}`.
  - `TestNoBitKinds`: the 23 NONE kinds of spec §9 → `NeedNone`; Submit, GetTaskLog → `NeedRetired`; ClientHello, PermissionDenied → `NeedNotRequest`; SetCaps, SetParent, TaskRename → `NeedIdentity`.
- [ ] **Step 2:** `make test` limited run: `env -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID -u HARNESS_RUNNER_ID -u HARNESS_SERVER_CID -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_PUBKEY_FILE -u HARNESS_WS_PATH -u HARNESS_PROXY_VIA_RUNNER go test ./authority/ -run 'Kind|Central|NoBit'` (the Makefile's `GO_TEST_ENV`) → FAIL (undefined).
- [ ] **Step 3:** Implement in `authority/need.go`: move the map literal from `server/capabilities.go:44-124` (comments included) as unexported `centralCap`; `CentralCap` reads it. `RequestNeed` switches on `req.Header.Kind`; this task fills MAP / NONE / RETIRED / NOTREQUEST / IDENTITY; REQ/EITHER/LOOKUP kinds temporarily return `NeedBits` with no clauses (Task 2 fills them; the exhaustiveness test already passes). `server/capabilities.go` keeps its own map until Task 4.
- [ ] **Step 4:** Same command → PASS.
- [ ] **Step 5:** Commit `authority: RequestNeed skeleton, CentralCap, every kind classified`.

### Task 2: request-determined and lookup branches, `Possible`, `ScopeBit`

**Files:**
- Modify: `authority/need.go`
- Create: `authority/need_lookup.go` (the §5.2 helpers)
- Modify: `authority/need_test.go`

**Interfaces:**
- Consumes: Task 1 types.
- Produces (lookup-value helpers, pure):
  ```go
  func ResumeCap(resumeID protocol.TaskID, exact bool) protocol.Capability // moved from server resumeRequestCap, body verbatim
  func ForwardCap(d protocol.PortForwardDirection) protocol.Capability    // moved from server forwardCapFor, body verbatim
  func FileTransferCap(d protocol.FileTransferDirection) protocol.Capability // pull/dir_pull → file_read, else file_write
  func RecordSwitchCap(on bool) protocol.Capability                        // on → record_persist, off → record_delete
  func RecordKeepCap(forever bool, ttl time.Duration, curForever bool, curTTL time.Duration) protocol.Capability // rule of record_handlers.go:194-197
  const RecordOnCap = protocol.Capability_RecordPersist
  const UnbindCap   = protocol.Capability_ScheduleEdit
  func SchedulePower(pendingOrNew, bound, binds bool) []Clause // spawn clause when (pendingOrNew && !binds); resume clause when (bound || binds)
  func ScheduleRunPower(state protocol.ScheduleTaskState) []Clause // pending → spawn; bound → ResumeCap(nonzero, true); else none
  func ScopeBit(req *protocol.TaskControlRequest, held protocol.Capability) protocol.Capability
  func Possible(kind protocol.TaskControlKind) protocol.Capability
  ```
- `RequestNeed` branches (clauses in server check order; `Report` = what the server sends today):
  - AttachSession: `[{AttachModeCap(mode), AttachModeCap(mode)}]`
  - OpenFileTransfer: `[{FileTransferCap(dir), same}]`
  - ListFiles: `[{file_read|file_write, file_read}]`
  - RegisterPortForward: `[{ForwardCap(dir), same}]`; OpenPortForward: `[{forward_local, forward_local}]`
  - RecordSwitch: `[{RecordSwitchCap(on), same}]`
  - RestoreTasks: list_only → `NeedNone`; else `[{prune, prune}]`
  - OpenInteractive: `[{ResumeCap(id, exact)}]` + `{exec_control}` when not detached + `{RecordOnCap}` when ext record set and on; `Later{"schedule binding", schedule_edit, schedule_edit}`
  - AwaitIdle / AwaitProgress: sink=notify → `[{notify}]`; sink=board → `Later{"board reach", board_peer, board_peer}`; reply → `NeedNone`
  - SessionMode: `[{cancel}]` + `Later{"schedule binding", schedule_edit, schedule_edit}`
  - KillPortForward: `Later{"forward direction", forward_local|forward_remote|forward_link, ...}` (Report: `ForwardCap` of the looked-up direction — document Report as 0 = "from helper")
  - ExecRunKill: `[{exec_run, exec_run}]` (target looked up; never PermissionDenied — note in comment)
  - RecordKeep: `[{record_persist|record_delete, record_persist}]` + `Later{"record retention", record_persist|record_delete, 0}`
  - ScheduleCreate: `[{schedule_edit}]` + `SchedulePower(true, false, specTask != "")` + `{RecordOnCap}` if spec record on
  - ScheduleEdit: `[{schedule_edit}]` + `Later{"schedule state", spawn|resume, 0}` + record clause if spec record on
  - ScheduleRun: `[{schedule_run}]` + `Later{"schedule state", spawn|resume|resume_exact, 0}`
  - BoardRetain/Flush/Archive: `[{board_persist}]` + `Later{"board reach", 0, 0}` (NotPublishable is a status)
  - BoardMembership: `[{board_member}]` + `Later{"grant reach", 0, 0}`
  - AgentSend: `Later{"board reach", board_peer, board_peer}`; AgentSubscribe / AgentWait: `Later{"board reach", board_observe, board_observe}`
- `ScopeBit`: the bit each server `inScope`/`scopeSet`/`authorize` call uses for that kind today (spec §5.3 site list); ListFiles → file_read if held else file_write; AttachSession → `AttachModeScopeCap(mode)`; OpenInteractive resume → `resume|resume_exact` set when exact else `resume`; RecordKeep → `record_persist|record_delete`; ScheduleCreate bind target → resume. Kinds that check no target → 0.
- `Possible`: computed by calling the branch builders over every value of the deciding field (directions, attach modes, on/off, list_only, sinks, detached, zero/non-zero resume, exact) and OR-ing clause bits and `Later.Possible` — one function per kind used by both `RequestNeed` and `Possible`, so a new branch cannot be missed.

- [ ] **Step 1: Write failing tests**:
  - `TestRequestNeedBranches`: one row per bullet above with the concrete request (e.g. OpenFileTransfer Pull → `{file_read,file_read}`; Push → file_write; AttachSession View → `{view|cowrite|control, view|cowrite|control}`; OpenInteractive zero id, not detached, record on → three clauses `{spawn}`, `{exec_control}`, `{record_persist}`; exact resume detached → `{resume|resume_exact}` only).
  - `TestPossibleCoversEveryBranch` (Review Focus 5): `Possible(OpenFileTransfer) == file_read|file_write`; for every REQ kind and every enumerated field value, each clause's `AnyOf` ⊆ `Possible(kind)`.
  - `TestScopeBit`: ListFiles with held=file_write → file_write; held=file_read|file_write → file_read; AttachSession Cowrite → exec_cowrite.
  - `TestLookupHelpers`: `RecordKeepCap` table (forever→persist; later ttl→persist; earlier ttl→delete; leaving forever→delete); `ScheduleRunPower(Bound)` = `[{resume|resume_exact}]`; `SchedulePower(false,false,false)` = nil.
- [ ] **Step 2:** the same `GO_TEST_ENV` prefix with `go test ./authority/` → FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** → PASS; `make test` green (server untouched so far).
- [ ] **Step 5:** Commit `authority: request-determined branches, lookup helpers, Possible, ScopeBit`.

### Task 3: pin today's server answers before touching sites

**Files:**
- Create: `server/request_need_pin_test.go`

**Interfaces:**
- Consumes: existing helpers in `server/capabilities_test.go` (`newTestHandler`, `fakeConn`, `encodeTaskControlRequest`, `lastTaskControlResponse`, `hexToTaskID`).

- [ ] **Step 1:** Write tests that pass on the CURRENT code (they are the red line for Tasks 4–7), each asserting the exact response kind and `RequiredCap` / status:
  - `TestPinListFilesReportsFileRead` (caller with no file bits → PermissionDenied RequiredCap=file_read) and `TestPinListFilesScopesThroughFileWrite` (caller holds only file_write, target in its file_write scope → not NoSuchTask) — Review Focus 1, 2.
  - `TestPinRecordKeepPregateReportsPersist`.
  - `TestPinOpenInteractiveUnbindAfterResumeGate`: caller with resume but target outside resume scope, schedule-bound target → ResumeNotFound, not PermissionDenied(schedule_edit) — Review Focus 3.
  - `TestPinExecRunKillInvisibleIsNotFound`, `TestPinKillPortForwardInvisibleIsNoSuchForward` — Review Focus 4.
  - `TestPinAgentSendReachReportsBoardPeer`, `TestPinAgentSubscribeReportsBoardObserve`.
  - `TestPinRecordSwitchDenials` (on → record_persist, off → record_delete), `TestPinRestoreListOnlyNeedsNothing`, `TestPinAwaitNotifySinkNeedsNotify`, `TestPinOpenFileTransferDirection`.
  Skip any whose exact assertion already exists in `server/*_test.go` (grep the kind name first; cite the existing test in a comment instead).
- [ ] **Step 2:** `make test` → PASS on unchanged server code. If one fails, the pin is wrong, not the server: fix the test against the code.
- [ ] **Step 3:** Commit `server: pin capability answers before the RequestNeed refactor`.

### Task 4: central gate and the moved helpers

**Files:**
- Modify: `server/capabilities.go` (delete `requiredCap`, `resumeRequestCap`, `resumeRequestBits` stays but reads `authority`), `server/port_forward.go:335-343` (delete `forwardCapFor`), `server/task_handler.go:525-527`, every caller of the deleted functions (`grep -rn 'requiredCap\|resumeRequestCap\|forwardCapFor' server/`).

- [ ] **Step 1:** Central gate: `if want, gated := authority.CentralCap(req.Header.Kind); gated {` — rest unchanged.
- [ ] **Step 2:** Replace callers with `authority.ResumeCap` / `authority.ForwardCap`; keep the comments that explain WHY (move them to `authority`, leave a one-line pointer at the old site only where the comment explained the site's order).
- [ ] **Step 3:** `make test` → PASS (Task 3 pins included).
- [ ] **Step 4:** Commit `server: central gate and resume/forward bits from authority`.

### Task 5: inline check sites in `task_handler.go`

**Files:**
- Modify: `server/task_handler.go` — OpenInteractive (634-652), OpenFileTransfer (713-721), ListFiles (742-753), OpenPortForward (860-865), RegisterPortForward (882-889), KillPortForward (916-926), AttachSession (965-970), the S-D15 re-check (1786), record on open (1834-1838 with `server/record_wiring.go:28`), stale comment at 1302-1303.

- [ ] **Step 1:** At each site, compute `n := authority.RequestNeed(&req)` once per case and use `n.Clauses[i].AnyOf` / `.Report` (or the named helper) in place of the literal; keep each `if … { deny(...); return }` where it is. Record-on uses `authority.RecordOnCap`; S-D15 uses `authority.UnbindCap`.
- [ ] **Step 2:** Replace the RestoreTasks comment at 1302-1303 with: gated on the prune bit in `restore_handler.go` (list_only needs none); not operator-identity.
- [ ] **Step 3:** `make test` → PASS.
- [ ] **Step 4:** Commit `server: task_handler inline bits from authority`.

### Task 6: inline check sites in the other handlers

**Files:**
- Modify: `server/record_handlers.go` (gate callers, 169-200 via `RecordKeepCap`, 216-220 via `RecordSwitchCap`), `server/schedule_handler.go` (`schedulePower`, `scheduleRunPower` via the Task 2 helpers — the scope/reach parts stay; 584 via `RecordOnCap`), `server/session_mode.go:53-56` (`UnbindCap`), `server/await_idle_handler.go:92,108`, `server/await_progress_handler.go:55,65`, `server/agent_taskcontrol.go:88,179,189,300` (Report values from `RequestNeed(...).Later[0].Report`), `server/restore_handler.go:89`, `server/exec_run.go:420`.

- [ ] **Step 1:** Convert each site; the handler keeps its lookup, order and answer shape (RecordNotPermitted stays RecordNotPermitted).
- [ ] **Step 2:** `make test` → PASS.
- [ ] **Step 3:** Commit `server: handler inline bits from authority`.

### Task 7: scope bits

**Files:**
- Modify: every `inScope` / `scopeSet` / `scopeSetWith` / `authorize` call passing a `protocol.Capability_*` constant (spec §5.3 list; find the rest with `grep -rn 'inScope(\|scopeSet(\|scopeSetWith(\|authorize(' server/*.go | grep -v _test | grep Capability_`).

- [ ] **Step 0:** For each site, find the test that pins its out-of-scope answer (`grep -rn <KindName> server/*_test.go`); where none exists, add one to `server/request_need_pin_test.go` and see it pass on the unconverted code first (spec V3c).
- [ ] **Step 1:** Each call takes `authority.ScopeBit(&req, h.callerCaps(cid))` where the request is in hand, or the Task 2 helper / clause bit already computed at that site (the check bit and the scope bit then come from one expression where they are the same today — RegisterPortForward's shape).
- [ ] **Step 2:** `make test` → PASS.
- [ ] **Step 3:** Commit `server: scope bits from authority`.

### Task 8: the guard

**Files:**
- Create: `server/capability_literal_guard_test.go`

- [ ] **Step 1:** Write `TestNoCapabilityLiteralOutsideAuthority`: parse every non-test `.go` file in `server/` with `go/parser`; fail on every `*ast.SelectorExpr` whose X is the `protocol` import and whose Sel starts with `Capability_`, unless `file:func` is in an allowlist map with a reason string (seed it with what remains after Tasks 4–7: `callerCaps` → `Capability_All`/`Capability_None`, `transcript.go` includeShared widening, `scheduleReadReach`, `ExercisedBits`-style uses, anything else the run reports — each entry justified as "not a requirement").
- [ ] **Step 2:** Negative control: temporarily add `_ = protocol.Capability_Spawn` to a handler, run the test → FAIL naming that file and function; remove it.
- [ ] **Step 3:** `make test` → PASS.
- [ ] **Step 4:** Commit `server: guard against capability bits written outside authority`.

### Task 9: live check, review, land

- [ ] **Step 1:** `make test` and `make vet` (or the repo's `check` target) → PASS.
- [ ] **Step 2:** Per the `dummy-harness` skill: bring up `scripts/dummy-harness.sh`, spawn a task with `--caps file_write` and run `harness-cli file pull <id> README.md /tmp/x` from it → `permission denied … requires capability file_read`; with `--caps exec_view`, `harness-cli session snapshot <id>` → succeeds. Down the dummy after.
- [ ] **Step 3:** One fresh reviewer on the whole branch (executing-plans' final review), given the spec and Global Constraints.
- [ ] **Step 4:** Commit the spec and this plan with the code (landing policy: the spec lands with the code it describes); land with `scripts/land.py`; `make build` in the main checkout (land.py does it).
- [ ] **Step 5:** Restart decision by the landed paths: `server/` and `authority/` changed → the server needs a restart to carry it; runners do not decode anything new. Say so; do not restart without the operator.
