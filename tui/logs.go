package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

type LogsModel struct {
	vp      viewport.Model
	taskID  string
	lines   []string
	focused bool
	// stickToBottom is true when the user has not manually scrolled away from
	// the tail. New chunks/Prepends auto-scroll to bottom in that mode. Once
	// the user scrolls up (with arrows / PgUp / k), it flips to false so live
	// chunks don't yank the view back to the bottom.
	stickToBottom bool
}

func NewLogs() LogsModel {
	vp := viewport.New(80, 10)
	vp.SetContent("(no task selected)")
	return LogsModel{vp: vp, stickToBottom: true}
}

func (m *LogsModel) Focus() { m.focused = true }
func (m *LogsModel) Blur() {
	m.focused = false
}
func (m *LogsModel) IsFocused() bool { return m.focused }

func (m *LogsModel) SetSize(w, h int) {
	m.vp.Width = w
	m.vp.Height = h
}

// Reset clears the viewport and sets the task ID we're following.
// taskID == "" means no task selected. Resets sticky-tail.
func (m *LogsModel) Reset(taskID string) {
	m.taskID = taskID
	m.lines = nil
	m.stickToBottom = true
	if taskID == "" {
		m.vp.SetContent("(no task selected)")
	} else {
		m.vp.SetContent("(following " + taskID[:12] + "…)")
	}
}

// TaskID returns which task we're currently following, or "" if none.
func (m *LogsModel) TaskID() string { return m.taskID }

// Append appends a chunk of bytes (already prefixed by the runner with [out]/[err]).
// Chunks may contain partial lines; we keep them as-is. When the user has
// scrolled up (stickToBottom == false), we don't yank the viewport back to
// the bottom — the new content is still in the buffer and will be visible
// when the user scrolls down.
func (m *LogsModel) Append(chunk []byte) {
	if m.taskID == "" {
		return
	}
	m.lines = append(m.lines, string(chunk))
	if len(m.lines) > 1000 {
		m.lines = m.lines[len(m.lines)-1000:]
	}
	m.vp.SetContent(strings.Join(m.lines, ""))
	if m.stickToBottom {
		m.vp.GotoBottom()
	}
}

// Prepend inserts content before any already-appended live chunks. Used to
// fold the historical log file (fetched via GetTaskLog) in front of pubsub
// chunks that may have started arriving while the fetch was in flight.
func (m *LogsModel) Prepend(content []byte) {
	if m.taskID == "" || len(content) == 0 {
		return
	}
	m.lines = append([]string{string(content)}, m.lines...)
	if len(m.lines) > 1000 {
		m.lines = m.lines[len(m.lines)-1000:]
	}
	m.vp.SetContent(strings.Join(m.lines, ""))
	if m.stickToBottom {
		m.vp.GotoBottom()
	}
}

// Update forwards key/mouse events to the embedded viewport when focused.
// We also update stickToBottom: any user-initiated scroll that takes us
// off the bottom flips it false; returning to the bottom (e.g. End / G)
// flips it true again so live chunks resume tailing.
func (m LogsModel) Update(msg tea.Msg) (LogsModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	m.stickToBottom = m.vp.AtBottom()
	return m, cmd
}

func (m LogsModel) View() string { return m.vp.View() }
