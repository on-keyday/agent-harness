package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type RunnersModel struct {
	table table.Model
	// baseCols is the natural sizing; SetSize re-derives the rendered widths
	// from it so a resize never compounds the previous one (see fitColumns).
	baseCols []table.Column
	focused  bool
	// rowRunners[i] is the full RunnerInfo for table row i; mirrored alongside
	// the bubbles/table rows so the detail popup can show fields the row
	// truncates (full repo path, full current task id, timestamps).
	rowRunners []protocol.RunnerInfo
}

func NewRunners() RunnersModel {
	cols := []table.Column{
		{Title: "Status", Width: 8},
		{Title: "Host", Width: 20},
		{Title: "Tasks", Width: 7},
		{Title: "Agent", Width: 14},
		{Title: "Roots", Width: 30},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return RunnersModel{table: t, baseCols: cols}
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

// SetSize fits the columns to w. Roots is the flex column: it is the only
// unbounded field here, so it both absorbs slack on a wide terminal and is the
// one worth truncating on a narrow one.
func (m *RunnersModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
	m.table.SetColumns(fitColumns(m.baseCols, w, flexColumn(m.baseCols, "Roots")))
}

// SetRows updates the runner rows from a snapshot.
func (m *RunnersModel) SetRows(rs []protocol.RunnerInfo) {
	rows := make([]table.Row, 0, len(rs))
	for _, r := range rs {
		rows = append(rows, table.Row{
			runnerStatusStr(r.Status),
			string(r.Hostname),
			runnerTasksCell(r),
			runnerAgentCell(r),
			runnerRootsCell(r),
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

// formatTaskID is a small helper used by tests / debug.
func formatTaskID(b []byte) string { return fmt.Sprintf("%x", b) }

// runnerTasksCell renders "active/max" for the Tasks column.
func runnerTasksCell(r protocol.RunnerInfo) string {
	return fmt.Sprintf("%d/%d", r.ActiveTasksLen, r.MaxTasks)
}

// agentDescriptor renders an agent identity (a runner's binary basename, or a
// task's resolved profile name) plus a skill marker, for tables / detail.
// "?" for an unknown binary. Injection is cross-tool — CLAUDE.md/AGENTS.md/
// GEMINI.md pointers plus the skill under both .claude/skills/ and
// .agents/skills/ — so "+skills" means a skill-aware peer whatever the agent
// is. The claude-only piece is the auto-inbox hook in .claude/settings.json,
// which this marker does NOT report.
func agentDescriptor(bin string, injected bool) string {
	if bin == "" {
		bin = "?"
	}
	if injected {
		return bin + "+skills"
	}
	return bin
}

// taskAgentCell renders a TASK's agent identity — its resolved profile plus the
// "+skills" marker read off the task itself. Returns "" when the task carries
// no profile (legacy WAL rows), leaving the placeholder to the caller: the
// table falls back to its runner and then to "-", the authority picker leaves
// the slot blank. Shared so the table and the picker cannot drift into showing
// the same task differently — which is how the marker got lost in the first
// place, by one renderer building the string by hand.
func taskAgentCell(t protocol.TaskInfo) string {
	if len(t.AgentProfile) == 0 {
		return ""
	}
	return agentDescriptor(string(t.AgentProfile), t.SkillsInjected())
}

// runnerAgentCell renders the Agent column for a runner row.
func runnerAgentCell(r protocol.RunnerInfo) string {
	return agentProfilesDescriptor(r.AgentProfiles, string(r.AgentBin), r.SkillsInjected())
}

// agentProfilesDescriptor renders a runner's agent identity, extended to the
// full profile set (§6 of the multi-agent-profile design): a multi-profile
// runner shows "claude,codex" instead of just its process-level AgentBin.
// A legacy runner that never advertised AgentProfiles falls back to
// agentDescriptor(bin, injected), unchanged from before this feature.
func agentProfilesDescriptor(profiles []protocol.AgentProfileName, bin string, injected bool) string {
	if len(profiles) == 0 {
		return agentDescriptor(bin, injected)
	}
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = string(p.Name)
	}
	desc := strings.Join(names, ",")
	if injected {
		desc += "+skills"
	}
	return desc
}

// runnerRootsCell renders the first AllowedRoot path (truncated) for the table.
// When multiple roots exist, the count is appended so the user knows to check detail.
func runnerRootsCell(r protocol.RunnerInfo) string {
	if len(r.AllowedRoots) == 0 {
		return "(any)"
	}
	first := truncateLeft(string(r.AllowedRoots[0].Path), 24)
	if len(r.AllowedRoots) > 1 {
		return fmt.Sprintf("%s (+%d)", first, len(r.AllowedRoots)-1)
	}
	return first
}
