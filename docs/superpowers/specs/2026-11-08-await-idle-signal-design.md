# await-idle on lifecycle and program status — design

Line references are to `main` at `4039949a`.

## Problem

- **P1 — `await-idle` watches one of three state signals.** The server has
  three readings of whether a session's program is busy:
  - PTY output quiescence (`SessionMux.lastOutput`, `server/session_mux.go`
    `ArmIdleWatcher`);
  - the agent hooks' **lifecycle** (`LifecycleState`, written by
    `lifecycle_set`, `server/lifecycle.go`);
  - the program's own **OSC 7501 report** (`ProgramState` / `ProgramKind`,
    read from the screen model by `SessionMux.Program()`).

  `session await-idle` fires on the first one only. The other two exist, are
  shown on every task list (lifecycle spec D28), and cannot be waited on.
- **P2 — quiescence is late by construction.** It fires `threshold` (default
  2500 ms, `server/await_idle_handler.go:19`) plus up to one 500 ms watch tick
  after the last byte (`idleWatchTick`). Lifecycle and program status change
  at the moment of the event. The operator notices the gap: lifecycle badges
  change "a little earlier", `await-idle` fires "a little later" (operator,
  2026-11-08).
- **P3 — a fire cannot say what kind of idle it found.** The supervising-workers
  skill (`runner/agentskills/supervising-workers/SKILL.md:259-262`) tells the
  waiter that a turn end, a permission prompt and a menu "all look the same at
  the byte level", and to `snapshot` after the fire to tell them apart. That
  costs a second call and the `exec_view` capability, while lifecycle
  (`waiting` vs `idle`) and program status (`blocked:permission` vs `done`)
  already tell them apart on the server.

Today's quiescence is not reported as a pain (operator, 2026-11-08: no
inconvenience so far). The motive is that the three signals the server holds
should all be usable by the one verb that waits for idleness.

## Decisions taken

**operator** = the human chose it in conversation (2026-11-08). **this spec** =
the author chose it while writing; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | `session await-idle` gains a choice of signal. **The default stays quiescence**; changing the default (e.g. to `any`) is left for later use to decide | operator |
| D2 | The choice is a flag on the existing verb, not a new verb. This reverses the shape the lifecycle spec sketched as its follow-up (`2026-10-27-task-lifecycle-design.md` D16: "`await-lifecycle`, shaped like `await-progress`", a row marked **this spec** there). What moved it: the operator asked for `await-idle` to work on either signal, and `any` (D4) only makes sense inside one verb | operator ("どっちでも await-idle できるようにすれば"), this spec (the D16 reversal) |
| D3 | Flag spelling: `--signal quiet\|lifecycle\|program\|any`. Wire: a new enum `AwaitIdleSignal` appended to `AwaitIdleRequest` | this spec |
| D4 | `any` fires on whichever of the three legs is satisfied first. Within one tick the legs are checked program → lifecycle → quiet, so a tick where several hold reports the most specific one | this spec |
| D5 | A leg is satisfied when its state is outside `working`: lifecycle ∈ {`waiting`, `idle`, `error`}; program ∈ {`idle`, `done`, `blocked`, `error`}. Which of these it was is reported (D8), not filtered on. A per-state `--until` is a non-goal: the reported state (D8) lets the waiter branch after one fire | this spec (proposed in conversation; the operator did not object, which is not assent) |
| D6 | **Stale-state rule.** A lifecycle or program state may still be the previous turn's when the watcher is armed right after input (`session send` then `await-idle`), which would fire at once. A non-working state therefore counts only if **either** (a) after the session's last input the same signal reported `working` and then the current non-working state, **or** (b) since the last input the session has produced no output for `threshold` (`now - max(lastInput, lastOutput) >= threshold`). (a) is the fast path: the input started a turn and the turn has ended. (a) requires the `working` because lifecycle hooks are async and `lifecycle_at` is the server's receipt time (`server/lifecycle.go:41`): turn N's late `Stop → idle` can land after the next `session send` and would otherwise satisfy "reported after the input" for a turn that has just begun. The `working` that follows the input is ordered after it by `reported_at` (lifecycle D10), and a late report older than it is dropped as stale. A turn too short for the 500 ms tick to see is still covered, because (a) reads stamps, not observed transitions (D7), and (b) catches anything (a) misses. With no input since the session started, (a) needs no `working`: there is no previous turn of this session to be late. (b) covers input that started nothing — text typed without Enter, cursor keys, a menu that opens no turn — where no new state will ever be reported; it falls back to quiescence timing. "The last input" is floored at the session's start (D18): with no input since the session started, a non-working state reported since the start fires at once, and a state reported before it does not count | operator (approved (a) 2026-11-08; raised the no-Enter case, approved (b); approved the `working` requirement in (a) after the builder check raised the late-hook race); the floor is D18 |
| D18 | **A state from before this session does not count.** Lifecycle lives on the task and is carried across a resume (`TaskStore` resets only `LifecycleReportedAt`, `server/taskstore.go:658`), while a resumed session gets a new mux. Without a floor, a watcher armed on a resumed session before the new claude's `SessionStart` would fire at once on the dead session's last state. D6's comparisons therefore use `max(lastInput, muxStart)`, `muxStart` being the mux's creation time (server clock). This mirrors the quiet leg, which does not fire before the session's first output (`ArmIdleWatcher` doc, `server/session_mux.go:1218`) | this spec (builder check) |
| D7 | D6 needs new server-clock stamps on `SessionMux`: `muxStart` (D18), `lastInput` (input bytes forwarded to the PTY, § Input), `programAt` (the last time the root 7501 record's state or kind changed, stamped in `recordFrame` where the screen is written) and `programWorkingAt` (the last time it changed TO `working`). Lifecycle already has `lifecycle_at` (server clock, `TaskStore.SetLifecycle`); the store gains `lifecycle_working_at`, the server-clock stamp of the last accepted `working` report, kept in memory beside `lifecycle_at` (not added to the progress snapshot: a restart makes a new mux, and D18's floor discards older stamps anyway). All are compared on the server clock | this spec |
| D8 | Every fire reports what it saw: `fired_by` (the leg that fired; for `--signal quiet` always `quiet`), and the lifecycle, program state and program kind at decision time. The reply-sink response, the board payload and the notify text all carry them. This answers P3 without a `snapshot` | this spec |
| D9 | Evaluation stays in the existing watcher goroutine (`ArmIdleWatcher`'s 500 ms tick), which gains a predicate per leg. No new event plumbing: program status has no change event at all (`SessionMux.Program()` is read on demand), and a tick bounds the fire latency at ~0.5 s against today's 2.5–3 s | this spec |
| D10 | **No refusal for a source that has not reported.** Arming `--signal lifecycle` or `--signal program` is accepted whatever the source has reported so far, and the watcher waits. A source that never reports leaves it armed until the session ends (`session_stopped`) or it is killed: an agent without hooks (lifecycle D5), any program on a Windows runner (lifecycle D29), a `--no-worktree` task. The watcher is visible in `await-idle ls` and removable with `kill`; `any` avoids the case. Reversal: the first draft refused with a new `signal_unavailable` status when the source had never reported. The builder check found that this also refuses a claude that has just been spawned and simply has not reported yet (`session new` then `await-idle`), with the outcome depending on timing. Deciding instead from whether the source *can* report (agent kind, worktree, runner OS) was weighed and rejected: it needs an agent-kind inference that can be wrong, for little gain | operator (chose to let it wait, 2026-11-08) |
| D11 | `threshold_ms` keeps its meaning for the quiet leg and is also the D6(b) window. One knob | this spec |
| D12 | `await-idle ls` shows each watcher's signal | this spec |
| D13 | Known gaps, accepted. (1) Lifecycle stays `working` after an Esc while the model is thinking (lifecycle D15): `--signal lifecycle` then waits for the next turn. (2) D6(b) misfires if, after an Enter that does start a turn, the PTY is silent for `threshold` before the program draws anything; claude's busy-side output gap was measured at ≤ ~0.5 s (`server/await_idle_handler.go:15-18`), but the gap right after Enter was not measured — the dummy-harness check measures it. `any` covers (1); (2) is the same exposure quiescence already has. (3) An agentboard wake is typed into the PTY by the runner (`runner/session.go:1264` `WakeStdin`), not through the mux, so it does not stamp `lastInput`: a lifecycle/program watcher armed right after `agent send` can fire at once on the previous turn's state. The supervising-workers skill already says not to arm `await-idle` for a peer you messaged (the reply is the completion edge); the skill text repeats it for the new signals | this spec |
| D14 | Only client Stdin frames stamp `lastInput` (§ Input). Resize, Control frames and the server's own 7501 probe reply do not | this spec |
| D15 | The one-key arms (TUI `w`/`W`, WebUI await-idle button, grid 🔔, task-sheet item) keep the default signal and get no picker. Choosing a signal is a command-line act on every surface | this spec (follows D1) |
| D16 | `Client.AwaitIdle` takes the signal as a positional parameter, so the compiler finds every caller (`cmd/harness-cli/session.go:696`, `tui/client.go:454`, `cmd/harness-webui-wasm/main.go:3855`) | this spec |
| ~~D17~~ | ~~`signal_unavailable` exits 5~~ — **withdrawn** with the refusal (D10). No new status and no new exit code | this spec, withdrawn |

## Non-goals

- Changing the default signal (D1).
- Waiting for one particular state (`--until waiting`) (D5).
- `await-idle` on a task without a live session. The verb keeps its
  `not_found` for that; a oneshot task's lifecycle is not waitable here.
- Event-driven firing (D9).
- Reconciling lifecycle with program status (lifecycle D3): with `any` the
  first leg wins and both readings are reported.

## Wire (`runner/protocol/message.bgn`)

Appended, like every change to these formats:

```
enum AwaitIdleSignal:
    :u8
    quiet = 0        # PTY output quiescence (today's behaviour, the default)
    lifecycle = 1    # agent hooks' LifecycleState
    program = 2      # OSC 7501 root record
    any = 3          # whichever leg is satisfied first (D4)

format AwaitIdleRequest:
    ...existing fields...
    signal :AwaitIdleSignal

format AwaitIdleResponse:
    ...existing fields...
    fired_by :AwaitIdleSignal        # status=fired only; quiet otherwise
    lifecycle :LifecycleState        # at decision time (D8)
    program_state :ProgramState
    program_kind :ProgramKind

format AwaitIdleWatcherInfo:
    ...existing fields...
    signal :AwaitIdleSignal          # D12; quiet for condition=progress rows
```

The board fire payload (`fireIdleBoard`) gains `"signal"` (the signal the
watcher was armed with), `"fired_by"` (the leg that fired, D8), `"lifecycle"`,
`"program_state"` and `"program_kind"` (wire names; `""` for unset/none).
`"kind":"session_idle"` and the existing keys are unchanged.

None of these formats is persisted (watchers are memory-only,
`server/idle_watchers.go:14-16`), so there is no disk axis.

## Server

- `handleAwaitIdle`: validate `signal` (an out-of-range value is
  `bad_request`, as an unknown sink is). No refusal by source state (D10).
- `SessionMux.ArmIdleWatcher` takes the signal and the threshold and evaluates,
  per tick, the legs the signal selects:
  - quiet: today's test, unchanged;
  - lifecycle: `TaskStore` lifecycle outside `working` and D6;
  - program: `Program()` outside `working`/`none` and D6.
  The lifecycle read (state, `lifecycle_at`, `lifecycle_working_at`) needs the
  task store; pass a reader closure from the handler rather than giving the
  mux a store reference.
- `muxStart`, `lastInput`, `programAt` and `programWorkingAt` (D7) live on the
  mux beside `lastOutput`; `TaskStore.SetLifecycle` also stamps
  `lifecycle_working_at` when it accepts a `working` report.
- The fire callback carries `fired_by` and the three state readings; the
  reply, board and notify deliveries render them (D8).

### Input

Every server-side write into a live PTY goes through `writeFrameToRunner`
(`server/session_mux.go:977`). `lastInput` is stamped for a `FrameType_Stdin`
frame forwarded from a client, and for nothing else:

| Path | Stamps `lastInput` |
| --- | --- |
| Control attach: `forwardControlFrames` (`:827`) | yes, Stdin frames only. The same loop forwards Control frames (signals); those do not stamp |
| Cowriter attach: `forwardObserverFrames` (`:880`, `case v.cowriter`). Reached by `session send`, `session exec`, the stream verbs' NDJSON writes and the WebUI preview | yes, Stdin frames only |
| Resize: `applyWinSizeFrame` / `applyObserverWinSize` | no — a size change, not input |
| 7501 probe reply: `answerProgramStatusProbes` (`:433`) | no — synthesized by the server |

`programAt` is stamped in `recordFrame` (`:361`) by comparing
`m.screen.ProgramStatus()` before and after `m.screen.Write`, under the
`screenMu` it already holds. `loadScreen` (restore from a previous server's
capture) sets it to the restore time when the restored status is not none.

## Surfaces

All three command lines parse from the one declaration in the verb table
(`cli/verb/table.go:2870`, `session await-idle`): adding the flag there gives
the CLI, the TUI cmdline (`tui/cmdline.go:43` → `verb.ParseTUICommand`) and
the WebUI cmdline (`harness.parseCommand`, `cmd/harness-webui-wasm/main.go:5486`)
the same parser. The code that reads the parsed field and calls the client is
hand-written on each surface. `go generate ./cli/verb` regenerates
`actions_gen.go` (`SessionAction` gains `Signal`).

| Surface | Change |
| --- | --- |
| Verb table | `{Name: "signal", Type: FlagString, Default: "quiet", OneOf: quiet,lifecycle,program,any, Field: "Signal"}`; Notes say what each signal waits for and that the fire reports the state |
| `cli/await_idle.go` | `Client.AwaitIdle` gains a `signal protocol.AwaitIdleSignal` parameter (D16). New `AwaitIdleSignalString` / parse helper, beside `AwaitIdleSinkString`. `AwaitIdleWatcherLines` / `AwaitIdleWatcherJSONLine` add `signal=` / `"signal"` |
| CLI arm (`cmd/harness-cli/session.go:677`) | pass the signal; the printed JSON gains `fired_by`, `lifecycle`, `program_state`, `program_kind` (wire names, `""` for unset/none); exit codes unchanged |
| CLI `ls` / `kill` | `ls` text and JSON show the signal (D12); `kill` unchanged |
| TUI cmdline (`tui/dispatch.go:604`) | pass `a.Signal` to `DoAwaitIdle` |
| TUI keys `w` / `W` (`tui/actions.go:336`) | unchanged: they arm the default signal (D15) |
| TUI fire result (`tui/app.go:1189`) | the result line names the leg and the state, e.g. `session is idle (program done)`; quiet keeps today's `last output …` wording |
| TUI watcher modal (`tui/idlewatchersmodal.go:38`, `watcherWaitsFor`) | `idle 2500ms` for quiet, `idle lifecycle` / `idle program` / `idle any 2500ms` otherwise |
| WebUI cmdline (`webui/static/cmdline.js:604-633`) | pass `signal`; print the new response fields; `watcherListText` (`:94`) shows the signal |
| WebUI button / grid 🔔 / task sheet (`main.js:5029`, `:5748`, `:6186`) | unchanged: default signal (D15) |
| WebUI watcher list (`main.js:1588`, `task_models.js:104` `watcherWaitsFor`) | same wording as the TUI modal |
| WASM (`cmd/harness-webui-wasm/main.go:3822` `harnessAwaitIdle`, `:1929` snapshot rows, `:4972` list) | read `signal`; return `firedBy`, `lifecycle`, `programState`, `programKind`; rows carry `signal` |
| Agent skill (`runner/agentskills/supervising-workers/SKILL.md:240-262`, and its mirrors under `.claude/skills/` and `.agents/skills/`) | document `--signal`, the reported state replacing "snapshot once after the fire" for lifecycle/program, that a source which never reports leaves the watcher armed (D10; use `any` when unsure), and the payload's new keys. The payload description at `:251` also gains the existing `last_output_at_unix_ms` it leaves out |

`surface-parity-checklist` is walked item by item in the plan; the table above
is the minimum, not the walk.

## Rollout and verification

- A `.bgn` change: server first, then clients and runner hosts; run
  `scripts/wire-skew-check.sh` unconditionally.
- Unit tests for each leg and for D6 (a) and (b) with stamps set by hand,
  including "non-working before input, no output for threshold → fires",
  "non-working before input, turn reports working then idle → fires on idle",
  "a previous turn's idle received after the input, before any working → does
  not fire" (D6 (a)), and "a lifecycle state carried from before `muxStart`
  → does not fire until a report since the start" (D18).
- Dummy-harness check at the operator's entry layer, in the exact spelling the
  help prints: `session send` a prompt to a claude task, then
  `session await-idle --signal program` and `--signal lifecycle`; confirm the
  fire arrives before quiescence would and reports `done`/`idle`. Then type
  text without Enter and confirm D6(b) fires after the threshold. Record the
  silence between Enter and the first output (D13 (2)).
