package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// keepForward declares value for taskID in workspace wsName (keep) or drops
// the declaration (!keep), re-reading the file first so a hand edit is never
// overwritten. Every one-forward edit — `w`, the start modal's keep, and
// `workspace forward add/rm` — goes through here, so they cannot differ in
// what they write or what they report.
//
// When wsName is the INSTALLED workspace, matching local sessions are adopted
// (keep) or released (!keep). Adopting is what stops the next apply binding a
// declared forward a second time; releasing is what stops an un-keep turning
// into a kill on the next reconnect. For any other workspace neither happens:
// FromWorkspace names no workspace, and planForwards reconciles against the
// installed one only.
func (a *App) keepForward(op, wsName, taskID, value string, keep bool) {
	if !a.reloadWorkspace(op) {
		return
	}
	cur, ok := a.workspaceFile.Workspace(wsName)
	if !ok {
		a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("%s: no workspace named %s", op, wsName)))
		return
	}
	ws := *cur
	ws.Tasks = make([]workspace.Task, len(cur.Tasks))
	for i, t := range cur.Tasks {
		t.Forwards = append([]string(nil), t.Forwards...)
		ws.Tasks[i] = t
	}
	var changed bool
	var err error
	if keep {
		changed, err = ws.AddForward(taskID, value)
	} else {
		changed, err = ws.RemoveForward(taskID, value)
	}
	if err != nil {
		a.cmdresult.Append(ErrorStyle.Render(op + ": " + err.Error()))
		return
	}
	if changed {
		a.workspaceFile.Set(&ws)
		path := a.workspaceConfigPath()
		if err := a.workspaceFile.Save(path); err != nil {
			a.cmdresult.Append(ErrorStyle.Render(op + ": " + err.Error()))
			return
		}
	}

	installed := a.workspace != nil && a.workspace.Name == wsName
	adopted := 0
	if installed {
		written, _ := a.workspaceFile.Workspace(wsName)
		a.SetWorkspace(written)
		if k, err := workspace.ForwardIdentity(value); err == nil {
			for _, s := range a.activeForwards {
				if sk, ok := sessionIdentity(s); ok && s.TaskID == taskID && sk == k {
					s.FromWorkspace = keep
					adopted++
				}
			}
		}
	}

	subject := fmt.Sprintf("%s on %s", value, pfShortID(taskID))
	switch {
	case keep && changed:
		line := fmt.Sprintf("workspace %s: kept %s", wsName, subject)
		if installed && adopted == 0 {
			line += " — held by another client; the next apply here will try to bind it too"
		}
		a.cmdresult.Append(OKStyle.Render(line))
	case keep:
		a.cmdresult.Append(fmt.Sprintf("workspace %s: %s was already kept", wsName, subject))
	case changed:
		a.cmdresult.Append(OKStyle.Render(fmt.Sprintf(
			"workspace %s: no longer keeping %s (still running; x stops it)", wsName, subject)))
	default:
		a.cmdresult.Append(fmt.Sprintf("workspace %s: %s was not kept", wsName, subject))
	}
}

// toggleKeepSelected is `w` in the forwards pane. The direction is decided
// from the file as it is NOW, not from the `ws` cell the pane last drew, so a
// hand edit landing between the two cannot invert the action silently — and
// the result line names which way it went.
func (a *App) toggleKeepSelected() tea.Cmd {
	fi, ok := a.forwardsModal.Selected()
	if !ok {
		return nil
	}
	if a.workspace == nil {
		a.cmdresult.Append(WarnStyle.Render("keep: no workspace installed — run `workspace apply <name>` first"))
		return nil
	}
	value, ok := cli.PortForwardConfigSpec(fi)
	if !ok {
		a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf(
			"keep: forward %d cannot be written down (in-process or ip link)", fi.ForwardId)))
		return nil
	}
	if !a.reloadWorkspace("keep") {
		return nil
	}
	if a.workspace == nil {
		return nil // reloadWorkspace already said it was uninstalled
	}
	taskID := FormatTaskID(fi.TaskId)
	a.keepForward("keep", a.workspace.Name, taskID, value, !a.workspace.Declares(taskID, value))
	a.refreshForwardMarks()
	return nil
}

// forwardMarks is each row's `ws` cell: "kept" when the installed workspace
// declares the forward for that row's task, "-" when it does not, and empty
// when the question does not apply — nothing installed, or a row no `forward`
// line can describe.
func (a *App) forwardMarks(fs []protocol.PortForwardInfo) map[uint64]string {
	marks := make(map[uint64]string, len(fs))
	for i := range fs {
		fi := &fs[i]
		if a.workspace == nil {
			marks[fi.ForwardId] = ""
			continue
		}
		value, ok := cli.PortForwardConfigSpec(fi)
		switch {
		case !ok:
			marks[fi.ForwardId] = ""
		case a.workspace.Declares(FormatTaskID(fi.TaskId), value):
			marks[fi.ForwardId] = "kept"
		default:
			marks[fi.ForwardId] = "-"
		}
	}
	return marks
}

func (a *App) refreshForwardMarks() {
	a.forwardsModal.SetMarks(a.forwardMarks(a.forwardsModal.forwards))
}
