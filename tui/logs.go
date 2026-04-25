package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type LogsModel struct {
	vp     viewport.Model
	taskID string
	// chunks are raw stdout/stderr fragments as published by the runner.
	// They may contain embedded newlines and partial lines; we concatenate
	// them with no separator and rewrap per the viewport width.
	chunks []string
}

func NewLogs() LogsModel {
	vp := viewport.New(80, 10)
	vp.SetContent("(no task selected)")
	return LogsModel{vp: vp}
}

func (m *LogsModel) SetSize(w, h int) {
	m.vp.Width = w
	m.vp.Height = h
	m.rebuildContent()
}

// Reset clears the viewport and sets the task ID we're following.
// taskID == "" means no task selected.
func (m *LogsModel) Reset(taskID string) {
	m.taskID = taskID
	m.chunks = nil
	if taskID == "" {
		m.vp.SetContent("(no task selected)")
	} else {
		m.vp.SetContent("(following " + taskID[:12] + "…)")
	}
}

// TaskID returns which task we're currently following, or "" if none.
func (m *LogsModel) TaskID() string { return m.taskID }

// Append appends a chunk of bytes (already prefixed by the runner with [out]/[err]).
// Chunks may contain partial lines; we keep them as-is and re-wrap on render.
func (m *LogsModel) Append(chunk []byte) {
	if m.taskID == "" {
		return
	}
	m.chunks = append(m.chunks, string(chunk))
	if len(m.chunks) > 1000 {
		m.chunks = m.chunks[len(m.chunks)-1000:]
	}
	m.rebuildContent()
}

// rebuildContent concatenates all chunks and wraps the whole text to the
// current viewport width so long lines (e.g. file paths in stack traces) are
// visible instead of being clipped. When chunks is empty, the call is a no-op
// so the "(no task selected)" / "(following …)" placeholders set by Reset
// survive a subsequent SetSize.
func (m *LogsModel) rebuildContent() {
	if len(m.chunks) == 0 {
		return
	}
	joined := strings.Join(m.chunks, "")
	if m.vp.Width <= 0 {
		m.vp.SetContent(joined)
		m.vp.GotoBottom()
		return
	}
	wrap := lipgloss.NewStyle().Width(m.vp.Width)
	m.vp.SetContent(wrap.Render(joined))
	m.vp.GotoBottom()
}

func (m LogsModel) Update(msg tea.Msg) (LogsModel, tea.Cmd) {
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m LogsModel) View() string { return m.vp.View() }
