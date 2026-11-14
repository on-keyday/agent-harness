# A3 — retiring oneshot: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the oneshot task kind with `session new --oneshot` (the profile's print-mode argv run in a PTY session), and remove `submit`, the task log and `logs`.

**Architecture:** `--oneshot` is one bit on the open request and on `OpenExecRunnerRequest`; the runner picks the profile's oneshot template instead of its interactive one, and the session ends when the process exits. A `RunnerHello` bit keeps the server from sending it to a runner that would ignore it. Everything only the oneshot kind used — the queue branch, `AssignTask`, the runner's `Process` path, the task log and its pubsub publish path, `GetTaskLog`, `logs`, the TUI log pane, `submit` — is removed; retired wire ordinals stay reserved.

**Tech Stack:** Go; `.bgn` wire regenerated with ebm2go (`make protoregen`); bubbletea TUI; wasm WebUI; Python preset scripts.

**Spec:** `docs/superpowers/specs/2026-11-13-pillar-a-run-modes-design.md`, §A3 (settled bullets, F29–F35, A-D29–A-D32). Pillar B (`2026-11-14-pillar-b-transcripts-design.md`) is landed and is what reads an agent's run afterwards.

## Global Constraints

- No new Go module or package dependency.
- Public repo: no local paths, hostnames, addresses or operator quotes in committed text.
- Retired wire ordinals stay as reserved entries (the `trsf_state` precedent); `TaskKind.oneshot` stays at 0 so old WAL records decode and display.
- Runner-decoded wire changes (OpenExecRunnerRequest bit, RunnerHello bit, retiring `assign_task`): run `scripts/wire-skew-check.sh` and restart the **server first**, then runners through their systemd units.
- `--agent-log-format`, `--agent-oneshot-argv`, `--agent-resume-oneshot-argv` and the preset key `logFormat` must still be accepted by a new runner (`restart.py` replays argv); `logFormat`/`--agent-log-format` are ignored.
- Files already under `<data-dir>/logs/` are never deleted by code.
- Surfaces: CLI, TUI and WebUI for every new option; walk `surface-parity-checklist` (1–39, S1–S6).
- Tests run with `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`, `GOTMPDIR`/`TMPDIR` off tmpfs; `make wasm-check` after any change reachable from the wasm build.

## Review Focus

1. **A `--oneshot` open naming a runner without the hello bit** (an old runner, or ghi left un-restarted): it is never started there with the interactive template. Foreground is refused as for a missing profile; background stays Queued. → Task 3 test.
2. **Upgrade replay of a WAL holding oneshot tasks** in every state: Queued and Held fail with `oneshot_retired`; terminal ones display unchanged; nothing is placed or sent `assign_task`. → Task 5 test.
3. **A `--oneshot` prompt with newlines, quotes, a leading `-` or several KB**: it reaches the agent as the single `{prompt}` token the oneshot template had, travelling on the A2 prompt stream (never in the control message). → Task 2 test.
4. **A `--oneshot` process that exits non-zero, or before anyone attaches**: the task ends with that exit code and end reason `process_exit`; a detached session that ends on its own does not linger Detached. → Task 2/3 test.
5. **A runner started from an argv replayed by `restart.py`** (carrying `--agent-log-format`, oneshot argv flags): it starts and registers. → Task 6 test.

---

### Task 1: Wire

**Files:** Modify `runner/protocol/message.bgn` (+ regenerated `message.go`), `runner/protocol/*_test.go`.

**Interfaces — Produces:**
- `OpenInteractiveExt.oneshot :u1` (from its `reserved :u6`).
- `OpenExecRunnerRequest.oneshot :u1` (from its `reserved :u4`).
- `RunnerHello.oneshot_sessions :u1` (from its `reserved :u6`).
- No new status: a missing prompt is the existing `prompt_required`.

- [ ] Step 1: failing test `TestOneshotBitsRoundTrip` (each of the three bits survives encode/decode; zero by default) and `TestOpenExecRunnerRequestFitsOneDatagram` still green with the bit set.
- [ ] Step 2: run → FAIL (undefined setters). Step 3: edit `.bgn`, `make protoregen`. Step 4: `go test ./runner/protocol/` → PASS; `go build ./... && make wasm-check`.
- [ ] Step 5: commit `protocol: oneshot bits on the open, OpenExec and RunnerHello`.

### Task 2: Runner — `--oneshot` sessions

**Files:** Modify `runner/session.go` (`resolveExec`, `handleOpenExec`), `runner/connect.go` (hello bit); test `runner/session_test.go`.

**Interfaces — Consumes:** Task 1 bits; A2's prompt stream (`prompt_present`). **Produces:** `resolveExec(profile, …, oneshot bool)` reached from the open path with the request's bit (today only tests pass `true`, F-inventory).

- [ ] Step 1: failing tests —
  - `TestOneshotOpenUsesOneshotTemplate`: an OpenExec with `oneshot=1` and a prompt builds `OneshotArgv` with `{prompt}`; with `resume_conversation` it builds `ResumeOneshotArgv`.
  - `TestOneshotOpenNeedsPrompt`: no prompt → `TaskFinished(-1, "prompt_required: …")`.
  - `TestOneshotPromptIsOneToken`: a prompt with `\n`, `"`, a leading `-` and 8 KB arrives as one argv element.
  - `TestOneshotExitEndsSession`: the child exiting with 3 sends `TaskFinished(3)` and closes the session (the existing PTY exit path; pin it for the oneshot argv).
  - `TestHelloAdvertisesOneshotSessions`.
- [ ] Step 2: run → FAIL. Step 3: implement (PTY as for interactive; only the argv source changes). Step 4: `go test ./runner/` → PASS.
- [ ] Step 5: commit `runner: --oneshot sessions run the profile's oneshot argv in the PTY`.

### Task 3: Server — the open path

**Files:** Modify `server/task_handler.go` (open/validation), `server/session_start.go` (relay the bit), `server/registry.go` (`RunnerEntry.OneshotSessions`, set in `runnerEntryFromHello`), candidate filtering (`Registry.Candidates` callers for interactive opens and `Tick`'s background placement); tests `server/oneshot_session_test.go`.

**Interfaces — Consumes:** Task 1. **Produces:** an interactive task opened with `oneshot` set; nothing persisted beyond today's fields (A-D29: a resume reuses nothing).

- [ ] Step 1: failing tests —
  - `TestOneshotOpenRelaysBit` (OpenExecRunnerRequest carries `oneshot=1`).
  - `TestOneshotOpenNeedsPrompt` (`prompt_required`, foreground and background).
  - `TestOneshotOpenSkipsRunnersWithoutHelloBit` (Review Focus 1): only an old runner → foreground answers `OpenInteractiveStatus.profile_unavailable`; background stays Queued and is placed once a runner with the bit connects.
  - `TestRunnerEntryRecordsOneshotSessionsFromHello`.
- [ ] Step 2 → FAIL. Step 3: implement. Step 4: `go test ./server/` (unfiltered) → PASS.
- [ ] Step 5: commit `server: --oneshot opens; only runners that advertise it take one`.

### Task 4: Clients — `--oneshot` on every surface

**Files:** Modify `cli/verb/table.go` (`session new` flag `oneshot`; Build: requires a prompt), `cli/session_opts.go` (field through the shared funnel `buildOpenInteractiveRequest`), `tui/` (`s` key + `tui/popup.go` repurposed to open a detached `--oneshot` session; `Authority.opts` builder), `cmd/harness-webui-wasm/main.go` (open option `oneshot`), `webui/static/main.js` (Compose: a "oneshot" checkbox beside the prompt), `go generate ./cli/verb`; tests in each package and `webui/static/*_test.mjs`.

**Interfaces — Consumes:** Task 1's ext bit.

- [ ] Step 1: failing tests —
  - `TestSessionNewOneshotNeedsPrompt` (verb Build) and `TestOneshotRidesTheOpenRequest` (every builder sets the bit — grep the `SessionOpts{` constructions, parity item 28a).
  - TUI: `TestSKeyOpensOneshotSession` (the popup's submit sends an open with `oneshot` and `detached`).
  - JS: the Compose checkbox maps to the bridge's `oneshot` option.
- [ ] Step 2 → FAIL. Step 3: implement. Step 4: `go test ./cli/... ./tui/... ./cmd/...`, `make js-test`, `make wasm-check` → PASS.
- [ ] Step 5: commit `clients: session new --oneshot on the CLI, TUI and WebUI`.

### Task 5: Retire submit and the oneshot kind

**Files:**
- Remove: the `submit` row and its `SpawnAction` uses (`cli/verb/table.go`, `cli/verb/spawn.go`), `cmd/harness-cli/dispatch.go` `Submit`, `cli/submit.go` (move `parseTaskIDHex` to its remaining user), TUI `DoSubmit*`/`SubmitResultMsg`/`tuiVerbs.Submit`, dead `runSpawnAction`, WebUI `case "submit"` and `harnessSubmit`.
- Server: `handleSubmit`, `handleSubmitResume`, `submitGrantRequest`, the `TaskControlKind_Submit` dispatch, `Tick`'s oneshot branch, `sendAssign`, `buildAssignMsg`.
- Runner: `handleAssign`, `waitForAssignTaskBody`, the `assign_task` dispatch, `runner/process.go`.
- Wire: `submit` and `assign_task` become reserved entries; `SubmitRequest/Response/Status`, `AssignTask`, `AssignTaskBody` removed.
- Replay: `server/taskstore.go` fails Queued and Held oneshots with `oneshot_retired`; `server/readopt.go` stops re-adopting oneshots.
- In-repo callers (F32): the integration tests and `scripts/netem-lab` use `session new -d --oneshot --agent bash --prompt "sleep 3600"`; `scripts/kvm-runner`, `scripts/sandbox/{probe.sh,README.md,agent-in-podman.sh}`, `scripts/dummy-harness.py` (comments/usages) move to `session new --oneshot`. `scripts/e2e-oneshot-progress.sh` is rewritten to open `--oneshot` and read the run with `session snapshot` and `transcript get` (it asserted decoded task-log lines, which no longer exist).

- [ ] Step 1: failing tests —
  - `TestReplayRetiresQueuedAndHeldOneshots` (Review Focus 2): a WAL with oneshot tasks Queued, Held, Running and Succeeded replays to Failed/`oneshot_retired`, Failed/`oneshot_retired`, Failed (today's crash rule), Succeeded; `Tick` places none of them.
  - `TestRetiredOrdinalsStayReserved` (wire: the reserved names keep their numbers).
  - `TestSubmitVerbIsGone` (`cli/verb`: no `submit` path; the help names `session new --oneshot`).
- [ ] Step 2 → FAIL. Step 3: remove and migrate. Step 4: `make test` (unfiltered) and `go test -tags integration ./integration/...` for the migrated tests → PASS.
- [ ] Step 5: commit `retire submit and the oneshot kind`.

### Task 6: Retire the task log and `logs`

**Files:**
- Runner: the task-log sink and the publish path (`TaskSender.Publish`, `TaskRegistry.publishWhenConnected`, `peerSender.Publish`; `peer.Conn.Publish` if no caller remains).
- Server: `server/logstore.go`, `server/logtap.go`, their wiring in `server/server.go`, the publish grant and `taskLogDelivery` in `server/pubsub_authz.go`, `handleGetTaskLog` and its dispatch, `LogsDir`, prune's `<id>.log` removal (`PruneByIDs`, `PruneTerminal` lose `logDir`), `topics.TaskLog` if unused.
- Clients: `cli/get_log.go`, `cli/logs.go`, the `logs` row, its dispatch; TUI `tui/logs.go`, `focusLogs` and the focus cycle, Enter-to-follow, `LogChunkMsg`/`LogHistoryMsg`/`SubscribeTaskLog`/`DoGetTaskLogGen`, the log filter keys; the freed rows go to the task table (spec: G23).
- Wire: `get_task_log` reserved; `GetTaskLogRequest/Response` removed.
- Profiles: `--agent-log-format` and `logFormat` accepted and ignored (Global Constraints); `agentlog.HasDecoder` and the unrecognised-format startup check removed; `scripts/agent_presets.py` stops emitting `--agent-log-format` (key kept, ignored).
- Caps text: `cli/verb/caps.go` `none` ("own task logs/ls" is stale) and `exec_view` ("task logs").

- [ ] Step 1: failing tests —
  - `TestRunnerAcceptsRetiredFlags` (Review Focus 5): `--agent-log-format`, `--agent-oneshot-argv`, `--agent-resume-oneshot-argv` parse; the first is ignored.
  - TUI: `TestFocusCycleHasNoLogPane` and `TestTaskTableTakesFreedRows` (taller than 10 rows at a 40-row terminal).
  - `TestPruneLeavesOldLogFilesAlone` (A-D30).
- [ ] Step 2 → FAIL. Step 3: remove. Step 4: `make test`, `make wasm-check` → PASS.
- [ ] Step 5: commit `retire the task log, logs and the TUI log pane`.

### Task 7: Docs, skills, parity, live checks, landing

**Files:** `README.md` (quick start, caps table, oneshot argv/log-format text, the TUI cmdline list), `runner/agentskills/{supervising-workers,harness-cli,…}` and their mirrors (the `submit`/`logs`/`watch` section becomes `session new --oneshot` + `watch` + reading a run with `transcript get` or an agentboard reply), `.claude/skills/{dummy-harness,implementation-pitfalls,surface-parity-checklist}`, `.claude/commands/runner-up.md`, the spec (an "as built" note under §A3), `.claude/skills/surface-parity-checklist/firing-log.md`.

- [ ] Step 1: walk `surface-parity-checklist` 1–39 and S1–S6 (S-items fire: oneshot argv and `logFormat` are preset fields); append the firing log.
- [ ] Step 2: live, dummy harness: `session new --oneshot` with the fake agent and with real claude (`-- --force-inject-harness-settings`), foreground and `--background`; attach shows the JSON as emitted; the task ends with the exit code; `transcript ls/get` reads claude's run; `--oneshot --agent bash --prompt 'exit 3'` ends exit 3; TUI `s` in a dummy PTY; WebUI Compose checkbox in Playwright. Screenshots to `a1-shots/`.
- [ ] Step 3: `make test`, `make wasm-check`, `scripts/wire-skew-check.sh <deployed sha>` → green.
- [ ] Step 4: final whole-branch review; land (FF-push, `make build` in the main checkout); report the restart order (server, then runners via systemd, the operator's slot last; an un-restarted runner simply never receives a `--oneshot` open).
