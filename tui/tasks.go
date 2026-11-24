package tui

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
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
	// mark is the capability the caps audit's `t` asked the tree to mark: its
	// holders' ID cells carry ◆ and the column title names it. 0 = none.
	mark protocol.Capability
	// hasRun is whether some row in rowTasks has a non-empty Run cell. The Run
	// column exists only then (on top of the width threshold): on interactive
	// rows it is 15 empty cells taken from Repo. Like width, it decides the
	// column SET, so a change of it goes through swapColumnsAndRebuild.
	hasRun bool
	// order is how the flat (non-tree) listing is ordered and grouped
	// (cli.OrderTasks). The grouped modes put a header row before each group:
	// rowHeader[i] is that row's text ("" for a task row), its rowTasks[i] a
	// zero TaskInfo and its rowIDs[i] "". The cursor never rests on one
	// (skipHeader), so SelectedID / SelectedTask only ever see a task.
	order     cli.TaskOrder
	rowHeader []string
	// finishedNote, when set, is a last header row saying what the history
	// cut leaves out and how H changes it (App.refreshTasksTable).
	finishedNote string
}

// SetFinishedNote sets the history row SetRows appends; "" for none. It takes
// effect on the next SetRows, which is what refreshTasksTable calls next.
func (m *TasksModel) SetFinishedNote(note string) { m.finishedNote = note }

// obsColumnMinWidth is the panel width at which the tasks table can afford the
// Obs column. Below it the column is DROPPED rather than squeezed: fitColumns
// floors every column at minColWidth, so an eighth column costs a whole floor
// plus padding taken from the rest, and at an 80-cell terminal the tasks panel
// gets about half — where eight columns overflow the frame and shred it
// (TestViewFitsTerminalWidth). The counts stay reachable at ANY width in the
// `d` detail popup, which is why dropping the column is a display choice and
// not a loss of information.
const obsColumnMinWidth = 60

// runColumnMinWidth is the panel width at which the tasks table also carries
// the Run column (a background task's run facts, A-D14). The column is kept at
// its full width (fitTaskColumns), so it costs the rest a fixed 17 cells; 70 is
// the narrowest panel where the other nine columns still keep their header
// floors after that (35 cells of floors + 18 of padding + 17), so adding Run
// never truncates the headers of a panel that had them whole. Below it the run
// facts are in the `d` popup, as the observer counts are below Obs's threshold.
// It must stay above obsColumnMinWidth: the row builder assumes Run never
// appears without Life and Obs.
const runColumnMinWidth = 70

// runColumnWidth fits every Run cell but the two longest end reasons, which
// truncate to a still-distinct prefix (turn_done_lif…, turn_done_pro…). Not
// 12: bubbles spends a truncated cell's last column on "…", so at 12 the
// three turn_done_* reasons all read "turn_done_…".
const runColumnWidth = 15

// colsFor is the ONE place the column set is decided, from the three things it
// depends on: tree mode, how wide the panel is (Obs, then Run), and whether any
// row has a Run cell (hasRun — DATA, not layout). Every caller (SetTree,
// SetSize, SetRows) goes through it so they cannot drift on what a given
// (mode, width, data) looks like — and so the row builder can ask the same
// question (showsRun).
func colsFor(tree bool, width int, hasRun bool) []table.Column {
	cols := flatCols()
	if tree {
		cols = treeCols()
	}
	if width >= obsColumnMinWidth {
		// Life is inserted before Act and Obs before Repo, so the live-session
		// columns (Life, Act, Obs) stay adjacent: what the agent's hooks say,
		// whether its screen is moving, and whether a HUMAN is on it. Run
		// follows them: how long the run has left, or why it waits or ended.
		out := make([]table.Column, 0, len(cols)+3)
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
				if width >= runColumnMinWidth && hasRun {
					out = append(out, table.Column{Title: "Run", Width: runColumnWidth})
				}
			}
			out = append(out, c)
		}
		return out
	}
	return cols
}

// fitTaskColumns is fitColumns with About as the flex column, except that Run
// keeps its declared width: fitColumns shrinks every column in proportion, and
// a Run cell cut to 8 cells loses the deadline's clock time (A-D14) — the one
// thing the column is there to show — while the columns it takes from (Repo,
// Agent) still read when trimmed.
func fitTaskColumns(base []table.Column, w int) []table.Column {
	ri := -1
	for i, c := range base {
		if c.Title == "Run" {
			ri = i
		}
	}
	if ri < 0 {
		return fitColumns(base, w, flexColumn(base, "About"))
	}
	rest := make([]table.Column, 0, len(base)-1)
	rest = append(rest, base[:ri]...)
	rest = append(rest, base[ri+1:]...)
	fitted := fitColumns(rest, w-base[ri].Width-tableCellPadding, flexColumn(rest, "About"))
	out := make([]table.Column, 0, len(base))
	out = append(out, fitted[:ri]...)
	out = append(out, base[ri])
	return append(out, fitted[ri:]...)
}

// showsObs reports whether the current column set includes Obs and Life, so
// SetRows builds exactly as many cells as there are columns. Below the
// threshold the Act cell carries the lifecycle badge instead (actCell).
func (m *TasksModel) showsObs() bool { return m.width >= obsColumnMinWidth }

// showsRun is showsObs for the Run column: wide enough AND some row has a Run
// cell. Must mirror colsFor's condition exactly.
func (m *TasksModel) showsRun() bool { return m.width >= runColumnMinWidth && m.hasRun }

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
		{Title: "About", Width: 0}, // resized later via SetSize; see aboutCell
	}
}

func NewTasks() TasksModel {
	cols := flatCols()
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return TasksModel{table: t, baseCols: cols, order: cli.OrderCreated}
}

// SetOrder chooses the flat listing's order; the next SetRows applies it.
// Returns the new mode so the caller can report it.
func (m *TasksModel) SetOrder(o cli.TaskOrder) cli.TaskOrder {
	m.order = o
	return o
}

// Order is the flat listing's order (the tree view has its own).
func (m *TasksModel) Order() cli.TaskOrder { return m.order }

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
		{Title: "About", Width: 0},
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
	m.swapColumnsAndRebuild(fitTaskColumns(m.baseCols, m.width))
	return m.tree
}

// TreeMode reports whether the tree ordering is active.
func (m *TasksModel) TreeMode() bool { return m.tree }

// columns is colsFor plus the one title that depends on state: the ID column
// names the capability being marked, so a ◆ never has to be guessed at.
func (m *TasksModel) columns() []table.Column {
	cols := colsFor(m.tree, m.width, m.hasRun)
	if m.tree && m.mark != 0 {
		cols[1].Title = "ID (◆ " + m.mark.String() + ")"
	}
	return cols
}

// SetMark marks the holders of c in tree order (0 clears it).
func (m *TasksModel) SetMark(c protocol.Capability) {
	m.mark = c
	m.baseCols = m.columns()
	m.swapColumnsAndRebuild(fitTaskColumns(m.baseCols, m.width))
}

// SelectID puts the cursor on the task's row, reporting whether it is listed.
func (m *TasksModel) SelectID(id string) bool {
	for i, rid := range m.rowIDs {
		if rid == id && id != "" {
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
	cols := fitTaskColumns(m.baseCols, w)
	// A width-only change keeps the same column set, and must NOT disturb the
	// rows: bubbles keeps the scroll position in the unexported
	// viewport.YOffset, and SetCursor does not restore it (only MoveUp/MoveDown
	// maintain it). Rebuilding on every resize left the selected row
	// highlighted off-screen — which showed up as "the row I picked
	// disappeared" after returning from an attach, since re-entering the alt
	// screen fires a WindowSizeMsg.
	// Unless the Repo column's fitted width moved: its cells are cut to that
	// width (repoCellWidth), so they are rebuilt — through
	// swapColumnsAndRebuild, which restores the cursor the way that keeps it on
	// screen.
	if len(cols) == len(m.table.Columns()) && m.repoCellWidth() == repoWidthIn(cols) {
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
	m.skipHeader(+1)
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
	var header []string
	if !m.tree {
		ordered, header = orderedRows(ts, m.order)
		gutter = make([]string, len(ordered))
	}
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
	// The history note goes last, as a header row: never selected, and
	// drawn across the line like a group header in either view.
	if m.finishedNote != "" {
		if header == nil {
			header = make([]string, len(ordered))
		}
		ordered = append(ordered[:len(ordered):len(ordered)], protocol.TaskInfo{})
		header = append(header, m.finishedNote)
		gutter = append(gutter, "")
	}
	// In a mode that reorders on every poll, the cursor follows its task, or
	// an action on the selection would hit whichever task moved under it.
	follow := ""
	if !m.tree && m.order != cli.OrderCreated {
		follow = m.SelectedID()
	}
	m.rowTasks = ordered
	m.rowHeader = header
	m.gutter = gutter
	m.runners = runners
	// The column set depends on the rows' DATA (Run), so it is re-decided here
	// from the new rows and swapped together with them — never rows first.
	hadRun := m.showsRun()
	m.hasRun = false
	for _, t := range ordered {
		if runCell(t) != "" {
			m.hasRun = true
			break
		}
	}
	if m.showsRun() != hadRun {
		m.baseCols = m.columns()
		m.swapColumnsAndRebuild(fitTaskColumns(m.baseCols, m.width))
	} else {
		m.rebuild()
	}
	if follow != "" && m.SelectedID() != follow {
		m.SelectID(follow)
	}
	m.skipHeader(+1)
}

// orderedRows is ts in the order o, with a header row (a zero TaskInfo)
// before each labelled group; header[i] is row i's header text, "" for a task.
func orderedRows(ts []protocol.TaskInfo, o cli.TaskOrder) ([]protocol.TaskInfo, []string) {
	var rows []protocol.TaskInfo
	var header []string
	for _, g := range cli.OrderTasks(ts, o, time.Now()) {
		if g.Label != "" {
			rows = append(rows, protocol.TaskInfo{})
			header = append(header, fmt.Sprintf("── %s (%d) ──", g.Label, len(g.Tasks)))
		}
		for _, t := range g.Tasks {
			rows = append(rows, t)
			header = append(header, "")
		}
	}
	return rows, header
}

// isHeader reports whether row i is a group header.
func (m *TasksModel) isHeader(i int) bool {
	return i >= 0 && i < len(m.rowHeader) && m.rowHeader[i] != ""
}

// skipHeader moves the cursor off a header row: to the nearest task in the
// direction dir (+1 down, -1 up), or the other way when there is none that
// way (the top header). The one place this is decided; called after every
// key the table moves the cursor on and after every rebuild.
func (m *TasksModel) skipHeader(dir int) {
	c := m.table.Cursor()
	if !m.isHeader(c) {
		return
	}
	for _, d := range []int{dir, -dir} {
		for j := c + d; j >= 0 && j < len(m.rowHeader); j += d {
			if m.isHeader(j) {
				continue
			}
			if j > c {
				m.table.MoveDown(j - c)
			} else {
				m.table.MoveUp(c - j)
			}
			return
		}
	}
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
			if m.showsRun() {
				row = append(row, runCell(t))
			}
		} else {
			row = append(row, actCell(t, true))
		}
		row = append(row,
			truncateLeft(string(t.RepoPath), m.repoCellWidth()),
			aboutCell(t),
		)
		if m.isHeader(i) {
			// Every cell blank but a marker the view swaps for the header
			// text (headerLines): a cell is cut to its column, and the ID
			// column is too narrow for "── needs you (3) ──".
			for j := range row {
				row[j] = ""
			}
			row[0] = headerMarker + strconv.Itoa(i)
			idHex = ""
		}
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

// repoCellWidth is the Repo column's width as fitted to the panel, so the
// cell is cut once, at its start: cut to the declared width, a narrowed column
// would be cut again by bubbles at its end — the repo's name. Rows are always
// built after the columns they go under are set (applyColumns), so the table
// holds the fitted set here; the declared width is the fallback before any.
// repoWidthIn is the Repo column's width in cols, or -1 without one.
func repoWidthIn(cols []table.Column) int {
	for _, c := range cols {
		if c.Title == "Repo" {
			return c.Width
		}
	}
	return -1
}

func (m *TasksModel) repoCellWidth() int {
	for _, c := range m.table.Columns() {
		if c.Title == "Repo" {
			return c.Width
		}
	}
	for _, c := range m.columns() {
		if c.Title == "Repo" {
			return c.Width
		}
	}
	return 28
}

// SelectedID returns the full 32-char hex ID of the focused row, or "" if empty.
// Rows returns a copy of the current task rows in table order, for callers
// that need the full snapshot (the authority picker's id checklist).
func (m *TasksModel) Rows() []protocol.TaskInfo {
	out := make([]protocol.TaskInfo, 0, len(m.rowTasks))
	for i, t := range m.rowTasks {
		if !m.isHeader(i) {
			out = append(out, t)
		}
	}
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
	if idx < 0 || idx >= len(m.rowTasks) || m.isHeader(idx) {
		return nil
	}
	return &m.rowTasks[idx]
}

func (m TasksModel) Update(msg tea.Msg) (TasksModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	before := m.table.Cursor()
	m.table, cmd = m.table.Update(msg)
	dir := +1
	if m.table.Cursor() < before {
		dir = -1
	}
	m.skipHeader(dir)
	return m, cmd
}

func (m TasksModel) View() string {
	return colorStateBadgesWithin(m.headerLines(m.table.View()), selectedRowOpen)
}

// headerMarker opens a header row's Status cell, followed by the row index.
// No task row can start with it: that cell is always a status word.
const headerMarker = "#"

// headerLines replaces each rendered header row (its Status cell holds the
// marker) with the header text across the whole line. Header rows are never
// selected, so the line is plain text and its width is the table's.
func (m *TasksModel) headerLines(v string) string {
	if len(m.rowHeader) == 0 {
		return v
	}
	lines := strings.Split(v, "\n")
	for k, l := range lines {
		rest, ok := strings.CutPrefix(strings.TrimLeft(l, " "), headerMarker)
		if !ok {
			continue
		}
		n := 0
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
		i, err := strconv.Atoi(rest[:n])
		if err != nil || !m.isHeader(i) {
			continue
		}
		w := runewidth.StringWidth(l)
		h := runewidth.Truncate(" "+m.rowHeader[i], w, "…")
		lines[k] = h + strings.Repeat(" ", max(0, w-runewidth.StringWidth(h)))
	}
	return strings.Join(lines, "\n")
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
	case protocol.TaskStatus_Active:
		// A live credential task (D19): no process, so not Running.
		return "Active"
	}
	return "?"
}

// aboutCell is the task table's last column, About: cli.TaskNowText, which
// the overview prints too.
func aboutCell(t protocol.TaskInfo) string { return cli.TaskNowText(t) }

// runCell is the Run column (A-D14), blank for a foreground task: while a
// background task is queued, why it waits (the wait reason's schema name, or
// placing); while it runs, the time left and the deadline on the reader's
// clock (the date instead from a day out); once it has ended, the end reason's
// schema name. The same facts as cli.RunStr, compacted to fit a cell; who
// cancelled it is in the `d` popup.
func runCell(t protocol.TaskInfo) string {
	if t.RunMode != protocol.RunMode_Background {
		return ""
	}
	switch {
	case t.Status == protocol.TaskStatus_Queued:
		switch t.WaitReason {
		case protocol.WaitReason_NoCandidate, protocol.WaitReason_AllBusy:
			return cli.EnumWireName(t.WaitReason.String())
		}
		return "placing"
	case t.EndedAt != 0:
		if t.EndReason == protocol.EndReason_None {
			return "unknown"
		}
		return cli.EnumWireName(t.EndReason.String())
	case t.DeadlineNs != 0:
		left := time.Duration(t.DeadlineRemainingMs) * time.Millisecond
		at := time.Unix(0, int64(t.DeadlineNs)).Local()
		if left >= 24*time.Hour {
			return fmt.Sprintf("%dd%02dh %s", int(left/(24*time.Hour)), int(left%(24*time.Hour)/time.Hour), at.Format("01-02"))
		}
		if left >= time.Hour {
			return fmt.Sprintf("%dh%02dm %s", int(left/time.Hour), int(left%time.Hour/time.Minute), at.Format("15:04"))
		}
		return fmt.Sprintf("%dm %s", int(left/time.Minute), at.Format("15:04"))
	}
	return ""
}

// lifeCell is the Life column: the lifecycle badge (cli.LifecycleBadge), blank
// for a finished task. A live task whose agent never reported a lifecycle (no
// hooks) shows its program's own report instead (cli.ProgramBadge), and lc:-
// only when there is neither. `wait` is also marked by case — WAIT:3m. The
// colour is added after the table renders (colorStateBadges): a style inside a
// cell is cut mid-sequence by bubbles' width truncation (see aboutCell).
func lifeCell(t protocol.TaskInfo) string {
	b := cli.TaskStateBadge(&t)
	if rest, ok := strings.CutPrefix(b, "wait:"); ok {
		return "WAIT:" + rest
	}
	return b
}

// The three tiers every surface colours the state badges by: busy (no look
// needed), ready (waiting for the next prompt), attention (asks for the
// operator). Foreground-only, ended with the default-foreground code rather
// than a full reset, so a selected row's highlight survives the badge.
const (
	stateBusyColor      = "\x1b[38;5;214m"
	stateReadyColor     = "\x1b[38;5;42m"
	stateAttentionColor = "\x1b[38;5;203m"
	stateColorReset     = "\x1b[39m"
)

var stateBadge = regexp.MustCompile(`(^|[\s│|])((work|ready|WAIT|err):[0-9]+[smh]|p:(working|idle|done|blocked|error))\b`)

// colorStateBadges colours each lifecycle / program badge in already-rendered
// text. It runs on the table's (or a pane header's) final string, after any
// truncation, so a cut can never split an escape sequence.
func colorStateBadges(s string) string { return colorStateBadgesWithin(s, "") }

// colorStateBadgesWithin is colorStateBadges for a table whose selected row is
// drawn with the style that opens with sel: on a line holding sel the badge
// ends by re-opening sel, so the rest of that row keeps its highlight instead of
// dropping to the default colour.
func colorStateBadgesWithin(s, sel string) string {
	if sel == "" || !strings.Contains(s, sel) {
		return colorLine(s, stateColorReset)
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		reset := stateColorReset
		if strings.Contains(l, sel) {
			reset = sel
		}
		lines[i] = colorLine(l, reset)
	}
	return strings.Join(lines, "\n")
}

func colorLine(s, reset string) string {
	return stateBadge.ReplaceAllStringFunc(s, func(m string) string {
		sub := stateBadge.FindStringSubmatch(m)
		lead, badge := sub[1], sub[2]
		color := stateReadyColor
		switch {
		case sub[3] == "work" || sub[4] == "working":
			color = stateBusyColor
		case sub[3] == "WAIT" || sub[3] == "err" || sub[4] == "blocked" || sub[4] == "error":
			color = stateAttentionColor
		}
		return lead + color + badge + reset
	})
}

// selectedRowOpen is the SGR sequence bubbles' default Selected style opens a
// row with ("" when the terminal profile renders no colour).
var selectedRowOpen = func() string {
	r := table.DefaultStyles().Selected.Render("\x00")
	if i := strings.IndexByte(r, 0); i > 0 {
		return r[:i]
	}
	return ""
}()

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
