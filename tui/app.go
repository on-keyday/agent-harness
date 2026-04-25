package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type focus int

const (
	focusRunners focus = iota
	focusTasks
	focusLogs
	focusCmdline
)

// App is the top-level Bubble Tea Model.
type App struct {
	server      string
	defaultRepo string

	runners   RunnersModel
	tasks     TasksModel
	cmdresult CmdResultModel
	cmdline   textinput.Model

	focus  focus
	width  int
	height int

	// connected mirrors the persistent connection's status (set later by main.go via msgs).
	connected bool

	// status is a one-line message at the top (e.g., "DISCONNECTED — retrying").
	// Reserved for later tasks.
	status string
}

type Config struct {
	Server      string
	DefaultRepo string
}

func New(cfg Config) *App {
	cmd := textinput.New()
	cmd.Prompt = "> "
	cmd.Placeholder = "submit / cancel / prune / clear / help / quit"
	cmd.CharLimit = 1024
	cmd.Width = 60
	a := &App{
		server:      cfg.Server,
		defaultRepo: cfg.DefaultRepo,
		runners:     NewRunners(),
		tasks:       NewTasks(),
		cmdresult:   NewCmdResult(),
		cmdline:     cmd,
		focus:       focusTasks,
		connected:   false,
		status:      "connecting…",
	}
	a.tasks.Focus()
	return a
}

func (a *App) Init() tea.Cmd {
	return textinput.Blink
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.layout()
		return a, nil

	case tea.KeyMsg:
		// Ctrl+C always quits.
		if msg.Type == tea.KeyCtrlC {
			return a, tea.Quit
		}
		// `q` quits when not in the cmdline (cmdline must accept literal 'q').
		if a.focus != focusCmdline && msg.String() == "q" {
			return a, tea.Quit
		}
		// Tab cycles focus.
		switch msg.Type {
		case tea.KeyTab:
			a.cycleFocus(+1)
			return a, nil
		case tea.KeyShiftTab:
			a.cycleFocus(-1)
			return a, nil
		}
		// Cmdline submit.
		if a.focus == focusCmdline && msg.Type == tea.KeyEnter {
			input := a.cmdline.Value()
			a.cmdline.SetValue("")
			act, err := ParseCommand(input, a.defaultRepo)
			if err != nil {
				a.cmdresult.Append(ErrorStyle.Render("error: " + err.Error()))
				return a, nil
			}
			if act == nil {
				return a, nil
			}
			a.cmdresult.Append("> " + input)
			return a.runAction(act)
		}
	}

	// Forward to focused panel.
	var cmd tea.Cmd
	switch a.focus {
	case focusRunners:
		a.runners, cmd = a.runners.Update(msg)
	case focusTasks:
		a.tasks, cmd = a.tasks.Update(msg)
	case focusCmdline:
		a.cmdline, cmd = a.cmdline.Update(msg)
	}
	return a, cmd
}

func (a *App) cycleFocus(delta int) {
	a.runners.Blur()
	a.tasks.Blur()
	a.cmdline.Blur()

	a.focus = focus((int(a.focus) + delta + 4) % 4)

	switch a.focus {
	case focusRunners:
		a.runners.Focus()
	case focusTasks:
		a.tasks.Focus()
	case focusCmdline:
		a.cmdline.Focus()
	}
}

// layout computes per-panel sizes from a.width / a.height. Header 1, runners
// + tasks 10 each, cmdresult 5, cmdline 1, footer 1, plus 4 border rows
// distributed across panels = 22 reserved. Log gets the rest (min 5).
func (a *App) layout() {
	if a.width < 80 || a.height < 24 {
		return
	}
	half := a.width / 2
	a.runners.SetSize(half-2, 10)
	a.tasks.SetSize(a.width-half-2, 10)
	a.cmdresult.SetSize(a.width-2, 5)
	a.cmdline.Width = a.width - 4
}

func (a *App) View() string {
	if a.width < 80 || a.height < 24 {
		return "terminal too small (need at least 80x24)"
	}

	connectedTag := ErrorStyle.Render("DISCONNECTED")
	if a.connected {
		connectedTag = OKStyle.Render("CONNECTED")
	}
	header := HeaderStyle.Render(fmt.Sprintf("harness-tui · %s · %s", a.server, connectedTag))

	runnersView := a.runners.View()
	tasksView := a.tasks.View()
	if a.runners.IsFocused() {
		runnersView = PanelStyleFocused.Render(runnersView)
	} else {
		runnersView = PanelStyle.Render(runnersView)
	}
	if a.tasks.IsFocused() {
		tasksView = PanelStyleFocused.Render(tasksView)
	} else {
		tasksView = PanelStyle.Render(tasksView)
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, runnersView, tasksView)

	// Log placeholder until Task 11 fills this in.
	logHeight := a.height - 22
	if logHeight < 5 {
		logHeight = 5
	}
	logView := PanelStyle.
		Width(a.width - 2).
		Height(logHeight).
		Render("(log will appear here when a task is selected)")

	cmdresultView := PanelStyle.Width(a.width - 2).Render(a.cmdresult.View())
	cmdlineView := a.cmdline.View()
	footer := FooterStyle.Render("tab focus · s submit · c cancel · enter follow · ? help · q quit")

	return strings.Join([]string{
		header,
		top,
		logView,
		cmdresultView,
		cmdlineView,
		footer,
	}, "\n")
}

// runAction is the placeholder dispatch — Task 13 fills in Submit/Cancel/Prune.
func (a *App) runAction(act Action) (tea.Model, tea.Cmd) {
	switch act.(type) {
	case QuitAction:
		return a, tea.Quit
	case ClearAction:
		a.cmdresult.Clear()
		return a, nil
	case HelpAction:
		a.cmdresult.Append("commands: submit / cancel / prune / clear / help / quit")
		return a, nil
	default:
		a.cmdresult.Append(WarnStyle.Render("(action not yet implemented)"))
		return a, nil
	}
}
