package tui

import (
	"fmt"
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

		switch {
		case workspaceWantsResume(info, decl):
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

		cmds = append(cmds, a.applyWorkspaceForwards(ws.Name, decl)...)
	}

	if ws.GridSet {
		if cmd, err := a.workspaceGridCmd(ws); err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("workspace %s: %v", ws.Name, err)))
		} else if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
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
		return a.saveWorkspace(v.Name)
	}
	return nil
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
func (a *App) saveWorkspace(name string) tea.Cmd {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("workspace save: not connected"))
		return nil
	}
	a.workspaceSaveName = name
	return DoListForwards(a.client, false)
}

// finishWorkspaceSave writes the named workspace from the live client state
// plus the forward snapshot, replacing only that workspace's lines in the file.
func (a *App) finishWorkspaceSave(forwards []protocol.PortForwardInfo) {
	name := a.workspaceSaveName
	a.workspaceSaveName = ""

	// tui.New copies cfg.Server into a.server and cfg.DefaultRepo into
	// a.defaultRepo; the App does not keep the Config itself.
	ws := &workspace.Workspace{Name: name, ServerCID: a.server, Repo: a.defaultRepo}
	if a.grid.IsOpen() {
		ws.Grid, ws.GridSet = a.gridArgsString(), true
	}

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
	if id := a.logs.TaskID(); id != "" {
		add(id)
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
	for _, id := range order {
		ws.Tasks = append(ws.Tasks, *byTask[id])
	}

	f := a.workspaceFile
	if f == nil {
		f = workspace.New()
		a.workspaceFile = f
	}
	f.Set(ws)
	path := a.workspaceConfigPath()
	a.workspacePath = path
	if err := f.Save(path); err != nil {
		a.cmdresult.Append(ErrorStyle.Render("workspace save: " + err.Error()))
		return
	}
	// The skipped count is printed even when it is zero: "0 in-process" and a
	// missing clause are different statements, and the operator wondering where
	// their `t` pane went needs the first one.
	a.cmdresult.Append(OKStyle.Render(fmt.Sprintf(
		"workspace %s saved to %s: %d task(s), %d forward(s), %d in-process skipped",
		name, path, len(ws.Tasks), countWorkspaceForwards(ws), skipped)))
	a.SetWorkspace(ws)
}

func countWorkspaceForwards(ws *workspace.Workspace) int {
	n := 0
	for _, t := range ws.Tasks {
		n += len(t.Forwards)
	}
	return n
}
