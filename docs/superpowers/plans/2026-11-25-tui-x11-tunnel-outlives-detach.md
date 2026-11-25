# TUI X11 tunnel outlives a detach — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** In the TUI an X11 tunnel lives until its task ends or the TUI exits, and `D` gives the selected task an X11 session in one key.

**Architecture:** The X11 -R forward becomes an entry in the TUI's existing forward registry (`App.activeForwards`) under a new direction `ForwardX11`, so it outlives a detach exactly as `b` forwards already do. The App picks a free display number. `D` maps the task's state to reattach / resume-with-X11 / confirm-then-restart, the restart being cancel → wait for a finished status → resume.

**Tech Stack:** Go, bubbletea (TUI), existing `cli` forward and session APIs. No `.bgn` change.

**Spec:** `docs/superpowers/specs/2026-11-25-tui-x11-tunnel-outlives-detach-design.md`

## Global Constraints

- TUI only. The CLI's `session new --x11` behaviour does not change; the WebUI does not change.
- No wire (`.bgn`) change.
- The key is `D`, tasks scope.
- A live session is never cancelled without a `y` answer, and never cancelled when `$DISPLAY` cannot be used.
- `make test` green at every commit; for targeted runs use the Makefile's `GO_TEST_ENV` prefix (`env -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID -u HARNESS_RUNNER_ID -u HARNESS_SERVER_CID -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_PUBKEY_FILE -u HARNESS_WS_PATH -u HARNESS_PROXY_VIA_RUNNER go test ./tui/ -run …`), under `nice -n 19`.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` before Task 1.

## Review Focus

1. **No usable `$DISPLAY` on a live session** — `D` must refuse before the confirm; cancelling first would kill the agent for nothing. Pinned in Task 3.
2. **`q` (or any key) while the restart question is up** — must not quit or act; only `y` restarts. Pinned in Task 3.
3. **`D` again on a task whose restart is already pending** — must not send a second cancel. Pinned in Task 3.
4. **A live background-mode task** — the server refuses X11 there; `D` must say so, not restart it into a refusal. Pinned in Task 3 (`x11OpenAction` table).
5. **The resume after the cancel is refused** (runner gone, display bind failure) — the pending restart and the display reservation are released and the error shown, not left to block the next `D`. Pinned in Task 2 (reservation release) and Task 3 (pending release).

---

### Task 1: the X11 forward joins `activeForwards` and survives a detach

**Files:** Modify `tui/portforward.go`, `tui/interactive.go`, `tui/app.go`, `tui/workspace_reconcile.go`, `tui/dispatch.go:536`, `tui/record_test.go` (stub signature). Test: new `tui/x11_test.go`.

**Produces:**
- `ForwardX11` (a `ForwardDirection` after `ForwardLink`); `flag()` returns `"--x11"`.
- `PortForwardSession.X11Display int` and `PortForwardStartedMsg.X11Display int` (0 for every other direction).
- `DoOpenX11Session(c *cli.Client, repo string, selOpts cli.SelectorOpts, extraArgs []string, resumeTaskID string, displayN int, forwardID int, program *tea.Program, auth Authority, resumeCapsOverride, resumeConversation, exact bool, agentProfile string, runMode cli.RunModeFlags) tea.Cmd` — one new parameter, `forwardID` (the client-side registry id, from `a.nextForwardID`). The `openX11Session` package var takes the same signature.
- `InteractiveReadyMsg` loses `X11Cancel`, gains `X11Display int` (the display the open used, set on success and on error, 0 for non-X11).
- `App.x11Cancel` is deleted.

- [ ] **Step 1: tests in `tui/x11_test.go`**
  - `TestDetachLeavesTheX11ForwardRunning`: `a := New(Config{})`; put `a.activeForwards[7] = &PortForwardSession{ID: 7, TaskID: id, Direction: ForwardX11, X11Display: 10, Cancel: func() { cancelled = true }}`; `a.Update(InteractiveDoneMsg{TaskID: id})`; assert `!cancelled` and `a.activeForwards[7] != nil`.
  - `TestX11ForwardIsNoWorkspaceForward`: `_, ok := sessionIdentity(&PortForwardSession{Direction: ForwardX11, Spec: ":10 -> localhost:0"})`; assert `!ok`.
  - `TestX11ForwardStopsWithItsTask`: entry 7 as above; `a.Update(PortForwardStoppedMsg{ID: 7, TaskID: id})`; assert entry gone.
- [ ] **Step 2:** run → FAIL (`ForwardX11` undefined).
- [ ] **Step 3: implement.**
  - `DoOpenX11Session`: after `c.OpenInteractiveX11` succeeds, call `c.OpenRemoteForward(ctx, taskID, sp)` with a fresh cancellable `ctx`. On error: `program.Send(PortForwardStatusMsg{Line: forwardFailLine(taskID, err)})` and still return the ready message (the session is open; only the tunnel failed). On success: `program.Send(PortForwardStartedMsg{ID: forwardID, TaskID: taskID, Direction: ForwardX11, Spec: fmt.Sprintf(":%d -> %s", displayN, os.Getenv("DISPLAY")), Cancel: cancel, ForwardID: fid, X11Display: displayN})` BEFORE starting the goroutine (sibling rule in `DoStartRemoteForward`), then `go func() { c.ServeRemoteForward(ctx, sp, ctrl, fid, forwardStatusLogf(ctx, program)); program.Send(PortForwardStoppedMsg{ID: forwardID, TaskID: taskID}) }()`.
  - `app.go`: delete `x11Cancel`, its use in the `termReleased` refusal branch and in `InteractiveDoneMsg`; keep the `X11Warn` line; the `"x11 forward started"` line is now the generic `PortForwardStartedMsg` line. Copy `X11Display` into `PortForwardSession` in the `PortForwardStartedMsg` case.
  - `sessionIdentity`: return false for `ForwardX11` beside `ForwardLink`.
  - `dispatch.go:536` passes `a.nextForwardID` (incremented) as `forwardID` — Task 2 replaces its display argument.
  - `record_test.go`: the stub gains the `forwardID int` parameter.
- [ ] **Step 4:** run → PASS; `nice -n 19 make test` green.
- [ ] **Step 5:** commit `tui: the X11 tunnel joins the forward registry and outlives a detach`.

### Task 2: the TUI picks a free display number

**Files:** Modify `tui/app.go` (field + method), `tui/dispatch.go:536`. Test `tui/x11_test.go`.

**Consumes:** `ForwardX11`, `PortForwardSession.X11Display`, `InteractiveReadyMsg.X11Display` (Task 1).

**Produces:**
- `App.x11Reserved map[int]bool` — displays handed to an open whose `InteractiveReadyMsg` has not arrived.
- `func (a *App) pickX11Display(preferred int) (int, bool)` — the lowest N in `[preferred, 99]` held by no `ForwardX11` entry of `activeForwards` and not in `x11Reserved`; reserves it; false when none is free.
- The `InteractiveReadyMsg` case deletes `x11Reserved[msg.X11Display]` first thing, on success and on error alike.

- [ ] **Step 1: tests**
  - `TestX11DisplaySkipsHeldAndReserved`: a `ForwardX11` entry with `X11Display: 10`; `pickX11Display(10)` → `11, true`; again → `12, true`.
  - `TestX11DisplayHonoursAHigherRequest`: `pickX11Display(15)` → `15, true`.
  - `TestX11DisplayRunsOut`: reserve 10..99; `pickX11Display(10)` → `_, false`.
  - `TestX11DisplayIsReleasedWhenTheOpenFails`: pick 10; `a.Update(InteractiveReadyMsg{X11Display: 10, Err: errors.New("x")})`; `pickX11Display(10)` → `10, true`.
  - `TestTuiSessionNewX11UsesAFreeDisplay`: stub `openX11Session` capturing `displayN`; a `ForwardX11` entry holds 10; `(tuiVerbs{a}).SessionNew(verb.SpawnAction{Repo: "/r", X11: true, X11Display: 10})`; captured `11`.
- [ ] **Step 2:** run → FAIL. **Step 3:** implement; `dispatch.go` calls `a.pickX11Display(int(v.X11Display))` and on false appends `ErrorStyle` "x11: no free display number from N to 99 in this TUI" and opens nothing. **Step 4:** PASS + `make test`. **Step 5:** commit `tui: pick a free X11 display number`.

### Task 3: `D` — reattach, resume with X11, or restart after y/n

**Files:** Modify `tui/keys.go`, `tui/taskaction.go`, `tui/actions.go`, `tui/app.go` (fields, footer, `SnapshotMsg` / `TaskEventMsg` / `CancelResultMsg` hooks), `tui/overlays.go` (`appOverlays` entry), `cli/x11.go`. Test `tui/x11_test.go`, `tui/taskaction_test.go` (or wherever `resumeReattachAction` is tested — grep first), `cli/x11_test.go`.

**Consumes:** `pickX11Display`, the `openX11Session` signature (Tasks 1–2), `DoAttachSession`, `DoCancel`, `resumeSelectorOpts`.

**Produces:**
- `cli.LocalX11Display() (string, error)` in `cli/x11.go`: `$DISPLAY`, validated by `localXServerDialSpec`; the error names the problem (`DISPLAY is empty`, malformed).
- `mainKeys.X11Open = "D"`; row `{Keys: []string{mainKeys.X11Open}, Scope: scopeTasks, Do: (*App).onX11Open, Short: "D x11", Long: "give the selected task X11: reattach if this TUI holds its tunnel, resume a finished task with X11, or restart a live one with X11 after a y/n (conversation kept)"}`.
- `actionX11Restart` (a new `taskAction` kind) and `func x11OpenAction(t *protocol.TaskInfo, held bool) taskAction`.
- App fields `x11RestartAsk string` (task id awaiting y/n, "" none) and `x11Restarts map[string]bool` (cancelled, awaiting a finished status); `type x11RestartTimeoutMsg struct{ TaskID string }`; `func (a *App) advanceX11Restarts() tea.Cmd`; `func (a *App) openX11Resume(t protocol.TaskInfo) tea.Cmd`.

- [ ] **Step 1: tests**
  - `TestLocalX11Display` (cli): `t.Setenv("DISPLAY", "")` → error containing `empty`; `"localhost:0"` → ok; `"nonsense"` → error.
  - `TestX11OpenAction` table: nil → none, hint `no task selected`; Interactive Running, held → reattach; Interactive Detached, not held → x11 restart; Interactive Running, `RunMode_Background` → none, hint names background; Interactive Succeeded / Failed / Cancelled → resume with `ResumeConversation: true`; Stream Running → none, hint says X11 needs a terminal session; Queued → none.
  - `TestDReattachesWhenThisTUIHoldsTheTunnel`: snapshot with one Running Interactive task id; a `ForwardX11` entry for id; press `D` → returned cmd non-nil, `openX11Session` stub not called.
  - `TestDResumesAFinishedTaskWithX11`: Cancelled task, `DISPLAY=localhost:0`; press `D` → stub called with `resumeTaskID == id`, `resumeConversation == true`, `displayN == 10`, `selOpts == resumeSelectorOpts(t.AssignedTo)`.
  - `TestDAsksBeforeRestarting`: Running task, no tunnel, `DISPLAY=localhost:0`; press `D` → `a.x11RestartAsk == id`, no cancel cmd, the footer (`a.View()`) contains `restart`; press `q` → `x11RestartAsk == ""`, cmdresult says nothing restarted, the returned cmd is not `tea.Quit`.
  - `TestDRefusesWithoutADisplay`: Running task, `DISPLAY` empty; press `D` → `x11RestartAsk == ""`, cmdresult mentions `DISPLAY`.
  - `TestDRestartCancelsThenResumes`: arm, press `y` → `x11Restarts[id]`; a second `D` → cmdresult `already restarting`, no new cancel; `a.Update(TaskEventMsg{…id, Status: Cancelled})` → stub called once with `resumeTaskID == id`, `x11Restarts` empty.
  - `TestDRestartGivesUpOnCancelError`: pending id; `a.Update(CancelResultMsg{Resolved: id, Err: errors.New("x")})` → `x11Restarts` empty, cmdresult has the error.
  - `TestDRestartGivesUpAfterTimeout`: pending id; `a.Update(x11RestartTimeoutMsg{TaskID: id})` → empty, cmdresult says it did not finish; a later Cancelled event opens nothing.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: implement.**
  - `onX11Open`: `held` = any `ForwardX11` entry for the task. For resume and restart, `cli.LocalX11Display()` first; on error append it and return. Reattach → `DoAttachSession(a.client, id, protocol.AttachMode_Control)`. Resume → `openX11Resume(*t)`. Restart → if `x11Restarts[id]` append `already restarting` and return; else arm `x11RestartAsk` and append the prompt.
  - Prompt text (also the footer, as `schedDeletePrompt` is): `D <id8>: restart the agent with X11? y restarts (conversation kept; a turn in flight is lost), any other key keeps it running`.
  - `appOverlays`: an entry right after the credential-revoke one, `open: x11RestartAsk != ""`, key `inX11RestartConfirm`: clears the ask; `y`/`Y` → `x11Restarts[id] = true`, return `tea.Batch(DoCancel(a.client, id, id), tea.Tick(30*time.Second, func(time.Time) tea.Msg { return x11RestartTimeoutMsg{TaskID: id} }))`; else append `D <id8>: kept running; nothing restarted`.
  - `advanceX11Restarts`, called at the end of the `SnapshotMsg` and `TaskEventMsg` cases and batched with their cmds: for each pending id whose `tasksByID` status is Succeeded / Failed / Cancelled, delete it and collect `openX11Resume`.
  - `CancelResultMsg` for a pending id with `Err` → delete + the existing error line. `x11RestartTimeoutMsg` for a still-pending id → delete + `D <id8>: the task did not finish within 30s; nothing restarted`.
  - `openX11Resume(t)`: `n, ok := a.pickX11Display(10)` (false → error line, nil); `id := a.nextForwardID; a.nextForwardID++`; `openX11Session(a.client, "", resumeSelectorOpts(t.AssignedTo), nil, FormatTaskID(t.Id), n, id, a.program, a.authority(), false, true, false, string(t.AgentProfile), cli.RunModeFlags{})`.
- [ ] **Step 4:** run → PASS; `make test` (the `keys_test` guards check the row's documentation, uniqueness and scope).
- [ ] **Step 5:** commit `tui: D gives the selected task X11`.

### Task 4: README, surface walk, live check

**Files:** Modify `README.md` (the `### X11 forwarding` section), the spec's Status line if anything changed.

- [ ] **Step 1:** walk `.claude/skills/surface-parity-checklist/SKILL.md` items 1–39 for the new key and the changed forward lifetime; record verdicts in the commit body. Expected: 4 done (keys.go), 3/7 n/a (no grammar change), 15 n/a (no cap), WebUI items n/a (`x11SurfaceReason`). Anything else that comes up is fixed in this task.
- [ ] **Step 2:** README: in the TUI, `D` on a task gives it X11 (resume, or restart a live one after y/n, conversation kept); the tunnel lives until the task ends or the TUI exits, and shows in `f` (killable with `x`); the CLI's `session new --x11` still stops it on detach; on Windows `DISPLAY` must name a host (`localhost:0`), a bare `:0` means a unix socket.
- [ ] **Step 3: live, dummy harness** (`dummy-harness` skill; two instances, `host` and `target`, per its "testing a client across" section):
  - Stand-in X server on the runner host: `xauth -f $XA add :77 MIT-MAGIC-COOKIE-1 $(openssl rand -hex 16)`, `socat UNIX-LISTEN:/tmp/.X11-unix/X77,fork EXEC:cat &` (an echo, so bytes through the tunnel come back).
  - In a `host` session: `DISPLAY=:77 XAUTHORITY=$XA harness-tui --server-cid <target>`; open a bash session on `target`; press `D` on it → y → it restarts; inside, `echo $DISPLAY` reads `127.0.0.1:10`; hold a connection: `socat - TCP:127.0.0.1:6010` and type a line, it echoes.
  - Ctrl+], then `r`: the same socat still echoes. `f` lists the `--x11` forward.
  - End the session: the forward leaves `f` and cmdresult says `forward stopped`.
  - `down` both instances; remove `/tmp/.X11-unix/X77` and `$XA`.
- [ ] **Step 4:** commit `README: X11 in the TUI` (with the surface walk in the body). Landing is `scripts/land.py`; the operator then checks once with XLaunch on Windows (spec, Verification).
