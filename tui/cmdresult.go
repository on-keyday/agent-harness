package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

type CmdResultModel struct {
	vp      viewport.Model
	lines   []string
	focused bool
}

func NewCmdResult() CmdResultModel {
	vp := viewport.New(80, 5)
	vp.SetContent("(no command yet)")
	return CmdResultModel{vp: vp}
}

func (m *CmdResultModel) Focus()         { m.focused = true }
func (m *CmdResultModel) Blur()          { m.focused = false }
func (m CmdResultModel) IsFocused() bool { return m.focused }

func (m *CmdResultModel) SetSize(w, h int) {
	m.vp.Width = w
	m.vp.Height = h
}

func (m *CmdResultModel) Append(line string) {
	m.lines = append(m.lines, line)
	if len(m.lines) > 200 {
		m.lines = m.lines[len(m.lines)-200:]
	}
	m.vp.SetContent(strings.Join(m.lines, "\n"))
	m.vp.GotoBottom()
}

func (m *CmdResultModel) Clear() {
	m.lines = nil
	m.vp.SetContent("")
}

// View renders the viewport (the caller adds the panel border).
func (m CmdResultModel) View() string { return m.vp.View() }

// Update forwards key/mouse events to the embedded viewport when focused, so
// scroll bindings (up/down, pgup/pgdn) reach the result log only while the
// panel holds focus. Unfocused, the viewport ignores keys but still re-renders
// when Append/Clear mutate state.
func (m CmdResultModel) Update(msg tea.Msg) (CmdResultModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}
