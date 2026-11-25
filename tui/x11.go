package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// x11RestartWait bounds how long D waits, after cancelling a live session to
// restart it with X11, for the task to reach a finished status. Past it the
// restart is given up and said so, rather than reopening the task whenever a
// late status happens to arrive.
const x11RestartWait = 30 * time.Second

// x11RestartTimeoutMsg fires x11RestartWait after a restart's cancel.
type x11RestartTimeoutMsg struct{ TaskID string }

// heldX11 reports whether this TUI holds an X11 tunnel for taskID.
func (a *App) heldX11(taskID string) bool {
	return len(selectForwards(a.activeForwards, taskID, ForwardX11)) > 0
}

// `D` gives the selected task X11 (x11OpenAction decides how). $DISPLAY is
// checked before anything that would cost the task: a restart that cannot
// open X11 afterwards must not cancel the agent first.
func (a *App) onX11Open(msg tea.KeyMsg) (tea.Cmd, bool) {
	t := a.tasks.SelectedTask()
	id := a.tasks.SelectedID()
	act := x11OpenAction(t, a.heldX11(id))
	switch act.Kind {
	case actionReattach:
		return DoAttachSession(a.client, id, protocol.AttachMode_Control), true
	case actionNone:
		a.cmdresult.Append(WarnStyle.Render(act.Hint))
		return nil, true
	}
	if _, err := cli.LocalX11Display(); err != nil {
		a.cmdresult.Append(ErrorStyle.Render("D: " + err.Error()))
		return nil, true
	}
	if act.Kind == actionResume {
		return a.openX11Resume(*t), true
	}
	if a.x11Restarts[id] {
		a.cmdresult.Append(WarnStyle.Render("D " + shortTaskID(id) + ": already restarting with X11"))
		return nil, true
	}
	a.x11RestartAsk = id
	a.cmdresult.Append(WarnStyle.Render(x11RestartPrompt(id)))
	return nil, true
}

func x11RestartPrompt(id string) string {
	return fmt.Sprintf("D %s: restart the agent with X11? y restarts (conversation kept; a turn in flight is lost), any other key keeps it running", shortTaskID(id))
}

// inX11RestartConfirm owns the next key while a restart awaits its answer:
// y cancels the session and arms the resume (advanceX11Restarts); anything
// else — q included, which must not quit under a destructive question —
// keeps it running.
func (a *App) inX11RestartConfirm(msg tea.KeyMsg) tea.Cmd {
	id := a.x11RestartAsk
	a.x11RestartAsk = ""
	if msg.String() != "y" && msg.String() != "Y" {
		a.cmdresult.Append(fmt.Sprintf("D %s: kept running; nothing restarted", shortTaskID(id)))
		return nil
	}
	a.x11Restarts[id] = true
	return tea.Batch(DoCancel(a.client, id, id), tea.Tick(x11RestartWait, func(time.Time) tea.Msg {
		return x11RestartTimeoutMsg{TaskID: id}
	}))
}

// advanceX11Restarts reopens, with X11, every task a restart cancelled once
// it has reached a finished status. Called wherever task statuses land.
func (a *App) advanceX11Restarts() tea.Cmd {
	var cmds []tea.Cmd
	for id := range a.x11Restarts {
		t, ok := a.tasksByID[id]
		if !ok {
			continue
		}
		switch t.Status {
		case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
			delete(a.x11Restarts, id)
			cmds = append(cmds, a.openX11Resume(t))
		}
	}
	return tea.Batch(cmds...)
}

// openX11Resume resumes t with X11 on the runner it last ran on, keeping the
// agent's conversation, as r does without X11.
func (a *App) openX11Resume(t protocol.TaskInfo) tea.Cmd {
	n, ok := a.pickX11Display(10)
	if !ok {
		a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("D: no free display number from 10 to %d in this TUI", x11DisplayMax)))
		return nil
	}
	fid := a.allocForwardID()
	return openX11Session(a.client, "", resumeSelectorOpts(t.AssignedTo), nil, FormatTaskID(t.Id), n, fid, a.program,
		a.authority(), false, true, false, string(t.AgentProfile), cli.RunModeFlags{})
}
