package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// idleWatcherRow maps one armed watcher to its row. Sink and "by" go through
// the cli renderers so this and `session await-idle ls` cannot disagree.
// Age is computed here, against now, for execRunInfoRow's reason.
func idleWatcherRow(w *protocol.AwaitIdleWatcherInfo, now time.Time) table.Row {
	age := "-"
	if w.ArmedUnixMs > 0 {
		age = now.Sub(time.UnixMilli(int64(w.ArmedUnixMs))).Truncate(time.Second).String()
	}
	sink := cli.AwaitIdleSinkString(w.Sink)
	if w.Sink == protocol.AwaitIdleSink_Board {
		sink += " " + string(w.Topic)
	}
	return table.Row{
		fmt.Sprintf("%d", w.WatcherId),
		pfShortID(FormatTaskID(w.TaskId)),
		sink,
		fmt.Sprintf("%dms", w.ThresholdMs),
		age,
		cli.AwaitIdleWatcherBy(w),
	}
}

// IdleWatchersModal lists the armed await-idle watchers this operator can
// see. Opened with `I`, closed with Esc, `x` arms a y/n kill confirmation.
//
// ForwardsModal-shaped: no ApplyEvent, because watchers have no push
// subscription — it is fetched on open and after a kill.
type IdleWatchersModal struct {
	open     bool
	table    table.Model
	baseCols []table.Column
	watchers []protocol.AwaitIdleWatcherInfo

	// confirmID == 0 means none: watcher ids start at 1.
	confirmID   uint64
	confirmTask string
}

// NewIdleWatchersModal constructs the modal. sink is the flex column: a board
// topic has no bound.
func NewIdleWatchersModal() IdleWatchersModal {
	cols := []table.Column{
		{Title: "id", Width: 6},
		{Title: "task", Width: 12},
		{Title: "sink", Width: 30},
		{Title: "threshold", Width: 10},
		{Title: "armed", Width: 9},
		{Title: "by", Width: 10},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	return IdleWatchersModal{table: t, baseCols: cols}
}

func (m *IdleWatchersModal) IsOpen() bool { return m.open }
func (m *IdleWatchersModal) Open()        { m.open = true }
func (m *IdleWatchersModal) Close()       { m.open = false }

// SetSize propagates terminal dimensions into the table (full-screen overlay),
// reserving 4 rows for border + header + footer as ExecsModal does.
func (m *IdleWatchersModal) SetSize(w, h int) {
	m.table.SetWidth(w - 4)
	m.table.SetColumns(fitColumns(m.baseCols, w-4, flexColumn(m.baseCols, "sink")))
	m.table.SetHeight(h - 4)
}

// ApplySnapshot replaces the rows with the given server listing.
func (m *IdleWatchersModal) ApplySnapshot(ws []protocol.AwaitIdleWatcherInfo) {
	m.watchers = make([]protocol.AwaitIdleWatcherInfo, len(ws))
	copy(m.watchers, ws)
	now := time.Now()
	rows := make([]table.Row, 0, len(m.watchers))
	for i := range m.watchers {
		rows = append(rows, idleWatcherRow(&m.watchers[i], now))
	}
	setTableRows(&m.table, rows)
}

// SelectedID returns the watcher id under the cursor.
func (m *IdleWatchersModal) SelectedID() (uint64, bool) {
	if len(m.watchers) == 0 {
		return 0, false
	}
	i := m.table.Cursor()
	if i < 0 || i >= len(m.watchers) {
		return 0, false
	}
	return m.watchers[i].WatcherId, true
}

// IsConfirming reports whether a kill confirmation is pending.
func (m *IdleWatchersModal) IsConfirming() bool { return m.confirmID != 0 }

// BeginKillConfirm asks before killing, as ExecsModal does: the operator sees
// every agent's watchers, so the row under the cursor may be someone else's
// insurance.
func (m *IdleWatchersModal) BeginKillConfirm() bool {
	id, ok := m.SelectedID()
	if !ok {
		return false
	}
	m.confirmID = id
	m.confirmTask = FormatTaskID(m.watchers[m.table.Cursor()].TaskId)
	return true
}

// CancelKillConfirm clears a pending confirmation without killing anything.
func (m *IdleWatchersModal) CancelKillConfirm() { m.confirmID, m.confirmTask = 0, "" }

// ConfirmKill returns the pending kill's id and clears the pending state.
func (m *IdleWatchersModal) ConfirmKill() (uint64, bool) {
	if m.confirmID == 0 {
		return 0, false
	}
	id := m.confirmID
	m.CancelKillConfirm()
	return id, true
}

func (m IdleWatchersModal) Update(msg tea.Msg) (IdleWatchersModal, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m IdleWatchersModal) View() string {
	header := HeaderStyle.Render(fmt.Sprintf("armed await-idle watchers (%d)", len(m.watchers)))
	box := PanelStyleFocused.Padding(0, 1)
	if m.confirmID != 0 {
		prompt := fmt.Sprintf("kill watcher %d (%s) ? (y/n)", m.confirmID, pfShortID(m.confirmTask))
		return box.Render(header + "\n" + m.table.View() + "\n" + FooterStyle.Render(prompt))
	}
	footer := FooterStyle.Render("x: kill · Esc: close")
	if len(m.watchers) == 0 {
		return box.Render(header + "\n" + "no armed watchers" + "\n" + footer)
	}
	return box.Render(header + "\n" + m.table.View() + "\n" + footer)
}
