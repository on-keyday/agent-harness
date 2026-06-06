package tui

import (
	"context"
	"fmt"
	"sort"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
)

// ForwardDirection distinguishes local (-L) and remote (-R) forwards.
type ForwardDirection int

const (
	ForwardLocal ForwardDirection = iota
	ForwardRemote
)

func (d ForwardDirection) flag() string {
	if d == ForwardRemote {
		return "-R"
	}
	return "-L"
}

func pfShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// PortForwardModal prompts for one forward spec for a selected task. mode picks
// local vs remote (placeholder + dispatch differ).
type PortForwardModal struct {
	open   bool
	taskID string
	mode   ForwardDirection
	input  textinput.Model
}

func (m *PortForwardModal) IsOpen() bool           { return m.open }
func (m *PortForwardModal) TaskID() string         { return m.taskID }
func (m *PortForwardModal) Mode() ForwardDirection { return m.mode }

// Open opens the modal in local mode (back-compat for existing call sites).
func (m *PortForwardModal) Open(taskID string) { m.OpenMode(taskID, ForwardLocal) }

// OpenMode opens the modal for taskID in the given direction.
func (m *PortForwardModal) OpenMode(taskID string, dir ForwardDirection) {
	m.taskID = taskID
	m.mode = dir
	if m.input.Prompt == "" {
		m.input = textinput.New()
	}
	if dir == ForwardRemote {
		m.input.Placeholder = "[bind:]runnerport:dialhost:dialport"
	} else {
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
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	footer := FooterStyle.Render("Enter to start · Esc to cancel")
	return box.Render("Port-forward task " + pfShortID(m.taskID) + "  " + m.mode.flag() + " " + m.input.View() + "\n\n" + footer)
}

// PortForwardSession tracks a running forward so it can be cancelled. ID is a
// client-side unique handle so a task can hold several forwards at once.
type PortForwardSession struct {
	ID        int
	TaskID    string
	Direction ForwardDirection
	Spec      string
	Cancel    context.CancelFunc
}

// selectForwards returns the active sessions for a task in one direction, sorted
// by ID for stable picker ordering.
func selectForwards(m map[int]*PortForwardSession, taskID string, dir ForwardDirection) []*PortForwardSession {
	var out []*PortForwardSession
	for _, s := range m {
		if s.TaskID == taskID && s.Direction == dir {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ForwardPicker lists active forwards (one task + direction) for digit-key
// selection, shown when more than one is active for a stop request.
type ForwardPicker struct {
	open     bool
	dir      ForwardDirection
	sessions []*PortForwardSession
}

func (p *ForwardPicker) IsOpen() bool { return p.open }

func (p *ForwardPicker) Open(dir ForwardDirection, sessions []*PortForwardSession) {
	p.open = true
	p.dir = dir
	p.sessions = sessions
}

func (p *ForwardPicker) Close() { p.open = false; p.sessions = nil }

// Pick maps a digit key ("1".."9") to a session, or nil if out of range.
func (p *ForwardPicker) Pick(key string) *PortForwardSession {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return nil
	}
	idx := int(key[0] - '1')
	if idx >= len(p.sessions) {
		return nil
	}
	return p.sessions[idx]
}

func (p *ForwardPicker) View() string {
	if !p.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	body := "Stop which " + p.dir.flag() + " forward?\n\n"
	for i, s := range p.sessions {
		body += fmt.Sprintf("%d) %s\n", i+1, s.Spec)
	}
	body += "\n" + FooterStyle.Render("press number to stop · Esc to cancel")
	return box.Render(body)
}

// PortForwardStatusMsg carries a line to append to cmdresult.
type PortForwardStatusMsg struct{ Line string }

// PortForwardStartedMsg registers a started forward in the App.
type PortForwardStartedMsg struct {
	ID        int
	TaskID    string
	Direction ForwardDirection
	Spec      string
	Cancel    context.CancelFunc
}

// PortForwardStoppedMsg removes a finished/failed forward from the App so it no
// longer lingers in the stop picker. Sent when the forward goroutine exits —
// including a bind failure, where the forward never actually ran.
type PortForwardStoppedMsg struct {
	ID     int
	TaskID string
}

// forwardFailLine renders a clearly-marked failure line so a failed forward is
// unmistakable (the old flow showed a green "forward started" even on failure).
func forwardFailLine(taskID string, err error) string {
	return WarnStyle.Render("✗ forward failed: ") + pfShortID(taskID) + "  " + err.Error()
}

// DoStartPortForward parses the spec and starts a background local (-L) forward
// using the long-lived client (NOT a fresh dial). program MUST be App's
// *tea.Program (goroutines emit messages via program.Send).
//
// Started is emitted via program.Send (not the cmd return value) so it is
// enqueued before the goroutine's Stopped message — otherwise a fast failure
// could enqueue Stopped first and leave a stale entry in activeForwards.
func DoStartPortForward(c *cli.Client, taskID, spec string, id int, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		sp, err := cli.ParseForwardSpec(spec)
		if err != nil {
			return PortForwardStatusMsg{Line: forwardFailLine(taskID, err)}
		}
		ctx, cancel := context.WithCancel(context.Background())
		program.Send(PortForwardStartedMsg{ID: id, TaskID: taskID, Direction: ForwardLocal, Spec: spec, Cancel: cancel})
		go func() {
			if err := cli.RunForward(ctx, c, taskID, []cli.ForwardSpec{sp}, func(s string) {
				program.Send(PortForwardStatusMsg{Line: s})
			}); err != nil {
				program.Send(PortForwardStatusMsg{Line: forwardFailLine(taskID, err)})
			}
			program.Send(PortForwardStoppedMsg{ID: id, TaskID: taskID})
		}()
		return nil
	}
}

// DoStartRemoteForward is the -R counterpart. It confirms the runner actually
// bound the listener (OpenRemoteForward blocks until the bind result) BEFORE
// registering, so a bind failure shows a clear error instead of a misleading
// "forward started" followed by an error.
func DoStartRemoteForward(c *cli.Client, taskID, spec string, id int, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		sp, err := cli.ParseRemoteForwardSpec(spec)
		if err != nil {
			return PortForwardStatusMsg{Line: forwardFailLine(taskID, err)}
		}
		ctx, cancel := context.WithCancel(context.Background())
		ctrl, _, err := c.OpenRemoteForward(ctx, taskID, sp)
		if err != nil {
			cancel()
			return PortForwardStatusMsg{Line: forwardFailLine(taskID, err)}
		}
		program.Send(PortForwardStartedMsg{ID: id, TaskID: taskID, Direction: ForwardRemote, Spec: spec, Cancel: cancel})
		go func() {
			c.ServeRemoteForwardControl(ctx, sp, ctrl, func(s string) {
				program.Send(PortForwardStatusMsg{Line: s})
			})
			program.Send(PortForwardStoppedMsg{ID: id, TaskID: taskID})
		}()
		return nil
	}
}
