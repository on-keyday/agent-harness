package tui

import (
	"encoding/hex"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type TasksModel struct {
	table   table.Model
	focused bool
	// rowIDs[i] is the full hex task ID for row i; bubbles/table doesn't carry
	// arbitrary metadata so we mirror.
	rowIDs []string
	// rowTasks[i] is the full TaskInfo for row i, mirrored for the detail
	// popup so it can show fields the row truncates (full prompt, worktree
	// dir, timestamps, exit code).
	rowTasks []protocol.TaskInfo
}

func NewTasks() TasksModel {
	cols := []table.Column{
		{Title: "Status", Width: 9},
		{Title: "ID", Width: 12},
		{Title: "Repo", Width: 28},
		{Title: "Prompt", Width: 0}, // resized later via SetSize
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return TasksModel{table: t}
}

func (m *TasksModel) Focus() {
	m.focused = true
	m.table.Focus()
}

func (m *TasksModel) Blur() {
	m.focused = false
	m.table.Blur()
}

func (m *TasksModel) IsFocused() bool { return m.focused }

func (m *TasksModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
	// Stretch the prompt column to fill remaining width.
	cols := m.table.Columns()
	used := 0
	for i := 0; i < len(cols)-1; i++ {
		used += cols[i].Width + 2 // table padding
	}
	if rest := w - used - 4; rest > 0 {
		cols[len(cols)-1].Width = rest
		m.table.SetColumns(cols)
	}
}

func (m *TasksModel) SetRows(ts []protocol.TaskInfo) {
	rows := make([]table.Row, 0, len(ts))
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		idHex := hex.EncodeToString(t.Id.Id[:])
		rows = append(rows, table.Row{
			taskStatusStr(t.Status),
			idHex[:12],
			truncateLeft(string(t.RepoPath), 28),
			renderPromptCell(t),
		})
		ids = append(ids, idHex)
	}
	m.rowIDs = ids
	m.rowTasks = ts
	m.table.SetRows(rows)
}

// SelectedID returns the full 32-char hex ID of the focused row, or "" if empty.
func (m *TasksModel) SelectedID() string {
	if len(m.rowIDs) == 0 {
		return ""
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rowIDs) {
		return ""
	}
	return m.rowIDs[idx]
}

// SelectedTask returns the full TaskInfo for the focused row, or nil when
// the table is empty / cursor out of range.
func (m *TasksModel) SelectedTask() *protocol.TaskInfo {
	if len(m.rowTasks) == 0 {
		return nil
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rowTasks) {
		return nil
	}
	return &m.rowTasks[idx]
}

func (m TasksModel) Update(msg tea.Msg) (TasksModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m TasksModel) View() string {
	return m.table.View()
}

func taskStatusStr(s protocol.TaskStatus) string {
	switch s {
	case protocol.TaskStatus_Queued:
		return "Queued"
	case protocol.TaskStatus_Running:
		return "Running"
	case protocol.TaskStatus_Succeeded:
		return "Done"
	case protocol.TaskStatus_Failed:
		return "Failed"
	case protocol.TaskStatus_Cancelled:
		return "Cancel"
	}
	return "?"
}

// renderPromptCell returns the prompt-column display string for a task.
// Interactive tasks are surfaced as "<interactive>" because their prompt
// is intentionally empty; oneshot tasks render their prompt truncated.
// The Kind field is the authoritative source — TaskStatusEvent carries it
// from the very first event, so a freshly-stubbed row knows its kind
// without needing the next List snapshot to disambiguate.
func renderPromptCell(t protocol.TaskInfo) string {
	if t.Kind == protocol.TaskKind_Interactive {
		return "<interactive>"
	}
	return truncatePrompt(string(t.Prompt))
}

// truncatePrompt collapses newlines and clips to ~140 chars (the column SetSize will further clip).
func truncatePrompt(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\n' || c == '\r' || c == '\t' {
			out = append(out, ' ')
		} else {
			out = append(out, c)
		}
	}
	if len(out) > 140 {
		out = out[:140]
	}
	return string(out)
}
