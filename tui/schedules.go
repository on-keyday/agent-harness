package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SchedulesModel is the third table (pillar S): the server-kept cron schedules,
// the rows `schedule ls` prints. Its cells are cli's (ScheduleNextCell,
// ScheduleLastCell, ScheduleTaskCell, ScheduleAboutCell), so the TUI and the
// command line cannot word the same schedule differently; none is elided at
// "nothing" (last=none, next=none, no-task — checklist 31).
type SchedulesModel struct {
	table table.Model
	// baseCols is the natural sizing; see RunnersModel.baseCols.
	baseCols []table.Column
	focused  bool
	// width decides the column SET (see schedColsFor), so the row builder
	// asks the same question the columns did.
	width int
	// rows[i] is the full ScheduleRow for table row i, mirrored for the keys
	// (which act on the full 32-hex id) and the detail popup.
	rows       []protocol.ScheduleRow
	now        func() time.Time
	liveStatus func(protocol.TaskID) (protocol.TaskStatus, bool)
}

// Column tiers (PANEL widths: the terminal less 2). Below schedAboutMinWidth the table is ID, Cron, Next, Last,
// Task: About (the prompt head) would be left 3 cells wide, which hides the one
// cell that says what the schedule does, so it is dropped rather than squeezed
// and is in the `d` popup. From schedAboutMinWidth About joins as the flex
// column; from schedWideMinWidth Agent and Repo join too, where About still
// keeps about 24 cells after every other column holds its declared width
// (measured with all eight at a 118-cell panel: 3). ID, Cron and Next keep their declared
// width at every tier (fitSchedColumns): they are the id the keys act on and
// the expression and instant that identify the schedule.
const (
	schedAboutMinWidth = 116
	schedWideMinWidth  = 160
)

// schedColsFor is the ONE place the column set is decided. NewSchedules,
// SetSize and the row builder (showsAbout, showsWide) all go through it, so a
// row cannot carry a different number of cells than the table has columns:
// bubbles indexes row[i] per COLUMN and a mismatch shifts every later value
// under the wrong header without an error.
func schedColsFor(width int) []table.Column {
	cols := []table.Column{
		{Title: "ID", Width: 8},
		{Title: "Cron", Width: 15},
		{Title: "Next", Width: 20}, // 2006-01-02 15:04 MST
		{Title: "Last", Width: 18},
		{Title: "Task", Width: 23}, // created-at-first-firing
	}
	if width >= schedWideMinWidth {
		cols = append(cols,
			table.Column{Title: "Agent", Width: 12},
			table.Column{Title: "Repo", Width: 24})
	}
	if width >= schedAboutMinWidth {
		cols = append(cols, table.Column{Title: "About", Width: 0})
	}
	return cols
}

// fitSchedColumns is fitColumns with About as the flex column (Task below the
// About tier), except that ID, Cron and Next keep their declared widths: a
// proportional shrink would cut the firing time to "2026-11-16 09:…" and the
// id to "3c4d…". The columns it shrinks instead (Last, Task, Agent, Repo) read
// when trimmed, and the `d` popup has them whole.
func fitSchedColumns(base []table.Column, w int) []table.Column {
	pinned := func(c table.Column) bool { return c.Title == "ID" || c.Title == "Cron" || c.Title == "Next" }
	var rest []table.Column
	budget := w
	for _, c := range base {
		if pinned(c) {
			budget -= c.Width + tableCellPadding
		} else {
			rest = append(rest, c)
		}
	}
	flex := flexColumn(rest, "About") // the last column when About is dropped: Task
	fitted := fitColumns(rest, budget, flex)
	out := make([]table.Column, 0, len(base))
	i := 0
	for _, c := range base {
		if pinned(c) {
			out = append(out, c)
		} else {
			out = append(out, fitted[i])
			i++
		}
	}
	return out
}

func NewSchedules() SchedulesModel {
	cols := schedColsFor(0)
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return SchedulesModel{table: t, baseCols: cols}
}

func (m *SchedulesModel) Focus() {
	m.focused = true
	m.table.Focus()
}

func (m *SchedulesModel) Blur() {
	m.focused = false
	m.table.Blur()
}

func (m *SchedulesModel) IsFocused() bool { return m.focused }

// showsWide and showsAbout mirror schedColsFor's conditions.
func (m *SchedulesModel) showsWide() bool  { return m.width >= schedWideMinWidth }
func (m *SchedulesModel) showsAbout() bool { return m.width >= schedAboutMinWidth }

// SetSize fits the columns to w. A width-only change keeps the column set and
// must not disturb the rows (see TasksModel.SetSize: bubbles keeps the scroll
// offset unexported), unless the Repo column's fitted width moved: its cells
// are cut to that width, so they are rebuilt through swapColumnsAndRebuild.
func (m *SchedulesModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
	m.width = w
	m.baseCols = schedColsFor(w)
	cols := fitSchedColumns(m.baseCols, w)
	if len(cols) == len(m.table.Columns()) && m.repoCellWidth() == repoWidthIn(cols) {
		m.table.SetColumns(cols)
		return
	}
	m.swapColumnsAndRebuild(cols)
}

// swapColumnsAndRebuild is TasksModel's, for the same reason: bubbles renders
// on every SetRows and SetColumns, so rows are emptied before the columns
// change and rebuilt after, and the cursor walks down (MoveDown maintains the
// viewport offset) rather than being set.
func (m *SchedulesModel) swapColumnsAndRebuild(cols []table.Column) {
	cursor := m.table.Cursor()
	m.table.SetRows(nil)
	m.table.SetColumns(cols)
	m.rebuild()
	if cursor > 0 && cursor < len(m.rows) {
		m.table.SetCursor(0)
		m.table.MoveDown(cursor)
	}
}

// repoCellWidth is the Repo column's fitted width, so the cell is cut once, at
// its start (truncateLeft): cut to the declared width, a narrowed column would
// be cut again by bubbles at its END — the repo's name.
func (m *SchedulesModel) repoCellWidth() int {
	for _, c := range m.table.Columns() {
		if c.Title == "Repo" {
			return c.Width
		}
	}
	return -1
}

// SetRows takes the schedules of a snapshot. The order is the server's.
func (m *SchedulesModel) SetRows(rs []protocol.ScheduleRow) {
	m.rows = rs
	m.rebuild()
}

// rebuild is the only place schedule cells are produced.
func (m *SchedulesModel) rebuild() {
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	rows := make([]table.Row, 0, len(m.rows))
	for _, r := range m.rows {
		if m.liveStatus != nil && r.TaskState == protocol.ScheduleTaskState_Bound {
			// The bound task's status is what its events say now, not what
			// the last list said.
			if st, ok := m.liveStatus(r.TaskId); ok {
				r.TaskStatus = st
			}
		}
		row := table.Row{
			hex.EncodeToString(r.Id.Id[:4]),
			string(r.Cron),
			cli.ScheduleNextCell(r),
			cli.ScheduleLastCell(r, now),
			cli.ScheduleTaskCell(r),
		}
		if m.showsWide() {
			row = append(row,
				string(r.Agent),
				truncateLeft(string(r.Repo), m.repoCellWidth()))
		}
		if m.showsAbout() {
			row = append(row, cli.ScheduleAboutCell(r))
		}
		rows = append(rows, row)
	}
	setTableRows(&m.table, rows)
}

// SelectedRow is the full row under the cursor, nil when there is none.
func (m *SchedulesModel) SelectedRow() *protocol.ScheduleRow {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return nil
	}
	return &m.rows[idx]
}

// SelectedID is the 32-hex id under the cursor ("" when none): the argument
// the schedule verbs take.
func (m *SchedulesModel) SelectedID() string {
	if r := m.SelectedRow(); r != nil {
		return hex.EncodeToString(r.Id.Id[:])
	}
	return ""
}

func (m SchedulesModel) Update(msg tea.Msg) (SchedulesModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m SchedulesModel) View() string { return m.table.View() }

// --- keys and results ---

// ScheduleShowMsg carries `schedule show`'s answer to the detail popup.
type ScheduleShowMsg struct {
	ID     string
	Detail protocol.ScheduleDetail
	Err    error
}

// DoScheduleShow fetches one schedule's detail through the long-lived client.
func DoScheduleShow(c *cli.Client, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d, err := c.ScheduleShow(ctx, id)
		return ScheduleShowMsg{ID: id, Detail: d, Err: err}
	}
}

// schedShort is the id8 the results name a schedule by.
func schedShort(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// scheduleIDCmd runs schedule run / rm for one id through cli.ScheduleIDWith —
// the function the command line's `schedule run` / `schedule rm` reach through
// tuiVerbs — so the key and the typed verb say the same thing and name the
// target and the change (checklist 29/30). The result lands in cmdresult as a
// RecordResultMsg, which also refreshes the table (see Update).
func (a *App) scheduleIDCmd(sub, id string) tea.Cmd {
	act := verb.ScheduleIDAction{Sub: sub, ID: id}
	h := tuiVerbs{a}
	exec := a.schedExec
	return h.scheduleRecord("schedule "+sub, id, func(ctx context.Context, c *cli.Client, out io.Writer) error {
		if exec != nil {
			return exec(ctx, act, out)
		}
		return cli.ScheduleIDWith(ctx, c, act, out)
	})
}

func (a *App) onScheduleRunNow(msg tea.KeyMsg) (tea.Cmd, bool) {
	id := a.schedules.SelectedID()
	if id == "" {
		a.cmdresult.Append(WarnStyle.Render("schedule run: no schedule selected"))
		return nil, true
	}
	return a.scheduleIDCmd(verb.SubRun, id), true
}

// onScheduleDelete only ASKS: it arms schedDeleteID, and inSchedDeleteConfirm
// (the first overlay) takes the next key.
func (a *App) onScheduleDelete(msg tea.KeyMsg) (tea.Cmd, bool) {
	id := a.schedules.SelectedID()
	if id == "" {
		a.cmdresult.Append(WarnStyle.Render("schedule rm: no schedule selected"))
		return nil, true
	}
	a.schedDeleteID = id
	a.cmdresult.Append(WarnStyle.Render(schedDeletePrompt(id)))
	return nil, true
}

func schedDeletePrompt(id string) string {
	return fmt.Sprintf("schedule rm %s: delete it? y deletes (its task stays, a run in progress is not cancelled), any other key keeps it", schedShort(id))
}

// inSchedDeleteConfirm owns the keys while a delete awaits its answer: y
// deletes, anything else — q included, which must not quit under a pending
// destructive question — keeps the schedule.
func (a *App) inSchedDeleteConfirm(msg tea.KeyMsg) tea.Cmd {
	id := a.schedDeleteID
	a.schedDeleteID = ""
	if msg.String() == "y" || msg.String() == "Y" {
		return a.scheduleIDCmd(verb.SubRm, id)
	}
	a.cmdresult.Append(fmt.Sprintf("schedule rm %s: cancelled; nothing deleted", schedShort(id)))
	return nil
}

// onScheduleDetail is `d` in the schedules table: the popup holds
// ScheduleDetailText, which has the history and wait statistics the row has
// no room for, so it comes from the server (ScheduleShowMsg), not the row.
func (a *App) onScheduleDetail() (tea.Cmd, bool) {
	id := a.schedules.SelectedID()
	if id == "" {
		a.cmdresult.Append(WarnStyle.Render("no schedule selected"))
		return nil, true
	}
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("schedule show: not connected"))
		return nil, true
	}
	return DoScheduleShow(a.client, id), true
}

func (a *App) onScheduleShowMsg(msg ScheduleShowMsg) {
	if msg.Err != nil {
		a.cmdresult.Append(ErrorStyle.Render("schedule show " + schedShort(msg.ID) + " failed: " + msg.Err.Error()))
		return
	}
	a.detail.Open("Schedule detail", strings.TrimRight(cli.ScheduleDetailText(msg.Detail, time.Now()), "\n"))
}
