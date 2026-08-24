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
