package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// NotifyModel is a bounded-scroll pane displaying the last ~200 notification
// events received via the notifications pubsub topic. Mirrors CmdResultModel
// in structure and Update/View conventions.
type NotifyModel struct {
	vp      viewport.Model
	lines   []string
	focused bool
}

func NewNotify() NotifyModel {
	vp := viewport.New(80, 5)
	vp.SetContent("(no notifications yet)")
	return NotifyModel{vp: vp}
}

func (m *NotifyModel) Focus()         { m.focused = true }
func (m *NotifyModel) Blur()          { m.focused = false }
func (m NotifyModel) IsFocused() bool { return m.focused }

func (m *NotifyModel) SetSize(w, h int) {
	m.vp.Width = w
	m.vp.Height = h
}

// Append renders and adds a NotifyEvent to the pane, capping at 200 entries.
func (m *NotifyModel) Append(ev protocol.NotifyEvent) {
	line := renderNotifyEvent(ev)
	m.lines = append(m.lines, line)
	if len(m.lines) > 200 {
		m.lines = m.lines[len(m.lines)-200:]
	}
	m.vp.SetContent(strings.Join(m.lines, "\n"))
	m.vp.GotoBottom()
}

// renderNotifyEvent formats a single event as:
// 15:04:05 [level] title — text  (origin[/hostname][ taskid])
func renderNotifyEvent(ev protocol.NotifyEvent) string {
	ts := time.Unix(int64(ev.Ts), 0).Local().Format("15:04:05") // ev.Ts is unix seconds
	level := ev.Level.String()
	origin := ev.Origin.String()
	if w := ev.Worker(); w != nil {
		if len(w.Hostname) > 0 {
			origin += "/" + string(w.Hostname)
		}
		if id := string(w.TaskId); len(id) > 0 {
			origin += " " + id // full id — copy-pasteable for task addressing
		}
	}
	title := string(ev.Title)
	text := string(ev.Text)
	// body: "title — text" with both; just "text" or "title" alone — no dangling
	// separator when one side is empty (untitled notifications are common).
	body := title
	if text != "" {
		if body != "" {
			body += " — " + text
		} else {
			body = text
		}
	}
	var sb strings.Builder
	sb.WriteString(ts)
	sb.WriteString(" [")
	sb.WriteString(level)
	sb.WriteString("] ")
	sb.WriteString(body)
	sb.WriteString("  (")
	sb.WriteString(origin)
	sb.WriteString(")")
	return sb.String()
}

// Update forwards key/mouse events to the embedded viewport when focused.
func (m NotifyModel) Update(msg tea.Msg) (NotifyModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// View renders the viewport (caller adds panel border).
func (m NotifyModel) View() string { return m.vp.View() }
