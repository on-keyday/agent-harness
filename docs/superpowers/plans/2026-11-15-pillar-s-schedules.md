# Pillar S — schedules — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `schedule new/ls/show/edit/run/rm` let a human or an agent holding the
new `schedule` bit fire background runs of one task at the times a cron
expression names, persisted in `<data-dir>/schedules.json`, at most once per
firing, on the CLI, TUI and WebUI.

**Architecture:**
- **Server:** a `ScheduleStore` (the file, CRUD, history, settling `pending`
  intents) and a firing loop on `Server` (one timer on the earliest `next_at`,
  a start-up pass, the firing function). Firings call `TaskStore.Resume` /
  a create-with-id the same way `openBackground` does. Six task-control verbs.
- **Wire:** client↔server only. No runner-decoded format changes.
- **Clients:** CLI verbs from `cli/verb/table.go` rows; a third TUI table; a
  WebUI dialog and Compose fields, through wasm bridge calls that reuse the
  CLI client.

**Tech Stack:** Go (server, runner/protocol, cli, tui, wasm), `.bgn` →
`make protoregen`, JS (webui). No new module: the cron parser is
self-written (spec S-D2, amended 2026-11-15).

**Spec:** `docs/superpowers/specs/2026-11-15-pillar-s-schedules-design.md`
(facts F1–F10, decisions S-D1–S-D22, Design, Testing), inside
`docs/superpowers/specs/2026-11-12-run-modes-and-records-grand-design.md`.

## Global Constraints

- **Before starting:** read `.claude/skills/implementation-pitfalls/SKILL.md`.
- **Where to work:** this worktree, its `harness/<dir>` branch. The spec is
  already committed there; it lands with this code.
- **Dependency:** none may be added. The cron parser is self-written
  (S-D2); `robfig/cron` was dropped (F1, F11).
- **`parseCron` is the only way an expression becomes a `cronExpr`**
  (S-D2): every path that reads one — create, edit, server start — calls it.
- **Wire:** every `.bgn` change is in Task 1 and only there; enum values are
  appended; `make protoregen`. No runner-decoded format changes, so the
  wire-skew check is expected to pass trivially; run it anyway.
- **Datagram size:** every control message is one datagram. Request fields
  are fixed-size or bounded: cron expression ≤ 256 bytes, zone name ≤ 64
  bytes, agent name and selector as `OpenInteractiveRequest` bounds them.
  The prompt travels on a payload stream (`readOpenPrompt`,
  `server/background_open.go:163`, ≤ `protocol.PromptMax`). `schedule ls` and
  `schedule show` answer on a send stream, as `ls` does.
- **Capabilities:** `schedule = 0x20000000`, `all = 0x3fffffff`. Same
  touch-list as pillar B's `transcript_read` (plan
  `2026-11-14-pillar-b-transcripts.md`, Global Constraints): `.bgn` (bit,
  `all`, the `all` comment's list), `runner/protocol/capability_test.go`,
  `cli/verb/caps.go` (ordered list, `CapDescription` naming the six verbs and
  the S-D15 unbinding, tier **TierStrong**: a schedule keeps starting runs
  after its creator is gone), `cli/verb/caps_audit_table_test.go`,
  `server/capabilities.go`, `server/cap_completeness_test.go`,
  `server/scope_percap_completeness_test.go`, and the skills listing caps.
  After this bit, 2 bits remain: note in the memory file that the next
  proposed bit triggers the u64 widening (F8).
- **Names:** `Schedule…` types; never `Scheduler` (F10).
- **Times:** stored as UTC instants (`time.Time` marshalled RFC 3339 with
  nanoseconds); the zone is applied only in `Next` and in display.
- **Tests that start a server or runner:** `env -u HARNESS_SERVER_PUBKEY -u
  HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`, and
  `GOTMPDIR`/`TMPDIR=$HOME/.cache/claude-gotmp` (never `/tmp`).
- **Live checks:** `dummy-harness` skill, every surface (CLI, TUI in a tmux
  PTY, WebUI in Playwright); screenshots kept under `a1-shots/` and reported.

## Review Focus

1. **A schedule whose runner is away for days**: its one run sits queued, every
   firing after it is `skipped_busy`, and 50 skips roll the `fired` entry out of
   the history. When the run finally starts and ends, completing the evicted
   entry must be a no-op, never a panic or a write into another entry. → Task 3
   test `TestCompletionOfEvictedEntryIsNoop`.
2. **`schedule edit --cron` while a run is in progress**: `next_at` follows the
   new expression from now, and the run in progress keeps its conditions and
   is not cancelled (GD G30). → Task 4 test `TestEditCronDuringRun`.
3. **A zone that stops loading** (a hand-edited file, or a host whose tz
   database lacks the name): the server still starts; that schedule's firings
   are `skipped_refused` naming the zone; the others fire. → Task 2 test
   `TestUnloadableZoneDoesNotBlockStart`, Task 3 test
   `TestUnloadableZoneSkipsRefused`.
4. **A daylight-saving zone** (`America/New_York`, `30 2 * * *` across the
   March gap and the November repeat): fires once on each day, never zero or
   twice. → Task 0 test `TestNextAcrossDST`.
5. **An oversize or missing prompt stream on `schedule new` / `edit`**:
   refused with a message, no schedule created or changed, the stream
   cancelled. → Task 4 test `TestCreatePromptTooLarge`.

---

### Task 0: The dependency and `parseCron`

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `server/schedule_cron.go`, `server/schedule_cron_test.go`

**Interfaces:**
- Produces:

```go
// parseCron is the only way an expression becomes a schedule (S-D2).
type cronExpr struct { minute, hour, dom, month, dow uint64; domStar, dowStar bool }
func parseCron(expr string) (cronExpr, error)
// nextWall: the smallest whole minute strictly after `after` (wall-clock
// fields carried in UTC) that matches; zero if none within five years.
func (c cronExpr) nextWall(after time.Time) time.Time
// nextFiring maps nextWall to an instant in loc (S-D3a); zero means none.
func nextFiring(c cronExpr, loc *time.Location, after time.Time) time.Time
// loadZone resolves "" to time.Local's name and refuses a name that does not load.
func loadZone(name string) (*time.Location, string, error)
```

- [ ] **Step 1: Failing tests.**
  - `TestParseCronRefuses`: `"TZ="`, `"CRON_TZ=UTC 0 0 * * *"`,
    `" TZ=Asia/Tokyo 0 9 * * *"`, `"@every 1h"`, `"0 0 30 2 *"`, `"60 * * * *"`,
    `""` each return an error; none panics.
  - `TestParseCronAccepts`: `"0 9 * * 1-5"`, `"*/30 * * * *"`, `"@daily"`,
    `"0 9 13 * 5"`.
  - `TestNextFiringOrRule`: `0 9 13 * 5` from 2026-11-15 08:59 Asia/Tokyo →
    2026-11-20 00:00 UTC (F3).
  - `TestNextAcrossDST` (Review Focus 4): `30 2 * * *` in America/New_York,
    walked day by day from 2027-03-12 to 2027-03-16 and 2027-11-05 to
    2027-11-09: exactly one firing per calendar day.
  - `FuzzParseCron`: arbitrary input, no panic (seed with the strings above).
- [ ] **Step 2: Run** `go test ./server -run 'ParseCron|NextFiring|NextAcross'` → FAIL (undefined).
- [ ] **Step 3: Implement.** `go get github.com/robfig/cron/v3@v3.0.1`. The
  guard order is S-D2's: trim, refuse a `TZ=`/`CRON_TZ=` prefix, refuse a
  `@every` prefix, `ParseStandard` inside a `recover`, refuse a zero `Next`
  from now.
- [ ] **Step 4: Run** the tests, then `go test ./server -run '^$' -fuzz
  FuzzParseCron -fuzztime 60s` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "server: parseCron and nextFiring, self-written (pillar S)"`

> Amended 2026-11-15 during execution: the library was dropped (spec S-D2,
> F11). The controller's instruction to the Task 0 implementer replaced
> Steps 1–4 with the S-D2 grammar, a table test, a brute-force comparison of
> `nextWall` against minute-by-minute stepping, the S-D3a DST test and the
> fuzz test; the ledger records it as Ruling R5.

---

### Task 1: Wire, the `schedule` bit, `ClientKind.schedule`

**Files:**
- Modify: `runner/protocol/message.bgn`, regenerate `message.go`
- Modify: the capability touch-list (Global Constraints)
- Modify: the `ClientHello` handling that reads `kind` (refuse `schedule`, S-D18)
- Test: `runner/protocol/schedule_wire_test.go`, the capability tests,
  `server/client_hello_schedule_test.go`

**Interfaces:**
- Produces (`.bgn`, names exact):
  - `ScheduleID { id :[16]u8 }`.
  - `ClientKind.schedule` (appended).
  - `TaskControlKind`: `schedule_create`, `schedule_edit`, `schedule_delete`,
    `schedule_run`, `schedule_list`, `schedule_show` (appended), with arms.
  - `ScheduleSpec` — what create and edit carry, every field with a presence
    bit so an edit changes only what it names (checklist item 25): cron, tz,
    repo, task (bind), agent, selector, extra args, resume_conversation, the
    run spec fields (`BackgroundConditions`, record, oneshot, pty choice),
    the authority request block `OpenInteractiveRequest` uses, and
    `prompt_stream_id :u64` (0 = no prompt in this request).
  - `ScheduleCreateRequest { spec :ScheduleSpec }`,
    `ScheduleEditRequest { id :ScheduleID; spec :ScheduleSpec }`,
    `ScheduleIDRequest { id :ScheduleID }` (delete, run, show),
    `ScheduleListRequest {}`.
  - `enum ScheduleStatus :u8`: `ok`, `no_such_schedule`, `invalid_cron`,
    `invalid_zone`, `no_candidate`, `deadline_required`, `prompt_required`,
    `profile_required`, `profile_unavailable`,
    `task_not_reachable`, `task_running_foreground`, `self_refused`,
    `spawn_limit`, `prompt_too_large`, `internal_error`, then (Task 4, Ruling
    R16) `repo_required`, `repo_conflict`, `scope_not_permitted`,
    `record_not_permitted`, `turn_end_unsupported`, `task_already_bound`,
    `not_ready`. (`stream_refused` was in the first list and is removed.)
  - `ScheduleResponse { status; message_len :u16; message; id :ScheduleID;
    stream_id :u64 }` (stream_id non-zero for list/show).
  - Stream bodies: `ScheduleRow` (everything `schedule ls` prints: id, cron,
    tz, next_at, task state + task id + its status, last history entry,
    agent, repo, prompt head ≤ 120 bytes or a `prompt_hidden` bit) and
    `ScheduleDetail` (the row plus the full prompt or hidden, extra args, run
    spec, grant, creator, created/edited, and `count :u8; history
    :[count]ScheduleHistoryEntry`).
  - `enum ScheduleOutcome :u8` (S-D7, S-D22): `pending`, `lost`, `fired`,
    `run_now`, `skipped_busy`, `skipped_no_task`, `skipped_task_gone`,
    `skipped_refused`, `resumed_by`, `unbound`.
  - `ScheduleHistoryEntry { outcome; due_at; at; queued_at; started_at;
    ended_at; end_reason :EndReason; by_kind :ClientKind; by :TaskID;
    message_len :u16; message }` (times u64 unix nanos, 0 = absent).
  - `TaskInfo` and `TaskStatusEvent`: `schedule_present :u1` (from reserved
    bits) + `if schedule_present == 1: schedule_id :ScheduleID`.
  - `Capability.schedule = 0x20000000`, `all = 0x3fffffff`.

- [ ] **Step 1: Failing tests.**
  - `TestScheduleDetailRoundTrip`: a detail with 3 history entries (one
    `pending`, one `fired` with all times, one `skipped_refused` with a
    message) round-trips equal.
  - `TestScheduleSpecPresenceBits`: a spec with only `cron` present decodes
    with every other presence bit 0.
  - `TestTaskInfoScheduleID`: present and absent round-trip.
  - `TestClientHelloRefusesScheduleKind` (server).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement**, `make protoregen`, walk
  the touch-list. **Step 4: Run** `go test ./runner/protocol ./cli/verb
  ./server -run 'Schedule|Cap|Scope|ClientHello'` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "protocol: schedule verbs, the schedule bit, ClientKind.schedule"`

---

### Task 2: `ScheduleStore`

**Files:**
- Create: `server/schedule_store.go`, `server/schedule_store_test.go`
- Modify: `cmd/harness-server/main.go` (load `<data-dir>/schedules.json` as
  runner keys are loaded, `:143`; refuse to start on error), `server/server.go`
  (`Config.Schedules *ScheduleStore`; an in-memory store when nil)

**Interfaces:**
- Consumes: Task 0's `loadZone`, `parseCron`, `nextFiring`.
- Produces:

```go
type TaskState uint8 // TaskStatePendingCreate, TaskStateBound, TaskStateUnbound (S-D21)

type Schedule struct {
	ID          string // 32 hex
	Cron, TZ    string
	State       TaskState
	TaskID      string // bound task, or the pre-chosen id of a pending creation
	Repo, Agent string
	Selector    protocol.RunnerSelector
	Prompt      string
	ExtraArgs   []string
	ResumeConversation bool
	Run         RunSpec          // Mode is always Background
	Grant       authority.Grant
	Creator     string           // task hex, "" = operator
	CreatedAt, EditedAt time.Time
	EditedBy    string
	NextAt      time.Time
	History     []HistoryEntry   // oldest first, len ≤ historyMax
}

const historyMax = 50

type HistoryEntry struct {
	Outcome   protocol.ScheduleOutcome
	DueAt, At time.Time
	ExpectRun int    // S-D22: the task's Run after this firing's resume
	TaskID    string // the task the entry is about (bound or pre-chosen)
	QueuedAt, StartedAt, EndedAt time.Time
	EndReason protocol.EndReason
	ByKind    protocol.ClientKind
	By        string
	Message   string
}

func LoadScheduleStore(path string) (*ScheduleStore, error) // "" = memory only
func (st *ScheduleStore) Get(id string) (Schedule, bool)
func (st *ScheduleStore) List() []Schedule
func (st *ScheduleStore) Put(s Schedule) error      // create or replace, then save
func (st *ScheduleStore) Delete(id string) error
func (st *ScheduleStore) Update(id string, f func(*Schedule) error) error // under the lock, then save
func (st *ScheduleStore) EarliestNext() (time.Time, bool)
// Settle resolves every Pending entry (S-D22) from the task store's view.
func (st *ScheduleStore) Settle(task func(id string) (run int, exists bool)) error
func (s *Schedule) appendHistory(e HistoryEntry) // evicts the oldest past historyMax
```

- The file is `{"version":1,"schedules":[…]}`, written to `path+".tmp"` with
  mode 0600, `fsync`ed, renamed, and the directory `fsync`ed.
- A zone that no longer loads is NOT a load error (Review Focus 3): the
  schedule loads; firing reports it (Task 3).

- [ ] **Step 1: Failing tests.**
  - `TestScheduleStoreRoundTrip`: Put two schedules with every field set,
    reload from the file, `reflect.DeepEqual` (times compared with `Equal`).
  - `TestScheduleStoreCorruptRefuses`: a truncated file → `LoadScheduleStore`
    errors; `version: 2` → errors naming the version.
  - `TestScheduleStoreFileMode`: 0600.
  - `TestHistoryEvictsOldest`: 51 appends keep the last 50 in order.
  - `TestSettlePending`: entries `pending` with (a) `ExpectRun` reached →
    `fired`, (b) not reached → `lost`, (c) a pending creation whose
    pre-chosen id exists → `fired` and State `Bound`, (d) whose id does not
    exist → `lost`, State stays `PendingCreate`.
  - `TestUnloadableZoneDoesNotBlockStart` (Review Focus 3): a file naming
    `Mars/Olympus` loads.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `go test ./server -run 'ScheduleStore|History|Settle|Unloadable'` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "server: ScheduleStore in schedules.json (S-D4, S-D7, S-D21, S-D22)"`

---

### Task 3: Firing

**Files:**
- Create: `server/schedule_fire.go`, `server/schedule_fire_test.go`
- Modify: `server/spawn_limit.go` (`CreateWithinLimitsID`, the same checks
  under a given id; `CreateWithinLimits` calls it with `newTaskID()`), a
  dry-run `CheckSpawnLimits` used by Task 4; `server/server.go` (start the
  loop next to `watchHeldDeadlines`, `:986`, after WAL replay)

**Interfaces:**
- Consumes: Task 2's store, Task 1's outcome enum and `ClientKind_Schedule`.
- Produces:

```go
func (s *Server) fireSchedule(id string, due time.Time, manual bool) protocol.ScheduleOutcome
func (s *Server) runSchedules(ctx context.Context)   // the timer loop (S-D5)
func (s *Server) startSchedules()                     // Settle, then missed-once (S-D9)
func (s *Server) scheduleTaskEvent(ev protocol.TaskStatusEvent) // history completion, resumed_by
func (st *TaskStore) CreateWithinLimitsID(id, repo, prompt string, kind protocol.TaskKind, origin protocol.ClientKind, creator protocol.TaskID, selector protocol.RunnerSelector, extraArgs []string, g authority.Grant, agentProfile string, run RunSpec) error
func (st *TaskStore) CheckSpawnLimits(creator protocol.TaskID) error
```

- `fireSchedule` follows the spec's Firing steps 1–6 exactly. The resume
  uses `ResumedByKind = ClientKind_Schedule`; a creation uses
  `origin = ClientKind_Schedule` and `creator` = the schedule's creator.
  `GrantUpdate` sets every field from `Schedule.Grant`.
- `runSchedules` arms one timer for `min(EarliestNext − now, 1h)` and is
  poked (a buffered channel) by every store change.
- `scheduleTaskEvent` is fed from the task status events the server already
  publishes. For a task bound to a schedule: assigned → `StartedAt`; ended →
  `EndedAt`, `EndReason`, on the newest entry with that `TaskID` and outcome
  `fired`/`run_now` and no `EndedAt`; a resume whose `ResumedByKind` is not
  `schedule` → append `resumed_by`.

- [ ] **Step 1: Failing tests** (fake clock injected into the loop and the
  firing function):
  - `TestFireOutcomes`: one sub-test per outcome of Firing steps 1–3 and 5–6,
    from a store and task store in the matching state, asserting the history
    entry and that `next_at` advanced (or not, for `manual`).
  - `TestFireConcurrentOnce`: the timer and `manual` firing the same schedule
    at once → one run, one `skipped_busy`.
  - `TestFirstFiringCreatesUnderChosenID`: the created task has the
    schedule's grant, run spec, origin `schedule`; State becomes `Bound`.
  - `TestRefusedCreationRetries` (S-D21): spawn limit refuses → entry
    `skipped_refused`, State `PendingCreate`; limit lifted → next firing creates.
  - `TestUnboundBeforeFirstFiring`: `unbound` with no history → `skipped_no_task`,
    no task created.
  - `TestMissedFiresOnce` (S-D9): `NextAt` two days ago → one firing with that
    `DueAt`, new `NextAt` in the future.
  - `TestCrashBetweenIntentAndResume` (S-D22): write the intent, do not
    resume, reload → `lost`, no firing on start.
  - `TestCrashAfterResume`: intent + resume, no outcome rewrite, reload, replay
    (the run becomes `Failed server_restart`) → entry `fired` with
    `EndReason` failed, no second run.
  - `TestUnloadableZoneSkipsRefused` (Review Focus 3).
  - `TestCompletionOfEvictedEntryIsNoop` (Review Focus 1).
  - `TestResumedByRecorded` (S-D10): a CLI resume of the bound task appends
    `resumed_by` with kind `cli`; its wait is not in the statistics.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `env -u … go test ./server -run 'Fire|Missed|Crash|Unbound|Refused|Resumed|Evicted|Unloadable'` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "server: schedule firing, at most once (S-D5–S-D9, S-D22)"`

---

### Task 4: Server verbs and the binding rules

**Files:**
- Create: `server/schedule_handler.go`, `server/schedule_handler_test.go`
- Modify: `server/task_handler.go` (dispatch the six kinds),
  `server/capabilities.go` (`requiredCap`: create/edit/delete/run →
  `Capability_Schedule`; list/show → none), `server/background_open.go`
  (extract `checkBackgroundRun(...)`, the deadline / prompt / profile /
  conditions / candidate checks, so `openBackground` and `schedule new`
  share it — S-D12), the foreground-resume and `session mode` paths (S-D15),
  the `TaskInfo` / `TaskStatusEvent` builders (`schedule_id`)

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: `func (h *TaskHandler) handleSchedule(cid string, conn ConnHandle, req *protocol.TaskControlRequest)`; `func checkBackgroundRun(...) (protocol.OpenInteractiveStatus, string)` with `openBackground` calling it.
- Rules, each one test:
  - **Reach** (S-D17): the target is the bound task, else the creator task;
    out of reach → `no_such_schedule` (not `PermissionDenied`, so ids are not
    probed). `--task T` out of reach → `task_not_reachable`.
  - **Self** (S-D20): caller == bound task → `self_refused` for edit, run,
    delete, rebind; the S-D15 unbinding by the bound task itself is refused
    too.
  - **Authority** (S-D16): create — `grantFor(g, r, "")`, or
    `grantFor(g, r, T)` with `--task T`; every edit recomputes with the editor.
  - **Bind** (S-D14): T running in foreground → `task_running_foreground`;
    otherwise the task's mode is written background (the existing mode write,
    `task_mode_changed`); `--repo` with `--task` → refused.
  - **Unbind** (S-D15): a foreground resume or `session mode … foreground`
    of a bound task needs `schedule` too; it sets State `Unbound` and appends
    `unbound` with the caller and time.
  - **Create checks** (S-D12): `checkBackgroundRun`, plus `CheckSpawnLimits`
    when no `--task`. (`--stream` cannot be expressed: `ScheduleSpec` has no
    stream field, so `stream_refused` was removed and the CLI does not offer
    `--stream` on schedule verbs — Ruling R16.)

- [ ] **Step 1: Failing tests:** `TestScheduleNeedsBit`, `TestScheduleOutOfReachIsNoSuch`,
  `TestScheduleSelfRefused` (each verb, the caller holding `schedule` with
  itself in scope), `TestEditRecomputesGrantWithoutFlags` (an editor narrower
  than the creator, no authority flags → stored grant ⊆ editor's),
  `TestBindRunningForegroundRefused`, `TestBindSetsBackground`,
  `TestForegroundResumeOfBoundTaskNeedsBitAndUnbinds`,
  `TestCreateSharesBackgroundChecks` (missing deadline, missing prompt,
  profile no candidate offers, no candidate, `--stream` — each status),
  `TestEditCronDuringRun` (Review Focus 2), `TestDeleteLeavesTask` (S-D19: the
  task and a run in progress are untouched), `TestCreatePromptTooLarge`
  (Review Focus 5), `TestTaskInfoCarriesScheduleID`, `TestListHidesPrompt`
  (a caller that could not read the task's prompt in `ls` gets
  `prompt_hidden`), and the existing `openBackground` tests unchanged.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `env -u … go test ./server` → PASS (the whole package: the extraction
  touches `openBackground`).
- [ ] **Step 5: Commit.** `git commit -m "server: schedule verbs, reach and self rules, binding (S-D12–S-D20)"`

---

### Task 5: CLI

**Files:**
- Create: `cli/schedule.go`, `cli/schedule_test.go`
- Modify: `cli/verb/spawn.go` (a `spawnSchedule` kind: `spawnFlags` minus
  `resume`, `exact`, `x11`, `x11-display`, `detach`, `stream`, `foreground`,
  `background`, `ip`, `host` stays; plus `--cron`, `--tz`, `--task`),
  `cli/verb/table.go` (six rows, `CmdlineSurfaces: CLI | TUI | WebUI`,
  `WebUIDispatch` per row), `cmd/harness-cli/` dispatch, `cli/list.go`
  (`schedule=<id8>` on a bound task's row, `from=schedule`, `ls --json`
  `schedule_id`)

**Interfaces:**
- Produces:

```go
func (c *Client) ScheduleCreate(ctx context.Context, spec ScheduleSpecOpts) (string, error)
func (c *Client) ScheduleEdit(ctx context.Context, id string, spec ScheduleSpecOpts) error
func (c *Client) ScheduleDelete(ctx context.Context, id string) error
func (c *Client) ScheduleRun(ctx context.Context, id string) error
func (c *Client) ScheduleList(ctx context.Context) ([]protocol.ScheduleRow, error)
func (c *Client) ScheduleShow(ctx context.Context, id string) (protocol.ScheduleDetail, error)
func ScheduleRowLine(r protocol.ScheduleRow, now time.Time) string
func ScheduleDetailText(d protocol.ScheduleDetail, now time.Time) string // includes WaitStats
func WaitStats(h []protocol.ScheduleHistoryEntry) (n int, median, max time.Duration) // S-D11
type ScheduleSpecOpts struct { /* every ScheduleSpec field + its presence */ }
```

- `ScheduleRowLine`: `<id8> <cron> <tz>  next=<local time with zone>  last=<outcome> <age>  task=<id8>:<status>|created-at-first-firing|no-task  <agent> <repo>  <prompt head>`. No field is elided at zero (checklist 31): `last=none` before any history.
- `ScheduleSpecOpts` is filled by ONE builder from the bound flags (checklist
  28a), used by the CLI, the TUI and the wasm bridge.

- [ ] **Step 1: Failing tests:** `TestScheduleRowLine` (each task state),
  `TestWaitStatsExcludesResumedBy` (S-D10/S-D11), `TestScheduleFlagsAreSpawnFlags`
  (every run and authority flag of `session new --background` is accepted by
  `schedule new` and `edit` with the same `Flag` row), `TestScheduleEditOnlyPresent`
  (an edit naming only `--cron` sends only that presence bit),
  `TestLsShowsSchedule`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `go test ./cli/...` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "cli: schedule new/ls/show/edit/run/rm"`

---

### Task 6: TUI

**Files:**
- Create: `tui/schedules.go` (`SchedulesModel`, a table with `ID`, `Cron`,
  `Next`, `Last`, `Task`, `Agent`, `Repo`, `About`; About is the flex
  column; path cells through `truncateLeft`), `tui/schedules_test.go`
- Modify: `tui/app.go` (`focusSchedules` after `focusTasks` in the cycle;
  `shownTable` and `tableStrip` name three tables: `[Tasks] Runners
  Schedules (Shift-Tab: runners, Tab: …)` — the hint names the real keys from
  each table), `tui/keys.go` (schedule-scope keys: `d` detail, `r` run now,
  `x` delete with a confirm), `tui/detail.go` (`ScheduleDetailText` in the
  popup; the task popup names its schedule), `tui/client.go`
  (`DoScheduleList`, refreshed with the snapshot), `tui/dispatch.go` (the six
  verbs through `ScheduleSpecOpts`'s builder)

- [ ] **Step 1: Failing tests:** `TestFocusCycleIncludesSchedules`,
  `TestStripNamesThreeTables` (the hint from each table is the key that
  works), `TestSchedulesTableRows`, `TestScheduleRunNowKey`,
  `TestScheduleDeleteConfirms`, `TestTaskDetailNamesSchedule`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `go test ./tui` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "tui: a Schedules table"`

---

### Task 7: WebUI

**Files:**
- Create: `cmd/harness-webui-wasm/schedule.go` (`scheduleCreate`,
  `scheduleEdit`, `scheduleDelete`, `scheduleRun`, `scheduleList`,
  `scheduleShow`, over Task 5's client and `ScheduleSpecOpts` builder),
  `webui/static/schedule_test.mjs`
- Modify: `cmd/harness-webui-wasm/main.go` (register), `webui/index.html`
  (a Schedules button opening a `<dialog class="picker-modal">`; Compose
  gains a cron input and a zone select prefilled with the server's zone),
  `webui/static/task_models.js` (`scheduleRowModel`, `scheduleDetailModel`),
  `webui/static/main.js` (the dialog: rows, a detail view with history and
  wait statistics, Run now and Delete buttons; Compose: a filled cron turns
  Submit into `scheduleCreate` with the form's background fields; the task
  row and sheet name the bound schedule), `Makefile` (`js-test` list)

- [ ] **Step 1: Failing tests:** the bridge exports the six calls (checked on
  `page.bridge`); `scheduleRowModel` for each task state;
  `scheduleDetailModel` history rows and wait statistics; Compose with a cron
  calls `scheduleCreate`, not `openSession`; `schedule …` typed on the
  command line dispatches.
- [ ] **Step 2: Run** `make js-test` → FAIL. **Step 3: Implement.**
  **Step 4: Run** `make js-test && make wasm-check` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "webui: schedules dialog and Compose cron"`

---

### Task 8: Docs, parity walk, live checks, landing

**Files:**
- Modify: `README.md` (a schedules section and the capability list),
  `runner/agentskills/supervising-workers/SKILL.md` and its mirrors (the
  verbs, the bit, S-D20's self rule), `.claude/skills/surface-parity-checklist/firing-log.md`,
  the memory file on capability widening (2 bits left)

- [ ] **Step 1:** Walk `surface-parity-checklist` items 1–39 for the verb
  family, the `schedule` bit and the task's `schedule_id`, one verdict per
  number; append the entry to the firing log.
- [ ] **Step 2: Live** (`dummy-harness`, `--agent bash` — the fake profile
  answered `profile_unavailable` on 2026-11-15):
  - `schedule new --cron '* * * * *' --repo <dummy repo> --agent bash
    --prompt 'echo tick' --deadline 2m` → within a minute `schedule show`
    has `fired` with a wait, the task exists with `from=schedule`;
  - a second minute with the run still going → `skipped_busy`;
  - `schedule run` → `run_now`; `schedule edit --cron '*/5 * * * *'` → `next`
    moves; a CLI foreground resume of the task without the bit → refused;
  - stop the dummy server across two firing minutes of a `* * * * *`
    schedule whose task has ended, start it again → exactly one `fired`,
    carrying the first missed `due_at` (S-D9). (The crash between intent and
    outcome cannot be timed by hand; Task 3's tests cover it.)
  - the TUI Schedules table and detail in a tmux PTY (120x40 and 80x24); the
    WebUI dialog and a Compose-created schedule in Playwright; screenshots to
    `a1-shots/`.
- [ ] **Step 3:** `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u
  HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`, `make wasm-check`,
  `scripts/wire-skew-check.sh` → all green.
- [ ] **Step 4:** Land per the landing policy: spec, this plan and the code
  together, FF-push, then `make build` in the main checkout. Only server-side
  and client paths changed: restart the server; runners need no restart
  (memory: drift by path).
