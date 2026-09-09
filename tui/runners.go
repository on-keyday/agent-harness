package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type RunnersModel struct {
	table table.Model
	// baseCols is the natural sizing; SetSize re-derives the rendered widths
	// from it so a resize never compounds the previous one (see fitColumns).
	baseCols []table.Column
	// width decides the column SET as well as the widths (see idColumnMinWidth),
	// so the row builder has to ask the same question the columns did.
	width   int
	focused bool
	// rowRunners[i] is the full RunnerInfo for table row i; mirrored alongside
	// the bubbles/table rows so the detail popup can show fields the row
	// truncates (full repo path, full current task id, timestamps).
	rowRunners []protocol.RunnerInfo
}

// idColumnMinWidth is the PANEL width at which the runners table can afford the
// identity column. Set from measurement, not arithmetic: fitColumns shrinks
// every column proportionally against a per-column floor of its header width,
// and this table is already over-subscribed (79 natural cells before the ID
// column), so the id got 3 cells at an 80-column terminal — two hex digits and
// an ellipsis, which identifies nothing while taking 5 cells from the columns
// that are most starved at exactly that width. At a 70-cell panel it keeps 6,
// which does distinguish the slots on one host. Below that the table keeps the
// five columns it had, and the full identity is still one `d` away in the
// detail popup.
//
// 72 is where fitColumns first hands the column 6 cells — measured, and pinned
// by TestIDColumnIsWideEnoughToIdentifyWhenShown so a later width change to any
// NEIGHBOURING column cannot quietly push it back under.
const idColumnMinWidth = 72

// runnerColsFor is the column set for a panel of the given width. One function
// so NewRunners, SetSize and the row builder cannot disagree about how many
// cells a row has: bubbles indexes row[i] per COLUMN and panics on a mismatch,
// and the ID column is not last — a stale extra cell would render every
// following value under the wrong header, silently.
func runnerColsFor(width int) []table.Column {
	cols := []table.Column{
		{Title: "Status", Width: 8},
		{Title: "Host", Width: 20},
		{Title: "Tasks", Width: 7},
		{Title: "Agent", Width: 14},
		{Title: "Roots", Width: 30},
	}
	if width < idColumnMinWidth {
		return cols
	}
	// Second, right after Status: it is the row's identity, and Host is not a
	// key — two slots on one machine share a hostname by design (the eBPF guest
	// runs exactly that pair), so without it the rows differ only by Agent. It
	// is also the value TaskInfo.assigned_to carries, so it is what joins a
	// task row to the runner running it.
	out := make([]table.Column, 0, len(cols)+1)
	out = append(out, cols[0], table.Column{Title: "ID", Width: 9})
	return append(out, cols[1:]...)
}

// showsID reports whether the current column set includes ID.
func (m *RunnersModel) showsID() bool { return m.width >= idColumnMinWidth }

func NewRunners() RunnersModel {
	cols := runnerColsFor(0)
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
	m.width = w
	m.baseCols = runnerColsFor(w)
	cols := fitColumns(m.baseCols, w, flexColumn(m.baseCols, "Roots"))
	// A width-only change keeps the column SET, and must not disturb the rows:
	// bubbles keeps the scroll offset unexported and SetCursor does not restore
	// it, so rebuilding on every resize leaves the selection highlighted
	// off-screen. Same split, and same reason, as TasksModel.SetSize.
	if len(cols) == len(m.table.Columns()) {
		m.table.SetColumns(cols)
		return
	}
	m.swapColumnsAndRebuild(cols)
}

// swapColumnsAndRebuild swaps columns and rows together. The order is
// load-bearing: bubbles re-renders on each of SetRows and SetColumns and
// indexes row[i] per column, so a moment holding N columns against
// N-1-cell rows panics. Empty first, then the columns, then rebuild.
func (m *RunnersModel) swapColumnsAndRebuild(cols []table.Column) {
	cursor := m.table.Cursor()
	m.table.SetRows(nil)
	m.table.SetColumns(cols)
	m.rebuild()
	if cursor >= 0 && cursor < len(m.rowRunners) {
		m.table.SetCursor(cursor)
	}
}

// SetRows updates the runner rows from a snapshot.
func (m *RunnersModel) SetRows(rs []protocol.RunnerInfo) {
	m.rowRunners = rs
	m.rebuild()
}

// rebuild is the ONLY place runner cells are produced, so the cell count cannot
// disagree with runnerColsFor's column count.
func (m *RunnersModel) rebuild() {
	rows := make([]table.Row, 0, len(m.rowRunners))
	for _, r := range m.rowRunners {
		row := table.Row{runnerStatusStr(r.Status)}
		if m.showsID() {
			row = append(row, cli.PrincipalShort(r.Id.Id[:]))
		}
		row = append(row,
			string(r.Hostname),
			runnerTasksCell(r),
			runnerAgentCell(r),
			runnerRootsCell(r),
		)
		rows = append(rows, row)
	}
	setTableRows(&m.table, rows)
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
