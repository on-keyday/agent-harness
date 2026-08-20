package tui

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type TasksModel struct {
	table table.Model
	// baseCols is the natural sizing; see RunnersModel.baseCols.
	baseCols []table.Column
	focused  bool
	// rowIDs[i] is the full hex task ID for row i; bubbles/table doesn't carry
	// arbitrary metadata so we mirror.
	rowIDs []string
	// rowTasks[i] is the full TaskInfo for row i, mirrored for the detail
	// popup so it can show fields the row truncates (full prompt, worktree
	// dir, timestamps, exit code).
	rowTasks []protocol.TaskInfo
	// tree orders rows by the creator link and draws a gutter in the ID
	// column instead of listing tasks flat. A mode, not a filter: every task
	// the flat view shows is still here, orphans re-rooted with a marker.
	tree bool
	// width is the last panel width SetSize was given. The column SET depends
	// on it (see obsColumnMinWidth), not just the column widths, so the rows
	// have to be rebuilt on a resize — a row carries one cell per column and a
	// mismatch would silently shift every cell after the missing one.
	width int
	// gutter[i] is the tree prefix for ordered row i; empty in flat mode. Kept
	// alongside rowTasks so rebuild() can redraw without re-deriving the tree.
	gutter []string
	// runners is the last snapshot's runner list, needed by rebuild() for the
	// legacy agent fallback on tasks that predate TaskInfo.AgentProfile.
	runners []protocol.RunnerInfo
}

// obsColumnMinWidth is the panel width at which the tasks table can afford the
// Obs column. Below it the column is DROPPED rather than squeezed: fitColumns
// floors every column at minColWidth, so an eighth column costs a whole floor
// plus padding taken from the rest, and at an 80-cell terminal the tasks panel
// gets about half — where eight columns overflow the frame and shred it
// (TestViewFitsTerminalWidth). The counts stay reachable at ANY width in the
// `d` detail popup, which is why dropping the column is a display choice and
// not a loss of information.
const obsColumnMinWidth = 60

// colsFor is the ONE place the column set is decided, from the two things it
// depends on: tree mode and whether the panel can afford the Obs column. Both
// callers (SetTree, SetSize) go through it so they cannot drift on what a given
// (mode, width) looks like — and so the row builder can ask the same question.
func colsFor(tree bool, width int) []table.Column {
	cols := flatCols()
	if tree {
		cols = treeCols()
	}
	if width >= obsColumnMinWidth {
		// Inserted before Repo, so the live-session columns (Act, Obs) stay
		// adjacent: one says whether the AGENT is working, the other whether a
		// HUMAN is on it.
		out := make([]table.Column, 0, len(cols)+1)
		for _, c := range cols {
			if c.Title == "Repo" {
				out = append(out, table.Column{Title: "Obs", Width: 7})
			}
			out = append(out, c)
		}
		return out
	}
	return cols
}

// showsObs reports whether the current column set includes Obs, so SetRows
// builds exactly as many cells as there are columns.
func (m *TasksModel) showsObs() bool { return m.width >= obsColumnMinWidth }

// flatCols is the default column set. Kept as a function so NewTasks and
// SetTree cannot drift apart on what "not tree mode" looks like.
func flatCols() []table.Column {
	return []table.Column{
		{Title: "Status", Width: 9},
		{Title: "ID", Width: 12},
		{Title: "From", Width: 6},
		{Title: "Agent", Width: 14},
		{Title: "Act", Width: 8},
		{Title: "Repo", Width: 28},
		{Title: "Prompt", Width: 0}, // resized later via SetSize
	}
}

func NewTasks() TasksModel {
	cols := flatCols()
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return TasksModel{table: t, baseCols: cols}
}

// treeCols is the column set for tree mode. ID is wider because the gutter
// shares the cell with the id: three columns per level, so ~24 fits four
// levels of an 8-hex id, which is deeper than the creator chains this harness
// produces in practice.
func treeCols() []table.Column {
	return []table.Column{
		{Title: "Status", Width: 9},
		{Title: "ID (by creator)", Width: 24},
		{Title: "From", Width: 6},
		{Title: "Agent", Width: 14},
		{Title: "Act", Width: 8},
		{Title: "Repo", Width: 22},
		{Title: "Prompt", Width: 0},
	}
}

// SetTree switches between the flat listing and the creator tree. Returns the
// new state so the caller can report it.
func (m *TasksModel) SetTree(on bool) bool {
	m.tree = on
	m.baseCols = colsFor(m.tree, m.width)
	m.applyColumns(fitColumns(m.baseCols, m.width, flexColumn(m.baseCols, "Prompt")))
	return m.tree
}

// TreeMode reports whether the tree ordering is active.
func (m *TasksModel) TreeMode() bool { return m.tree }

func (m *TasksModel) Focus() {
	m.focused = true
	m.table.Focus()
}

func (m *TasksModel) Blur() {
	m.focused = false
	m.table.Blur()
}

func (m *TasksModel) IsFocused() bool { return m.focused }

// SetSize fits the columns to w, with Prompt as the flex column. It used to
// only ever GROW the prompt column, which meant the fixed columns alone (89
// cells) overflowed any panel narrower than that and shredded the frame.
func (m *TasksModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
	// Width decides the column SET, not only the widths.
	m.width = w
	m.baseCols = colsFor(m.tree, w)
	m.applyColumns(fitColumns(m.baseCols, w, flexColumn(m.baseCols, "Prompt")))
}

// applyColumns swaps the table's columns and rebuilds the rows to match.
//
// The order is load-bearing and cost a panic to learn: bubbles' SetRows and
// SetColumns each re-render the viewport immediately, and renderRow indexes
// row[i] for every COLUMN i. So a moment where the table holds N columns and
// N-1-cell rows is an index-out-of-range, not a cosmetic glitch. Emptying the
// rows first makes the intermediate state unrenderable-but-valid, and the
// cursor is carried across because a resize must not move the selection.
//
// "Just always emit the widest row" does not work: the optional column is not
// last (Obs sits before Repo), so an extra cell rendered against a shorter
// column set would put every following value under the wrong header — which is
// worse than a panic, because nothing reports it.
func (m *TasksModel) applyColumns(cols []table.Column) {
	cursor := m.table.Cursor()
	m.table.SetRows(nil)
	m.table.SetColumns(cols)
	m.rebuild()
	if cursor >= 0 && cursor < len(m.rowIDs) {
		m.table.SetCursor(cursor)
	}
}

// SetRows takes a fresh snapshot: it decides the ORDER (which depends on tree
// mode, not on width) and hands the cell-building to rebuild. A resize calls
// rebuild alone — the order does not change, only how many columns there are.
func (m *TasksModel) SetRows(ts []protocol.TaskInfo, runners []protocol.RunnerInfo) {
	// One ordering decision, applied to the row cells AND to the parallel
	// rowIDs / rowTasks slices below. They index by table position, so a
	// reordering applied to only one of them opens the detail popup on a
	// different task than the cursor is on.
	ordered := ts
	gutter := make([]string, len(ts))
	if m.tree {
		treeRows := cli.BuildTaskTree(ts)
		ordered = make([]protocol.TaskInfo, len(treeRows))
		gutter = make([]string, len(treeRows))
		for i, r := range treeRows {
			ordered[i] = r.Task
			gutter[i] = cli.TreePrefix(r)
			if r.Orphan {
				gutter[i] += "\u2020"
			}
		}
	}
	m.rowTasks = ordered
	m.gutter = gutter
	m.runners = runners
	m.rebuild()
}

// rebuild renders m.rowTasks into table rows for the CURRENT column set. It is
// the only place cells are produced, so the cell count cannot disagree with
// colsFor's column count — a disagreement would not error, it would silently
// shift every cell after the missing one into the wrong column.
func (m *TasksModel) rebuild() {
	ordered, gutter := m.rowTasks, m.gutter
	// Index runners by ConnID string so each task can show its runner's agent.
	runnerByID := make(map[string]protocol.RunnerInfo, len(m.runners))
	for _, r := range m.runners {
		runnerByID[protocol.RunnerIDToConnID(r.Id).String()] = r
	}

	rows := make([]table.Row, 0, len(ordered))
	ids := make([]string, 0, len(ordered))
	for i, t := range ordered {
		idHex := hex.EncodeToString(t.Id.Id[:])
		// Prefer the task's own resolved AgentProfile (§6 of the
		// multi-agent-profile design) — the agent this task actually ran
		// (or will run) under, which may differ from its assigned runner's
		// process-level AgentBin on a multi-profile runner. Falls back to
		// the runner-derived descriptor for tasks predating this field
		// (WAL-replayed from before the feature landed).
		// Both arms carry the "+skills" marker. The AgentProfile arm used to
		// drop it — and since every live task carries a profile, the marker
		// disappeared from the whole column. The task's own bit is the right
		// source: runnerByID is empty for a confined caller (the server sends
		// no runners without info_global), so the fallback cannot supply it.
		agent := "-"
		if c := taskAgentCell(t); c != "" {
			agent = c
		} else if r, ok := runnerByID[protocol.RunnerIDToConnID(t.AssignedTo).String()]; ok {
			agent = agentDescriptor(string(r.AgentBin), r.SkillsInjected())
		}
		// Busy/idle badge from the live session's server-computed idle age;
		// blank for tasks without a live interactive session (the server
		// leaves last_output_at at 0 for those).
		act := ""
		if t.LastOutputAt > 0 {
			act = cli.ActivityStr(t.OutputIdleMs)
		}
		idCell := idHex[:12]
		if m.tree && i < len(gutter) {
			// 8 hex is what `by=` has always shown, so a reader comparing the
			// tree against a log line sees the same prefix.
			idCell = gutter[i] + idHex[:8]
		}
		row := table.Row{
			taskStatusStr(t.Status),
			idCell,
			originCell(t.OriginKind),
			agent,
			act,
		}
		if m.showsObs() {
			row = append(row, observerCell(t))
		}
		row = append(row,
			truncateLeft(string(t.RepoPath), repoCellWidth(m.tree)),
			renderPromptCell(t),
		)
		rows = append(rows, row)
		ids = append(ids, idHex)
	}
	m.rowIDs = ids
	m.table.SetRows(rows)
	// bubbles drives the cursor to -1 whenever SetRows is handed an empty
	// slice (`cursor > len(rows)-1` with len 0), and never lifts it back when
	// rows return. A negative cursor is not cosmetic: UpdateViewport computes
	// `end = cursor + viewport.Height`, so at -1 it renders one row SHORT and
	// the last visible slot sits blank until the first keypress moves the
	// cursor to 0 — which looks like the list ending early, then "growing"
	// when you touch it.
	//
	// Two routes reach it. The latent one: any moment with zero tasks (a fresh
	// server, everything pruned) poisons the cursor for the rest of the
	// session. The certain one: applyColumns empties the rows on every column
	// swap, and the first SetSize runs before any task has arrived — so this
	// was guaranteed at startup, not incidental.
	if len(rows) > 0 && m.table.Cursor() < 0 {
		m.table.SetCursor(0)
	}
}

// observerCell renders the Obs column: who is on this task's live session,
// cowriters first. Blank when the task has NO session — the Status column
// already says which those are. A live session with nobody on it renders "0c
// 0v" rather than blank, so an empty cell never has to mean two things.
func observerCell(t protocol.TaskInfo) string {
	if !taskSessionAlive(t.Status) {
		return ""
	}
	return fmt.Sprintf("%dc %dv", t.Cowriters, t.Viewers)
}

// The observer counts (TaskInfo.viewers / .cowriters) deliberately have NO
// column here. An eighth column does not fit: fitColumns floors every column at
// minColWidth, so the cost is the COUNT, not the declared width, and at an
// 80-cell terminal the tasks panel already spends its half on seven — adding
// one puts the table back outside its frame, which is the bug columns.go
// exists to fix (TestViewFitsTerminalWidth guards it). They are shown in the
// `d` detail popup instead; the CLI row, ls --json and the WebUI carry them
// unconditionally. Revisit only with width-conditional column sets.

// repoCellWidth matches the Repo column width of the active column set; the
// tree's wider ID column takes its space from Repo.
func repoCellWidth(tree bool) int {
	if tree {
		return 22
	}
	return 28
}

// SelectedID returns the full 32-char hex ID of the focused row, or "" if empty.
// Rows returns a copy of the current task rows in table order, for callers
// that need the full snapshot (the authority picker's id checklist).
func (m *TasksModel) Rows() []protocol.TaskInfo {
	out := make([]protocol.TaskInfo, len(m.rowTasks))
	copy(out, m.rowTasks)
	return out
}

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

// originCell renders the From column as lowercase "cli" / "tui" / "webui",
// or "-" when no ClientHello has attributed the task. Mirrors cli.originStr
// so cli ls and the TUI agree on display.
func originCell(k protocol.ClientKind) string {
	if k == protocol.ClientKind_Unspecified {
		return "-"
	}
	return strings.ToLower(k.String())
}

// taskSessionAlive reports whether the runner's session for the task is still
// alive — Running, or Detached because the client disconnected while the
// runner-side session and its worktree stayed up.
//
// This one predicate is behind several questions that look different but are
// not: can the grid tile it, can `r` reattach it, does it have a worktree the
// file picker can list. The server draws the same line (it answers NoSuchTask
// for file ops outside it, server/file_transfer.go), so it is named once here
// rather than open-coded at each call site.
func taskSessionAlive(s protocol.TaskStatus) bool {
	return s == protocol.TaskStatus_Running || s == protocol.TaskStatus_Detached
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
	case protocol.TaskStatus_Detached:
		return "Detachd"
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
