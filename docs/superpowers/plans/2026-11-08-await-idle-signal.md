# await-idle signal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `harness-cli session await-idle --signal quiet|lifecycle|program|any` fires on PTY quiescence (default, unchanged), the hooks' lifecycle, the OSC 7501 program status, or whichever comes first, and every fire reports which leg fired and the states it saw.

**Architecture:** The existing per-watcher goroutine (`SessionMux.ArmIdleWatcher`, 500 ms tick) evaluates one pure predicate per leg over server-clock stamps: new mux stamps (`muxStart`, `lastInput`, `programAt`, `programWorkingAt`) and a new store stamp (`LifecycleWorkingAt`). The wire gains one request enum and four response fields; every surface passes the flag through the one verb-table declaration and renders the new fields.

**Tech Stack:** Go (server, cli, tui, wasm), `.bgn` schema + `make protoregen`, JS (webui/static), verb table codegen (`go generate ./cli/verb`).

**Spec:** `docs/superpowers/specs/2026-11-08-await-idle-signal-design.md` — read it in full first; decisions are cited below as D<n>.

## Global Constraints

- Work in the harness worktree on branch `harness/70fbad4a6eb6f1e992be8a669f1bcefd` (memory: commit on the task branch; never a feature branch). Verify with `git rev-parse --abbrev-ref HEAD` before each commit.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` before writing code.
- Default signal is `quiet`; quiet behaviour, threshold default (2500 ms) and exit codes (3 session_stopped, 4 cancelled, 1 otherwise) are unchanged (D1, D17 withdrawn).
- Signal words on every surface are exactly `quiet`, `lifecycle`, `program`, `any`. State words are the existing wire words (`cli.LifecycleWord`, `cli.ProgramWord`, `cli.ProgramKindWord`), `""` for unset/none.
- No refusal by source state: arming lifecycle/program always arms (D10). No new `AwaitIdleStatus` value.
- Only client `FrameType_Stdin` frames stamp `lastInput` (D14).
- Build hygiene: compile-check with `go build ./...` / `go vet ./...`, never bare `go build ./cmd/<x>/`. Verify with make targets (`make check`, `make test`, `make js-test`, `make wasm-check`) — memory: `./...` alone hides pattern breaks.
- `make test` inside a task inherits `HARNESS_SERVER_PUBKEY`; run it as `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`.
- Skill edits go to `runner/agentskills/…` and are mirrored byte-identically to `.claude/skills/…` and `.agents/skills/…` in the same commit (`TestMirrorsMatchEmbeddedSkills`).

## Review Focus

1. A watcher armed with `--signal lifecycle` on a hookless or Windows session never fires by its leg; it must still end with `session_stopped` when the session stops, and `kill` must disarm it (D10). Test owned by Task 3.
2. Lifecycle stuck at `working` after an Esc-while-thinking (spec D13 (1)): `--signal any` must still fire through the quiet leg. Test owned by Task 3.
3. A cowriter that only resizes, or the server's own 7501 probe reply, must not move `lastInput` (D14) — otherwise D6 waits for a `working` that never comes and falls back to the slower (b). Test owned by Task 2.
4. An old `harness-cli` (request without the trailing `signal` byte) against a new server: whatever the decoder does, the CLI must not hang silently. Check in Task 7 against the dummy harness with the pre-change binary; record the observed behaviour in the commit message. No shim (spec Rollout; pitfall 10).
5. `--signal any` on a tick where several legs hold at once (program done, lifecycle idle, output quiet) must report `fired_by: program` (D4 order), so a waiter branching on `fired_by` sees the most specific reading. Test owned by Task 3. (A session restored from a held capture fires at once on the restored program state when no input arrived since: that state is the program's real current state, since the program kept running across the held restart.)

---

### Task 1: Wire schema

**Files:**
- Modify: `runner/protocol/message.bgn` (`AwaitIdleRequest` ~:2581, `AwaitIdleResponse` ~:2588, `AwaitIdleWatcherInfo` ~:2606, new enum beside `AwaitCondition` ~:2634)
- Regenerate: `runner/protocol/message.go` via `make protoregen`
- Test: `runner/protocol/await_idle_signal_wire_test.go` (new; model on `runner/protocol/lifecycle_wire_test.go`)
- Modify: `cli/await_idle.go` (signal word helpers beside `AwaitIdleSinkString` ~:163); Test: `cli/await_idle_test.go`

**Interfaces:**
- Produces: `func AwaitIdleSignalString(s protocol.AwaitIdleSignal) string` → `quiet|lifecycle|program|any`, `unknown(N)` otherwise; `func ParseAwaitIdleSignal(s string) (protocol.AwaitIdleSignal, error)` → `""` maps to Quiet, any other unknown word is an error.
- Produces: `protocol.AwaitIdleSignal` with `AwaitIdleSignal_Quiet=0`, `_Lifecycle=1`, `_Program=2`, `_Any=3`; `AwaitIdleRequest.Signal`; `AwaitIdleResponse.FiredBy AwaitIdleSignal`, `.Lifecycle LifecycleState`, `.ProgramState ProgramState`, `.ProgramKind ProgramKind`; `AwaitIdleWatcherInfo.Signal AwaitIdleSignal`. (Field names are whatever ebm2go generates from the spec's snake_case names; confirm after regen and use those everywhere.)

- [ ] **Step 1: Write the failing test** `TestAwaitIdleSignalRoundTrip`: encode an `AwaitIdleRequest{Signal: AwaitIdleSignal_Any, ThresholdMs: 100, Sink: AwaitSink_Board}` with topic `"t"`, decode, assert `Signal == AwaitIdleSignal_Any` and topic `"t"`; same for an `AwaitIdleResponse{Status: Fired, FiredBy: Program, Lifecycle: Idle, ProgramState: Done, ProgramKind: None}` and an `AwaitIdleWatcherInfo{Signal: Lifecycle}` with a non-empty topic and origin cid (the field sits after variable-length arrays).
  Also `TestParseAwaitIdleSignal` (four words, `""`→Quiet, `"busy"`→error) and `TestAwaitIdleSignalString` in `cli/await_idle_test.go`.
- [ ] **Step 2: Run it** — `go test ./runner/protocol/ -run TestAwaitIdleSignalRoundTrip` and `go test ./cli/ -run 'AwaitIdleSignal'` — expect compile failure (undefined).
- [ ] **Step 3: Edit the schema** exactly as the spec's § Wire block (append only; enum comments as in the spec). Run `make protoregen`. Regen churn in `message.go` is expected (memory: a constant of ebm2go); check build/vet/tests and that the new symbols exist, nothing more.
  Add the two cli helpers.
- [ ] **Step 4: Run** `go test ./runner/protocol/ ./cli/` and `go build ./... && go vet ./runner/protocol/ ./cli/` — PASS.
- [ ] **Step 5: Commit** — `git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/await_idle_signal_wire_test.go cli/await_idle.go cli/await_idle_test.go && git commit` ("await-idle signal: wire and signal words").

### Task 2: Stamps (store + mux)

**Files:**
- Modify: `server/taskstore.go` (`TaskEntry` ~:140, `SetLifecycle` :404)
- Modify: `server/session_mux.go` (struct fields beside `lastOutput`; `NewSessionMux`; `recordFrame` :361; `loadScreen` ~:419; `forwardControlFrames` :827; `forwardObserverFrames` :880)
- Test: `server/idle_signal_stamps_test.go` (new)

**Interfaces:**
- Produces: `TaskEntry.LifecycleWorkingAt int64` (server clock of the last accepted `working`; memory-only — NOT added to the progress snapshot `progressLines`/restore, spec D7).
- Produces on `*SessionMux`: `MuxStartUnixNano() int64`, `LastInputUnixNano() int64`, `ProgramStamps() (at, workingAt int64)`. All server-clock unix nanos; 0 = never.

- [ ] **Step 1: Write failing tests:**
  - `TestSetLifecycle_StampsWorkingAt`: `SetLifecycle(id, Working, 1, 100)` → `LifecycleWorkingAt == 100`; then `SetLifecycle(id, Idle, 2, 200)` → `LifecycleWorkingAt` still 100, `LifecycleAt == 200`; a stale report (`reportedAt` 1 again, state Working) leaves `LifecycleWorkingAt` unchanged.
  - `TestSessionMux_MuxStartStamped`: `MuxStartUnixNano()` is non-zero and ≤ `time.Now().UnixNano()` right after `NewSessionMux`.
  - `TestSessionMux_LastInputStdinOnly`: through a control attach and through a cowriter attach (use the helpers `server/await_idle_test.go` / `session_mux` tests already use for fake streams), a `FrameType_Stdin` frame moves `LastInputUnixNano()`; a window-size frame, a Control (signal) frame, and a viewer's (non-cowriter) Stdin frame do not.
  - `TestSessionMux_ProbeReplyIsNotInput`: feed output containing `ESC ] 7501 ; ? ESC \` into a terminal mux; after `answerProgramStatusProbes` runs, `LastInputUnixNano()` is still 0.
  - `TestSessionMux_ProgramStamps`: feed a 7501 report `state=working` → `ProgramStamps()` both non-zero and equal; then `state=done` → `at` advances, `workingAt` unchanged; a repeat of `state=done` (no change) does not move `at`.
  - `TestSessionMux_LoadScreenStampsProgramAt`: restoring a capture whose repaint carries a program status sets `programAt` to roughly the restore time and leaves `programWorkingAt` 0 unless the restored state is `working`.
- [ ] **Step 2: Run** `go test ./server/ -run 'TestSetLifecycle_StampsWorkingAt|TestSessionMux_(MuxStart|LastInput|ProbeReply|ProgramStamps|LoadScreen)'` — FAIL (undefined).
- [ ] **Step 3: Implement.** Stamps are `atomic.Int64` beside `lastOutput`. In `recordFrame`, compare `m.screen.ProgramStatus()` State/Kind before and after `m.screen.Write` inside the existing `screenMu` section; stamp `programAt` on a change and `programWorkingAt` when the new state is `working`. In both forward functions stamp only when `frame.FrameType(fb[0]) == frame.FrameType_Stdin`, and only on the branch that actually writes to the runner (not the discarded viewer branch).
- [ ] **Step 4: Run** the same tests, then `go test ./server/` — PASS.
- [ ] **Step 5: Commit** ("await-idle signal: input, program and working stamps").

### Task 3: Predicate, watcher and handler

**Files:**
- Create: `server/idle_signal.go`
- Modify: `server/session_mux.go` (`ArmIdleWatcher` :1224), `server/await_idle_handler.go` (whole arm path, `fireIdleNotify`, `fireIdleBoard`), `server/idle_watchers.go` (`idleWatcher.signal`, `idleWatcherInfo`)
- Modify (signature update only): `server/idle_watchers_test.go:26`, `server/await_idle_test.go:60,92,117`
- Test: `server/idle_signal_test.go` (new), additions to `server/await_idle_test.go`

**Interfaces:**
- Consumes: Task 1 types; Task 2 stamps and `TaskEntry.LifecycleWorkingAt`.
- Produces (all in `server/idle_signal.go`):
  ```go
  type LifecycleReading struct{ State protocol.LifecycleState; At, WorkingAt int64 }
  type IdleWatchSpec struct {
      Signal    protocol.AwaitIdleSignal
      Threshold time.Duration
      Lifecycle func() LifecycleReading // required when Signal is Lifecycle or Any
  }
  type IdleFire struct {
      Stopped      bool
      LastOutput   int64
      FiredBy      protocol.AwaitIdleSignal
      Lifecycle    protocol.LifecycleState
      ProgramState protocol.ProgramState
      ProgramKind  protocol.ProgramKind
  }
  type idleStamps struct{ Now, MuxStart, LastInput, LastOutput int64; Threshold time.Duration }
  // legSatisfied is D6 + D18 for one lifecycle/program leg.
  func legSatisfied(nonWorking bool, stateAt, workingAt int64, s idleStamps) bool
  ```
  `func (m *SessionMux) ArmIdleWatcher(spec IdleWatchSpec, stop <-chan struct{}, fn func(IdleFire))` replaces the old signature.

`legSatisfied` (the rule the tests pin; write it from this, it is not derivable from the signature):
```
if !nonWorking: false
floor := max(s.LastInput, s.MuxStart)
inputSinceStart := s.LastInput > s.MuxStart
a := stateAt > floor && (!inputSinceStart || workingAt > s.LastInput)
b := s.LastOutput != 0 && s.Now - max(s.LastInput, s.LastOutput) >= s.Threshold
return a || b
```
Non-working sets (D5): lifecycle ∈ {Waiting, Idle, Error}; program ∈ {Idle, Done, Blocked, Error}. Per tick, in this order (D4): program leg (if Signal ∈ {Program, Any}), lifecycle leg (if ∈ {Lifecycle, Any}), quiet leg (if ∈ {Quiet, Any}; today's test `lo != 0 && now-lo >= threshold`). First satisfied leg is `FiredBy`. Every `IdleFire` (including `Stopped`) carries the lifecycle and program readings at decision time.

- [ ] **Step 1: Write failing unit tests for `legSatisfied`** (table test `TestLegSatisfied`, threshold 2500 ms, times in ms→ns):
  - working state → false regardless of stamps
  - no input, state reported after muxStart → true (fires at once)
  - no input, state reported before muxStart (resumed task) → false; same with LastOutput 0 → false (D18)
  - input at 1000, previous turn's idle received at 1200, workingAt 500 → false (late hook, D6 (a))
  - input at 1000, working at 1100, idle at 1300 → true
  - input at 1000, idle stamp 900 (no new report), LastOutput 1050, Now 3600 → true via (b); Now 3500 → false
- [ ] **Step 2: Write failing watcher/handler tests** in `server/await_idle_test.go` using the existing fake stream helpers:
  - `TestAwaitIdleSignal_ProgramFiresOnDone`: arm `--signal program` after a Stdin input; feed `state=working` then `state=done`; reply-sink response has `Status=Fired`, `FiredBy=Program`, `ProgramState=Done`, within ~1 s of the done report (well before 2500 ms of quiet)
  - `TestAwaitIdleSignal_LifecycleFires`: `SetLifecycle` Working then Idle after the input → `FiredBy=Lifecycle`, `Lifecycle=Idle`
  - `TestAwaitIdleSignal_AnyFallsBackToQuiet` (Review Focus 2): lifecycle stuck at Working, no program reports, output stops → fires with `FiredBy=Quiet` after the threshold
  - `TestAwaitIdleSignal_NeverReportingSourceEndsOnStop` (Review Focus 1): `--signal lifecycle` on a task with `LifecycleAt == 0`; arm succeeds (no refusal); `Stop()` the mux → `SessionStopped`; a second watcher on the same session is removed by `kill` with `Cancelled`
  - `TestAwaitIdleSignal_AnyReportsMostSpecific` (Review Focus 5): program `done`, lifecycle `idle` and output quiet all hold when the watcher is armed → `FiredBy=Program`
  - `TestAwaitIdleSignal_BadSignal`: `Signal = 9` → `BadRequest`
  - `TestAwaitIdleSignal_BoardPayload`: board sink fire publishes JSON containing `"signal":"any"`, `"fired_by":"program"`, `"lifecycle":""`, `"program_state":"done"`, `"program_kind":""`, and still `"kind":"session_idle"`, `"status":"fired"`, `"last_output_at_unix_ms"`
  - `TestAwaitIdleList_ShowsSignal`: `AwaitIdleWatcherInfo.Signal` equals the armed signal; progress rows carry `Quiet`
- [ ] **Step 3: Run** `go test ./server/ -run 'TestLegSatisfied|TestAwaitIdleSignal_|TestAwaitIdleList_ShowsSignal'` — FAIL.
- [ ] **Step 4: Implement.** Handler: validate signal (out of range → `BadRequest` before registering), store it on `idleWatcher.signal`, build `IdleWatchSpec` with `Lifecycle` reading `h.Tasks.Get(taskIDHex)` → `{Lifecycle, LifecycleAt, LifecycleWorkingAt}` (zero reading if the task is gone). Reply response fills `FiredBy`/`Lifecycle`/`ProgramState`/`ProgramKind` from `IdleFire` (and from current readings for `Armed`/`Cancelled`). `fireIdleBoard` adds the five keys using `cli.AwaitIdleSignalString` (Task 1), `cli.LifecycleWord`, `cli.ProgramWord`, `cli.ProgramKindWord`. `fireIdleNotify` text: quiet keeps `session %s idle (%ds since last output)`; other legs `session %s idle (%s %s)` → e.g. `session 1a2b3c4d idle (program done)`, `(lifecycle waiting)`; append `:kind` after a blocked program state (`program blocked:permission`). Update the four existing test call sites to the new signature (`IdleWatchSpec{Signal: Quiet, Threshold: …}`).
- [ ] **Step 5: Run** `go test ./server/` (whole package; it has a known ~1/6 flake family — re-run once before investigating, memory) and `go vet ./server/` — PASS.
- [ ] **Step 6: Commit** ("await-idle signal: lifecycle/program/any legs on the server").

### Task 4: CLI and verb table

**Files:**
- Modify: `cli/verb/table.go:2870-2893` (session await-idle), then `go generate ./cli/verb` (regenerates `cli/verb/actions_gen.go`)
- Modify: `cli/await_idle.go` (`AwaitIdle` :23, helpers ~:140-260)
- Modify: `cmd/harness-cli/session.go:677-715`, `cmd/harness-cli/dispatch.go` (ls printing path ~:887-913 if it formats rows itself)
- Test: `cli/await_idle_test.go`, `cli/verb/await_idle_route_test.go`

**Interfaces:**
- Consumes: Task 1 types.
- Produces:
  - `func (c *Client) AwaitIdle(ctx context.Context, taskIDHex string, thresholdMs uint32, sink protocol.AwaitSink, topic string, signal protocol.AwaitIdleSignal) (*protocol.AwaitIdleResponse, error)` (D16)
  - `verb.SessionAction.Signal string` (generated)
  - Consumes Task 1's `AwaitIdleSignalString` / `ParseAwaitIdleSignal`.
- Verb flag: `{Name: "signal", Type: FlagString, Default: "quiet", OneOf: []string{"quiet", "lifecycle", "program", "any"}, Field: "Signal", Help: "what to wait for: quiet (PTY output, default), lifecycle (agent hooks), program (OSC 7501), any (first of the three)"}`. Add a Note: "the fire reports fired_by and the lifecycle/program state it saw; a source that never reports leaves a lifecycle/program watcher armed (use any when unsure)".

- [ ] **Step 1: Failing tests:** `TestAwaitIdleWatcherLines_Signal` (text row contains `signal=program`; JSON row has `"signal":"program"`); in `cli/verb/await_idle_route_test.go`, `session await-idle --signal any <id>` parses to `SessionAction{Sub:"await-idle", Signal:"any"}` on CLI, TUI and WebUI surfaces, and `--signal bogus` is rejected by `OneOf`.
- [ ] **Step 2: Run** `go test ./cli/ ./cli/verb/ -run 'AwaitIdle|ParseAwaitIdleSignal'` — FAIL.
- [ ] **Step 3: Implement**; `go generate ./cli/verb`; update the three `AwaitIdle` callers (CLI here; `tui/client.go:454` and `cmd/harness-webui-wasm/main.go:3855` pass `protocol.AwaitIdleSignal_Quiet` for now so the tree builds — Tasks 5/6 wire the real value). CLI JSON output adds `fired_by`, `lifecycle`, `program_state`, `program_kind` (strings via the helpers; `fired_by` only meaningful for `fired`, print it always).
- [ ] **Step 4: Run** `go build ./... && go test ./cli/... ./cmd/harness-cli/...` and `make check` — PASS.
- [ ] **Step 5: Commit** ("await-idle signal: CLI flag and output").

### Task 5: TUI

**Files:**
- Modify: `tui/dispatch.go:604-626`, `tui/client.go:441-470` (`AwaitIdleResultMsg`, `DoAwaitIdle`), `tui/app.go:1189-1213`, `tui/idlewatchersmodal.go:38` (`watcherWaitsFor`)
- Keys `w`/`W` (`tui/actions.go:336-352`): pass `protocol.AwaitIdleSignal_Quiet` explicitly (D15), no picker
- Test: `tui/idlewatchersmodal_test.go`, `tui/cmdline_test.go`, a result-line test beside the existing ones for `AwaitIdleResultMsg`

**Interfaces:**
- Consumes: Task 4's `ParseAwaitIdleSignal`, `AwaitIdleSignalString`, `AwaitIdle` signature; `cli.LifecycleWord/ProgramWord/ProgramKindWord`.
- Produces: `DoAwaitIdle` keeps its existing parameters and gains a trailing `signal protocol.AwaitIdleSignal`; `AwaitIdleResultMsg` gains `FiredBy protocol.AwaitIdleSignal`, `Lifecycle`, `ProgramState`, `ProgramKind`.

- [ ] **Step 1: Failing tests:** `watcherWaitsFor` → `idle 2500ms` (quiet), `idle lifecycle`, `idle program`, `idle any 2500ms`; the fire result line for `FiredBy=Program, ProgramState=Done` contains `program done`, for blocked contains `program blocked:permission`, for quiet keeps today's `last output` wording; cmdline `session await-idle --signal program <id>` reaches `DoAwaitIdle` with `AwaitIdleSignal_Program` (sibling pattern: thread the held `a.client`, pitfall 3).
- [ ] **Step 2: Run** `go test ./tui/ -run 'WatcherWaitsFor|AwaitIdle|Cmdline'` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./tui/` — PASS.
- [ ] **Step 5: Commit** ("await-idle signal: TUI").

### Task 6: WebUI and WASM

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (`harnessAwaitIdle` :3822-3870, snapshot `idle_watchers` rows :1929-1946, `harnessAwaitIdleList` :4972)
- Modify: `webui/static/cmdline.js:94-99` (`watcherListText`), `:604-633` (arm), `webui/static/task_models.js:104` (`watcherWaitsFor`), `webui/static/main.js` fire display for the await-idle button path (:5029-5047) — button, grid 🔔 (:5748) and task sheet (:6186) keep the default signal (D15)
- Test: `webui/static/cmd_test.mjs` (and the task_models test file if one exists beside it)

**Interfaces:**
- Consumes: Task 4 helpers.
- Produces: `harness.awaitIdle({taskId, thresholdMs, sink, topic, signal})` resolves to `{status, lastOutputAt, watcherId, firedBy, lifecycle, programState, programKind}` (strings for the last four, wire words); snapshot/list rows carry `signal`.

- [ ] **Step 1: Failing JS tests:** `watcherWaitsFor` matches the TUI wording table in Task 5; `session await-idle --signal any <id>` passes `signal: "any"` to `harness.awaitIdle`; the printed fire line names the leg and state (`program done`); `watcherListText` includes the signal.
- [ ] **Step 2: Run** `make js-test` — FAIL.
- [ ] **Step 3: Implement** (wasm parses `signal` with `cli.ParseAwaitIdleSignal`; an invalid word rejects the promise with the parse error).
- [ ] **Step 4: Run** `make wasm-check && make js-test && make check` — PASS.
- [ ] **Step 5: Commit** ("await-idle signal: WebUI and wasm").

### Task 7: Skill text, parity walk, skew check, live check

**Files:**
- Modify: `runner/agentskills/supervising-workers/SKILL.md:240-262` and its byte-identical mirrors `.claude/skills/supervising-workers/SKILL.md`, `.agents/skills/supervising-workers/SKILL.md`

- [ ] **Step 1: Edit the skill:** document `--signal` and what each waits for; for lifecycle/program the fire's `fired_by`/state replaces "snapshot once after the fire"; a never-reporting source (no hooks, Windows runner) leaves the watcher armed — use `any` when unsure; repeat that an agentboard wake does not count as input (spec D13 (3)), so do not arm lifecycle/program right after `agent send`; list the payload keys including the already-existing `last_output_at_unix_ms` and the new `signal`, `fired_by`, `lifecycle`, `program_state`, `program_kind`. Copy to both mirrors. Run `go test ./runner/agentskills/` — PASS.
- [ ] **Step 2: Walk `.claude/skills/surface-parity-checklist`** items 1–39 with a verdict per number against this diff; fix any gap in the owning task's files; record the walk in the commit message body (one line per item that is not "n/a").
- [ ] **Step 3: Full verification:** `go build ./... && go vet ./...`, `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`, `make js-test`, `make wasm-check`, `scripts/wire-skew-check.sh`. All green.
- [ ] **Step 4: Dummy-harness live check** (`dummy-harness` skill; exact spellings the help prints): with a claude interactive task, (a) `session send` a short prompt then `session await-idle --signal program <id>` → `fired`, `fired_by: program`, `program_state: done`, arriving before 2.5 s of quiet would; (b) same with `--signal lifecycle` → `lifecycle: idle`; (c) type text without Enter via `session send` (no `--enter`) then `--signal lifecycle` → fires after ~2.5 s via D6(b); (d) measure the silence between Enter and the first output byte (spec D13 (2)) and put the number in the commit message with the command that produced it; (e) Review Focus 4: run the pre-change `harness-cli` binary's `session await-idle` against the new server and record what happens; (f) the TUI cmdline and the WebUI cmdline each arm one `--signal any` watcher and show it in their watcher list. Tear the dummy harness down after.
- [ ] **Step 5: Commit** ("await-idle signal: skill text"), then land per `landing-to-main` (rebase on local main, FF-push, then `make build` in the main checkout) and restart the server before runners (pitfall 10).
