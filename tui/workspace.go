package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SetWorkspace installs the workspace an apply works from. Nil disables every
// apply path.
func (a *App) SetWorkspace(ws *workspace.Workspace) { a.workspace = ws }

// ArmWorkspace asks for one apply on the next snapshot. The snapshot is where
// an apply must run — deciding whether a task needs resuming requires its
// status — and arming is how the three entry points (first join, rejoin, and
// the `workspace apply` command) all reach that one place.
func (a *App) ArmWorkspace() { a.workspaceArmed = true }

// workspaceWantsResume reports whether this task, as the snapshot shows it, is
// one the workspace should bring back. A task that is alive is left alone,
// which is what keeps a reconnect after a network blip from spawning anything:
// the blip does not end the task, so the snapshot still shows it Running or
// Detached. A server restart does end it — the WAL replay marks an interrupted
// Running task Failed — and that is the case this exists for.
//
// t is nil when the task is not in the visible set at all.
func workspaceWantsResume(t *protocol.TaskInfo, decl workspace.Task) bool {
	if t == nil || decl.Resume == workspace.ResumeNo || decl.Resume == "" {
		return false
	}
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return true
	}
	return false
}

// applyWorkspace reconciles the live client to the installed workspace. Every
// step reports its own outcome and none aborts the others: a port already bound
// must not cost the operator the resume or the grid.
func (a *App) applyWorkspace() tea.Cmd {
	ws := a.workspace
	if ws == nil || a.client == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, decl := range ws.Tasks {
		// a.tasksByID is map[string]protocol.TaskInfo — VALUES, not pointers. A
		// missing task reads back as the zero TaskInfo, whose Status is Queued,
		// so "absent" has to be carried separately rather than read off it.
		var info *protocol.TaskInfo
		if t, ok := a.tasksByID[decl.ID]; ok {
			info = &t
		}

		resuming := workspaceWantsResume(info, decl)
		switch {
		case resuming:
			a.cmdresult.Append(fmt.Sprintf("workspace %s: resuming %s (%s, %s)",
				ws.Name, pfShortID(decl.ID), decl.Resume, decl.Runner))
			cmds = append(cmds, DoResumeSessionDetached(a.client, info.AssignedTo,
				decl.Runner == workspace.RunnerAny, decl.ID, a.authority(),
				decl.Resume == workspace.ResumeContinue, string(info.AgentProfile),
				TermSize{Rows: uint16(a.height), Cols: uint16(a.width)}))
		case info == nil && decl.Resume != workspace.ResumeNo && decl.Resume != "":
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf(
				"workspace %s: task %s is not in the visible task set — not resumed",
				ws.Name, pfShortID(decl.ID))))
		}

		// A task being resumed gets NO forwards in this pass. tea.Batch runs its
		// commands concurrently, so a forward registered here would race the
		// resume that makes the task exist and lose — the server answers "no such
		// task (id unknown or task not running)", which is what a live reconnect
		// after a server restart actually printed. The SessionStartedMsg handler
		// re-arms, and the snapshot that follows reconciles with the task alive.
		if !resuming {
			cmds = append(cmds, a.applyWorkspaceForwards(ws.Name, decl)...)
		}
	}

	if ws.GridSet {
		if cmd, err := a.workspaceGridCmd(ws); err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("workspace %s: %v", ws.Name, err)))
		} else if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
		// An apply that had nothing to do must SAY so. Reconciling to a no-op is
		// the healthy steady state and the expected answer to `workspace apply`
		// after fixing a port conflict that turned out not to need fixing —
		// silence there reads as a command that did not run.
		a.cmdresult.Append(fmt.Sprintf("workspace %s: already reconciled (nothing to start, resume or stop)", ws.Name))
		return nil
	}
	return tea.Batch(cmds...)
}

// applyWorkspaceForwards stops the workspace-owned forwards this task no longer
// declares and starts the declared ones that are not running.
func (a *App) applyWorkspaceForwards(wsName string, decl workspace.Task) []tea.Cmd {
	plan := planForwards(decl.Forwards, a.activeForwards, decl.ID)
	for _, s := range plan.Stop {
		a.cmdresult.Append(fmt.Sprintf("workspace %s: stopping %s %s on %s (no longer declared)",
			wsName, s.Direction.flag(), s.Spec, pfShortID(decl.ID)))
		if s.Cancel != nil {
			s.Cancel()
		}
	}
	var cmds []tea.Cmd
	for _, value := range plan.Start {
		// NOTE two similarly named constant pairs are in play here:
		// workspace.ForwardLocal/ForwardRemote (workspace.ForwardDir, what
		// ParseForwardValue returns) and this package's own pair
		// (ForwardDirection, what a session records). Different types.
		dir, _, _, err := workspace.ParseForwardValue(value)
		if err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("workspace %s: %v", wsName, err)))
			continue
		}
		_, spec, _ := strings.Cut(strings.TrimSpace(value), " ")
		spec = strings.TrimSpace(spec)
		id := a.nextForwardID
		a.nextForwardID++
		// One call per spec: cli.RunForward aborts every spec in a call when
		// any one of them fails to listen, so batching would let a conflict on
		// 3000 take 8080 down with it.
		if dir == workspace.ForwardRemote {
			cmds = append(cmds, DoStartRemoteForward(a.client, decl.ID, spec, id, a.program, true))
		} else {
			cmds = append(cmds, DoStartPortForward(a.client, decl.ID, spec, id, a.program, true))
		}
	}
	return cmds
}

// workspaceGridCmd turns the workspace's grid value into the openGrid call the
// g / z / Z keys make. The value is the `grid` command's argument string and is
// parsed by that command's own parser.
func (a *App) workspaceGridCmd(ws *workspace.Workspace) (tea.Cmd, error) {
	args, err := ws.GridArgs()
	if err != nil {
		return nil, fmt.Errorf("grid: %w", err)
	}
	mode, anchor, ids, err := cli.ParseGridArgs(args)
	if err != nil {
		return nil, err
	}
	return a.openGrid(mode, anchor, ids), nil
}

// gridArgsString renders the open grid's selection back into the `grid`
// command's argument string. It reads the selection App recorded at openGrid
// rather than GridModel's scope field, which holds a display label with a
// truncated anchor id and cannot be parsed back.
func (a *App) gridArgsString() string {
	return cli.GridArgsString(a.gridSelMode, a.gridSelAnchor, a.gridSelIDs)
}

// runWorkspaceAction handles the `workspace` verb. save and apply act on the
// live client; ls and show read the file.
func (a *App) runWorkspaceAction(v WorkspaceAction) tea.Cmd {
	switch v.Sub {
	case "apply":
		if v.Name != "" {
			ws, ok := a.workspaceFile.Workspace(v.Name)
			if !ok {
				a.cmdresult.Append(ErrorStyle.Render("workspace apply: no workspace named " + v.Name))
				return nil
			}
			if err := ws.Validate(); err != nil {
				a.cmdresult.Append(ErrorStyle.Render(err.Error()))
				return nil
			}
			a.SetWorkspace(ws)
		}
		if a.workspace == nil {
			a.cmdresult.Append(WarnStyle.Render("workspace apply: no workspace installed (start with --workspace <name>, or name one here)"))
			return nil
		}
		return a.applyWorkspace()

	case "ls":
		names := a.workspaceFile.Names()
		if len(names) == 0 {
			a.cmdresult.Append("workspace ls: no workspaces in " + a.workspaceConfigPath())
			return nil
		}
		for _, n := range names {
			mark := "  "
			if a.workspace != nil && a.workspace.Name == n {
				mark = "* "
			}
			a.cmdresult.Append(mark + n)
		}
		return nil

	case "show":
		name := v.Name
		if name == "" && a.workspace != nil {
			name = a.workspace.Name
		}
		ws, ok := a.workspaceFile.Workspace(name)
		if !ok {
			a.cmdresult.Append(ErrorStyle.Render("workspace show: no workspace named " + name))
			return nil
		}
		for _, line := range strings.Split(strings.TrimRight(string(workspace.Block(ws)), "\n"), "\n") {
			a.cmdresult.Append(line)
		}
		return nil

	case "save":
		return a.saveWorkspace(v.Name, v.All)
	}
	return nil
}

// workspaceDeclares reports whether the installed workspace names this task.
// Used to decide whether a session start belongs to the workspace: an operator
// running `session new -d` by hand must not trigger an apply.
func (a *App) workspaceDeclares(taskID string) bool {
	if a.workspace == nil || taskID == "" {
		return false
	}
	for _, t := range a.workspace.Tasks {
		if t.ID == taskID {
			return true
		}
	}
	return false
}

// workspaceConfigPath is where a save would write and where a load came from.
func (a *App) workspaceConfigPath() string {
	if a.workspacePath != "" {
		return a.workspacePath
	}
	return workspace.DefaultPath
}

// saveWorkspace starts a save by asking the server which forwards exist. The
// write happens in finishWorkspaceSave when the snapshot arrives.
//
// The registry is the source rather than a.activeForwards, so a forward the
// operator established from a `harness-cli forward` in another terminal is
// captured too. What must NOT be written is an in-process forward — a raw `t`
// pane, a WebUI preview pin — and cli.PortForwardConfigSpec is where that test
// lives, once, for this path and the CLI's alike.
func (a *App) saveWorkspace(name string, all bool) tea.Cmd {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("workspace save: not connected"))
		return nil
	}
	a.workspaceSaveName = name
	a.workspaceSaveAll = all
	return DoListForwards(a.client, false)
}

// finishWorkspaceSave has the forward snapshot a save was waiting for. Unless
// --all was given it OPENS THE PICKER rather than writing: which tasks belong
// in a workspace is the operator's statement, and every automatic rule tried
// before this was a guess wearing a criterion.
func (a *App) finishWorkspaceSave(forwards []protocol.PortForwardInfo) {
	name := a.workspaceSaveName
	all := a.workspaceSaveAll
	a.workspaceSaveName, a.workspaceSaveAll = "", false

	byTaskFwd := map[string][]string{}
	skippedFwd := 0
	for i := range forwards {
		spec, ok := cli.PortForwardConfigSpec(&forwards[i])
		if !ok {
			skippedFwd++
			continue
		}
		id := FormatTaskID(forwards[i].TaskId)
		byTaskFwd[id] = append(byTaskFwd[id], spec)
	}

	if !all {
		existing, _ := a.workspaceFile.Workspace(name)
		a.workspacePicker.Open(name, gridLiveTasks(a.visibleTasks()),
			a.resumableTasks(), existing, byTaskFwd)
		a.workspacePicker.SetSize(a.width, a.height)
		if skippedFwd > 0 {
			a.cmdresult.Append(fmt.Sprintf(
				"workspace %s: %d in-process forward(s) cannot be written down and are not listed", name, skippedFwd))
		}
		return
	}
	a.writeWorkspaceAll(name, forwards)
}

// writeWorkspaceAll is `workspace save <name> --all`: every live session, no
// questions. Kept for the scripted case and as the escape hatch when the picker
// would only be pressing enter.
func (a *App) writeWorkspaceAll(name string, forwards []protocol.PortForwardInfo) {
	byTask := map[string]*workspace.Task{}
	var order []string
	add := func(id string) *workspace.Task {
		if t, ok := byTask[id]; ok {
			return t
		}
		t := &workspace.Task{ID: id, Resume: workspace.ResumeContinue, Runner: workspace.RunnerAssigned}
		byTask[id] = t
		order = append(order, id)
		return t
	}
	// --all's rule: every LIVE interactive session, by the same predicate the
	// grid uses, plus every task with a savable forward. It is a defensible
	// default and still only a default — which is why it is not what a bare
	// `workspace save` does any more.
	live := gridLiveTasks(a.visibleTasks())
	for i := range live {
		add(FormatTaskID(live[i].Id))
	}
	skipped := 0
	for i := range forwards {
		spec, ok := cli.PortForwardConfigSpec(&forwards[i])
		if !ok {
			skipped++ // in-process: no local address to write down
			continue
		}
		t := add(FormatTaskID(forwards[i].TaskId))
		t.Forwards = append(t.Forwards, spec)
	}

	// tui.New copies cfg.Server into a.server and cfg.DefaultRepo into
	// a.defaultRepo; the App does not keep the Config itself.
	ws := &workspace.Workspace{Name: name, ServerCID: a.server, Repo: a.defaultRepo}
	if a.grid.IsOpen() {
		ws.Grid, ws.GridSet = a.gridArgsString(), true
	}
	for _, id := range order {
		ws.Tasks = append(ws.Tasks, *byTask[id])
	}

	// Observed = every task this save could say something about: the ones just
	// enumerated, plus the ones the installed workspace already declares.
	// Including the declared ones is what lets a save CLEAR a task's forwards
	// after the operator stopped them — the registry reports presence, never
	// absence. --all drops nothing: it made no per-task decision.
	observed := map[string]bool{}
	for _, id := range order {
		observed[id] = true
	}
	if a.workspace != nil {
		for _, t := range a.workspace.Tasks {
			observed[t.ID] = true
		}
	}
	a.writeWorkspace(ws, observed, 0, skipped)
}

func countWorkspaceForwards(ws *workspace.Workspace) int {
	n := 0
	for _, t := range ws.Tasks {
		n += len(t.Forwards)
	}
	return n
}

// commitWorkspacePicker writes the workspace the picker was filled in for.
//
// The picker's LISTED set is what the save observed, so an unticked row is a
// decision to drop that task's block — not the merge's "I did not look at this
// one, keep it".
func (a *App) commitWorkspacePicker() tea.Cmd {
	name := a.workspacePicker.Name()
	tasks, observed := a.workspacePicker.Result()
	dropped := len(a.workspacePicker.ExcludedIDs())
	a.workspacePicker.Close()

	ws := &workspace.Workspace{
		Name: name, ServerCID: a.server, Repo: a.defaultRepo, Tasks: tasks,
	}
	if a.grid.IsOpen() {
		ws.Grid, ws.GridSet = a.gridArgsString(), true
	}
	a.writeWorkspace(ws, observed, dropped, 0)
	return nil
}

// writeWorkspace merges ws into the file and reports what it wrote. Both save
// paths — the picker and --all — end here, so the merge rules and the result
// line cannot differ between them.
func (a *App) writeWorkspace(ws *workspace.Workspace, observed map[string]bool, dropped, skipped int) {
	f := a.workspaceFile
	if f == nil {
		f = workspace.New()
		a.workspaceFile = f
	}
	existing, _ := f.Workspace(ws.Name)
	merged := workspace.Merge(existing, ws, observed)
	f.Set(merged)

	path := a.workspaceConfigPath()
	a.workspacePath = path
	if err := f.Save(path); err != nil {
		a.cmdresult.Append(ErrorStyle.Render("workspace save: " + err.Error()))
		return
	}
	line := fmt.Sprintf("workspace %s saved to %s: %d task(s), %d forward(s)",
		merged.Name, path, len(merged.Tasks), countWorkspaceForwards(merged))
	// Zeros are printed: "0 dropped" and a missing clause are different
	// statements, and an operator who unticked a row needs to see it counted.
	line += fmt.Sprintf(", %d dropped, %d in-process skipped", dropped, skipped)
	a.cmdresult.Append(OKStyle.Render(line))
	a.SetWorkspace(merged)
}

// resumableTasks are the tasks a workspace could bring back: the ones the r/R
// keys would RESUME rather than reattach, most recent first.
//
// The predicate is resumeReattachAction's own verdict rather than a status list
// written out again here — the picker must offer exactly what an apply can
// actually do, and that decision already exists in one place.
func (a *App) resumableTasks() []protocol.TaskInfo {
	var out []protocol.TaskInfo
	for _, t := range a.visibleTasks() {
		ti := t
		if resumeReattachAction(&ti, true).Kind == actionResume {
			out = append(out, ti)
		}
	}
	// Most recent first: the task just finished is the one most likely wanted,
	// and it is what the listing cap keeps when there are more than fit.
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}
