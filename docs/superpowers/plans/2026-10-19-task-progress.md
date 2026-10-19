# Task Progress and Window Title Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Agents self-report a one-line progress that every task list shows beside the server-held window title. Supervisors can wait for a progress change. `tasks.status` is filtered like `ls`, and `notify` identity is derived on the server.

**Architecture:** Progress is a stored `TaskEntry` field written by `progress_set`, keyed to the caller's principal, and persisted as a WAL record. The title is a live read of `SessionMux.screen`. Both ride `TaskInfo` (List) and `TaskStatusEvent` (pubsub). `await-progress` is a second condition in the existing idle-watcher registry.

**Tech Stack:** Go; `.bgn` schema regenerated with `make protoregen`; verb table `cli/verb/table.go` + `go generate ./cli/verb`; bubbletea TUI; Go-wasm + vanilla JS WebUI.

**Spec:** `docs/superpowers/specs/2026-10-19-task-progress-design.md`. Read its **Problem** section (P1–P5) and **Decisions taken** table (D1–D21) before any task. Every task cites the D-numbers it implements.

## Global Constraints

- Every enum addition in `message.bgn` is APPENDED. Renames keep positions (D15).
- Progress text: ≤ 256 bytes, valid UTF-8, no C0 (0x00–0x1F), DEL (0x7F) or C1 (U+0080–U+009F). Violations are refused with `bad_text`, never sanitized (D12).
- Title on the wire: C0/DEL/C1 removed, truncated to 256 bytes on a UTF-8 boundary (D13).
- `progress_set` takes no capability and writes only `lookupPrincipal(cid)`'s task. A zero principal gets `no_principal` (D7).
- Live-only fields (title, `live_terminal`) are never stored and never written to the WAL (D11).
- No compat shim or skew guard (D16).
- Operator output never elides a zero. Gate on existence (`progress_at != 0`, `live_terminal`), not on value.
- Repo is public: no hostnames, IPs or private paths in code, tests or docs.
- Work in `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd` on branch `harness/70fbad4a6eb6f1e992be8a669f1bcefd`. Use absolute paths under that directory. Compile-check with `make check` / `go build ./...`. Never bare `go build ./cmd/x/`.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.

## Review Focus

1. **Forged identity.** `HARNESS_TASK_ID` / `HARNESS_HOSTNAME` set to another task's values must not change who a notify or a progress write is attributed to. Covered in Task 1 and Task 3.
2. **A supervisor arms after a write.** `ls` shows stage X, the child writes stage Y, then the supervisor arms with `--since <X's progress_at>`. It must fire at once with Y. Covered in Task 5.
3. **Control bytes in text reaching a terminal.** An agent passes `"\x1b[2J"` as progress, or a program titles itself with escape bytes. The progress write is refused. The title reaches the TUI with the bytes removed. Covered in Task 3 and Task 4.
4. **A confined subscriber.** A task subscribed to `tasks.status` must receive nothing about tasks its `ls` hides. Its parent's row arrives with progress and title empty. Covered in Task 2.
5. **Restart.** Progress set before a deliberate server restart is still in `ls` after it. The title returns via the held capture. Covered in Task 3 (WAL replay). The title half is existing behaviour; Task 4 adds a regression test.

---

### Task 1: Wire schema, regen, renames, notify identity (D6, D10, D15, D20, D21 + all wire)

All schema changes land here, in one place. The only behaviour change is notify, because removing `NotifyRequest.worker` forces it.

**Files:**
- Modify: `runner/protocol/message.bgn` (TaskInfo ~:862–953, TaskControlKind tail ~:578, NotifyRequest ~:2405, StatusEventKind tail ~:2506, TaskStatusEvent ~:2508, await-idle block ~:2071–2150)
- Regenerate: `runner/protocol/message.go` (`make protoregen`)
- Modify: every Go/JS user of `AwaitIdleSink*`, `TaskControlKind_AwaitIdleList`, `TaskControlKind_AwaitIdleKill`. Find them with `grep -rn 'AwaitIdleSink\|AwaitIdleList\b\|AwaitIdleKill\b\|TaskControlKind_AwaitIdleList\|TaskControlKind_AwaitIdleKill' --include=*.go --include=*.js .`
- Modify: `cli/notify.go` (drop env identity), `server/task_handler.go:2437-2484` (`handleNotify`)
- Test: `server/notify_identity_test.go` (create), `runner/protocol` round-trip test beside existing ones

**Interfaces:**
- Produces (generated): `protocol.ProgressSetRequest{Clear bool; Text []byte}`, `ProgressSetResponse{Status ProgressSetStatus; ProgressAt uint64}`, `ProgressSetStatus_{Ok,NoPrincipal,BadText,NotFound}`, `TaskControlKind_ProgressSet`, `TaskControlKind_AwaitProgress`, `AwaitSink_{Reply,Notify,Board}` (renamed), `TaskControlKind_AwaitList` / `_AwaitKill` (renamed), `AwaitCondition_{Idle,Progress}`, `AwaitProgressRequest{TaskId; SinceProgressAt uint64; Sink AwaitSink; Topic []byte}`, `AwaitProgressStatus_{Fired,Armed,TaskEnded,NotFound,BadRequest,Cancelled}`, `AwaitProgressResponse{Status; ProgressAt uint64; Progress []byte; WatcherId uint64}`, `AwaitIdleWatcherInfo.Condition`, `StatusEventKind_TaskProgress`, `TaskInfo.{LiveTerminal bool; Progress []byte; ProgressAt, ProgressAgeMs uint64; Title []byte}`, `TaskStatusEvent.{LiveTerminal; Progress; ProgressAt; ProgressAgeMs; Title}`, and `NotifyRequest` without `origin` / `worker`.
- Produces: `func (h *TaskHandler) notifyWorkerInfo(conn ConnHandle) (protocol.WorkerInfo, bool)`. It returns false for a zero principal.

- [ ] **Step 1: Edit `message.bgn` exactly as the spec's § Wire gives it.** Field names and comments come from the spec. `live_terminal :u1` goes after `skills_injected` in `TaskInfo`, and `reserved :u6` becomes `:u5`. `TaskStatusEvent` appends `live_terminal :u1`, `reserved :u7`, then the progress/title fields. Rename `AwaitIdleSink` → `AwaitSink`, `await_idle_list` → `await_list`, `await_idle_kill` → `await_kill`. Append `progress_set`, then `await_progress`, to `TaskControlKind`. Add the request/response union arms beside the `await_idle` ones.
- [ ] **Step 2: Run `make protoregen`.** Expect `runner/protocol/message.go` to be rewritten. Large churn is normal (memory: ebm2go regen churn is constant).
- [ ] **Step 3: Fix the rename fallout until `make check && make wasm-check` pass.** Keep CLI/TUI/WebUI user-facing strings unchanged here; Task 6 renames what operators see.
- [ ] **Step 4: Write the failing test `TestHandleNotify_IdentityFromPrincipalNotClient` in `server/notify_identity_test.go`.**
  - Setup: a `newTestHandler` with a registry entry whose `Identity` = R and `Hostname` = `"runner-host"`. A task T is created and assigned to R. The caller conn's principal is T, and `h.BoardConnState` returns an identity carrying runner id R.
  - Send a `NotifyRequest{Level: Info, Title: "t", Text: "x"}` and capture `h.OnNotify`.
  - Assert `ev.Origin == NotifyOrigin_Worker`, `string(ev.Worker().TaskId) == T`, `string(ev.Worker().Hostname) == "runner-host"`, `string(ev.Worker().Repo) == TaskEntry.RepoPath`.
  - Second case: an operator conn (no principal) gives `Origin == NotifyOrigin_External` and `ev.Worker() == nil`.
- [ ] **Step 5: Run `go test ./server -run TestHandleNotify_Identity -v`.** Expected: FAIL.
- [ ] **Step 6: Implement `notifyWorkerInfo` and use it in `handleNotify`.**
  - Task id: `lookupPrincipal`, hex-encoded.
  - Runner id: `boardState(conn).Identity()`, string form.
  - Hostname: `h.Registry.GetLiveByIdentity(entry.AssignedTo)`, `""` if absent (D20).
  - Repo: `TaskEntry.RepoPath`.
  - The notify hook payload takes the same values.
  - In `cli/notify.go`, delete `newNotifyRequestFromEnv`'s env reads and build `NotifyRequest{Level, Title, Text}`. Keep `mtuGuardNotify`.
- [ ] **Step 7: Run `go test ./server ./cli ./runner/protocol` and `make check && make wasm-check`.** Expected: PASS.
- [ ] **Step 8: Commit** `proto: progress/title/await-progress wire; notify identity from the principal`.

### Task 2: `tasks.status` filtered like `ls` (D8, D14, D19)

**Files:**
- Modify: `server/server.go:428-463` (`publishTaskEvent`)
- Create: `server/task_event_audience.go`
- Test: `server/task_event_audience_test.go` (follow `server/conn_event_test.go:225` `TestConnEvents_SubtreeGating`)

**Interfaces:**
- Produces: `type taskAudience uint8` (`audienceNone`, `audienceFull`, `audienceParentHop`) and `func (h *TaskHandler) taskEventAudience(subCID, taskHex string) taskAudience`. It is computed from `listVisibleToCaller`: `all || allowed[taskHex]` gives Full, `parentHex == taskHex` gives ParentHop, anything else gives None.
- Produces: `func redactTaskStatusEvent(ev *protocol.TaskStatusEvent)`. It clears `Progress` and `Title` and leaves counters and status alone, mirroring `redactParentTaskInfo`.

- [ ] **Step 1: Write the failing test `TestTaskEvents_FilteredByListVisibility`.**
  - Subscribers: an operator conn, an agent conn A whose subtree contains child C, and conn K, a child of A whose only view of A is the parent hop. All subscribe to `tasks.status`.
  - A `task_started` on C: the operator and A receive it, K does not.
  - A `task_started` on A: K receives it with `Title`/`Progress` empty while `Viewers`/`TaskStatus` are intact. The operator receives it unredacted.
  - A `task_pruned` on C: all three receive it (D19).
- [ ] **Step 2: Run `go test ./server -run TestTaskEvents_Filtered -v`.** Expected: FAIL (K receives C's event).
- [ ] **Step 3: Implement.** For every kind except `StatusEventKind_TaskPruned`, `publishTaskEvent` makes two `PublishFiltered` calls per topic (`TasksStatus()` and `TaskStatus(id)`): the full payload with `audience == audienceFull`, and the redacted payload with `audience == audienceParentHop`. `task_pruned` keeps plain `Publish`.
- [ ] **Step 4: Run `go test ./server` .** Expected: PASS, including the existing TUI-facing event tests.
- [ ] **Step 5: Commit** `server: tasks.status delivers to whom ls would show the task`.

### Task 3: Progress write, store, WAL, List (D2, D7, D11, D12, D14)

**Files:**
- Create: `server/progress.go` (validation + handler)
- Modify: `server/taskstore.go` (`TaskEntry` :18–114, new setter, replay switch :1048)
- Modify: `server/task_handler.go` (dispatch case for `TaskControlKind_ProgressSet`, `toTaskInfo` :2345, `redactParentTaskInfo` :2423, `handleList` live enrichment)
- Modify: `server/capabilities.go:42-70` (record `progress_set` as no-cap beside `whoami`; check how whoami is exempted and do the same)
- Modify: `server/server.go` (wire `OnTaskProgress` → `publishTaskEvent(id, StatusEventKind_TaskProgress, status, 0)`; stamp progress fields on every event)
- Test: `server/progress_test.go`

**Interfaces:**
- Produces: `TaskEntry.Progress string`, `TaskEntry.ProgressAt int64` (unix nanos).
- Produces: `func (s *TaskStore) SetProgress(id, text string, at int64) (TaskEntry, bool)`. It writes WAL `{"type":"task_progress","task_id":…,"progress":…,"progress_at":…}` as one snapshot record. Add the `Progress` / `ProgressAt` fields to `WALEvent` in `server/wal.go`.
- Produces: `const maxProgressBytes = 256` and `func validateProgressText(text string, clear bool) error`.
- Produces: `TaskHandler.OnTaskProgress func(taskHex string)`, called after a successful write and BEFORE the response is sent.
- Produces: `func (h *TaskHandler) handleProgressSet(conn ConnHandle, req *protocol.TaskControlRequest)`.

- [ ] **Step 1: Write the failing tests in `server/progress_test.go`.**
  - `TestProgressSet_WritesCallersOwnTask`: the principal is T, caps `Capability_None`. Send text `"task A / stage 2"`. Expect `Status == Ok`, `ProgressAt != 0`, `Tasks.Get(T).Progress == "task A / stage 2"`, and a List row for T carrying the text plus `ProgressAgeMs < 5000`.
  - `TestProgressSet_OperatorRefused`: no principal gives `NoPrincipal`.
  - `TestProgressSet_BadText`: one case each for 257 bytes, `"\x1b[2J"`, `"\x7f"`, `"\u0085"`, invalid UTF-8 `"\xff"`, and `clear=true` with text `"x"`. Each gives `BadText`, and the store is unchanged.
  - `TestProgressSet_ClearKeepsStamp`: set, then clear. `Progress == ""`, `ProgressAt` is greater than the first stamp.
  - `TestProgressSet_SurvivesWALReplay`: a store with a WAL in `t.TempDir()`. `SetProgress`, close, then a new store replaying gives the same `Progress` / `ProgressAt`.
  - `TestProgressSet_RedactedOnParentHop`: the parent row seen by a child has `Progress == ""` and `Title == ""`.
  - `TestProgressSet_PublishesTaskProgress`: `OnTaskProgress` is called once with T.
- [ ] **Step 2: Run `go test ./server -run TestProgressSet -v`.** Expected: FAIL (undefined).
- [ ] **Step 3: Implement the store, validation, handler, dispatch, no-cap entry, `toTaskInfo` mapping, redaction and server wiring.** `ProgressAgeMs` is computed at List / event time on the server clock, like `OutputIdleMs`. `publishTaskEvent` reads `Progress` / `ProgressAt` from `s.tasks.Get` (it already does for `Kind`).
- [ ] **Step 4: Run `go test ./server` and `make check`.** Expected: PASS.
- [ ] **Step 5: Commit** `server: progress_set — a task's self-reported line, stored and listed`.

### Task 4: Window title and `live_terminal` (D11, D13, D21)

**Files:**
- Modify: `vtgrid/repaint.go` (sanitizer beside `titleOSC` :135)
- Modify: `server/session_mux.go` (getters near `LastOutputUnixNano` :1087)
- Modify: `server/task_handler.go` (`handleList` live enrichment ~:2079), `server/server.go` (`publishTaskEvent` enrichment :444)
- Test: `vtgrid/repaint_test.go`, `server/session_title_test.go`

**Interfaces:**
- Produces: `func SanitizeTitle(s string, maxBytes int) string` in `vtgrid`. It removes C0/DEL/C1 and truncates on a rune boundary. `titleOSC` calls it with no byte cap of its own (pass `len(s)`) so the two share one rule.
- Produces: `func (m *SessionMux) Title() string` (under `screenMu`, `""` when `!m.terminal`) and `func (m *SessionMux) HasTerminal() bool`.
- Produces: `const maxWireTitleBytes = 256` in `server`.

- [ ] **Step 1: Write the failing tests.**
  - `TestSanitizeTitle`: `"✳ fix\x07 bug\x1b"` gives `"✳ fix bug"`. A 300-byte string of `"✳"` gives a result of ≤ 256 bytes that is valid UTF-8. `"\u0085x"` gives `"x"`.
  - `TestList_TitleFromLiveMux`: a mux fed `"\x1b]0;✳ topic\x07"` as Stdout. The List row has `LiveTerminal == true` and `Title == "✳ topic"`.
  - `TestList_NoMuxNoTerminal`: a oneshot task gives `LiveTerminal == false` and `Title == ""`.
  - `TestTaskEvent_CarriesTitle`: an event published for that task carries the same title.
  - `TestHeldCapture_TitleSurvives` (regression for Review Focus 5): `heldCapture()` → `loadScreen` into a fresh mux gives `Title() == "✳ topic"`.
- [ ] **Step 2: Run `go test ./vtgrid ./server -run 'Title|NoMuxNoTerminal|HeldCapture_Title' -v`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Set `LiveTerminal` / `Title` at the same point where `LastOutputAt` is set in both places.
- [ ] **Step 4: Run `go test ./vtgrid ./server`.** Expected: PASS.
- [ ] **Step 5: Commit** `server: window title and live_terminal on TaskInfo and task events`.

### Task 5: `await-progress` watcher (D9, D15, D17)

**Files:**
- Modify: `server/idle_watchers.go` (`idleWatcher` :21–40 gains `condition`; `idleWatcherInfo` :141 sets `Condition`)
- Create: `server/await_progress_handler.go`
- Modify: `server/task_handler.go` (dispatch `TaskControlKind_AwaitProgress` beside :1019)
- Modify: `server/progress.go` (fire watchers after a write)
- Modify: `server/server.go` (on `task_ended` / `task_pruned`, end the progress watchers for that task)
- Test: `server/await_progress_test.go`

**Interfaces:**
- Consumes: `OnTaskProgress` / `handleProgressSet` (Task 3).
- Produces: `func (h *TaskHandler) handleAwaitProgress(conn ConnHandle, req *protocol.TaskControlRequest)`.
- Produces: `func (h *TaskHandler) fireProgressWatchers(taskHex string, status protocol.AwaitProgressStatus, text string, at int64)`. It fires every progress-condition watcher on `taskHex` exactly once. The fire/kill race resolves to one winner, as with idle watchers.
- Board payload: `{"kind":"task_progress","task":"<hex>","progress":"<text>","progress_at_unix_ms":N,"status":"fired|task_ended"}`, sent with `SenderKind_Server` like `fireIdleBoard`.
- Notify text: `"progress <short-id>: <text>"`, level info, `origin = external`, like `fireIdleNotify`.

- [ ] **Step 1: Write the failing tests** (reuse `awaitIdleViaHandle`-style helpers and `armBoardWatcher` from `server/idle_watchers_test.go`).
  - `TestAwaitProgress_ReplyFiresOnNextWrite`: arm a reply long-poll on T with since=0. A write of `"s2"` gives a response of `Fired` with `Progress == "s2"` and `ProgressAt` equal to the write's stamp.
  - `TestAwaitProgress_SinceFiresImmediately`: write `"s1"` (stamp a), then `"s2"` (stamp b). Arm with since=a. Expect an immediate `Fired` with `"s2"`.
  - `TestAwaitProgress_SinceCurrentWaits`: arming with since=b does not fire until a third write.
  - `TestAwaitProgress_TerminalAfterBaseline`: write `"final"`, then end T, then arm with since=(stamp before "final"). Expect `Fired` with `"final"`, not `TaskEnded`.
  - `TestAwaitProgress_EndedWhileArmed`: arm, then T ends. Expect `TaskEnded`.
  - `TestAwaitProgress_OneshotNoMux`: a oneshot target with no session arms successfully (unlike idle).
  - `TestAwaitProgress_NotVisible`: a target outside the caller's scope gives `NotFound`.
  - `TestAwaitProgress_NotifySinkRequiresNotifyCap`: mirror `TestHandleAwaitIdle_NotifySinkRequiresNotifyCap`.
  - `TestAwaitList_ShowsCondition`: one idle watcher and one progress watcher. `await_list` returns both, with `Condition` `Idle` and `Progress`, and `ThresholdMs == 0` for the progress watcher.
- [ ] **Step 2: Run `go test ./server -run 'AwaitProgress|AwaitList_ShowsCondition' -v`.** Expected: FAIL.
- [ ] **Step 3: Implement.** The gates are copied from `handleAwaitIdle`: target via `inScope(…, Capability_None, …)`, `notify` cap for sink=notify, `Board.Publishable` for sink=board. The baseline check runs before the terminal check (D17, spec § Watchers). No mux lookup.
- [ ] **Step 4: Run `go test ./server` (run it 3× with `-count=3`; this package has a known ~1/6 flake family — a failure outside these tests is not this task's).** Expected: PASS.
- [ ] **Step 5: Commit** `server: await-progress — one-shot watcher on a task's next progress write`.

### Task 6: CLI — client helpers, verbs, `ls` (D5, D10, D21)

**Files:**
- Create: `cli/progress.go`, `cli/await_progress.go`
- Modify: `cli/await_idle.go` (shared list/kill now carry `Condition`; add a condition filter)
- Modify: `cli/list.go` (`taskLine` :180, `taskJSON` :382–442, `newTaskJSON` :489)
- Modify: `cli/verb/table.go` (new entries beside `notify` :1578 and `session await-idle` :2325), then `go generate ./cli/verb`
- Modify: `cmd/harness-cli/` dispatch for the new Actions (follow how `NotifyAction` / `SessionAction` `await-idle` are dispatched)
- Test: `cli/list_test.go` (or the existing `taskLine` test file), `cli/verb/*_test.go` (the invariant tests run automatically)

**Interfaces:**
- Produces: `func (c *Client) ProgressSet(ctx context.Context, text string, clear bool) (*protocol.ProgressSetResponse, error)` and `func ProgressSet(ctx context.Context, peerCID objproto.ConnectionID, text string, clear bool) (*protocol.ProgressSetResponse, error)`.
- Produces: `func (c *Client) AwaitProgress(ctx context.Context, taskIDHex string, since uint64, sink protocol.AwaitSink, topic string) (*protocol.AwaitProgressResponse, error)`, `func AwaitProgressStatusString(s protocol.AwaitProgressStatus) string`.
- Produces: `func FilterWatchers(ws []protocol.AwaitIdleWatcherInfo, c protocol.AwaitCondition) []protocol.AwaitIdleWatcherInfo`.
- Produces: `func ProgressStr(t *protocol.TaskInfo) string`, which returns `progress="…" (12m ago)` or `""` when `ProgressAt == 0`. It is shared with the TUI, like `ActivityStr`.
- Verb table entries:
  - `progress`: Path `["progress"]`, Action `ProgressAction`, CmdlineSurfaces `CLI`. Trailing `text`, `Required: false`, with the Reason "the progress line is free-form". Flag `--clear` (bool). Validation: exactly one of text / `--clear`. NoModalSurface: "an operator connection has no principal; the server would refuse every write (spec D7)".
  - `await-progress`: Path `["await-progress"]`, CmdlineSurfaces `CLI | TUI | WebUI`. Arg `task-id`. Flags `--since` (uint, `"progress_at from ls --json; fire at once if the task has written since"`), `--notify`, `--topic`, with `Exclusive notify/topic`.
  - `await-progress ls` / `await-progress kill`: the same shape as `session await-idle ls/kill`. `ls` filters to `AwaitCondition_Progress`, and `session await-idle ls` filters to `Idle`. `kill` accepts any id.

- [ ] **Step 1: Write the failing tests.**
  - `TestTaskLine_ProgressAndTitle`: `ProgressAt != 0` and Progress `"stage 2"`, age 720000ms, give `progress="stage 2" (12m ago)`. `LiveTerminal == true` with empty Title gives `title=""`. `LiveTerminal == false` gives no `title=`.
  - `TestTaskJSON_ProgressFields`: the keys `progress`, `progress_at`, `progress_age_ms`, `live_terminal`, `title` are present even when zero.
  - `TestFilterWatchers`.
- [ ] **Step 2: Run `go test ./cli -run 'TaskLine_Progress|TaskJSON_Progress|FilterWatchers' -v`.** Expected: FAIL.
- [ ] **Step 3: Implement the helpers, `ls` rendering, verb entries, regen, and dispatch.** Text output of an arm and a fire follows `session await-idle`'s existing printing, with `watcher_id`, plus `progress=`.
- [ ] **Step 4: Run `go generate ./cli/verb && go test ./cli/... && make check && make wasm-check`.** Expected: PASS. The `cli/verb` invariant / surface-notes / flag-order tests must stay green. If one fails, it is pointing at a missing declaration; fix the declaration, not the test.
- [ ] **Step 5: Commit** `cli: progress, await-progress, and progress/title in ls`.

### Task 7: TUI (D1, D21)

**Files:**
- Modify: `tui/tasks.go:300-323` + `tui/columns.go` (the progress/title column), `tui/detail.go:207-213`, `tui/app.go:496-533` (event patch copies the new fields), `tui/actions.go:325` (arm key), `tui/idlewatchersmodal.go` (condition column), `tui/keys.go` (key binding), `tui/cmdline.go` (if the verb table does not already route `await-progress`)
- Test: the existing tests in `tui/` for these files

**Interfaces:**
- Consumes: `cli.ProgressStr`, `cli.Client.AwaitProgress` (call it on `a.client`, the long-lived client — never a fresh dial), `cli.FilterWatchers`.

- [ ] **Step 1: Write the failing tests.**
  - The table cell shows the progress text when `ProgressAt != 0`, and otherwise the title rendered with the dim style. A long value is truncated to the column width.
  - `applyEvent*` copies `Progress` / `ProgressAt` / `ProgressAgeMs` / `Title` / `LiveTerminal` from a `TaskStatusEvent` into the row.
  - The detail popup shows `progress:` with its age next to `last output:`, and a `title:` line.
  - The watcher modal renders a condition column.
- [ ] **Step 2: Run `go test ./tui -run <names> -v`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Pick an unused key next to `w` (check `tui/keys.go` and the help text). The arm sends since = the selected row's `ProgressAt`, so a write the operator has not seen yet fires at once.
- [ ] **Step 4: Run `go test ./tui`.** Expected: PASS.
- [ ] **Step 5: Commit** `tui: progress/title column, detail lines, await-progress key`.

### Task 8: WebUI + wasm bridge (D1, D21)

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (task object ~:1385–1428 gains `progress`, `progressAt`, `progressAgeMs`, `liveTerminal`, `title`; event bridge ~:3088–3112; `awaitProgress` beside `awaitIdle` :3157–3198; `idle_watchers` :1555 gains `condition`)
- Modify: `webui/static/main.js` (`renderTaskList` :5256, badge area :5312; task sheet; `runCmd` routing comes from the verb table via `WebUIDispatch{Fn: "awaitProgress"}` set in Task 6), `webui/index.html` (await-progress button beside `#await-idle-btn`; condition column in `#await-idle-list`)
- Test: `webui/static/cmd_test.mjs` (command routing), `make js-test`

- [ ] **Step 1: Write the failing JS test.** In `cmd_test.mjs`, `await-progress <id> --since 5` routes to `awaitProgress` with `{since: 5}`.
- [ ] **Step 2: Run `make js-test`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Render text with the page's existing text-escaping path (`textContent`, never `innerHTML`). The row shows progress with its age on a second line, then the title in a muted style when `liveTerminal`.
- [ ] **Step 4: Run `make js-test && make wasm-check`.** Expected: PASS.
- [ ] **Step 5: Use the `webui/INDEX.md` Playwright procedure to screenshot a list with one progress row and one title-only row at 1280 and 390 widths.** Report the screenshot paths; they are deliverables.
- [ ] **Step 6: Commit** `webui: progress/title in the task list, await-progress`.

### Task 9: Agent skill text and end-to-end check

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md` (new section after notify :779–800), `runner/agentskills/supervising-workers/SKILL.md` (beside `last_output_at` :210–255)
- Copy the result byte-identically to `.claude/skills/harness-cli/SKILL.md` and `.agents/skills/harness-cli/SKILL.md`
- Test: `go test ./runner/agentskills`

The skill content comes from spec § Agent skill: when to write (the task or stage changes), what (one line naming task and stage), who sees it (the operator and tasks that can see yours), `--clear`, and that frequency is the agent's call. The supervising section covers `await-progress --since <progress_at>` and the `ls --json` fields.

- [ ] **Step 1: Write both skill sections and copy them.** Then `diff runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md` and the same for `.agents/` should print nothing.
- [ ] **Step 2: Run `go test ./runner/agentskills`.** Expected: PASS.
- [ ] **Step 3: Run the end-to-end check with `scripts/dummy-harness.sh`** (load the `dummy-harness` skill first). Spawn an interactive task, run the following from inside it in the help text's exact spelling, and observe the output from an operator shell. Then take the dummy harness down.
  - `harness-cli progress task A / stage 1`: `ls` shows `progress="task A / stage 1"`.
  - `harness-cli progress --clear`: `progress=""` with a newer age.
  - `HARNESS_TASK_ID=<other> harness-cli notify x`: the notification names the real task.
  - From a second task, `harness-cli await-progress --since <progress_at> <first>` fires when the first task writes.
  - Shell into the session with `printf '\033]0;my title\007'`: `ls` shows `title="my title"`.
- [ ] **Step 4: Run `scripts/wire-skew-check.sh`.** Expected: exit 0. The skew is exercised and fails recoverably.
- [ ] **Step 5: Run `make check && make vet && make test && make wasm-check`.** Expected: PASS.
- [ ] **Step 6: Commit** `skills: progress and await-progress`.
