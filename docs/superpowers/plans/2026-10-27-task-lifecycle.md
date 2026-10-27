# Task Lifecycle and Program Status Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every task list shows whether a claude task is working, waiting on the human, ready for the next prompt, or stopped on an error. It also shows the program's own OSC 7501 report, which the server makes claude emit by answering its probe.

**Architecture:**
- **Lifecycle** is a stored `TaskEntry` field. It is written by the new `lifecycle_set` (caller's principal, no cap), which `harness-cli lifecycle <state>` sends. That CLI is invoked by async claude hooks that the runner adds to the worktree `.claude/settings.json`. It rides `TaskInfo` and `TaskStatusEvent` beside progress.
- **Program status** is live-only, parsed by `vtgrid.Terminal` beside the title. `SessionMux` answers the `OSC 7501 ; ?` probe on the live output path.

**Tech Stack:** Go; `.bgn` schema regenerated with `make protoregen`; verb table `cli/verb/table.go` + `go generate ./cli/verb`; bubbletea TUI; Go-wasm + vanilla JS WebUI.

**Spec:** `docs/superpowers/specs/2026-10-27-task-lifecycle-design.md`. Read its **Problem** (P1–P3) and **Decisions taken** (D1–D25) before any task; every task cites the D-numbers it implements. The sibling it mirrors is progress: spec `docs/superpowers/specs/2026-10-19-task-progress-design.md`, plan `docs/superpowers/plans/2026-10-19-task-progress.md`. Where this plan says "as progress does", read that code and copy its shape.

## Global Constraints

- Work in `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/5b65231c141da46b2239912efd108196` on branch `harness/5b65231c141da46b2239912efd108196`, rebased on `main` @ `8fbeb114`. Use absolute paths under that directory. Never bare `go build ./cmd/x/`; compile-check with `make check` / `go build ./...`.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.
- Every `.bgn` enum addition is APPENDED; enums are positional.
- `lifecycle_set` takes no capability and writes only `lookupPrincipal(cid)`'s task. A zero principal gets `no_principal` (D12).
- Lifecycle is memory-only and carried across a deliberate stop by `progress.json`. Program status is live-only and never stored (D13, D17).
- Operator output never elides a zero. Gate on existence (`lifecycle_at != 0`, `live_terminal`), not on value.
- Display words are `wait` / `ready` / `err` (D19). Wire names are `working` / `waiting` / `idle` / `error`.
- `harness-cli lifecycle` prints nothing to stdout and exits 0 in every case. A failure goes to stderr as one line (spec § CLI).
- Repo is public: no hostnames, IPs or private paths in code, tests or docs.
- No skew guard. Server first, then clients and the runner host's `harness-cli`.

## Review Focus

1. **Server unreachable or slow while a hook fires.** The hook must exit 0 with empty stdout. Because it is `async`, claude never waits on it. Covered in Task 3 (`TestLifecycleCmd_ServerDownExitsZeroSilently`).
2. **Two async hooks land out of order, including across a resume to another runner.** The older `reported_at` is answered `stale` and leaves the field unchanged. A resume resets the reporter stamp, so the new host's first report lands. Covered in Task 2.
3. **A probe recorded in the past.** A probe inside a held capture fed by `loadScreen` must not be answered. Only `recordFrame` answers. Covered in Task 6.
4. **A hostile or broken OSC 7501.** An oversized `msg`, a decoded control character, bad base64 or an unknown state is ignored whole, and the previous program status stays. Covered in Task 6.
5. **A worktree whose `settings.json` already has user hooks and the old harness hook.** The merge keeps them, adds the lifecycle entries with `async`/`matcher`, and a second run changes nothing. Covered in Task 4.

---

### Task 1: Wire schema for both phases + regen (D7, D8, D10, D17, all wire)

All schema changes land here, in one place.

**Files:**
- Modify: `runner/protocol/message.bgn`. Locations:
  - `TaskControlKind` (`enum` at :434): append after `runner_keys`.
  - The request and response unions: after the `runner_keys` arms at ~:2407 / ~:2489.
  - `TaskInfo`: after `title` (:1040), before `authority_source`.
  - `StatusEventKind` (:2721): append after `task_progress`.
  - `TaskStatusEvent`: after `title`, at its end.
- Regenerate: `runner/protocol/message.go` (`make protoregen`)
- Modify: `server/cap_completeness_test.go` (:49 block) and `server/scope_completeness_test.go` (:69 block). Add `TaskControlKind_LifecycleSet: capNone` / `noTarget`.
- Test: `runner/protocol/lifecycle_wire_test.go` (create, beside `progress_wire_test.go`)

**Interfaces:**
- Produces (generated):
  - `TaskControlKind_LifecycleSet`.
  - `LifecycleState_{Unset,Working,Waiting,Idle,Error}` = 0..4.
  - `LifecycleSetRequest{State LifecycleState; ReportedAt uint64}`.
  - `LifecycleSetStatus_{Ok,NoPrincipal,BadRequest,NotFound,Stale}` = 0..4.
  - `LifecycleSetResponse{Status; LifecycleAt uint64}`.
  - `StatusEventKind_TaskLifecycle`.
  - `ProgramState_{None,Idle,Working,Done,Blocked,Error}` = 0..5.
  - `ProgramKind_{None,Permission,Question,Auth}` = 0..3.
  - On both `TaskInfo` and `TaskStatusEvent`: `Lifecycle LifecycleState; LifecycleAt, LifecycleAgeMs uint64; ProgramState ProgramState; ProgramKind ProgramKind; ProgramMsg []byte`.
- Schema text for the lifecycle half is the spec's § Phase 1 Wire verbatim, comments included. The program half, after the lifecycle fields:

```
    # program status: the root OSC 7501 record the session's program last
    # reported (spec D17). Live-only like title; read only when
    # live_terminal = 1. none = never reported (or cleared with state=clear).
    program_state :ProgramState
    program_kind :ProgramKind        # only with program_state = blocked
    program_msg_len :u16
    program_msg :[program_msg_len]u8 # decoded, sanitized like title, <= 256 B
```

- [ ] **Step 1: Write `TestLifecycleWire_RoundTrip`.**
  - Encode then decode a `TaskControlRequest{Kind: LifecycleSet}` carrying `{State: Waiting, ReportedAt: 123}`, and a `TaskInfo` and a `TaskStatusEvent` carrying `Lifecycle=Idle, LifecycleAt=7, LifecycleAgeMs=9, ProgramState=Blocked, ProgramKind=Question, ProgramMsg="q?"`.
  - Every field survives.
  - `len(encoded request) < 64`.
- [ ] **Step 2: Run `go test ./runner/protocol -run LifecycleWire -v`.** Expected: FAIL (undefined).
- [ ] **Step 3: Edit `message.bgn`, then run `make protoregen`.** Large churn in `message.go` is normal.
- [ ] **Step 4: Run `go test ./runner/protocol ./server -run 'LifecycleWire|Completeness' && make check && make wasm-check`.** Expected: PASS.
- [ ] **Step 5: Commit** `proto: lifecycle_set, lifecycle and program status on TaskInfo / task events`.

### Task 2: Server — lifecycle store, handler, snapshot, resume reset, List, events (D10, D12, D13, D14, D20, D23)

**Files:**
- Create: `server/lifecycle.go` (handler + `lifecycleAgeMs` reuse of `progressAgeMs`)
- Modify: `server/taskstore.go`:
  - `TaskEntry` gains fields after `ProgressAt`, at :134.
  - The setter goes beside `SetProgress` (:377).
  - `progressLine`, `progressLines` and `restoreProgress` (:392–427).
  - `Resume` (:579) zeroes `LifecycleReportedAt`.
- Modify: `server/task_handler.go`:
  - Dispatch beside `ProgressSet` (:902).
  - Field `OnTaskLifecycle` beside `OnTaskProgress` (:50).
  - `toTaskInfo` beside the progress fields (:2516).
  - `redactParentTaskInfo` (:2555) does NOT clear lifecycle (D20). Add a comment saying so.
- Modify: `server/server.go`:
  - `publishTaskEvent` stamps the lifecycle fields next to progress (:440–455).
  - Wire `OnTaskLifecycle` → `publishTaskEvent(id, StatusEventKind_TaskLifecycle, t.Status, 0)`, beside `OnTaskProgress` (:528).
- Modify: `server/task_event_audience.go` (:30–36). The parent-hop event redaction keeps lifecycle (D20).
- Test: `server/lifecycle_test.go` (fixture style of `server/progress_test.go`)

**Interfaces:**
- Produces: `TaskEntry.Lifecycle protocol.LifecycleState`, `TaskEntry.LifecycleAt int64` (server clock), `TaskEntry.LifecycleReportedAt int64` (reporter clock).
- Produces: `func (s *TaskStore) SetLifecycle(id string, st protocol.LifecycleState, reportedAt, at int64) (TaskEntry, protocol.LifecycleSetStatus)`.
  - Returns `NotFound` when the task is absent.
  - Returns `Stale` when `reportedAt < LifecycleReportedAt`. An equal value is accepted.
  - Otherwise it stores the state with `at` made monotonic as `SetProgress` does.
- Produces: `progressLine` gains `Lifecycle uint8 json:"lifecycle"`, `LifecycleAt int64 json:"lifecycle_at"`, `LifecycleReportedAt int64 json:"lifecycle_reported_at"`. The filter becomes `ProgressAt != 0 || LifecycleAt != 0`. `restoreProgress` restores all three.
- Produces: `func (h *TaskHandler) handleLifecycleSet(conn ConnHandle, req *protocol.TaskControlRequest)`.
  - It answers `BadRequest` for `State == Unset` or an out-of-range value.
  - On `Ok` it calls `OnTaskLifecycle(taskHex)` before responding.
  - On `Stale` it publishes nothing.

- [ ] **Step 1: Write the failing tests in `server/lifecycle_test.go`.**
  - `TestLifecycleSet_WritesCallersOwnTask`: principal T sends `Waiting` with reportedAt 100. Expect `Ok`, `LifecycleAt != 0`, `Tasks.Get(T).Lifecycle == Waiting`, and a List row with `Lifecycle == Waiting` and `LifecycleAgeMs < 5000`.
  - `TestLifecycleSet_OperatorRefused`: expect `NoPrincipal`.
  - `TestLifecycleSet_UnsetRefused`: `State=Unset` gives `BadRequest`, and the store is unchanged.
  - `TestLifecycleSet_OlderReportIsStale`: Idle@200, then Working@150. The second gets `Stale`, the state stays `Idle`, and `OnTaskLifecycle` was called exactly once.
  - `TestLifecycleSet_ResumeResetsReporterStamp`: Idle@200, end T, `Tasks.Resume(T, …)`, then Working@50 gives `Ok`.
  - `TestLifecycleSet_SurvivesParentHopRedaction`: the parent row seen by a child carries `Lifecycle` while `Progress` is empty.
  - `TestLifecycleSnapshot_RoundTrip`: a task with lifecycle and no progress. `writeProgressSnapshot` → a new store → `loadProgressSnapshot` restores `Lifecycle`, `LifecycleAt` and `LifecycleReportedAt`.
  - `TestTaskEvent_CarriesLifecycle`: an event published for T carries `Lifecycle` / `LifecycleAt`.
- [ ] **Step 2: Run `go test ./server -run 'LifecycleSet|LifecycleSnapshot|TaskEvent_CarriesLifecycle' -v`.** Expected: FAIL.
- [ ] **Step 3: Implement as progress does.** Copy the response helper of `handleProgressSet`, using `resp.SetLifecycleSet`.
- [ ] **Step 4: Run `go test ./server -count=3` and `make check`.** Expected: PASS. A known ~1/6 package flake family exists; a failure outside these tests is not this task's.
- [ ] **Step 5: Commit** `server: lifecycle_set — hook-reported task state, stored and listed`.

### Task 3: CLI — `lifecycle` verb, local dedup, `ls` (D7, D11, D19, spec § CLI)

**Files:**
- Create: `cli/lifecycle.go` (client helper, dedup cache, string helpers)
- Modify: `cli/list.go`:
  - `taskLine`: after `ProgressStr` at :254.
  - `taskJSON`: beside the `progress` keys at :510.
  - `newTaskJSON`: at :622.
- Modify: `cli/verb/table.go` (entry after `progress`, :1680), then `go generate ./cli/verb`
- Modify: `cmd/harness-cli/dispatch.go` (`func (h cliVerbs) Lifecycle(a verb.LifecycleAction) error`, beside `Progress` :804)
- Test: `cli/lifecycle_test.go`, `cmd/harness-cli/lifecycle_test.go`

**Interfaces:**
- Produces: `func (c *Client) LifecycleSet(ctx context.Context, st protocol.LifecycleState, reportedAt int64) (*protocol.LifecycleSetResponse, error)` and the package-level `func LifecycleSet(ctx context.Context, peerCID objproto.ConnectionID, st protocol.LifecycleState, reportedAt int64) (*protocol.LifecycleSetResponse, error)`, in the shape of `ProgressSet` (`cli/progress.go:16-41`).
- Produces: `func ParseLifecycleState(s string) (protocol.LifecycleState, bool)`, which accepts the wire words only.
- Produces: `func LifecycleWord(st protocol.LifecycleState) string`, which returns the wire word (`""` for Unset). It is used by `ls` text and json.
- Produces: `func LifecycleBadge(t *protocol.TaskInfo) string`, the display used by the TUI and WebUI parity tests.
  - It returns `"wait:3m"`, `"ready:12m"` or `"err:1m"` for Waiting / Idle / Error when `LifecycleAt != 0` and the status is non-terminal.
  - It returns `""` for Working, Unset, a terminal status, or a never-written field.
  - The age format is `ActivityStr`'s minute/second form without the `ago`.
- Produces: `func LifecycleStr(t *protocol.TaskInfo) string`. It returns `lifecycle=waiting (3m ago)` when `LifecycleAt != 0` and non-terminal, else `""`.
- Produces: `func ProgramWord(s protocol.ProgramState) string` and `func ProgramKindWord(k protocol.ProgramKind) string`. Each returns the wire word, or `""` for None. `taskJSON` uses them here, and Task 7 reuses them.
- Produces: `func lifecycleDedup(taskHex string, st protocol.LifecycleState) (skip bool, record func())`. The file is `filepath.Join(os.UserCacheDir(), "harness", "lifecycle-"+taskHex)` and holds the wire word.
  - `skip` is true when the file holds the same word.
  - `record` writes the word after a successful (`Ok` or `Stale`) send.
  - Any file error means "do not skip" (D11: the file is an optimization only).
  - An empty `taskHex` (`HARNESS_TASK_ID` unset) means never skip.
- Verb table entry:
  - Path `["lifecycle"]`, `CmdlineSurfaces: CLI`, `Action: "LifecycleAction"`.
  - `Args: []Arg{{Name: "state", Type: ArgString, Field: "State", OneOfArg: []string{"working", "waiting", "idle", "error"}}}`.
  - `NoModalSurface: "written by the claude hooks the runner injects into the worktree's .claude/settings.json; an operator connection has no principal and is refused (spec D12)"`.
  - Notes:
    - `"report THIS task's lifecycle state; written by hooks, not called by hand or by the agent"`
    - `"silent on stdout and exits 0 even on failure, so a hook never disturbs the agent; failures go to stderr"`
  - Example: `"lifecycle waiting"`.
- Dispatch order:
  1. `reportedAt := time.Now().UnixNano()` comes first.
  2. Parse the state.
  3. Dedup via `os.Getenv("HARNESS_TASK_ID")`.
  4. Send with `context.WithTimeout(h.ctx, 5*time.Second)`.
  5. `record()` on `Ok`/`Stale`.
  6. Write any error to stderr as one line, and return nil.

- [ ] **Step 1: Write the failing tests.**
  - `TestLifecycleBadge`: Waiting, age 180000, Running gives `"wait:3m"`. Idle, age 720000 gives `"ready:12m"`. Error, age 30000 gives `"err:30s"`. Working gives `""`. Waiting with status Succeeded gives `""`. `LifecycleAt == 0` gives `""`.
  - `TestTaskLine_Lifecycle`: a Running task with Waiting, age 180000 contains `lifecycle=waiting (3m ago)`. With `LifecycleAt == 0` it contains no `lifecycle=`.
  - `TestTaskJSON_LifecycleFields`: keys `lifecycle` (`""` when unset), `lifecycle_at`, `lifecycle_age_ms`, `program_state`, `program_kind`, `program_msg` are present at zero.
  - `TestLifecycleDedup`: with `t.Setenv("XDG_CACHE_HOME", t.TempDir())`:
    - first call: no skip;
    - after `record()`, the same state skips;
    - a different state does not skip;
    - an unwritable cache dir does not skip;
    - an empty taskHex never skips.
  - `TestLifecycleCmd_ServerDownExitsZeroSilently` (`cmd/harness-cli`): run the dispatcher with an unreachable server CID and `HARNESS_TASK_ID` set. It returns nil, stdout is empty, and stderr is one non-empty line.
- [ ] **Step 2: Run `go test ./cli ./cmd/harness-cli -run 'Lifecycle|TaskLine_Lifecycle|TaskJSON_Lifecycle' -v`.** Expected: FAIL.
- [ ] **Step 3: Implement, then run `go generate ./cli/verb`.** `taskJSON` also gains the three program keys here, mapped from the Task 1 fields (Task 7 renders them elsewhere).
- [ ] **Step 4: Run `go test ./cli/... ./cmd/harness-cli && make check && make wasm-check`.** Expected: PASS. If a `cli/verb` invariant test fails, it is pointing at a missing declaration; fix the declaration, not the test.
- [ ] **Step 5: Commit** `cli: lifecycle verb (hook-only, silent, deduped) and lifecycle in ls`.

### Task 4: Runner — inject the lifecycle hooks (D4, D9, D21, D22, D24)

**Files:**
- Modify: `runner/settings.go`:
  - `harnessHookEntries` (:34) gains fields `Matcher string` and `Async bool`, plus the new entries.
  - `mergeHarnessHooks` writes `matcher` when non-empty and `"async": true` when set, in the new group's entry.
  - Update the `WriteAgentSettings` doc comment: remove "There is deliberately no Stop hook" and replace it with D6's reasoning in two sentences.
- Test: `runner/settings_test.go`

**Interfaces:**
- Entries appended. Event, then matcher, then command; all `Async: true`:
  - `SessionStart`, `startup|resume|clear`, `harness-cli lifecycle idle`
  - `UserPromptSubmit`, —, `harness-cli lifecycle working`
  - `PreToolUse`, `AskUserQuestion`, `harness-cli lifecycle waiting`
  - `PostToolUse`, —, `harness-cli lifecycle working`
  - `PermissionRequest`, —, `harness-cli lifecycle waiting`
  - `Elicitation`, —, `harness-cli lifecycle waiting`
  - `Stop`, —, `harness-cli lifecycle idle`
  - `StopFailure`, —, `harness-cli lifecycle error`
- The existing inbox entry stays first and stays synchronous.

- [ ] **Step 1: Write the failing tests.**
  - `TestWriteAgentSettings_LifecycleHooks`: into an empty dir. Each of the 8 events holds a group whose hook has the listed command, `"async": true`, and (for `SessionStart` / `PreToolUse`) the listed `matcher`. The `UserPromptSubmit` event holds both the inbox hook (no `async` key) and the lifecycle hook.
  - `TestWriteAgentSettings_LifecycleMergeIdempotentWithUserHooks`: start from a file with a user `PostToolUse` hook `"my-linter"`, a user `Stop` hook, and the inbox hook. After two runs the user hooks are intact, each lifecycle command appears exactly once per event, and the second run's file bytes equal the first's.
- [ ] **Step 2: Run `go test ./runner -run WriteAgentSettings -v`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./runner` and `make check`.** Expected: PASS.
- [ ] **Step 5: Commit** `runner: inject async lifecycle hooks into the worktree settings`.

### Task 5: TUI + WebUI — lifecycle display (D1, D19, spec § Surfaces)

**Files:**
- Modify: `tui/tasks.go`:
  - `flatCols` and `treeCols` (:93–123): `Act` width 8 → 10.
  - The act cell (:305–308): when `cli.LifecycleBadge(&t) != ""` use it, rendered bold for `wait`. Otherwise keep `cli.ActivityStr`.
- Modify: `tui/app.go`:
  - `applyEventProgress` (:2044) also copies the lifecycle fields and the three program fields.
  - Add `a.lifecycleRecvAt` beside `progressRecvAt` (:511, :519).
  - `agedTask` (:2059) ages `LifecycleAgeMs` like `ProgressAgeMs`.
- Modify: `tui/detail.go`: after the progress line (:218), add `lifecycle:     waiting (3m ago)` via `cli.LifecycleStr`'s word and age, when `LifecycleAt != 0`.
- Modify: `cmd/harness-webui-wasm/main.go` (task object :1810–1816 and the event bridge). Add `lifecycle` (wire word), `lifecycleAt` (decimal string, like `progressAt`) and `lifecycleAgeMs`, plus `programState`, `programKind` and `programMsg` (wire words / string).
- Modify: `webui/static/main.js`:
  - Add a pure `taskLifecycleModel(t)` next to `taskNowModel` (:7801). It returns `{word, age, attention}` or null with the same rules as `cli.LifecycleBadge`. `attention` is true for `wait` / `err`.
  - A badge span `task-lifecycle` after `statusEl` in the row's `line1` (:5726). Its color is `taskStatusColor("Failed")` when `attention`, else `taskStatusColor("")`. Text via `textContent`.
  - The same badge on the task sheet header and in the tree node's status line.
- Modify: `webui/static/style.css` (`.task-lifecycle` spacing only).
- Test: `tui/lifecycle_test.go`, `webui/static/cmd_test.mjs`

**Interfaces:**
- Consumes: `cli.LifecycleBadge`, `cli.LifecycleStr`, `cli.LifecycleWord` (Task 3).

- [ ] **Step 1: Write the failing tests.**
  - `tui`:
    - The act cell of a Running row with Waiting, age 180000 renders `wait:3m`, and a Working row renders `ActivityStr`'s output.
    - `applyEventProgress` copies `Lifecycle` / `LifecycleAt` / `LifecycleAgeMs` and the program fields.
    - `agedTask` advances `LifecycleAgeMs`.
    - The detail popup contains `lifecycle:` only when `LifecycleAt != 0`.
  - `cmd_test.mjs`: `taskLifecycleModel` gives:
    - `{lifecycle:"waiting", lifecycleAt:"5", lifecycleAgeMs:180000, status:"Running"}` → `{word:"wait", age:"3m", attention:true}`;
    - `"idle"` → `word:"ready"`, `attention:false`;
    - `"working"` → null;
    - `lifecycleAt:"0"` → null;
    - status `"Succeeded"` → null.
- [ ] **Step 2: Run `go test ./tui -run Lifecycle -v` and `make js-test`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./tui && make js-test && make wasm-check`.** Expected: PASS.
- [ ] **Step 5: Screenshots.** Follow `webui/INDEX.md`'s Playwright procedure (memory) to screenshot a list with one `wait`, one `ready` and one working row at 1280 and 390 widths. Report the paths; they are deliverables.
- [ ] **Step 6: Commit** `tui, webui: lifecycle badge (wait / ready / err) in lists, detail and tree`.

### Task 6: Server — OSC 7501 parse and probe answer (D17, D18)

**Files:**
- Modify: `vtgrid/terminal.go`:
  - The struct gains `program ProgramStatus` and `programQueries int` beside `title` (:99).
  - `endString` (:576) handles `"7501"`.
  - Add the getters.
- Create: `vtgrid/program_status.go`. This is the parser, ported from stablyai/orca `src/shared/program-status/protocol.ts` (MIT; keep its limits and header attribution comment). Before porting, check the reply and the grammar against the current revision of the spec (spec § The probe).
- Modify: `server/session_mux.go`:
  - `recordFrame` (:349): after `m.screen.Write`, take the query count and write one reply per query with `m.writeFrameToRunner(encodeFrame(frame.FrameType_Stdin, programStatusQueryReply))`.
  - `loadScreen` (:406): after its write, take and DISCARD the count.
  - Add a `Program()` getter beside `Title()` (:1109).
- Modify: `server/task_handler.go` `handleList` live enrichment and `server/server.go` `publishTaskEvent` (:458–465). Set the program fields where `LiveTerminal` / `Title` are set. The parent-hop redaction clears `ProgramMsg` (content) and keeps the state and kind. This decision belongs to this plan: msg is content like title, and the state is liveness like lifecycle (D20).
- Test: `vtgrid/program_status_test.go`, `server/session_program_test.go`

**Interfaces:**
- Produces: `type ProgramStatus struct { State, Kind, Msg string }` in `vtgrid`. State and Kind are the protocol's words (`""` = none). `vtgrid` does not import `protocol`; the server maps the words to the enums.
- Produces: `func ParseProgramStatus(body string) (st ProgramStatus, query bool, ok bool)`. It handles the root record only (no `id=`). `state=clear` yields the zero `ProgramStatus` with `ok = true`.
- Produces: `func (t *Terminal) ProgramStatus() ProgramStatus` and `func (t *Terminal) TakeProgramStatusQueries() int`. The latter returns the count and resets it.
- Produces: `const ProgramStatusQueryReply = "\x1b]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth\x1b\\"` in `vtgrid`.
- Produces: `func (m *SessionMux) Program() (state protocol.ProgramState, kind protocol.ProgramKind, msg string)`. `msg` passes through `vtgrid.SanitizeTitle(msg, maxWireTitleBytes)`.

- [ ] **Step 1: Write the failing tests.**
  - `TestParseProgramStatus`:
    - `"state=blocked:kind=question:msg=cT8="` → `{blocked, question, "q?"}`.
    - `"?"` → query.
    - `"state=working:id=a/b"` → not ok (child record).
    - `"state=bogus"` → not ok.
    - A `msg` whose decoded bytes contain `\x1b` → not ok.
    - A body over 4096 bytes → not ok.
    - `"state=working:kind=question"` → the kind is dropped (only with blocked).
  - `TestTerminal_ProgramStatusAcrossWrites`: write `"\x1b]7501;state=wor"` then `"king\x07"`. `ProgramStatus().State == "working"`. A later invalid report leaves it unchanged.
  - `TestTerminal_ProbeCounted`: write `"\x1b]7501;?\x1b\\"` twice. `TakeProgramStatusQueries() == 2`, then `0`.
  - `TestSessionMux_AnswersLiveProbe`: feed a Stdout frame carrying the probe through `recordFrame`. The runner side receives exactly one Stdin frame whose payload equals `vtgrid.ProgramStatusQueryReply`.
  - `TestSessionMux_DoesNotAnswerLoadedProbe`: `loadScreen` with the probe writes no Stdin frame, and a later live frame without a probe writes none either.
  - `TestList_ProgramStatusFromLiveMux`: after a live `"\x1b]7501;state=blocked:kind=permission\x07"`, the List row has `ProgramState == Blocked` and `ProgramKind == Permission`. The parent-hop row keeps them with `ProgramMsg` empty.
- [ ] **Step 2: Run `go test ./vtgrid ./server -run 'ProgramStatus|Probe|ProgramStatusFromLiveMux' -v`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./vtgrid ./server -count=3 && make check`.** Also run `go test ./vtgrid -run VTCorpus`: the corpus parity must not move.
- [ ] **Step 5: Commit** `server: parse OSC 7501 and answer its probe on the live path`.

### Task 7: Program status display (spec § Phase 2 Surfaces)

**Files:**
- Modify: `cli/list.go` `taskLine`: `program=blocked:permission` (kind only with blocked) when `LiveTerminal() && ProgramState != None`. No msg.
- Modify: `tui/detail.go`: a `program:` line under the lifecycle line. The format is `program:       blocked (permission) — <msg>`, and each part is omitted when absent.
- Modify: `webui/static/main.js`: the task sheet shows the same line under the lifecycle badge. Not on the list row or the tree node.
- Test: `cli/lifecycle_test.go` (extend), `tui/lifecycle_test.go` (extend), `webui/static/cmd_test.mjs` (a pure `programStatusLine(t)` helper)

**Interfaces:**
- Consumes: `cli.ProgramWord`, `cli.ProgramKindWord` (Task 3).
- Produces: `func ProgramStr(t *protocol.TaskInfo) string` in `cli`, `""` when not shown.

- [ ] **Step 1: Write the failing tests.**
  - `TestTaskLine_Program`: live, Blocked/Question gives `program=blocked:question`. Working gives `program=working`. `ProgramState == None` gives no `program=`. Not live gives no `program=`.
  - The TUI detail shows the `program:` line with the msg.
  - `programStatusLine` gives `"blocked (question) — q?"` for those fields, and null when `programState` is `""`.
- [ ] **Step 2: Run the three suites.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./cli ./tui && make js-test && make wasm-check`.** Expected: PASS.
- [ ] **Step 5: Commit** `cli, tui, webui: program status (OSC 7501) in ls and detail views`.

### Task 8: Skill note, end-to-end, rollout check

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md`. After the progress section (:808–835), add a short section:
  - `ls` now shows `lifecycle=` (written by hooks the runner injects: working / waiting / idle / error, with its age) and `program=` (the program's own report).
  - Agents never call `harness-cli lifecycle` themselves.
  - Copy it byte-identically to `.claude/skills/harness-cli/SKILL.md` and `.agents/skills/harness-cli/SKILL.md`.
- Test: `go test ./runner/agentskills`

- [ ] **Step 1: Write the section and copy it.** Then `diff runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md` and the same for `.agents/` must print nothing.
- [ ] **Step 2: Run `go test ./runner/agentskills`.** Expected: PASS.
- [ ] **Step 3: Run the end-to-end check with `scripts/dummy-harness.sh`.** Load the `dummy-harness` skill first, and take the instance down after.
  - Interactive claude task in auto mode:
    - prompt → `ls` shows `lifecycle=working`;
    - prompt claude to use `AskUserQuestion` → `lifecycle=waiting`;
    - answer → `working`;
    - turn end → `lifecycle=idle`, and the TUI `Act` shows `ready:…`.
  - The same task in a permission-prompting mode with a file Write: `waiting`, then approve, then `working`.
  - `ls` shows `program=` changing during the turn, and the `title=` has no `◐`/`✳` prefix (D18 effect).
  - One event-stream task through the same prompt → idle sequence. Record whether `waiting` appears (D24 open check) in the commit message.
  - Stop the server with a held stop and restart it. `lifecycle=` survives on the held task.
- [ ] **Step 4: Run `scripts/wire-skew-check.sh`.** Expected: exit 0.
- [ ] **Step 5: Run `make check && make vet && make wasm-check` and `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`.** Expected: PASS.
- [ ] **Step 6: Commit** `skills: lifecycle and program status in ls`.
