# Pillar A2 — background run mode — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A session task (interactive or stream) can run in background mode.
It is queued until a runner is free, started with a prompt, and ended by the
server on deadline, turn end, agent error, blocked too long, or queue timeout,
with an end reason on every task. Its run mode can be chosen at each resume
and switched while it runs.

**Architecture:**
- **Open:** a background open is created Queued. `Scheduler.Tick` places it
  through the session-start code that `handleOpenInteractive` already runs
  after choosing a runner, extracted into one function.
- **Prompt:** the prompt rides a stream on both legs and becomes the agent's
  first turn: through `{prompt}` in the interactive argv template, or through
  the stream adapter's `--prompt`.
- **Ending:** a per-session run watcher and one deadline watcher call
  `endBackgroundRun`. It cancels with an end reason.
- **Persistence:** conditions and deadlines are persisted in the WAL. The
  "this run has worked" facts cross a deliberate restart in the stop-time
  files.

**Tech Stack:** Go (server, runner, runner/protocol, cli, tui, wasm), `.bgn`
→ `make protoregen`, JS (webui), Python (`scripts/agent_presets.py`).

**Spec:** `docs/superpowers/specs/2026-11-13-pillar-a-run-modes-design.md`
(§A2, decisions A-D1…A-D19, facts F1…F21), inside
`docs/superpowers/specs/2026-11-12-run-modes-and-records-grand-design.md`.

## Global Constraints

- **Before starting:** read `.claude/skills/implementation-pitfalls/SKILL.md`.
- **Where to work:** in this worktree, on its `harness/<dir>` branch. No
  feature branch.
- **Wire changes:** all of them are in Task 1, and only there (memory: no
  split schemas).
  - New flag bits go at the END of a reserved run, and enum values are
    appended (`message.bgn` B:566-568, B:5245-5251).
  - Regenerate with `make protoregen`.
  - Run `scripts/wire-skew-check.sh` before landing, because the runner
    decodes `OpenExecRunnerRequest`. Restart order: **runners first**, then the
    server. An old runner reads a new server's `prompt_present` as reserved and
    starts the agent with no prompt, silently; a new runner never sees the bit
    from an old server. The handshake path is guarded both ways since
    `9f164ca1`, so runner-first is safe there (Task 1 checkpoint review, I1).
- **WAL format:** every new WAL field is a JSON key whose zero value means
  today's behaviour.
  - Each one is added to `WALEvent`, `walEventJSON`, `MarshalJSON` and
    `UnmarshalJSON` (server/wal.go).
  - `TestWALEventJSONRoundTripCopiesEveryField` must stay green.
- **Default values:**
  - quiet 5 min;
  - blocked 5 min;
  - turn end `any` (for the stream kind, `any` means lifecycle);
  - queue timeout unset;
  - CLI `--rows`/`--cols` 40 and 120;
  - deadline prefill in the TUI/WebUI 1 h;
  - no server default for the deadline or the PTY size.
- **Prompt cap:** `protocol.PromptMax = 16 << 10` bytes. One cap for both
  kinds and both legs. Reason: it fits the stream adapter's `--prompt` inside
  Windows' 32,767-character command line (A-D17). Every surface states it.
- **What reads a run mode:**
  - A resume that names no run mode takes the task's current one.
  - A resume that names no agent takes the task's.
  - Prompt and conditions are never reused from the task (A-D1, A-D5, A-D7).
- **Status machine:** `TaskStatus` is not changed. The end reason is a
  separate field (A-D11).
- **Out of scope:** schedules, and the schedule-bit check of A-D1/G46
  (pillar S). The plan leaves no hook for them.
- **Verification commands:**
  - `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`
    (the inherited `HARNESS_*` fail two UDP-forward tests otherwise).
  - `make build`.
  - `go generate ./cli/verb` after any verb-table change.
- **Repo is public:** no local paths, hostnames or task ids in committed
  files.

## Review Focus

1. **A TUI/WebUI resume key on a background task.** It must open the
   background form prefilled from the task (prompt, agent, conditions). It
   must not send a prompt-less request that comes back `prompt_required`, and
   it must not open attached. → Task 10, Task 11 tests.
2. **A prompt over 16 KiB.** It is refused `prompt_too_large`, with the cap in
   the message, at open time. It must not be silently truncated or dropped at
   placement. → Task 4 test.
3. **A human cancel of a Queued background task.** It reads end reason
   `cancelled` with `cancelled_by` set, and `Tick` never places it afterwards.
   → Task 3 test.
4. **Two background tasks on a one-slot runner.** The second waits with wait
   reason `all_busy`, and is placed when the first ends. → Task 3 test.
5. **A runner that disconnects mid-run.**
   - A Running background task ends `Failed` with end reason `failed` and
     message `runner_disconnected`.
   - Its deadline watcher entry does not later "cancel" a terminal task.
   → Task 2 and Task 5 tests.

---

### Task 1: Wire schema

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go`
- Create: `runner/protocol/prompt.go`
- Test: `runner/protocol/runmode_wire_test.go`
- Modify:
  - `server/cap_completeness_test.go:53,165`
  - `server/scope_completeness_test.go:71,170,197`
  - `server/mapper_completeness_test.go`
  - `server/capabilities.go:43` (`requiredCap`)
  - `server/task_handler.go` (dispatch stub)
- Fix the stale comment at `message.bgn:2406-2411` ("This response carries no
  error_msg field").

**Interfaces:**
- Produces (bgn → Go):
  - **New enums:**
    - `enum RunMode :u8 { foreground = 0, background = 1 }`
    - `enum TurnEndSignal :u8 { unset = 0, off, quiet, lifecycle, program, any }`
    - `enum EndReason :u8 { none = 0, process_exit, cancelled, failed, deadline, turn_done_lifecycle, turn_done_program, turn_done_quiet, agent_error, blocked_timeout, queue_timeout }`
    - `enum WaitReason :u8 { none = 0, no_candidate, all_busy }`
  - **New formats:**
    - `format BackgroundConditions { deadline_ms :u64, turn_end :TurnEndSignal, quiet_ms :u64, blocked_ms :u64, queue_timeout_ms :u64 }`. In every `_ms` field, 0 = default (deadline: 0 = absent).
    - `format PromptBody { prompt_len :u32, prompt :[prompt_len]u8 }`. It is the only content of a prompt stream on either leg.
  - **`OpenInteractiveRequest`:**
    - The second flag byte becomes `delegate_scope_present, spawn_depth_present, max_descendants_present, run_mode_set :u1, background :u1, conditions_present :u1, prompt_present :u1, reserved2 :u1`.
    - New blocks, appended after the `detached` block:
      - `if conditions_present == 1: conditions :BackgroundConditions`
      - `if prompt_present == 1: prompt_stream_id :u64`
    - The `background` bit means something only with `run_mode_set == 1`.
  - **`OpenInteractiveStatus`, appended:** `deadline_required, profile_required, prompt_required, turn_end_unsupported, prompt_too_large, bad_prompt`.
  - **`OpenInteractiveResponse`:** `attached :u1, queued :u1, reserved :u6`.
  - **`SubmitStatus`, appended:** `mode_mismatch`.
  - **`OpenExecRunnerRequest`:**
    - Flags become `x11_enabled, resume_conversation, event_stream, prompt_present :u1, reserved :u4`.
    - Appended after the x11 block: `if prompt_present == 1: prompt_stream_id :u64`.
  - **`TaskInfo`, appended at the end:** `run_mode :RunMode, end_reason :EndReason, cancelled_by :TaskID, deadline_ns :u64, queue_deadline_ns :u64, wait_reason :WaitReason, conditions :BackgroundConditions`. In `cancelled_by`, zero = operator.
  - **`TaskStatusEvent`, appended:** `run_mode :RunMode, end_reason :EndReason`.
  - **`TaskControlKind`, appended:** `session_mode`.
  - **The `session_mode` request:**
    - `format SessionModeRequest { task_id :TaskID, background :u1, conditions_present :u1, reserved :u6, if conditions_present == 1: conditions :BackgroundConditions }`
    - `enum SessionModeStatus :u8 { ok = 0, not_found, not_session, queued, deadline_required, turn_end_unsupported, internal_error }`
    - `format SessionModeResponse { status :SessionModeStatus, error_msg_len :u16, error_msg :[error_msg_len]u8 }`
    - Arms are added to the `TaskControlRequest` and `TaskControlResponse` unions.
  - **`runner/protocol/prompt.go`:** `const PromptMax = 16 << 10`.
- Produces (server):
  - `requiredCap[protocol.TaskControlKind_SessionMode] = protocol.Capability_Cancel`.
  - The scope verdict is "target task".
  - A dispatch case that answers `SessionModeStatus_InternalError` "not built". Task 7 replaces it within this landing.

- [ ] **Step 1: Write the failing wire test** `runner/protocol/runmode_wire_test.go`
  - `TestOpenInteractiveRequestRunModeRoundTrip`: set `run_mode_set`, `background`, `conditions_present` (deadline 3600000, turn_end lifecycle), `prompt_present` (stream id 7), plus `detached` with rows 40 / cols 120. Encode, then `DecodeExact`, then assert every field.
  - `TestOpenInteractiveRequestOldFlagsKeepTheirBits`: an encoding with only `max_descendants_present` set decodes with `RunModeSet()==false` and `PromptPresent()==false`.
  - `TestOpenExecRunnerRequestPromptStreamRoundTrip`.
  - `TestSessionModeRoundTrip`.
  - `TestTaskInfoRunFieldsRoundTrip`.
- [ ] **Step 2: Run it.**
  Run: `go test ./runner/protocol -run 'RunMode|PromptStream|SessionMode|RunFields'`.
  Expected: build failure (undefined setters).
- [ ] **Step 3: Edit the schema and regenerate.**
  - Edit `message.bgn` as above.
  - Run `make protoregen`. Add `prompt.go`.
  - Widen the completeness tests' loop bounds to `TaskControlKind_SessionMode`, and add the verdicts.
  - Add the `requiredCap` entry and the stub dispatch case.
- [ ] **Step 4: Verify.**
  Run: `go test ./runner/protocol ./server -run 'RunMode|PromptStream|SessionMode|RunFields|Completeness'`, then `go build ./...`.
  Expected: PASS.
- [ ] **Step 5: Run the skew check.**
  Run: `scripts/wire-skew-check.sh`.
  Expected: exit 0 (recoverable, or handshake-compatible).
- [ ] **Step 6: Commit** `proto: run mode, background conditions, prompt streams, end reason, session_mode`.

---

### Task 2: TaskStore and WAL — run mode, conditions, deadlines, end reason

**Files:**
- Create: `server/runmode.go`
- Modify: `server/taskstore.go`, `server/spawn_limit.go:49`, `server/wal.go`, `server/task_handler.go` (`toTaskInfo`, all `Tasks.Resume`/`CreateWithinLimits`/`Cancel` call sites), `server/server.go` (task events; startup Detached sweep)
- Test: `server/runmode_store_test.go`, plus mechanical call-site updates in existing tests

**Interfaces:**
- Produces, in `server/runmode.go`:
  - The types:
    ```go
    type RunConditions struct {
        Deadline, Quiet, Blocked, QueueTimeout time.Duration
        TurnEnd    protocol.TurnEndSignal
        Rows, Cols uint16
    }
    type RunSpec struct {
        Mode   protocol.RunMode
        Cond   RunConditions
        Prompt string
    }
    ```
  - `func (c RunConditions) WithDefaults(kind protocol.TaskKind) RunConditions` fills the defaults: Quiet 5m, Blocked 5m, TurnEnd `any`, and for the stream kind `any`→`lifecycle`.
  - `func ConditionsFromWire(b *protocol.BackgroundConditions) RunConditions`
  - `func (c RunConditions) Wire() protocol.BackgroundConditions`
- New `TaskEntry` fields:
  - `RunMode protocol.RunMode`
  - `Cond RunConditions`
  - `DeadlineAt int64` (absolute ns, 0 = none)
  - `QueueDeadlineAt int64`
  - `EndReason protocol.EndReason`
  - `CancelledBy protocol.TaskID`
  - `ModeSwitchedAt int64`
- Changed signatures:
  - `CreateWithinLimits(repo, prompt string, kind, origin, creator, boundRunnerID, selector, extraArgs, g, agentProfile, run RunSpec) (string, error)`. `Create` and `createLocked` get the same trailing `run RunSpec`. With `run.Mode == background`: store `Cond`, and set `QueueDeadlineAt = now + Cond.QueueTimeout` when `QueueTimeout > 0`.
  - `Resume(..., resumeConversation bool, run RunSpec) (TaskEntry, error)`. It overwrites `RunMode`, `Cond` and `QueueDeadlineAt` from `run`. It resets `EndReason`, `CancelledBy`, `DeadlineAt` and `ModeSwitchedAt`. It keeps `e.Prompt = prompt`.
  - `Cancel(id string, reason protocol.EndReason, by protocol.TaskID)`. It records `EndReason` and `CancelledBy`.
    - `task_handler.go:446` passes `(taskID, EndReason_Cancelled, h.lookupPrincipal(cid))`.
    - The startup Detached sweep (`server.go` after `loadProgressSnapshot`) passes `EndReason_Failed`.
    - `afterMuxStopped` passes `EndReason_ProcessExit`.
  - `Finish(id, exit, msg)`: on a Cancelled entry it keeps `EndReason`. Otherwise it sets `EndReason_Failed` when `exit == -1`, else `EndReason_ProcessExit`.
  - `MarkFailed`, `FailHeld`, and the replay sweep set `EndReason_Failed`.
  - `Assign`: on Queued → Running of a background task, it sets `DeadlineAt = now + Cond.Deadline` and writes it on `task_assigned`.
  - New `func (s *TaskStore) SwitchRunMode(id string, mode protocol.RunMode, cond RunConditions, now int64) (TaskEntry, error)`.
    - It refuses a Queued task, a terminal task, and a oneshot task, with errors `ErrSwitchQueued`, `ErrSwitchNotActive` and `ErrSwitchNotSession`.
    - It sets `RunMode`, `Cond` and `ModeSwitchedAt = now`. `DeadlineAt` becomes `now + cond.Deadline` (background) or 0 (foreground).
    - It writes WAL `task_mode_changed`.
  - New `func (s *TaskStore) BackgroundDeadlines() (due []DeadlineHit, next int64)`, with `type DeadlineHit struct{ ID string; Reason protocol.EndReason }`. It is read by Task 5.
- New WAL fields:
  - On `task_created`/`task_resumed`: `RunMode uint8`, `TurnEnd uint8`, `DeadlineMs`, `QuietMs`, `BlockedMs`, `QueueTimeoutMs int64`, `Rows`, `Cols uint16`, `QueueDeadlineNs int64`.
  - On `task_assigned`: `DeadlineNs int64`.
  - On `task_cancelled`/`task_finished`/`task_failed`: `EndReason uint8`.
  - On `task_cancelled`: `CancelledBy string`.
  - On `task_mode_changed`: the run mode, conditions, `DeadlineNs` and `SwitchedAtNs`.
  - `ReplayEvents` gets a `task_mode_changed` case, and every touched case restores its fields.
- `toTaskInfo` fills `RunMode`, `EndReason`, `CancelledBy`, `DeadlineNs`, `QueueDeadlineNs` and `Conditions`. The task event fills `RunMode` and `EndReason`.

- [ ] **Step 1: Write the failing tests** in `server/runmode_store_test.go`.
  - `TestBackgroundCreateStoresConditionsAndQueueDeadline`
  - `TestAssignResolvesTheDeadlineFromRunStart`: `DeadlineAt - StartedAt == Cond.Deadline`.
  - `TestResumeReusesNoConditions`: the second resume with a zero `RunSpec.Cond` has the default `Quiet`, not the first resume's 1m.
  - `TestCancelRecordsReasonAndActor`.
  - `TestFinishAfterCancelKeepsTheCancelReason`.
  - `TestFinishMinusOneIsFailedReason`.
  - `TestMarkFailedRunnerDisconnectedIsFailedReason` (Review Focus 5).
  - `TestSwitchRunModeRefusesQueuedAndOneshot`.
  - `TestRunFieldsSurviveWALReplay`: use `storeWithWAL`/`replayInto` from `hold_test.go`, and cover every new field including `task_mode_changed`.
- [ ] **Step 2: Run them.**
  Run: `go test ./server -run 'Background|ResumeReuses|CancelRecords|FinishAfterCancel|FinishMinusOne|MarkFailedRunner|SwitchRunMode|RunFieldsSurvive'`.
  Expected: build failure.
- [ ] **Step 3: Implement** the types and methods above.
  - Update every call site: in production, `RunSpec{}` keeps today's foreground behaviour; tests get `, RunSpec{}` appended.
  - Add the WAL fields to the four places in `wal.go`.
- [ ] **Step 4: Run the suite.**
  Run: `go test ./server`.
  Expected: PASS, including `TestWALEventJSONRoundTripCopiesEveryField` and `TestOnlyCreateWithinLimitsInProduction`.
- [ ] **Step 5: Commit** `server: run mode, conditions, deadlines and end reason on the task store`.

---

### Task 3: One session-start function; `Tick` places background tasks

**Files:**
- Modify: `server/task_handler.go:1944-2097` (extract), `server/scheduler.go`, `server/server.go:261` (wiring)
- Create: `server/session_start.go`
- Test: `server/scheduler_test.go`, `server/background_place_test.go`

**Interfaces:**
- Produces, in `server/session_start.go`:
  ```go
  type sessionStart struct {
      TaskIDHex          string
      Runner             RunnerEntry
      Profile, Repo      string
      Args               []string
      ResumeConversation bool
      EventStream        bool
      Detached           bool
      Rows, Cols         uint16
      X11                *protocol.X11Forward // nil = off
      TUIConn            ConnHandle          // nil when Detached
      Prompt             string
  }
  func (h *TaskHandler) startSession(ss sessionStart) (tuiStream trsf.BidirectionalStream, status protocol.OpenInteractiveStatus)
  ```
  - It is today's lines 1950-2090: ticket, board registration, client stream, runner stream, OpenExec send, mux, detached sizing, `afterMuxStopped`. On failure it calls the `finishWithError` behaviour and returns a non-ok status.
  - When `ss.Prompt != ""` it opens a server→runner send stream, writes an encoded `PromptBody`, EOFs it, and sets `oer.SetPromptPresent(true)` and `PromptStreamId`. This happens before `sendRunnerRequest`, in `sendAssign`'s order (`server/server.go:1755-1769`).
  - For a background task it arms the run watcher (Task 5's `h.armRunWatcher(taskIDHex, mux)`). Until Task 5 that is a no-op method.
- `func (h *TaskHandler) StartQueuedSession(runnerID objproto.ConnectionID, taskID string) error` reads the `TaskEntry` and calls `startSession` with:
  - `Detached: true`;
  - `Rows`/`Cols` from `Cond`;
  - `EventStream: t.Kind == TaskKind_Stream`;
  - `Prompt: t.Prompt`;
  - `Profile: t.AgentProfile`;
  - `Args: t.ExtraArgs`;
  - `ResumeConversation: t.ResumeConversation`.
  It returns an error iff the status is not ok (the task is already Failed then).
- `NewScheduler(reg *Registry, store *TaskStore, assign AssignFunc, start AssignFunc) *Scheduler`.
  - `Tick` skips a task unless `task.Kind == TaskKind_Oneshot || task.RunMode == RunMode_Background`.
  - For a background task, after `BindTask` and the re-read, it calls `store.Assign`, then `s.start(c.ID, task.ID)`.
  - A start error is logged. The task is already Failed, so the walk goes on.
  - `server.go:261` passes `s.taskHandler.StartQueuedSession`.
- Ruling, recorded here: Assign comes before start, as in `handleOpenInteractive`. A failed start fails the task. It does not leave the task Queued: `finishWithError` has already told the runner and the board.
- Blocking: `startSession` writes at most `PromptMax` bytes to a fresh send stream (buffer 1 MiB, `cli/payload_stream.go:24-31`), so it does not block `Tick`. No goroutine.

- [ ] **Step 1: Write the failing tests.**
  - In `scheduler_test.go`:
    - `TestTickSkipsForegroundSessions` (F3). A Queued interactive foreground task is never passed to `assign` or `start`.
    - `TestTickStartsBackgroundSessionWithStart`. `start` is called, not `assign`, and the task is Running.
    - `TestTickWaitsSecondBackgroundOnOneSlot` (Review Focus 4). With `MaxTasks 1`, the second task stays Queued. After `UnbindTask` plus `Finish` of the first and a `Tick`, it is started.
    - `TestTickNeverPlacesCancelledQueuedBackground` (Review Focus 3).
  - In `background_place_test.go`, `TestStartQueuedSessionSendsPromptStreamAndDetachedMux`. Use `openRunner(t, h)` from `open_seat_test.go:95`. Assert that the runner received `OpenExec` with `PromptPresent()`, that the stream carries a `PromptBody` equal to the task prompt, that the mux is registered and the task is Detached, and that the size is 40×120.
- [ ] **Step 2: Run them.**
  Run: `go test ./server -run 'Tick|StartQueuedSession'`.
  Expected: FAIL.
- [ ] **Step 3: Implement.** Do the extraction and rewire `handleOpenInteractive` to call `startSession`. `handleOpenInteractive` keeps `RunnerBusy`, `AmbiguousRunner` and the other answers for foreground.
- [ ] **Step 4: Verify.**
  Run: `go test ./server`.
  Expected: PASS. The existing `TestDetachedOpenAppliesInitialSize`, `TestHandleOpenInteractive_RegistersSessionMux` and the resume suites must stay unchanged.
- [ ] **Step 5: Commit** `server: one session-start function; Tick places background sessions`.

---

### Task 4: Opening — run mode, prompt stream, required fields, queued answer

**Files:**
- Modify: `server/task_handler.go:496-529` (dispatch case), `:1723` (`handleOpenInteractive` gains `prompt string`), `server/agent_handler.go:272-288` (`payloadStreamOf`)
- Test: `server/background_open_test.go`, plus the existing direct callers of `handleOpenInteractive` (append `""`)

**Interfaces:**
- Consumes: Task 2 (`RunSpec`, `CreateWithinLimits`/`Resume`), Task 3 (`startSession`).
- **Dispatch:** when `oi.PromptPresent()`, the `open_interactive` case runs off the receive loop in a goroutine, in exec_run's shape (`task_handler.go:602-629`). It reads with `readAgentPayloadStream(conn, oi.PromptStreamId, protocol.PromptMax+4)` and decodes a `PromptBody`.
  - `errPayloadTooLarge` → `prompt_too_large`, with `error_msg` "prompt exceeds 16384 bytes".
  - Any other error → `bad_prompt`.
  - `payloadStreamOf` lists `open_interactive` when `PromptPresent()`, so a cap-denied request's stream is discarded.
- **Resolve the mode** inside `handleOpenInteractive`, after the resume target is known:
  - `mode = background` if `RunModeSet() && Background()`;
  - `foreground` if `RunModeSet() && !Background()`;
  - else the existing task's `RunMode` on a resume;
  - else foreground.
- **Background branch**, taken instead of the Candidates/RunnerBusy/Assign path:
  1. Require the profile on a create (`profile_required`), a deadline (`deadline_required`: `ConditionsPresent()` with `DeadlineMs > 0`), and a prompt (`prompt_required`).
  2. Refuse `turn_end_unsupported` for the stream kind with `quiet`/`program`.
  3. On a resume with no profile, take the task's.
  4. If `Candidates` is non-empty and none `HasProfile`, answer `ProfileUnavailable`.
  5. `CreateWithinLimits` or `Resume` with `RunSpec{Mode: background, Cond: ConditionsFromWire(...).WithDefaults(kind), Prompt: prompt}`. `Rows`/`Cols` come from `InitialRows`/`InitialCols` when detached.
  6. Answer `ok` with `queued=1`, then call `h.OnChange()`.
- **Foreground branch:** unchanged, plus `Prompt: prompt` into `startSession`, and `RunSpec{Mode: foreground}` on create/resume.
- `handleSubmitResume`: if the target's `RunMode == background`, answer `SubmitStatus_ModeMismatch`.
- **Wait reason (A-D14), computed rather than stored:** `handleList` fills
  `TaskInfo.WaitReason` for a Queued background task from
  `h.Registry.Candidates(t.RepoPath, t.Selector)` filtered by `HasProfile`.
  - The list is empty → `no_candidate`.
  - No entry is Idle and non-displaced → `all_busy`.

- [ ] **Step 1: Write the failing tests** in `background_open_test.go`.
  - `TestBackgroundOpenWithNoRunnerIsQueued`. No runner registered → ok, `queued`, task Queued, `RunMode` background.
  - `TestBackgroundOpenOnFullRunnerIsQueued`.
  - `TestBackgroundCreateRequiresProfileDeadlinePrompt`. Three sub-cases, three statuses.
  - `TestBackgroundResumeTakesRecordedProfile`.
  - `TestBackgroundResumeRequiresDeadlineAndPrompt`.
  - `TestBackgroundStreamRefusesQuietTurnEnd`.
  - `TestResumeWithoutRunModeKeepsBackground`, and `TestResumeNamingForegroundSwitchesIt`.
  - `TestPromptOverCapRefused` (Review Focus 2). This one goes through `h.Handle` with a real payload stream of 16385 bytes. Use the agent_send tests' fake payload-stream helper (grep `readAgentPayloadStream` in `server/*_test.go`).
  - `TestSubmitResumeOfBackgroundIsModeMismatch`.
  - `TestForegroundOpenPassesPromptToRunner`.
  - `TestListShowsWaitReason`: `no_candidate` with no runner; `all_busy`
    with a full one.
- [ ] **Step 2: Run them.**
  Run: `go test ./server -run 'BackgroundOpen|BackgroundCreate|BackgroundResume|BackgroundStream|ResumeWithoutRunMode|ResumeNamingForeground|PromptOverCap|SubmitResumeOfBackground|ForegroundOpenPasses|WaitReason'`.
  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.**
  Run: `go test ./server`.
  Expected: PASS.
- [ ] **Step 5: Commit** `server: background opens are queued; prompts ride a stream; required fields`.

---

### Task 5: Ending — run evaluator, run watcher, deadline watcher

**Files:**
- Create: `server/run_end.go`, `server/run_watch.go`, `server/deadline_watch.go`
- Modify: `server/idle_signal.go:58-68` (`legSatisfied` calls the shared helper), `server/server.go` (start the deadline watcher next to `watchHeldDeadlines`, `:984-988`; wire `TaskHandler.wakeDeadlines`), `server/session_start.go` (`armRunWatcher`)
- Test: `server/idle_signal_test.go` (new `TestReportCounts`; `TestLegSatisfied` unmodified), `server/run_end_test.go`, `server/run_watch_test.go`, `server/deadline_watch_test.go`

**Interfaces:**
- **One rule, shared with await-idle**, in `server/idle_signal.go`. Both
  evaluators decide "does this non-working report end the turn" with it.
  What stays in each evaluator is which states count and what a fire
  returns.
  ```go
  // reportCounts reports whether a non-working state reported at stateAt
  // counts: it was reported after floor; when there was input after base, a
  // `working` at workingAt followed that input; and, with requireWork, a
  // `working` since base preceded the report.
  func reportCounts(stateAt, workingAt, floor, base, lastInput int64, requireWork bool) bool
  ```
  - `legSatisfied` becomes
    `reportCounts(stateAt, workingAt, max(s.LastInput, s.MuxStart), s.MuxStart, s.LastInput, false) || <its quiet fallback, unchanged>`.
    This keeps await-idle's D6/D18 exactly. `TestLegSatisfied` and the other
    await-idle tests pass without edits, and that is the check.
  - `evaluateRun` calls it with
    `floor = max(RunStart, SwitchedAt, LastInput)`, `base = RunStart`,
    `requireWork = true` (G16).
- Produces, in `server/run_end.go` (pure, table-tested):
  ```go
  type runInputs struct {
      Now, RunStart, SwitchedAt, MuxStart, LastInput, LastOutput int64
      Kind     protocol.TaskKind
      Cond     RunConditions            // after WithDefaults
      Life     LifecycleReading         // State, At, WorkingAt
      Prog     protocol.ProgramState
      ProgAt, ProgWorkingAt int64
      SawOutputThisRun bool             // LastOutput > RunStart, or carried (Task 6)
  }
  func evaluateRun(in runInputs) (protocol.EndReason, bool)
  ```
  - **Order:** `agent_error`, then `blocked_timeout`, then the turn-end leg(s) chosen by `Cond.TurnEnd`, in program → lifecycle → quiet order. `off` disables turn end only.
  - **Lifecycle turn end:** `Life.State == Idle` and
    `reportCounts(Life.At, Life.WorkingAt, max(RunStart, SwitchedAt, LastInput), RunStart, LastInput, true)`.
    With `requireWork`, this means
    `Life.WorkingAt >= RunStart && Life.WorkingAt < Life.At`.
  - **Program turn end:** the same call over `Prog ∈ {idle, done}`, `ProgAt` and `ProgWorkingAt`.
  - **Quiet:** fires when `SawOutputThisRun` and `Now - max(LastOutput, LastInput, SwitchedAt, MuxStart) >= Cond.Quiet`. It is not used for the stream kind.
  - **Blocked:** the current state is lifecycle `Waiting` or program `Blocked`, and `Now - max(stateAt, SwitchedAt) >= Cond.Blocked`.
  - **Error:** lifecycle `Error` or program `Error`, with `stateAt > max(RunStart, SwitchedAt)`.
- `func (h *TaskHandler) endBackgroundRun(taskID string, reason protocol.EndReason)` calls `h.Tasks.Cancel(taskID, reason, protocol.TaskID{})`, then `h.OnChange()`.
- **Run watcher**, in `server/run_watch.go`:
  - `func (h *TaskHandler) armRunWatcher(taskID string, mux *SessionMux)` runs a 500 ms ticker goroutine (the `ArmIdleWatcher` idiom, `session_mux.go:1293-1323`). It builds `runInputs` from the `TaskEntry` and the mux each tick, and calls `endBackgroundRun` once on a fire.
  - It ends on mux stop, on fire, or on `h.disarmRunWatcher(taskID)`.
  - Registry: `h.runWatchers map[string]chan struct{}` under its own mutex.
  - It is armed only when `RunMode == background`. Re-arming replaces the previous one.
- **Deadline watcher**, in `server/deadline_watch.go`:
  - `func (s *Server) watchBackgroundDeadlines(ctx context.Context, wake <-chan struct{})` runs a startup pass, then sleeps until `next` from `TaskStore.BackgroundDeadlines`. It wakes early on `wake`, and never exits while ctx lives.
  - Each due hit calls `endBackgroundRun(id, reason)`, with `deadline` for Running/Detached/Held and `queue_timeout` for Queued.
  - `TaskStore.BackgroundDeadlines` skips terminal tasks (Review Focus 5).
  - `TaskHandler.wakeDeadlines func()` is called after any write that sets a deadline (create/resume with a queue timeout, Assign, SwitchRunMode). Use a buffered (1) channel and a non-blocking send.

- [ ] **Step 1: Write the failing tests.**
  - `TestReportCounts`, a table in `idle_signal_test.go`:
    - a report at or before `floor` does not count;
    - after input, it needs `workingAt > lastInput`;
    - with `requireWork`, `workingAt < base` or `workingAt >= stateAt`
      does not count;
    - without `requireWork` and with no input, any report after `floor`
      counts (await-idle's case).
  - `TestEvaluateRun`, a table in `run_end_test.go`. One row per rule:
    - `waiting`/`blocked` never end a turn but start the clock;
    - a turn end before any `working` since `RunStart` does not fire;
    - after input, a turn end needs a `working` after it;
    - a turn that ended before `SwitchedAt` does not fire, and one after does;
    - `off` disables turn end but not the deadline-independent rules;
    - the stream kind ignores quiet;
    - `agent_error` beats a turn end.
  - `TestRunWatcherEndsOnLifecycleIdleAfterWorking`. Use `newSignalFixture` (`idle_signal_test.go:56`) with the task set to background. Expect end reason `turn_done_lifecycle` and status Cancelled.
  - `TestRunWatcherBlockedTimeout`. Use `Blocked = 1s`.
  - `TestDeadlineWatcherEndsAtDeadlineAndQueueTimeout`.
  - `TestDeadlineWatcherStartupPassEndsExpiredHeld`.
  - `TestDeadlineWatcherIgnoresTerminal`.
- [ ] **Step 2: Run them.**
  Run: `go test ./server -run 'ReportCounts|EvaluateRun|RunWatcher|DeadlineWatcher'`.
  Expected: FAIL.
- [ ] **Step 3: Implement.** Write `reportCounts` first and move
  `legSatisfied` onto it. Run
  `go test ./server -run 'LegSatisfied|AwaitIdle|IdleWatcher|EvaluateIdle'`.
  Expected: PASS, with no test file edited. Then write `evaluateRun` and the
  watchers.
- [ ] **Step 4: Verify.**
  Run: `go test -count=3 ./server -run 'ReportCounts|EvaluateRun|RunWatcher|DeadlineWatcher'`, then `go test ./server`.
  Expected: PASS. The `-count=3` run is there because these tests are timing-based. Memory: a server flake family exists; isolation is not evidence.
- [ ] **Step 5: Commit** `server: background runs end on deadline, turn end, error, blocked, queue timeout`.

---

### Task 6: Crossing a deliberate restart

**Files:**
- Modify: `server/taskstore.go:463-505` (`progressLine`), `server/hold.go:351-417` (capture writer, reader, sweep), `server/readopt.go:177-258`, `server/session_mux.go` (carried facts)
- Test: `server/run_restart_test.go`

**Interfaces:**
- `progressLine` gains `LifecycleWorkingAt int64 json:"lifecycle_working_at"`. `progressLines` and `restoreProgress` copy it.
- Sidecar file `<DataDir>/held/<taskIDHex>.run.json`:
  ```go
  type heldRunFacts struct {
      ProgramWorkingAt int64 `json:"program_working_at"`
      SawOutput        bool  `json:"saw_output"`
  }
  ```
  - `captureHeldScreen` writes it next to `.frames`. `SawOutput` is `mux.LastOutputUnixNano() > runStart`.
  - `readHeldRunFacts(taskIDHex) (heldRunFacts, bool)` reads it and removes it.
  - `sweepHeldScreens` also removes orphan `.run.json` files.
  - Ruling: a sidecar JSON, not an extra frame in `.frames`. That file is wire frames replayed into the screen, and the facts are not screen content.
- `SessionMux` gains:
  - `func (m *SessionMux) carryRunFacts(f heldRunFacts)`, which stores `carriedProgramWorkingAt` and `carriedSawOutput`.
  - Accessors used by `armRunWatcher`. They are `max(programWorkingAt, carried)` and `carriedSawOutput || LastOutput > RunStart`.
- `rebindHeldSessions`: after `loadHeldCapture`, it applies `readHeldRunFacts`. After `Sessions.Add`, it calls `h.armRunWatcher(taskID, mux)` when the task is background.

- [ ] **Step 1: Write the failing tests** in `run_restart_test.go`.
  - `TestProgressSnapshotCarriesLifecycleWorkingAt`.
  - `TestHeldRunFactsRoundTripAndSweep`.
  - `TestReadoptedBackgroundEndsOnRestoredIdleWhenItHadWorked`. Build the mux as `rebindHeldSessions` does: `NewSessionMux` + `loadHeldCapture` + `carryRunFacts`. Expect `turn_done_program`.
  - `TestReadoptedBackgroundDoesNotEndWhenItNeverWorked`.
  - `TestReadoptedBackgroundWithMissingFactsRunsToDeadline`.
- [ ] **Step 2: Run them.**
  Run: `go test ./server -run 'LifecycleWorkingAt|HeldRunFacts|ReadoptedBackground'`.
  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.**
  Run: `go test ./server`.
  Expected: PASS.
- [ ] **Step 5: Commit** `server: background run facts cross a deliberate restart in the stop-time files`.

---

### Task 7: Switching a running session's run mode

**Files:**
- Create: `server/session_mode.go`
- Modify: `server/task_handler.go` (replace Task 1's stub)
- Test: `server/session_mode_test.go`

**Interfaces:**
- `func (h *TaskHandler) handleSessionMode(cid string, conn ConnHandle, req *protocol.TaskControlRequest)`.
  - The `cancel` bit is checked centrally (`requiredCap`). Here it checks `h.inScope(cid, Capability_Cancel, taskID)` and answers `not_found` if that fails, as the cancel handler does.
  - Validation for background: `deadline_required`, and `turn_end_unsupported` for the stream kind.
  - It calls `Tasks.SwitchRunMode`. The errors map to `queued`, `not_found` and `not_session`.
  - On ok:
    - background: `armRunWatcher` if a mux exists;
    - foreground: `disarmRunWatcher`;
    - then `wakeDeadlines()` and `OnChange()`.

- [ ] **Step 1: Write the failing tests** in `session_mode_test.go`.
  - `TestSwitchToForegroundDisarmsWatcherAndDeadline`. After the switch, a lifecycle idle following `working` does not end the task, and `DeadlineAt == 0`.
  - `TestSwitchToBackgroundNeedsDeadline`.
  - `TestSwitchToBackgroundIgnoresTurnEndedBeforeSwitch`. Lifecycle is idle before the switch: no end. A new working→idle afterwards gives `turn_done_lifecycle`.
  - `TestSwitchQueuedRefused`.
  - `TestSwitchOutOfScopeIsNotFound`.
- [ ] **Step 2: Run them.**
  Run: `go test ./server -run 'Switch'`.
  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.**
  Run: `go test ./server`.
  Expected: PASS.
- [ ] **Step 5: Commit** `server: session_mode switches a running session between foreground and background`.

---

### Task 8: Runner — prompt on OpenExec, `{prompt}` templates, presets, cancel comments

**Files:**
- Modify:
  - `runner/connect.go:1054-1107` (add `waitForPromptBody`)
  - `runner/session.go:880-1252`
  - `runner/agent_command.go`
  - `runner/agent_profile.go:83-86`
  - `cmd/agent-runner/main.go:120-122,348,357` (help text)
  - `scripts/agent_presets.py`
  - `scripts/test_agent_presets.py:85-91`
  - The wrong-comment fixes of F5: `server/dispatch.go:183`, `runner/session.go:1187-1189`.
- Test: `runner/agent_command_test.go`, `runner/prompt_open_test.go`, `scripts/test_agent_presets.py`

**Interfaces:**
- `func waitForPromptBody(ctx context.Context, p StreamLookup, id trsf.StreamID) (string, error)` uses the type `waitForStreamBody` takes. It refuses `len(raw) > protocol.PromptMax+4`, then `PromptBody.DecodeExact`.
- **Template rule**, the new `validateAgentArgvTemplate` for interactive templates:
  - At most one token may contain `{prompt}`. It may be the whole token (`{prompt}`), or embedded (`--prompt={prompt}`).
  - `expandAgentArgvTemplate` substitutes inside the token. When the prompt is empty, it drops every token that contains `{prompt}`.
  - The oneshot rule (exactly one, whole token) is unchanged.
- `buildInteractiveArgs(extra, template, resumeTemplate []string, resumeConversation bool, prompt string) ([]string, error)`.
  - It returns `errPromptUnsupported` when `prompt != ""` and the chosen template has no `{prompt}` token. That includes the empty-template identity case.
  - `resolveExec` passes the prompt through.
- **`handleOpenExec`:**
  - If `oer.PromptPresent()`, read the prompt right after the stream lookup (915). A read failure is `finishWithError(-1, "prompt: "+err)`.
  - `errPromptUnsupported` becomes `finishWithError(-1, "prompt_unsupported: profile "+name+" has no {prompt} in its interactive template")`.
  - The stream kind sets `StreamTask.Prompt` (1085), and removes the "No prompt" comment.
- **Presets.** The `interactiveArgv` / `resumeInteractiveArgv` forms come from F18. The plan's Step 6 confirms each one live.

  | preset | interactiveArgv | resumeInteractiveArgv |
  | --- | --- | --- |
  | claude | `{args} {prompt}` | `{args} --continue {prompt}` |
  | codex | `--no-daemon {args} {prompt}` | `resume --last --no-daemon {args} {prompt}` |
  | agy | `{args} --prompt-interactive={prompt}` | `{args} --continue --prompt-interactive={prompt}` |
  | opencode | `{args} --prompt={prompt}` | `{args} --continue --prompt={prompt}` |
  | pi | `{args} -- {prompt}` | `{args} --continue -- {prompt}` |

  - bash and cmd keep `{args}`.
  - The sandbox twins inherit these, because they are derived.

- [ ] **Step 1: Write the failing tests.**
  - In `agent_command_test.go`:
    - `TestInteractiveTemplateAcceptsOnePromptToken`.
    - `TestInteractiveTemplateRejectsTwoPromptTokens`.
    - `TestEmbeddedPromptTokenDroppedWhenEmpty`: `--prompt={prompt}` with an empty prompt leaves no token.
    - `TestBuildInteractiveArgsPromptUnsupported`.
    - Replace `TestValidateInteractiveArgvTemplateRejectsPrompt`.
  - In `runner/prompt_open_test.go`:
    - `TestOpenExecWithPromptOnProfileWithoutTokenFinishesPromptUnsupported`. Drive `handleOpenExec` with a fake sender, as the existing session tests do. Grep `handleOpenExec(` in `runner/*_test.go` for the harness.
    - `TestWaitForPromptBodyRefusesOverCap`.
  - Python: change `test_every_preset_declares_a_fresh_interactive_template` to assert that `{prompt}` is present for claude, codex, agy, opencode and pi, and absent for bash and cmd.
- [ ] **Step 2: Run them.**
  Run: `go test ./runner -run 'PromptToken|PromptUnsupported|PromptBody|EmbeddedPrompt'` and `python3 -m unittest scripts/test_agent_presets.py`.
  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.**
  Run: `go test ./runner ./cmd/agent-runner`, then `python3 -m unittest scripts/test_agent_presets.py`.
  Expected: PASS.
- [ ] **Step 5: Commit** `runner: initial prompt on PTY and stream opens; {prompt} in interactive templates`.

The live check of each preset's prompt form needs Task 9's CLI, so it is Task
12 Step 0.

---

### Task 9: CLI — flags, `session mode`, client prompt stream, listing

**Files:**
- Modify:
  - `cli/verb/spawn.go:26-160`, `cli/verb/table.go:1758-1781` (+ new `session mode` entry near `:3035`), regenerated `cli/verb/actions_gen.go` (`go generate ./cli/verb`)
  - `cmd/harness-cli/spawn.go:17-59`, `cmd/harness-cli/session.go:379-433` (+ `runSessionModeWith`), `cmd/harness-cli/dispatch.go`
  - `cli/session_opts.go` (struct, builder, the stale agy comment `:53-54`), `cli/open_interactive_native.go:82`, `cli/open_interactive_wasm.go:135-146`
  - Create `cli/session_mode.go`
  - `cli/list.go`
- Test: `cli/verb/*_test.go` (the table invariants), `cli/session_opts_test.go`, `cli/session_mode_test.go`, `cmd/harness-cli/session_new_test.go`

**Interfaces:**
- **Flags.** These are `session new` flags only (`spawnSessionNew`), each with help text.
  - `--background`: implies `--detach`; `Build` sets `Detach`.
  - `--foreground`: exclusive with `--background`.
  - `--prompt string`: "initial prompt, ≤16384 bytes; required with --background".
  - `--deadline duration`.
  - `--turn-end off|quiet|lifecycle|program|any`.
  - `--quiet duration`.
  - `--blocked duration`.
  - `--queue-timeout duration`.
  - `rows`/`cols` defaults become `40`/`120`. The help text becomes "initial PTY rows of a detached/background open; 0 with --cols 0 sends no size (PTY 0x0)".
  - The CLI does not check required fields. The server's refusal is the one rule; the client prints its status text.
- **`SessionOpts` additions:**
  - `RunModeSet bool`
  - `Background bool`
  - `Conditions *protocol.BackgroundConditions` (nil = none)
  - `Prompt string`
  - The builder sets the new bits and blocks.
  - `Prompt != ""` routes the open through `c.TaskControlWithPayload` (`cli/payload_stream.go:38`) with an encoded `PromptBody`.
  - The client refuses `len(Prompt) > protocol.PromptMax` before sending.
- **`session mode` verb:**
  - `Path []string{"session","mode"}`, `Action "SessionAction"`, `Const {"Sub":"mode"}`.
  - `Args task-id`, and `mode` (`foreground|background`).
  - It takes the condition flags.
  - `CmdlineSurfaces: CLI | TUI`.
  - `ModalSurfaces`: TUI `tui/actions.go:onRunMode`, and WebUI `webui/index.html#task-sheet-run-mode`.
- `func (c *Client) SessionMode(ctx context.Context, taskIDHex string, background bool, cond *protocol.BackgroundConditions) (protocol.SessionModeStatus, string, error)`, with an `X(serverCID)` wrapper as its siblings have.
- **Status texts** for every new `OpenInteractiveStatus` / `SessionModeStatus` value, next to the existing ones. The `prompt_too_large` text names the cap.
- **`ls` row**, appended per task:
  - `run=bg`
  - `end=<reason>` (on terminal tasks)
  - `deadline=<remaining>` (while Running/Detached/Held background)
  - `wait=no_candidate|all_busy` (Queued)
  - `by=<8-hex>|operator` on `end=cancelled`
  - Never elide a zero that exists (memory).

- [ ] **Step 1: Write the failing tests.**
  - `TestSessionNewBackgroundImpliesDetach`.
  - `TestBuildOpenInteractiveRequestCarriesRunModeConditionsPrompt`.
  - `TestRowsColsZeroSendsNoSize`.
  - `TestPromptOverCapRefusedClientSide`.
  - `TestSessionModeVerbParses`.
  - `TestListRowShowsRunFields`.
  - The `cli/verb` table invariants must pass for `session mode`: `TestEveryVerbHasASurfaceVerdict`, `TestEveryFlagHasHelp`, `TestEveryDeclaredFlagIsReadByItsBuild`, `TestGeneratedFileIsCurrent`.
- [ ] **Step 2: Run them.**
  Run: `go test ./cli/... ./cmd/harness-cli`.
  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.**
  Run: `go generate ./cli/verb && go test ./cli/... ./cmd/harness-cli`.
  Expected: PASS.
- [ ] **Step 5: Commit** `cli: background flags, prompt, session mode, run fields in ls`.

---

### Task 10: TUI

**Files:**
- Create: `tui/bgform.go`
- Modify:
  - `tui/client.go:109-172` (`sessionRequest` + `Authority.opts`, the one `SessionOpts` builder)
  - `tui/interactive.go:279` (`DoStartDetachedSession` gains `bg *bgRequest`)
  - `tui/dispatch.go:529-541`, `tui/app.go:2493-2512`
  - `tui/actions.go:466-514` (`onResume`) and a new `onRunMode`
  - `tui/keys.go`
  - the task-list and detail rendering
- Test: `tui/bgform_test.go`, `tui/actions_test.go`

**Interfaces:**
- `type bgRequest struct { Prompt string; Cond protocol.BackgroundConditions }`. `sessionRequest` gains `RunModeSet, Background bool; Prompt string; Conditions *protocol.BackgroundConditions`.
- **`BgFormModel`.** Fields: repo, agent, prompt (textarea), deadline (prefilled `1h`), turn-end select, quiet, blocked, queue-timeout, resume id.
  - New background open: key `B` on the tasks panel.
  - `onResume` on a task whose `RunMode == background` opens the form prefilled from the task's `Prompt`, `AgentProfile`, `Conditions` and id (Review Focus 1). It does not open attached.
  - Submit calls `DoStartDetachedSession(..., bg)`.
- **Run-mode key `M`** on a non-terminal session task: foreground→background opens the form with only the conditions; background→foreground sends at once.
- The cmdline `session new --background …` and `session mode …` come from the verb table. Wire `tuiVerbs.SessionMode`.
- **Rendering:** run mode, end reason, deadline remaining and wait reason in the task list and detail pane. A Queued background task shows its wait reason.

- [ ] **Step 1: Write the failing tests.**
  - `TestResumeKeyOnBackgroundTaskOpensPrefilledForm`.
  - `TestBgFormPrefillsOneHourDeadline`.
  - `TestRunModeKeyForegroundSendsImmediately`.
  - `TestTaskRowShowsEndReasonAndWait`.
- [ ] **Step 2: Run them.**
  Run: `go test ./tui -run 'BgForm|ResumeKeyOnBackground|RunModeKey|EndReasonAndWait'`.
  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.**
  Run: `go test ./tui`.
  Expected: PASS. `TestSessionOptsIsBuiltInOnePlace` must stay green.
- [ ] **Step 5: Commit** `tui: background form, resume prefill, run-mode switch, run fields`.

---

### Task 11: WebUI and wasm bridge

**Files:**
- Modify:
  - `cmd/harness-webui-wasm/main.go:3450-3600` (`harnessStartInteractive`)
  - a new `harnessSessionMode` export next to `:119`
  - `webui/static/main.js` (`sessionReq` 371-425, `composeRequest` 3894-3907, `openInteractive` 5019-5052, `resumeTaskById` 4874-4896, `buildTaskSheet` 6192/6307-6355, the task row)
  - `webui/index.html:102-184`
  - `webui/static/cmdline.js:610ff` (only the `session mode` hint text; `session new` stays buttons-only as today)
- Test: Playwright run on a dummy harness (below). The repo has no JS unit test harness for main.js. Confirm with `ls webui` before writing one.

**Interfaces:**
- **wasm keys** read by `harnessStartInteractive`:
  - `prompt` (string)
  - `runMode` (`"foreground"|"background"|""`)
  - `deadlineMs`, `quietMs`, `blockedMs`, `queueTimeoutMs` (numbers)
  - `turnEnd` (string)
  - `rows`, `cols`
  - `background` sets `Detached` and `InitialRows`/`InitialCols` from `rows`/`cols`.
- `harness.sessionMode({taskId, runMode, deadlineMs, …}) -> Promise<status>`.
- **Compose:**
  - a prompt textarea, with the 16384-byte cap stated in its label;
  - a "Background" checkbox revealing deadline (prefilled `1h`), turn-end select, quiet, blocked and queue-timeout;
  - `#open-detachable` with background checked sends `runMode: "background"` and does not attach.
- **Task sheet:**
  - On a background task, the resume buttons fill Compose from the task (prompt, agent, conditions, resume id), check Background, and scroll to it (Review Focus 1).
  - A run-mode button switches foreground↔background. Switching to background reveals the deadline field, prefilled `1h`.
  - Rows show run mode, end reason, deadline remaining and wait reason.

- [ ] **Step 1: Implement.**
  - wasm: run `GOOS=js GOARCH=wasm go build -o /dev/null ./cmd/harness-webui-wasm`. Expected: success.
  - JS: no unit harness exists; the check is Step 2.
- [ ] **Step 2: Verify in the browser.**
  - Bring up a dummy harness with `--agent claude`.
  - Drive the page with Playwright (`webui` memory index: reading procedure).
  - Open a background session from Compose: it reads Queued then Running, with deadline remaining.
  - Resume it from the task sheet: Compose is prefilled.
  - Switch a running one to foreground.
  - Take screenshots. Keep them, and report their paths.
  - Expected: every step as described. No console errors.
- [ ] **Step 3: Commit** `webui: background compose, resume prefill, run-mode switch, run fields`.

---

### Task 12: Live verification, measurements, docs

**Files:**
- Modify: `runner/agentskills/` (the harness-cli / supervising-workers skill text that documents `session new`: add `--background`, `--prompt`, `--deadline`, `session mode`), `README.md` (the session section, if it lists `session new` flags)
- Test: none new; this task runs the spec's live checks

- [ ] **Step 0: Confirm each preset's prompt form live** (Task 8's templates).
  - Run `scripts/dummy-harness.sh up --agent claude --detach --name prompts -- --agent-profiles "$(cd scripts && python3 -c 'import agent_presets as a; print(a.agent_profiles_json(["codex","agy","opencode","pi"]))')"`.
  - For each of the five, run `session new -d --agent <p> --prompt 'reply with the word PONG'`. Then run it again with `--resume <id> --resume-conversation --prompt 'reply with PONG2'`.
  - Answer the folder-trust dialog once (A-D18).
  - Read `session snapshot`.
  - Expected: PONG, then PONG2, on all five.
  - If a form fails, fix that preset's template in `scripts/agent_presets.py`, and record the working form and the agent version in the commit message.
  - Run `down` afterwards.
- [ ] **Step 1: Run the spec's E2E** on `scripts/dummy-harness.sh up --agent claude`, with the CLI spelled exactly as its help prints it.
  - A background open with a short prompt ends `turn_done_lifecycle`.
  - A prompt that triggers a permission prompt under manual mode ends `blocked_timeout` with `--blocked 1m`.
  - `--deadline 2m` on a long task ends `deadline`.
  - With `--max-tasks` filled by foreground sessions, a background open shows `wait=all_busy`, and it starts when one ends.
  - Check every surface: `ls`, the TUI in a dummy PTY, and the WebUI.
- [ ] **Step 2: Deliberate-restart check.**
  - Stop the dummy server with its shutdown sentinel while a background claude turn is running.
  - Let the turn end. Restart the server with the same data dir.
  - Expected: the task ends `turn_done_*` after re-adoption, not at its deadline.
- [ ] **Step 3: Stream kind.**
  - Run `session new --stream --background --agent claude --prompt … --deadline 10m`.
  - Record whether lifecycle `working`/`idle` are reported under the adapter, and whether `waiting` is reported during a pending approval. Read them from `ls`.
  - Expected: it ends `turn_done_lifecycle`.
  - **Gate:** if `waiting` is not reported, or lifecycle is not reported at all, stop here and return to planning. A-D9 then adds a stage: the pending count, or a refusal. Its wire change cannot be written before the measurement.
- [ ] **Step 4: The F5 measurement (A-D16).**
  - Cancel a background claude mid-turn while a `sleep 300` tool call runs.
  - Check `pgrep -f 'sleep 300'` on the runner host.
  - Resume with `--resume-conversation`, and check the conversation continues.
  - Record both results, with the commands, in the spec's F5 row.
  - **Gate:** if either fails, stop and return to planning. A-D16 then adds a graceful-stop stage.
- [ ] **Step 5: Docs.** Update the skills text and the README. Run `make build`.
- [ ] **Step 6: Commit** `docs: background mode in the agent skills; F5 measurement recorded`.

---

## Self-review notes

- **Spec coverage:**

  | Spec item | Task |
  | --- | --- |
  | A-D1 | 2, 4, 7, 9 (the schedule bit is pillar S) |
  | A-D2 | 4, 5, 8 |
  | A-D3 | 8, 3 |
  | A-D4 | 4, 3 |
  | A-D5 | 4 |
  | A-D6 / A-D7 | 2, 4, 9 |
  | A-D8 / A-D9 | 5 |
  | A-D10 | 2 |
  | A-D11 | 2 |
  | A-D12 | 3 |
  | A-D13 | 5, 6 |
  | A-D14 | 2, 9, 10, 11 |
  | A-D15 | 9, 10, 11 |
  | A-D16 | 8 (comments), 12 (measurement + gate) |
  | A-D17 | 1, 3, 4, 8, 9 |
  | A-D18 | 9 (stale comment), 12 (trust answered once) |
  | A-D19 | 2, 7, 9, 10, 11 |
  | F21 sizes | 9 |

- **Not in this plan, deliberately:**
  - the schedule-bit check (A-D1, G46): pillar S;
  - the graceful stop (A-D16) and the stream pending-count stage (A-D9): both
    are gated on Task 12's measurements, and are planned only if a gate
    fires.
