# Workspace: keep one forward, and the picker writes what it shows — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One forward can be kept in / dropped from the installed workspace with one key, the picker writes exactly what it shows, forwards are compared by parsed identity, and the TUI re-reads `.harness/config` at each use.

**Architecture:** All file semantics live in `cli/workspace` (identity, per-forward add/remove, `Merge` with explicit decisions). The TUI gains one write routine (`keepForward`) used by the forwards pane, the start modal and the `workspace forward` verb, one reload routine used by every workspace operation, and switches `planForwards` to identity. A new verb pair `workspace forward add/rm` is declared in the verb table and generated.

**Tech Stack:** Go, bubbletea / bubbles (TUI), the repo's verb table + `go generate ./cli/verb` codegen.

**Spec:** `docs/superpowers/specs/2026-08-24-workspace-startup-config-design.md`, section "Amendment (2026-10-29, fifth round)". Read its Problem list before starting any task.

## Global Constraints

- Work in this worktree (`/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`), branch `workspace-keep-forward`. Use paths under the worktree; a bare `/home/kforfk/workspace/remote-agent-harness/<rel>` path hits the PARENT checkout.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` before the first task.
- No `.bgn`, server, runner or WebUI change.
- `cli/workspace` compiles for `js/wasm`: it must not import `cli/sshgw` or anything `//go:build !js`.
- Every write goes through `Merge` / per-forward helpers + `File.Set`; comments inside the target workspace's span are not preserved (DECIDED, operator).
- Verify with `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test` (a task env inherits those and fails unrelated tests; `HARNESS_AUTH_TICKET` also makes `workspace.Load` return nothing). Never `go build ./cmd/<x>` without `-o /dev/null`.
- After touching `cli/verb/table.go`, run `go generate ./cli/verb` and commit `cli/verb/actions_gen.go` with it.
- Committed files carry no LAN addresses, hostnames or private paths (public repo).
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. A hand edit that leaves `.harness/config` unparsable while the TUI runs: the next workspace operation prints the parse error and does nothing; the TUI keeps running and the installed workspace is unchanged. → Task 4 test `TestReloadRefusesABrokenFile`.
2. Keeping a forward started on local port `0`: refused, nothing written. → Task 1 test `TestForwardKeyValueRefusesPortZero`, Task 7 relies on it.
3. The same forward kept twice under different spellings (`-L 3000`, then the long form): one line in the file. → Task 1 test `TestAddForwardIsIdempotentAcrossSpellings`.
4. Keeping a forward into a block whose `resume = no`: the block's policy is untouched. → Task 1 test `TestAddForwardLeavesPolicy`.
5. Two tasks forwarding the same spec in the forwards pane: the `ws` mark is per task. → Task 6 test `TestForwardMarksArePerTask`.

---

### Task 1: Forward identity and per-forward edits in `cli/workspace`

**Files:**
- Create: `cli/workspace/identity.go`
- Test: `cli/workspace/identity_test.go`

**Interfaces:**
- Produces:
  - `type ForwardKey struct { Dir ForwardDir; BindAddr string; BindPort int; TargetHost string; TargetPort int; Protocol protocol.ForwardProtocol }` — for `-R`: `BindPort = RunnerPort`, `TargetHost = DialHost`, `TargetPort = DialPort`.
  - `func ForwardIdentity(value string) (ForwardKey, error)` — via `ParseForwardValue`.
  - `func (k ForwardKey) Value() (string, error)` — builds a `protocol.PortForwardInfo` (`ClientEndpoint = OsSocket`, direction from `Dir`) and returns `cli.PortForwardConfigSpec`'s string; error `"local port 0 is chosen at bind time and cannot be kept"` when `BindPort == 0`.
  - `func (ws *Workspace) Declares(taskID, value string) bool`
  - `func (ws *Workspace) AddForward(taskID, value string) (added bool, err error)` — appends `value` verbatim; creates `Task{ID, Resume: ResumeContinue, Runner: RunnerAssigned}` if absent; `added=false` when an identical forward is already declared.
  - `func (ws *Workspace) RemoveForward(taskID, value string) (removed bool, err error)` — removes every identity match; the block stays even when empty.

- [ ] **Step 1: Write the failing tests**

```go
func TestForwardIdentityEquatesSpellings(t *testing.T)
// ForwardIdentity("-L 3000") == ForwardIdentity("-L 127.0.0.1:3000:127.0.0.1:3000")
// == ForwardIdentity(spec) where spec, _ := cli.PortForwardConfigSpec(&protocol.PortForwardInfo{
//    Direction: Local, ClientEndpoint: OsSocket, BindAddr: "127.0.0.1", BindPort: 3000,
//    TargetHost: "127.0.0.1", TargetPort: 3000})
func TestForwardIdentitySeparatesDirectionAndProtocol(t *testing.T)
// "-L 3000" != "-R 3000"; "-L 3000/udp" != "-L 3000"
func TestForwardKeyValueRoundTrips(t *testing.T)
// k, _ := ForwardIdentity("-L 3000/udp"); v, _ := k.Value(); ForwardIdentity(v) == k; v has "/udp"
func TestForwardKeyValueRefusesPortZero(t *testing.T)
func TestAddForwardCreatesBlock(t *testing.T)
// empty ws; AddForward(wsTaskA, "-L 3000") → added; Tasks[0] = {ID: wsTaskA, continue, assigned, ["-L 3000"]}
func TestAddForwardIsIdempotentAcrossSpellings(t *testing.T)
// after "-L 3000", AddForward(..., "-L 127.0.0.1:3000:127.0.0.1:3000") → added=false, len(Forwards)==1
func TestAddForwardLeavesPolicy(t *testing.T)
// block {resume: no, runner: any}; AddForward → policy still no/any
func TestRemoveForwardKeepsBlock(t *testing.T)
// block with "-L 3000"; RemoveForward(..., long form) → removed; block present, Forwards empty
```

Define `wsTaskA`-style 32-hex fixtures locally in the test file (the tui package's fixtures are not visible here).

- [ ] **Step 2: Run** `go test ./cli/workspace -run 'Forward|AddForward|RemoveForward'` — expect FAIL (undefined).
- [ ] **Step 3: Implement** `identity.go` per the Interfaces block.
- [ ] **Step 4: Run** the same command — expect PASS; then `GOOS=js GOARCH=wasm go vet ./cli/workspace` — expect no output.
- [ ] **Step 5: Commit** `feat(workspace): forward identity, and keep/drop one forward in a workspace`.

---

### Task 2: `Merge` takes explicit decisions; saves add forwards and never remove them

**Files:**
- Modify: `cli/workspace/save.go` (`Merge`, around `:120-188`)
- Modify: `cmd/harness-cli/workspace.go` (SubSave, `:112-139`)
- Modify: `tui/workspace.go` (`writeWorkspaceAll` `:452-478`, `commitWorkspacePicker` `:491-523`, `writeWorkspace` `:540-579`)
- Modify: `tui/workspacepicker.go` (`Result`, `ExcludedIDs`)
- Modify: `cli/verb/table.go` (`workspace save` Notes and `--task` Help, `:1021-1046`) + regenerate
- Test: `cli/workspace/save_test.go`, `tui/workspacepicker_test.go`

**Interfaces:**
- Consumes: `ForwardIdentity` (Task 1).
- Produces:
  - `type MergeDecisions struct { Observed, Drop, Policy map[string]bool }`
  - `func Merge(existing, observed *Workspace, d MergeDecisions) *Workspace` — per existing block: `Drop[id]` → omitted; `Policy[id]` → resume/runner from the observed task of that id; `Observed[id]` → forwards replaced by the observed task's; otherwise the observed task's forwards not already declared (by identity) are appended. New tasks appended unless in `Drop`. Workspace-level keys unchanged from today.
  - `func (m *WorkspacePickerModel) Decisions() workspace.MergeDecisions` — `Observed` = every listed row, `Drop` = unticked rows, `Policy` = ticked rows.
  - `func (a *App) writeWorkspace(ws *workspace.Workspace, d workspace.MergeDecisions, dropped, skipped int, decided workspaceDecided)`

- [ ] **Step 1: Write the failing tests** in `save_test.go`:

```go
func TestMergePickerDecisions(t *testing.T)
// existing A{continue,assigned}, B{continue,assigned,["-L 3000"]}; observed Tasks=[A{fresh,any}]
// d = {Observed:{A,B}, Drop:{B}, Policy:{A}} → out.Tasks == [A{fresh,any}]
func TestMergeAddOnlyKeepsDeclaredForwards(t *testing.T)
// existing A{no,any,["-L 3000"]}; observed A{continue,assigned,["-L 127.0.0.1:8080:127.0.0.1:8080"]}; d = {}
// → A{no,any,["-L 3000","-L 127.0.0.1:8080:127.0.0.1:8080"]}
func TestMergeAddOnlySkipsAnIdenticalForward(t *testing.T)
// existing A ["-L 3000"]; observed A ["-L 127.0.0.1:3000:127.0.0.1:3000"]; d = {} → A has one forward
```

Update the existing `TestMergeKeepsUnobservedBlocksAndHandEditedPolicy`, `TestMergeClearsForwardsOfAnObservedTask` (pass `Observed`), `TestMergeWithNoExistingWorkspace`, `TestMergeSSHGateway` to the new signature without changing what they assert. In `workspacepicker_test.go` add `TestWorkspacePickerDecisions`: two declared rows, one unticked → `Drop` holds it, `Policy` holds the other, `Observed` holds both.

- [ ] **Step 2: Run** `go test ./cli/workspace ./tui -run 'Merge|Decisions'` — expect FAIL.
- [ ] **Step 3: Implement** `MergeDecisions`/`Merge`; `Decisions()`; `writeWorkspace` takes `d`. Callers: picker → `a.workspacePicker.Decisions()` (captured before `Close`); `--all` and the CLI → `workspace.MergeDecisions{}`. In the CLI, delete the `observed` map and the "named but forward-less: clear its forwards" branch; `--task` only narrows the registry query now.
- [ ] **Step 4: Update the table text:** `workspace save` Notes become: records every task the registry reports a forward for (`--task` narrows it); ADDS forwards not yet declared and never removes one — drop one with `workspace forward rm`; an existing block's resume/runner are never overwritten; in-process forwards are skipped with a count. `--task` Help: `record only this task (32 hex); omitted = every task the registry reports a forward for`. Run `go generate ./cli/verb`.
- [ ] **Step 5: Run** `go test ./cli/... ./tui ./cmd/...` — expect PASS.
- [ ] **Step 6: Commit** `fix(workspace): the picker's policy and unticked rows reach the file; saves add forwards, never remove`.

---

### Task 3: `planForwards` compares by identity; a hand-started forward satisfies a declaration

**Files:**
- Modify: `tui/workspace_reconcile.go` (whole file, 74 lines)
- Test: `tui/workspace_reconcile_test.go`

**Interfaces:**
- Consumes: `workspace.ForwardIdentity`, `workspace.ForwardKey`.
- Produces: `func sessionIdentity(s *PortForwardSession) (workspace.ForwardKey, bool)` — `ForwardIdentity(s.Direction.flag() + " " + s.Spec)`; `false` for `ForwardLink` or a parse failure. `planForwards` keeps its signature; `forwardKey` and `splitForwardValue` are deleted.

Rule: a workspace-owned session for `taskID` whose identity is not declared → `Stop`. A declared value is in `Start` unless ANY session for `taskID` (owned or not) has its identity. A hand-started session is never in `Stop` and its `FromWorkspace` is not changed.

- [ ] **Step 1: Write the failing tests**

```go
func TestPlanForwardsMatchesByIdentity(t *testing.T)
// owned session {TaskID: A, Direction: ForwardLocal, Spec: "3000", FromWorkspace: true}
// declared ["-L 127.0.0.1:3000:127.0.0.1:3000"] → Start empty, Stop empty
func TestPlanForwardsHandStartedSatisfies(t *testing.T)
// hand session {Spec: "3000", FromWorkspace: false}; declared ["-L 3000"] → Start empty, Stop empty, FromWorkspace false
func TestPlanForwardsLocalAndRemoteStayDistinct(t *testing.T)
// owned -R session "3000"; declared ["-L 3000"] → Start ["-L 3000"], Stop [that session]
```

- [ ] **Step 2: Run** `go test ./tui -run PlanForwards` — expect the first two FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./tui -run 'PlanForwards|Workspace'` — expect PASS (the existing reconcile tests included).
- [ ] **Step 5: Commit** `fix(tui): reconcile forwards by identity, not spelling`.

---

### Task 4: The TUI re-reads the config at each workspace operation

**Files:**
- Modify: `tui/app.go` (field + `New`, around `:137-152`, `:335-358`)
- Modify: `tui/workspace.go` (`applyWorkspace`, SubApply `:215-232`, SubLs/SubShow/SubRm `:273-328`, `saveWorkspace` `:366`, `writeWorkspace`)
- Test: create `tui/workspace_reload_test.go`

**Interfaces:**
- Produces:
  - App field `loadWorkspaceFile func() (*workspace.File, string, error)`; `New` sets it to `func() (*workspace.File, string, error) { return workspace.Load(cfg.ConfigPath) }`. Tests replace it with `workspace.LoadFrom(path, "", "", false)`.
  - `func (a *App) reloadWorkspace(op string) bool` — on error appends `ErrorStyle.Render(op + ": " + err.Error())` and returns false, touching nothing. On success: `a.workspaceFile` = result (keep `workspace.New()` semantics when nil at write time), `a.workspacePath` = path when non-empty; if a workspace is installed, replace `a.workspace` with the file's same-name workspace, or uninstall and append `"workspace <name>: no longer in <path>; nothing is installed now"`.

Call sites: first line of SubLs, SubShow, SubRm, SubApply (before the name lookup), `saveWorkspace` (before the request), `writeWorkspace` (before `existing` is read), and `applyWorkspace` (covers start and reconnect; return nil when it fails).

- [ ] **Step 1: Write the failing tests** (temp dir, file written with `os.WriteFile`, `a := New(Config{})`, loader replaced):

```go
func TestReloadSeesAHandEdit(t *testing.T)
// install "dev"; rewrite the file adding "forward = -L 4000" to dev's task block;
// a.runWorkspaceAction(verb.WorkspaceAction{Sub: verb.SubShow}) → a.cmdresult.lines contain "-L 4000"; a.workspace has it
func TestReloadRefusesABrokenFile(t *testing.T)
// install "dev"; overwrite the file with "[workspace dev]\nnope = 1\n"; SubShow → one error line naming the file;
// a.workspace is the same pointer as before
func TestReloadUninstallsAWorkspaceRemovedByHand(t *testing.T)
func TestSaveAfterHandEditKeepsIt(t *testing.T)
// hand edit adds a task block; a.writeWorkspace(..., workspace.MergeDecisions{}, ...) → the block is still in the file
```

- [ ] **Step 2: Run** `go test ./tui -run Reload` (under the `env -u …` prefix) — expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./tui` — expect PASS.
- [ ] **Step 5: Commit** `fix(tui): re-read .harness/config at every workspace operation`.

---

### Task 5: Picker rows start from the file, and a save refuses a file that changed under it

**Files:**
- Modify: `tui/workspacepicker.go` (`workspacePickerRow`, `Open` `:123-228`, `View` `:520-581`)
- Modify: `tui/workspace.go` (`finishWorkspaceSave` `:380-415`, `commitWorkspacePicker`)
- Modify: `tui/app.go` (one field)
- Test: `tui/workspacepicker_test.go`, `tui/workspace_reload_test.go`

**Interfaces:**
- Consumes: `Workspace.Declares` (Task 1), `reloadWorkspace` (Task 4).
- Produces: row field `UnkeptRunning int`; App field `workspacePickerBase []byte` = `workspace.Block(existing)` at open (nil when absent).

Seeding: a declared row's `Forwards` = the block's declared values; `UnkeptRunning` = registry forwards for that task that the block does not declare (by identity). A not-yet-declared row is seeded from the registry as today. View appends `  +N running, not kept` when `UnkeptRunning > 0`.

Commit: `reloadWorkspace("workspace save")`; if `workspace.Block` of the current target differs from `workspacePickerBase`, append `ErrorStyle.Render("workspace save: " + name + " changed in " + path + " while the picker was open; nothing written — reopen `workspace save " + name + "`")` and write nothing.

- [ ] **Step 1: Write the failing tests**

```go
func TestWorkspacePickerSeedsDeclaredForwards(t *testing.T)
// existing task A forwards ["-L 3000"], registry map empty → row A Forwards ["-L 3000"]; Decisions+Result keep it
func TestWorkspacePickerCountsUnkeptRunning(t *testing.T)
// declared ["-L 3000"]; registry A ["-L 127.0.0.1:3000:127.0.0.1:3000", "-L 127.0.0.1:9000:127.0.0.1:9000"] → UnkeptRunning == 1;
// View contains "+1 running, not kept"
func TestPickerSaveRefusedWhenFileChanged(t *testing.T)
// open the picker for "dev" via a.finishWorkspaceSave(nil); edit dev's block on disk; a.commitWorkspacePicker()
// → file bytes unchanged from the edit; cmdresult has "changed in"
```

- [ ] **Step 2: Run** `go test ./tui -run 'Picker'` — expect the new ones to FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./tui` — expect PASS.
- [ ] **Step 5: Commit** `fix(tui): picker rows start from the file, and a stale picker does not write`.

---

### Task 6: `w` in the forwards pane, the `ws` column, and the shared keep routine

**Files:**
- Create: `tui/workspace_keep.go`
- Modify: `tui/portforward.go` (`portForwardInfoRow` `:436-458`, `NewForwardsModal` `:493-519`, `ApplySnapshot`, `ApplyEvent`, `View` footer `:673`)
- Modify: `tui/keys.go` (`modalKeyMap`/`modalKeys`: `ForwardKeep: "w"`)
- Modify: `tui/overlays.go` (`inForwardsModal` `:211-253`)
- Modify: `tui/app.go` (where `ForwardsSnapshotMsg` and forward status events reach `a.forwardsModal`; where `f` opens the pane)
- Test: create `tui/workspace_keep_test.go`

**Interfaces:**
- Consumes: Task 1 helpers, `reloadWorkspace`, `writeWorkspace`'s save path.
- Produces:
  - `func (a *App) keepForward(op, wsName, taskID, value string, keep bool) error` — reload; refuse an absent `wsName` (`"<op>: no workspace named <wsName>"`); `AddForward` / `RemoveForward` on a copy of the workspace; `File.Set` + `Save`; when `wsName` is the installed workspace, set (keep) or clear (drop) `FromWorkspace` on every session of `taskID` whose `sessionIdentity` equals `value`'s; install the written workspace. Lines (exact):
    - `workspace <ws>: kept <value> on <task8>`; suffix ` — held by another client; the next apply here will try to bind it too` when keep adopted nothing and `wsName` is installed
    - `workspace <ws>: <value> on <task8> was already kept`
    - `workspace <ws>: no longer keeping <value> on <task8> (still running; x stops it)`
    - `workspace <ws>: <value> on <task8> was not kept`
    where `<task8>` is `pfShortID(taskID)`.
  - `func (a *App) toggleKeepSelected() tea.Cmd` — the `w` handler.
  - `func (a *App) forwardMarks(fs []protocol.PortForwardInfo) map[uint64]string` — `"kept"` / `"-"` / `""` (no workspace installed, or `PortForwardConfigSpec` not ok).
  - `func (m *ForwardsModal) SetMarks(marks map[uint64]string)`; `portForwardInfoRow(fi *protocol.PortForwardInfo, mark string) table.Row`.

`toggleKeepSelected`: no installed workspace → `WarnStyle.Render("keep: no workspace installed — run `workspace apply <name>` first")`; row not savable → `WarnStyle.Render(fmt.Sprintf("keep: forward %d cannot be written down (in-process or ip link)", id))`; otherwise `reloadWorkspace("keep")`, decide by `a.workspace.Declares(task, value)` AFTER the reload, call `keepForward`, then recompute marks. Marks are recomputed (with a reload) when the pane opens, on `r`, and on every snapshot/event applied to the pane. Column `{Title: "ws", Width: 5}` goes immediately before `origin`. Footer: `t: tap · r: refresh · x: kill · w: keep · Esc: close`.

- [ ] **Step 1: Write the failing tests** (temp-file loader as in Task 4; installed `dev` with an empty block for task A; `a.activeForwards[1] = &PortForwardSession{ID: 1, TaskID: A, Direction: ForwardLocal, Spec: "3000", ForwardID: 7}`; `a.forwardsModal.Open(); a.forwardsModal.ApplySnapshot([]protocol.PortForwardInfo{{ForwardId: 7, TaskId: A, Direction: Local, ClientEndpoint: OsSocket, BindAddr: "127.0.0.1", BindPort: 3000, TargetHost: "127.0.0.1", TargetPort: 3000}})`; press with `a.inForwardsModal(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})`):

```go
func TestKeepWritesAndAdopts(t *testing.T)
// after w: file's dev task A block has "forward = -L 127.0.0.1:3000:127.0.0.1:3000"; session 1 FromWorkspace true;
// line "workspace dev: kept -L 127.0.0.1:3000:127.0.0.1:3000 on " + pfShortID(A)
func TestUnkeepReleasesAndKeepsTheBlock(t *testing.T)
// w twice: forward line gone, "[workspace dev task A]" still present, FromWorkspace false, "(still running; x stops it)"
func TestKeepWithoutALocalSessionSaysSo(t *testing.T)
// no activeForwards → line has "held by another client"
func TestKeepRefusesAnInProcessRow(t *testing.T)
// ClientEndpoint InProcess → file unchanged, "cannot be written down"
func TestKeepRefusesALinkRow(t *testing.T)
// Direction Link → file unchanged, "cannot be written down"
func TestKeepWithNoWorkspaceInstalled(t *testing.T)
func TestForwardMarksArePerTask(t *testing.T)
// same spec on tasks A and B, only A declared → marks {idA: "kept", idB: "-"}
func TestKeepDecidesFromTheFileNotTheColumn(t *testing.T)
// marks computed while undeclared ("-"); the file is then edited by hand to declare the row;
// w → the line says "no longer keeping" and the file no longer declares it
```

- [ ] **Step 2: Run** `go test ./tui -run 'Keep|ForwardMarks'` — expect FAIL.
- [ ] **Step 3: Implement.** Run `go test ./tui -run Keys` too: the keymap disjointness test must stay green with `w`.
- [ ] **Step 4: Run** `go test ./tui` — expect PASS.
- [ ] **Step 5: Commit** `feat(tui): keep or drop one forward from the forwards pane`.

---

### Task 7: Tab in the start modal keeps the forward once it is registered

**Files:**
- Modify: `tui/portforward.go` (`PortForwardModal` `:47-111`)
- Modify: `tui/overlays.go` (`inPortForwardModal` `:771-797`)
- Modify: `tui/app.go` (the `p`/`b` open sites; `PortForwardStartedMsg` `:1491`, `PortForwardRegisteredMsg`, `PortForwardStoppedMsg` handlers; one field)
- Test: `tui/workspace_keep_test.go`

**Interfaces:**
- Consumes: `keepForward` (Task 6), `ForwardKey.Value` (Task 1).
- Produces: `func (m *PortForwardModal) SetKeepTarget(name string)`, `ToggleKeep()`, `Keep() bool`; App field `pendingKeep map[int]string` (client session id → workspace name).

Modal: after `OpenMode`, the App calls `SetKeepTarget(installed name or "")`; `OpenMode` resets `keep` to false. View adds a line under the input, not in link mode: `[x] keep in workspace <name>  (Tab)` / `[ ] keep in workspace <name>  (Tab)`, or `(no workspace installed)` when the target is empty. Tab toggles only with a target and outside link mode.

Enter with keep on: record `pendingKeep[a.nextForwardID] = name` with the id the forward is started under. Write when the id gets a forward id — `PortForwardRegisteredMsg` (`-L`) or `PortForwardStartedMsg` with `ForwardID != 0` (`-R`): delete the entry, build the value with `ForwardIdentity(dir.flag()+" "+spec)` then `.Value()` (a port-0 error is appended as `keep: <err>`), and call `keepForward("keep", name, taskID, value, true)`, which adopts the session. `PortForwardStoppedMsg` for a pending id deletes it and writes nothing.

- [ ] **Step 1: Write the failing tests**

```go
func TestStartModalKeepWritesOnRegistered(t *testing.T)
// installed dev; open -L modal for A; Tab; type "3000"; Enter; feed a.Update(PortForwardStartedMsg{ID: id, TaskID: A, Direction: ForwardLocal, Spec: "3000"})
// then a.Update(PortForwardRegisteredMsg{ID: id, ForwardID: 9}) → file has the long form; session FromWorkspace true
func TestStartModalKeepNothingWhenItFails(t *testing.T)
// same, then PortForwardStoppedMsg{ID: id} with no Registered → file unchanged
func TestStartModalTabWithoutWorkspace(t *testing.T)
// no install → View contains "(no workspace installed)"; Tab leaves Keep() false
func TestStartModalNoKeepLineInLinkMode(t *testing.T)
```

- [ ] **Step 2: Run** `go test ./tui -run StartModal` — expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./tui` — expect PASS.
- [ ] **Step 5: Commit** `feat(tui): keep a forward from the start modal`.

---

### Task 8: `workspace forward add/rm` on CLI and TUI

**Files:**
- Modify: `cli/verb/table.go` (after `workspace detach`, `:1121-1136`) + `go generate ./cli/verb`
- Modify: `cmd/harness-cli/workspace.go`, `cmd/harness-cli/dispatch.go` (`:1075-1086`)
- Modify: `tui/workspace.go` (`runWorkspaceAction`), `tui/dispatch.go` (`:672-677`)
- Test: `tui/cmdline_workspace_test.go`, create `cmd/harness-cli/workspace_forward_test.go`

**Interfaces:**
- Consumes: Task 1 helpers; `keepForward` (Task 6) in the TUI.
- Produces: two table entries, paths `{"workspace","forward","add"}` / `{"workspace","forward","rm"}`, `Action: "WorkspaceAction"`, `Const: {"Sub": "forward-add"}` / `{"Sub": "forward-rm"}` (generated `verb.SubForwardAdd` / `verb.SubForwardRm`), `CmdlineSurfaces: CLI | TUI`, `Args: name (ArgString, Field "Name"), task-id (ArgTaskID, Field "TaskID")`, `Flags: L, R (FlagString, Custom: argListValue, Fields "L", "R", Help "a forward value's spec (repeatable)")`, `AtLeastOne: {L, R}` with reason `"nothing to add or remove"`, `ModalSurfaces: {TUI, "tui/portforward.go:ForwardsModal"}`, Notes: `edits .harness/config only; starts and stops nothing`. Generated methods `WorkspaceForwardAdd` / `WorkspaceForwardRm` on both dispatchers.

CLI: load; `<name>` absent → `fmt.Errorf("no workspace named %q in %s — create it with `workspace save %s`", name, path, name)`; each `-L x` / `-R x` becomes the value `"-L x"` / `"-R x"`, validated through `ParseForwardValue`, applied with `AddForward` / `RemoveForward`; one `Save`; print `workspace <name>: <n> added, <m> already present` (add) or `<n> removed, <m> not declared` (rm). TUI: the same refusal, then `keepForward("workspace forward add"/"rm", name, task, value, keep)` per value — it adopts/releases only when `name` is the installed workspace (Task 6 already gates on that).

Before editing, walk the `surface-parity-checklist` skill's items for a new verb in an existing family and record each verdict in the commit message body.

- [ ] **Step 1: Write the failing tests**

```go
// cmd/harness-cli/workspace_forward_test.go
func TestWorkspaceForwardParse(t *testing.T)
// verb.ParseCmdWorkspaceForwardAdd(verb.CLI, []string{"dev", taskA, "-L", "3000", "-R", "8080"}, nil) → L ["3000"], R ["8080"];
// []string{"dev", taskA} → error
func TestWorkspaceForwardAddWritesTheFile(t *testing.T)
// workspaceCfgPath = temp file with [workspace dev]; runWorkspaceAction(ctx, add with L ["3000"], nil) → file has "forward = -L 3000"
// under [workspace dev task <taskA>] with resume = continue; running it again prints "0 added, 1 already present"
func TestWorkspaceForwardAddRefusesAMissingWorkspace(t *testing.T)
// tui/cmdline_workspace_test.go: extend TestParseWorkspace with
// {"workspace forward add dev " + wsTaskA + " -L 3000", "forward-add", "dev"}
func TestTUIWorkspaceForwardAddOnOtherWorkspaceAdoptsNothing(t *testing.T)
// installed dev; session for A "3000"; file also has [workspace other]; "workspace forward add other A -L 3000"
// → other gets the line, session FromWorkspace stays false
```

- [ ] **Step 2: Run** `go test ./cmd/harness-cli ./tui -run WorkspaceForward` — expect FAIL.
- [ ] **Step 3: Implement**, then `go generate ./cli/verb`.
- [ ] **Step 4: Run** the full suite with the Global Constraints command — expect PASS (the usage/help completeness tests included).
- [ ] **Step 5: Commit** `feat: workspace forward add/rm on CLI and TUI`.

---

### Task 9: Drive it against a dummy harness

Pitfall 13: keys, argv and the modal cannot be reached by a Go test at the layer the operator enters.

- [ ] **Step 1:** Load the `dummy-harness` skill and start an instance per it; `make build` into the worktree's `bin/`.
- [ ] **Step 2:** With a temp `--config`, start a session, `p` → `3000` → Enter → `f`: the row's `ws` cell is `-`; `w`: `kept`, and the config file has the long form under that task.
- [ ] **Step 3:** `p` → Tab → `3001` → Enter: the file gains `3001` once the forward is listed.
- [ ] **Step 4:** Edit the file by hand (add `forward = -L 3002` to the block), run `workspace apply`: `3002` starts, `3000`/`3001` keep their forward ids (`forward ls`).
- [ ] **Step 5:** `bin/harness-cli --config <file> workspace forward rm <name> <task> -L 3001` in the help's spelling; in the TUI `f` → `r`: `3001` shows `-` and is still running.
- [ ] **Step 6:** `workspace save <name>`, cycle one row to `fresh`, untick another, Enter: `workspace show` matches the screen.
- [ ] **Step 7:** Tear the dummy harness down per the skill. Report what was observed for each step; keep any screenshots and report their paths.
