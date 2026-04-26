package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type RunnersModel struct {
	table   table.Model
	focused bool
	// rowRunners[i] is the full RunnerInfo for table row i; mirrored alongside
	// the bubbles/table rows so the detail popup can show fields the row
	// truncates (full repo path, full current task id, timestamps).
	rowRunners []protocol.RunnerInfo
}

func NewRunners() RunnersModel {
	cols := []table.Column{
		{Title: "Status", Width: 8},
		{Title: "Repo", Width: 40},
		{Title: "Current Task", Width: 14},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return RunnersModel{table: t}
}

func (m *RunnersModel) Focus() {
	m.focused = true
	m.table.Focus()
}

func (m *RunnersModel) Blur() {
	m.focused = false
	m.table.Blur()
}

func (m *RunnersModel) IsFocused() bool { return m.focused }

func (m *RunnersModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
}

// SetRows updates the runner rows from a snapshot.
func (m *RunnersModel) SetRows(rs []protocol.RunnerInfo) {
	rows := make([]table.Row, 0, len(rs))
	for _, r := range rs {
		rows = append(rows, table.Row{
			runnerStatusStr(r.Status),
			truncateLeft(string(r.RepoPath), 40),
			shortHexNonZero(r.CurrentTask.Id[:]),
		})
	}
	m.rowRunners = rs
	m.table.SetRows(rows)
}

// SelectedRunner returns the full RunnerInfo for the focused row, or nil
// when the table is empty / cursor out of range.
func (m *RunnersModel) SelectedRunner() *protocol.RunnerInfo {
	if len(m.rowRunners) == 0 {
		return nil
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rowRunners) {
		return nil
	}
	return &m.rowRunners[idx]
}

func (m RunnersModel) Update(msg tea.Msg) (RunnersModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m RunnersModel) View() string {
	return m.table.View()
}

func runnerStatusStr(s protocol.RunnerStatus) string {
	switch s {
	case protocol.RunnerStatus_Idle:
		return "Idle"
	case protocol.RunnerStatus_Busy:
		return "Busy"
	default:
		return "Offline"
	}
}

// truncateLeft keeps the right-most part of s within max chars (left side gets "…").
// Repo paths are most informative on the right (the last directory).
func truncateLeft(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-(max-1):]
}

// shortHexNonZero renders the first 12 hex chars of b, or "-" if b is all-zero.
func shortHexNonZero(b []byte) string {
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "-"
	}
	const tab = "0123456789abcdef"
	out := make([]byte, 0, 12)
	for i := 0; i < 6 && i < len(b); i++ {
		out = append(out, tab[b[i]>>4], tab[b[i]&0xf])
	}
	return string(out)
}

// formatTaskID is a small helper used by tests / debug.
func formatTaskID(b []byte) string { return fmt.Sprintf("%x", b) }
