package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
)

// PortForwardModal prompts for one -L spec for a selected task.
type PortForwardModal struct {
	open   bool
	taskID string
	input  textinput.Model
}

func (m *PortForwardModal) IsOpen() bool   { return m.open }
func (m *PortForwardModal) TaskID() string { return m.taskID }

func (m *PortForwardModal) Open(taskID string) {
	m.taskID = taskID
	if m.input.Prompt == "" {
		m.input = textinput.New()
		m.input.Placeholder = "[bind:]localport:remotehost:remoteport"
	}
	m.input.SetValue("")
	m.input.Focus()
	m.open = true
}

func (m *PortForwardModal) Close() {
	m.open = false
	m.input.Blur()
}

func (m *PortForwardModal) Spec() string { return m.input.Value() }

func (m *PortForwardModal) Update(msg tea.Msg) (PortForwardModal, tea.Cmd) {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return *m, cmd
}

func (m *PortForwardModal) View() string {
	if !m.open {
		return ""
	}
	short := m.taskID
	if len(short) > 12 {
		short = short[:12]
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	footer := FooterStyle.Render("Enter to start · Esc to cancel")
	return box.Render("Port-forward task " + short + "  -L " + m.input.View() + "\n\n" + footer)
}

// PortForwardSession tracks a running forward so it can be cancelled.
type PortForwardSession struct {
	TaskID string
	Spec   string
	Cancel context.CancelFunc
}

// PortForwardStatusMsg carries a line to append to cmdresult.
type PortForwardStatusMsg struct{ Line string }

// PortForwardStartedMsg registers a started forward in the App.
type PortForwardStartedMsg struct {
	TaskID string
	Spec   string
	Cancel context.CancelFunc
}

// DoStartPortForward parses the spec and starts a background forward using
// the long-lived client (NOT a fresh dial). The program handle MUST be
// the same *tea.Program stored on App — matching the SubscribeTaskLog
// pattern in events.go where goroutines emit messages via program.Send.
func DoStartPortForward(c *cli.Client, taskID, spec string, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		sp, err := cli.ParseForwardSpec(spec)
		if err != nil {
			return PortForwardStatusMsg{Line: "forward: " + err.Error()}
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			err := cli.RunForward(ctx, c, taskID, []cli.ForwardSpec{sp}, func(s string) {
				program.Send(PortForwardStatusMsg{Line: s})
			})
			if err != nil {
				program.Send(PortForwardStatusMsg{Line: "forward: " + err.Error()})
			}
			short := taskID
			if len(short) > 12 {
				short = short[:12]
			}
			program.Send(PortForwardStatusMsg{Line: fmt.Sprintf("forward stopped: %s", short)})
		}()
		return PortForwardStartedMsg{TaskID: taskID, Spec: spec, Cancel: cancel}
	}
}
