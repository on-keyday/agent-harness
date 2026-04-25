package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

type LogsModel struct {
	vp     viewport.Model
	taskID string
	lines  []string
}

func NewLogs() LogsModel {
	vp := viewport.New(80, 10)
	vp.SetContent("(no task selected)")
	return LogsModel{vp: vp}
}

func (m *LogsModel) SetSize(w, h int) {
	m.vp.Width = w
	m.vp.Height = h
}

// Reset clears the viewport and sets the task ID we're following.
// taskID == "" means no task selected.
func (m *LogsModel) Reset(taskID string) {
	m.taskID = taskID
	m.lines = nil
	if taskID == "" {
		m.vp.SetContent("(no task selected)")
	} else {
		m.vp.SetContent("(following " + taskID[:12] + "…)")
	}
}

// TaskID returns which task we're currently following, or "" if none.
func (m *LogsModel) TaskID() string { return m.taskID }

// Append appends a chunk of bytes (already prefixed by the runner with [out]/[err]).
// Chunks may contain partial lines; we keep them as-is.
func (m *LogsModel) Append(chunk []byte) {
	if m.taskID == "" {
		return
	}
	m.lines = append(m.lines, string(chunk))
	if len(m.lines) > 1000 {
		m.lines = m.lines[len(m.lines)-1000:]
	}
	m.vp.SetContent(strings.Join(m.lines, ""))
	m.vp.GotoBottom()
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
	m.vp.GotoBottom()
}

func (m LogsModel) Update(msg tea.Msg) (LogsModel, tea.Cmd) {
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m LogsModel) View() string { return m.vp.View() }
