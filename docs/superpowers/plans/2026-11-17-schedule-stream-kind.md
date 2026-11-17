# Schedule stream kind Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A schedule stores a task kind (`interactive` / `stream`) and every firing creates or resumes its task as that kind, settable from all three UIs.

**Architecture:** The wire carries the kind as `TaskKind` behind a presence bit; the server overlays it onto the stored schedule, checks it with the same functions a background open uses, persists it by name in `schedules.json`, and fires with it. The client side is one field through the shared `ScheduleSpecOptsFromAction`, two form controls, and one line in the shared detail renderer.

**Tech Stack:** `.bgn` (ebm2go via `scripts/protoregen.sh`), Go (server, cli, tui, wasm), vanilla JS.

**Spec:** `docs/superpowers/specs/2026-11-17-schedule-stream-kind-design.md` (decisions K1–K9).

## Global Constraints

- Go tests: `make test`, or one package with the Makefile's scrub: `env -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID -u HARNESS_RUNNER_ID -u HARNESS_SERVER_CID -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_PUBKEY_FILE -u HARNESS_WS_PATH -u HARNESS_PROXY_VIA_RUNNER go test ./<pkg>/ -run <Name>` (`$SCRUB` below).
- Regenerate after editing `runner/protocol/message.bgn`: `scripts/protoregen.sh`; the 40k-line churn in `message.go` is expected — check build, vet, tests and the new symbols, nothing more.
- `TaskKind`'s zero value is `oneshot`. Never persist or default the kind numerically: the JSON field holds the name, `""` and absent mean `interactive`.
- Allowed kinds: `interactive`, `stream`. Anything else from the wire → `kind_unsupported`.
- Commit on the current branch, staging explicit paths only.

## Review Focus

1. A `schedules.json` written before this change (no `kind`) → every schedule reads as `interactive` (Task 1 `TestScheduleStoreKindAbsentIsInteractive`).
2. An edit that sets `--stream` on a schedule stored with `--oneshot` → refused `oneshot_stream`, stored schedule unchanged (Task 1 `TestScheduleEditStreamOverOneshotRefused`).
3. An edit that switches an interactive schedule with `--turn-end program` to stream → refused by the stream kind's condition check, not stored (Task 1 `TestScheduleEditStreamKeepsCheckingConditions`).
4. `kind = oneshot` or an unknown value on the wire → `kind_unsupported` (Task 1 `TestScheduleKindUnsupported`).
5. A stream schedule's firing → the created/resumed task has `Kind == TaskKind_Stream` and `NoPTY == false` (Task 1 `TestScheduleFiresStreamKind`), and the live dummy-harness check in Task 3.

---

### Task 1: wire, server overlay, persistence, firing

**Files:**
- Modify: `runner/protocol/message.bgn` (`ScheduleSpec`, `ScheduleDetail`, `ScheduleStatus`), regenerated `runner/protocol/message.go`
- Modify: `server/schedule_store.go` (`Schedule.Kind`, JSON), `server/schedule_handler.go` (overlay ~527-600, detail ~1099), `server/schedule_fire.go:285,294`
- Test: `server/schedule_handler_test.go`, `server/schedule_store_test.go`, `server/schedule_fire_test.go`

**Interfaces:**
- Produces (wire): `ScheduleSpec.kind_present :u1` (taken from `reserved`, u4 → u3) and `if kind_present == 1: kind :TaskKind`, placed after the existing `if size_present == 1:` block; `ScheduleDetail.kind :TaskKind` right after its bit byte (`reserved :u2` stays); `ScheduleStatus` appended `kind_unsupported`, `oneshot_stream`, `pty_stream` (after the last existing value, with a `# appended:` comment as R16 did).
- Produces (server): `Schedule.Kind protocol.TaskKind`; `scheduleJSON.Kind string \`json:"kind,omitempty"\`` holding `cli.EnumWireName(kind.String())`-style names (`"interactive"`, `"stream"`); a load of `""` → `TaskKind_Interactive`, of any other name → load error naming the schedule.

Overlay rules (in the spec overlay, after the run block, before `checkBackgroundRun`):
- create: `sc.Kind = Interactive` unless `kind_present`.
- `kind_present`: value not in {Interactive, Stream} → `kind_unsupported`; else `sc.Kind = *spec.Kind()`.
- after the overlay: `sc.Kind == Stream && sc.Run.Oneshot` → `oneshot_stream`; `sc.Kind == Stream && spec.PtySet() && spec.Pty()` → `pty_stream`; `sc.Kind == Stream` → `sc.Run.NoPTY = false`.
- `sc.Run.Cond = sc.Run.Cond.WithDefaults(sc.Kind)` whenever the conditions or the kind changed; `checkBackgroundRun(sc.Kind, …)` replaces the `TaskKind_Interactive` literal.
- Firing: both `TaskKind_Interactive` literals in `schedule_fire.go` become `sc.Kind`.
- Detail: `d.Kind = sc.Kind`.

- [ ] **Step 1: Edit the `.bgn`** as above and run `scripts/protoregen.sh`; `go build ./...` — Expected: builds (new symbols `KindPresent`, `Kind`, `SetKind` on `ScheduleSpec`; `ScheduleStatus_KindUnsupported` etc.).
- [ ] **Step 2: Write the failing tests** (follow the helpers the neighbouring tests in each file use):
  - `TestScheduleStoreKindAbsentIsInteractive`: a `schedules.json` fixture without `kind` loads with `Kind == Interactive`; a save/load round trip keeps `Stream`; the file holds `"kind": "stream"`.
  - `TestScheduleCreateStreamKind`: create with `kind = Stream` → stored `Kind == Stream`, `Run.NoPTY == false`, turn-end default `lifecycle`.
  - `TestScheduleKindUnsupported`: `kind = Oneshot` → status `kind_unsupported`; `kind = 99` → same.
  - `TestScheduleCreateStreamWithOneshotRefused` / `...WithPtyRefused`: → `oneshot_stream` / `pty_stream`.
  - `TestScheduleEditStreamOverOneshotRefused`: stored oneshot schedule, edit `kind = Stream` alone → `oneshot_stream`, stored unchanged.
  - `TestScheduleEditStreamKeepsCheckingConditions`: stored interactive schedule with turn-end `program`, edit `kind = Stream` → refused with the status `checkBackgroundRun` maps for that condition; stored unchanged.
  - `TestScheduleEditKindBack`: stream → edit `kind = Interactive` → stored Interactive.
  - `TestScheduleFiresStreamKind`: a firing of a stream schedule creates the task with `Kind == Stream`; a second firing (resume) keeps `Stream`; a bound interactive task resumed by a stream schedule becomes `Stream` (K6).
  - `TestScheduleDetailCarriesKind`: `schedule show` detail has `Kind == Stream`.
- [ ] **Step 3: Run** `$SCRUB go test ./server/ -run 'Schedule.*(Kind|Stream)'` — Expected: FAIL (field absent / literal Interactive).
- [ ] **Step 4: Implement** the server changes above.
- [ ] **Step 5: Run** `$SCRUB go test ./server/ ./runner/protocol/` then `make test` — Expected: PASS (the CLI may need Task 2's status words; if `cli` has a test that every `ScheduleStatus` has a word, add the three words in `cli/schedule.go`'s status map here, in this task).
- [ ] **Step 6: Commit** — `schedule: a schedule stores its task kind; stream allowed (G1)`.

### Task 2: CLI flag, presence, rules, detail line

**Files:**
- Modify: `cli/verb/spawn.go` (`scheduleOff`, `schedulePresence`, `scheduleExclusive`), regenerated `cli/verb/actions_gen.go` (`go generate ./cli/verb`)
- Modify: `cli/schedule.go` (`ScheduleSpecOpts.Kind *protocol.TaskKind`; `ScheduleSpecOptsFromAction`; `buildScheduleSpec`; status words; `ScheduleDetailText`/`Parts` line `kind: interactive|stream`; `scheduleDetailJSON.Kind string \`json:"kind"\``)
- Test: `cli/schedule_test.go`, `cli/verb/` schedule flag tests

**Interfaces:**
- Consumes: Task 1's `ScheduleSpec.SetKindPresent/SetKind`, `ScheduleDetail.Kind`, the three statuses.
- Produces: `ScheduleSpecAction.Stream bool` + `StreamPresent bool` (generated from the presence entry).

Rules: drop `"stream"` from `scheduleOff` (and the comment's R16 sentence → a line citing this spec's K1); add `"stream": "StreamPresent"` to `schedulePresence`; `scheduleExclusive` gains `{oneshot, stream}` and `{pty, stream}` with the reasons the spawn rules use. `ScheduleSpecOptsFromAction`: `StreamPresent` → `Kind = Stream` if `a.Stream` else `Interactive`. `--stream` alone on a create is enough (no `--detach` rule here: a schedule's runs are always background).

- [ ] **Step 1: Write the failing tests**: `schedule new … --stream` binds and builds a spec with `KindPresent && *Kind == Stream`; `schedule edit <id> --stream=false` → `Interactive`; `schedule edit <id> --prompt x` → `!KindPresent`; `schedule new … --stream --oneshot` and `--stream --pty` refused by the declaration; `ScheduleDetailText` of a stream detail contains `kind: stream`, and of an interactive one `kind: interactive`; the JSON has `"kind":"stream"`.
- [ ] **Step 2: Run** `$SCRUB go test ./cli/ ./cli/verb/ -run Schedule` — Expected: FAIL.
- [ ] **Step 3: Implement**, `go generate ./cli/verb`.
- [ ] **Step 4: Run** `make test` — Expected: PASS.
- [ ] **Step 5: Commit** — `cli: schedule new/edit --stream; schedule show prints the kind`.

### Task 3: TUI form, WebUI Compose, live check, docs

**Files:**
- Modify: `tui/bgform.go` (a `bgFieldStream` yes/no field in the `bgSchedule` order beside `bgFieldOneshot`, emitted as `--stream`), `tui/bgform_schedule_test.go`
- Modify: `webui/static/task_models.js` (`COMPOSE_TAB_DEFS.schedule.fields` gains `"stream"`; `composeScheduleTokens` emits `--stream` when `f.stream`), `webui/static/schedule_test.mjs`
- Modify: `docs/superpowers/specs/2026-11-15-pillar-s-schedules-design.md` (strike the Non-goals line with `~~…~~` and a pointer; Decisions row `S-D27` (or the next free number): "A schedule stores a task kind; stream is allowed — the exclusion reversed G1 without a row" | operator (2026-11-17)), `README.md` schedule section (one line: `--stream`), `docs/backlog.md` (delete the "A schedule cannot run the stream kind" entry)
- Record: `.claude/skills/surface-parity-checklist/firing-log.md` entry

- [ ] **Step 1: Write the failing tests**: TUI — the schedule form with stream `yes` produces argv containing `--stream`, with `no` not; WebUI — `composeTabModel("schedule").fields` includes `stream`, and `composeScheduleTokens({…, stream: true})` includes `--stream`.
- [ ] **Step 2: Run** `$SCRUB go test ./tui/ -run Schedule` and `make js-test` — Expected: FAIL.
- [ ] **Step 3: Implement** both controls.
- [ ] **Step 4: Run** `make test` — Expected: PASS.
- [ ] **Step 5: Live check** (`dummy-harness` skill, `--agent fake`, `--known-psk`): `harness-cli schedule new --cron '* * * * *' --repo $REPO --agent <stream-capable profile> --prompt hi --deadline 2m --stream`, then `schedule run <id>`; `ls --json` shows the task `kind: stream`; `schedule show <id>` prints `kind: stream`; `schedule edit <id> --oneshot` refused `oneshot_stream`. In the TUI and the WebUI, create one stream schedule through the form and confirm `schedule show` says `kind: stream`. Down the dummy after.
- [ ] **Step 6: Docs** as listed; walk `surface-parity-checklist` 1–39 and append the firing-log entry.
- [ ] **Step 7: Commit** — `schedule: --stream on the TUI and WebUI forms; pillar S records the reversal`.
