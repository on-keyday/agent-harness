package tui

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
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
	// mark is the capability the caps audit's `t` asked the tree to mark: its
	// holders' ID cells carry ◆ and the column title names it. 0 = none.
	mark protocol.Capability
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
		// Life is inserted before Act and Obs before Repo, so the live-session
		// columns (Life, Act, Obs) stay adjacent: what the agent's hooks say,
		// whether its screen is moving, and whether a HUMAN is on it.
		out := make([]table.Column, 0, len(cols)+2)
		for _, c := range cols {
			if c.Title == "Act" {
				// 10 fits ready:999m.
				out = append(out, table.Column{Title: "Life", Width: 10})
			}
			if c.Title == "Repo" {
				// 10, not 7: the cell can carry "0c 0v 12x" when commands are
				// running in the worktree, and bubbles hard-truncates a cell to its
				// column width — a narrower column would hide the exec count
				// exactly when there is one to show.
				out = append(out, table.Column{Title: "Obs", Width: 10})
			}
			out = append(out, c)
		}
		return out
	}
	return cols
}

// showsObs reports whether the current column set includes Obs and Life, so
// SetRows builds exactly as many cells as there are columns. Below the
// threshold the Act cell carries the lifecycle badge instead (actCell).
func (m *TasksModel) showsObs() bool { return m.width >= obsColumnMinWidth }

// flatCols is the default column set. Kept as a function so NewTasks and
// SetTree cannot drift apart on what "not tree mode" looks like.
func flatCols() []table.Column {
	return []table.Column{
		{Title: "Status", Width: 9},
		{Title: "ID", Width: 12},
		{Title: "From", Width: 6},
		{Title: "Agent", Width: 14},
		{Title: "Act", Width: 10},
		{Title: "Repo", Width: 28},
		{Title: "Doing", Width: 0}, // resized later via SetSize; see doingCell
	}
}

func NewTasks() TasksModel {
	cols := flatCols()
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return TasksModel{table: t, baseCols: cols}
}

// treeCols is the column set for tree mode. ID is wider because the gutter
// shares the cell with the id: three columns per level, plus the ◆ mark and
// the tier mark (" 強3"), so ~28 fits three levels with both, which is deeper
// than the creator chains this harness produces in practice.
func treeCols() []table.Column {
	return []table.Column{
		{Title: "Status", Width: 9},
		{Title: "ID (by creator)", Width: 28},
		{Title: "From", Width: 6},
		{Title: "Agent", Width: 14},
		{Title: "Act", Width: 10},
		{Title: "Repo", Width: 18},
		{Title: "Doing", Width: 0},
	}
}

// SetTree switches between the flat listing and the creator tree. Returns the
// new state so the caller can report it.
func (m *TasksModel) SetTree(on bool) bool {
	m.tree = on
	m.baseCols = m.columns()
	// Always rebuilds, unlike a resize: tree mode changes the row CONTENT (the
	// ID cell carries the gutter), and flat and tree happen to have the SAME
	// column count — so a count-based check would swap the headers and leave
	// every row showing its flat id.
	m.swapColumnsAndRebuild(fitColumns(m.baseCols, m.width, flexColumn(m.baseCols, "Doing")))
	return m.tree
}

// TreeMode reports whether the tree ordering is active.
func (m *TasksModel) TreeMode() bool { return m.tree }

// columns is colsFor plus the one title that depends on state: the ID column
// names the capability being marked, so a ◆ never has to be guessed at.
func (m *TasksModel) columns() []table.Column {
	cols := colsFor(m.tree, m.width)
	if m.tree && m.mark != 0 {
		cols[1].Title = "ID (◆ " + m.mark.String() + ")"
	}
	return cols
}

// SetMark marks the holders of c in tree order (0 clears it).
func (m *TasksModel) SetMark(c protocol.Capability) {
	m.mark = c
	m.baseCols = m.columns()
	m.swapColumnsAndRebuild(fitColumns(m.baseCols, m.width, flexColumn(m.baseCols, "Doing")))
}

// SelectID puts the cursor on the task's row, reporting whether it is listed.
func (m *TasksModel) SelectID(id string) bool {
	for i, rid := range m.rowIDs {
		if rid == id {
			// MoveDown, not SetCursor: it is the path that keeps the row on
			// screen (see swapColumnsAndRebuild).
			m.table.SetCursor(0)
			m.table.MoveDown(i)
			return true
		}
	}
	return false
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

// SetSize fits the columns to w, with Prompt as the flex column. It used to
// only ever GROW the prompt column, which meant the fixed columns alone (89
// cells) overflowed any panel narrower than that and shredded the frame.
func (m *TasksModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
	// Width decides the column SET, not only the widths.
	m.width = w
	m.baseCols = m.columns()
	cols := fitColumns(m.baseCols, w, flexColumn(m.baseCols, "Doing"))
	// A width-only change keeps the same column set, and must NOT disturb the
	// rows: bubbles keeps the scroll position in the unexported
	// viewport.YOffset, and SetCursor does not restore it (only MoveUp/MoveDown
	// maintain it). Rebuilding on every resize left the selected row
	// highlighted off-screen — which showed up as "the row I picked
	// disappeared" after returning from an attach, since re-entering the alt
	// screen fires a WindowSizeMsg.
	if len(cols) == len(m.table.Columns()) {
		m.table.SetColumns(cols)
		return
	}
	m.swapColumnsAndRebuild(cols)
}

// swapColumnsAndRebuild swaps the table's columns and rebuilds the rows to
// match. Call it only when the column set or the row content actually changes;
// a width-only resize must go the cheap route (see SetSize).
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
func (m *TasksModel) swapColumnsAndRebuild(cols []table.Column) {
	cursor := m.table.Cursor()
	m.table.SetRows(nil)
	m.table.SetColumns(cols)
	m.rebuild()
	// Walk the cursor down rather than SetCursor'ing to it: MoveDown is the
	// path that maintains YOffset, so this lands the selection on screen the
	// same way the operator's own keypresses would.
	if cursor > 0 && cursor < len(m.rowIDs) {
		m.table.SetCursor(0)
		m.table.MoveDown(cursor)
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
			gutter[i] = cli.TreePrefix(r.IsLast)
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
	// Index runners by identity, the form TaskInfo.assigned_to carries, so each
	// task can resolve the runner it was assigned to. Shared with the detail
	// popup (runnerIndex) rather than built here: the comment on this line said
	// "by ConnID string" while the code keyed it by identity hex, which is the
	// drift two hand-built maps invite.
	runnerByID := runnerIndex(m.runners)

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
		// no runners without a global visibility rank), so the fallback cannot
		// supply it.
		agent := "-"
		if c := taskAgentCell(t); c != "" {
			agent = c
		} else if r, ok := runnerByID[t.AssignedTo.Hex()]; ok {
			agent = agentDescriptor(string(r.AgentBin), r.SkillsInjected())
		}
		idCell := idHex[:12]
		if m.tree && i < len(gutter) {
			// 8 hex is what `by=` has always shown, so a reader comparing the
			// tree against a log line sees the same prefix.
			idCell = treeMarks(t.Capabilities, m.mark) + gutter[i] + idHex[:8]
		}
		row := table.Row{
			taskStatusStr(t.Status),
			idCell,
			originCell(t.OriginKind),
			agent,
		}
		if m.showsObs() {
			row = append(row, lifeCell(t), actCell(t, false), observerCell(t))
		} else {
			row = append(row, actCell(t, true))
		}
		row = append(row,
			truncateLeft(string(t.RepoPath), repoCellWidth(m.tree)),
			doingCell(t),
		)
		rows = append(rows, row)
		ids = append(ids, idHex)
	}
	m.rowIDs = ids
	// setTableRows, not table.SetRows: it carries the negative-cursor lift that
	// keeps this table from rendering one row short. See its doc comment.
	setTableRows(&m.table, rows)
}

// treeMarks is the fixed-width head of a tree-order ID cell: a ◆ slot while a
// capability is marked, then the tier mark padded to four cells. It leads the
// cell because bubbles truncates a cell from the RIGHT — after a deep gutter
// and the id the marks were cut to "…" at 120–160 columns, and a marked holder
// read as unmarked. Fixed width keeps the gutters of every row aligned.
func treeMarks(caps, marked protocol.Capability) string {
	head := ""
	if marked != 0 {
		head = "  "
		if caps&marked == marked {
			head = "◆ "
		}
	}
	mark, _, _ := cli.TierMark(caps)
	return head + mark + strings.Repeat(" ", 4-runewidth.StringWidth(mark)) + " "
}

// observerCell renders the Obs column: what is working on this task. Observers
// on its live session, cowriters first, plus a count of commands running in its
// worktree. Blank when there is neither — the Status column already says which
// tasks have no session. A live session with nobody on it renders "0c 0v"
// rather than blank, so an empty cell never has to mean two things.
//
// The exec count appears only when non-zero, and its cell is drawn even for a
// task whose session is gone: an exec runs against the WORKTREE, so a
// terminal-but-dirty task can legitimately have one with no session at all —
// the case where a silent column would be most surprising. Printing "0x" on
// every row instead would need a ninth column this table cannot afford at 80
// cells (see the Obs note below); the unconditional count is in the `d` detail
// popup, in the CLI row, in ls --json and in the WebUI row.
func observerCell(t protocol.TaskInfo) string {
	execs := ""
	if t.ExecCount > 0 {
		execs = fmt.Sprintf("%dx", t.ExecCount)
	}
	if !taskSessionAlive(t.Status) {
		return execs
	}
	cell := fmt.Sprintf("%dc %dv", t.Cowriters, t.Viewers)
	if execs != "" {
		cell += " " + execs
	}
	return cell
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
		return 18
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
// Held is deliberately absent. "Alive" here means a live SERVER-SIDE session —
// a mux to attach to, a worktree the file picker can reach through it — and a
// held task is precisely the case with a live CHILD and no session: its mux
// died with the previous server. Adding it here would offer a reattach the
// server then refuses.
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
	case protocol.TaskStatus_Held:
		return "Held"
	}
	return "?"
}

// renderPromptCell returns the prompt-column display string for a task.
// Interactive tasks are surfaced as "<interactive>" because their prompt
// is intentionally empty; oneshot tasks render their prompt truncated.
// The Kind field is the authoritative source — TaskStatusEvent carries it
// from the very first event, so a freshly-stubbed row knows its kind
// without needing the next List snapshot to disambiguate.
// doingCell is the task table's last column: what the task is doing now. Its
// own progress line when it has one, else its program's window title marked
// "(title)" — a plain-text mark, because bubbles truncates cells by width and
// an ANSI style inside one can be cut mid-sequence — else the prompt cell as
// before. Interactive sessions used to show only "<interactive>" here.
//
// The line carries its age, which is how the reader judges a stale one (D4).
// Both strings have their control characters stripped HERE: bubbles hands a
// cell's bytes to the terminal as they are, and a task event can be forged by
// any subscriber, so the server's own rules are not a substitute.
func doingCell(t protocol.TaskInfo) string {
	if t.ProgressAt != 0 && len(t.Progress) > 0 {
		p := string(t.Progress)
		return vtgrid.SanitizeTitle(p, len(p)) + " (" + cli.AgeStr(t.ProgressAgeMs) + ")"
	}
	if t.LiveTerminal() && len(t.Title) > 0 {
		ti := string(t.Title)
		return "(title) " + vtgrid.SanitizeTitle(ti, len(ti))
	}
	return renderPromptCell(t)
}

// lifeCell is the Life column: the lifecycle badge (cli.LifecycleBadge), blank
// for a finished task. `wait` is marked by case — WAIT:3m — not by an ANSI
// style: bubbles truncates cells by width and a style can be cut mid-sequence
// (see doingCell).
func lifeCell(t protocol.TaskInfo) string {
	b := cli.LifecycleBadge(&t)
	if rest, ok := strings.CutPrefix(b, "wait:"); ok {
		return "WAIT:" + rest
	}
	return b
}

// actCell is the Act column: the busy/idle badge from the live session's
// output age, blank for a task without a live session (the server leaves
// last_output_at at 0 for those). In a panel too narrow for the Life column
// (merged), the lifecycle badge takes the cell when there is one: what the
// hooks say is the answer the operator scans the list for.
func actCell(t protocol.TaskInfo, merged bool) string {
	if merged {
		if b := lifeCell(t); b != "" {
			return b
		}
	}
	if t.LastOutputAt > 0 {
		return cli.ActivityStr(t.OutputIdleMs)
	}
	return ""
}

func renderPromptCell(t protocol.TaskInfo) string {
	switch t.Kind {
	case protocol.TaskKind_Interactive:
		return "<interactive>"
	case protocol.TaskKind_Stream:
		// Distinct from <interactive>: same "no prompt" reason, different
		// thing on the other end, and the row is where an operator first
		// notices which one they are looking at.
		return "<stream>"
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
