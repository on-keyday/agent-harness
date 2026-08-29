package tui

import (
	"fmt"
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// execRunInfoRow maps one running exec to its table row.
//
// Origin and the command both go through the cli renderers rather than being
// re-derived here, for the reason portForwardInfoRow gives: the registry is
// shared, so two identical argvs from different clients are told apart only by
// origin, and a second rendering would let this surface and `harness-cli exec
// ls` disagree about what an argv or an origin looks like.
//
// Age is computed HERE, against now, because the row is a snapshot: the table
// holds strings, so a row rendered once does not tick. Reopening or a kill's
// refresh re-fetches and re-ages it. Anything better would mean a ticker
// redrawing a modal nobody is watching.
func execRunInfoRow(e *protocol.ExecRunInfo, now time.Time) table.Row {
	age := "-"
	if e.StartedUnixMs > 0 {
		age = now.Sub(time.UnixMilli(int64(e.StartedUnixMs))).Truncate(time.Second).String()
	}
	return table.Row{
		fmt.Sprintf("%d", e.ExecId),
		pfShortID(FormatTaskID(e.TaskId)),
		age,
		cli.ExecRunOrigin(e),
		cli.ExecRunArgvString(e.Argv),
	}
}

// ExecsModal is a full-screen overlay listing every running exec visible to
// this operator — not just ones this TUI started. Opened with `e`, closed with
// Esc, `x` arms a y/n kill confirmation for the selected row.
//
// It is ForwardsModal with a different row shape, deliberately: `exec ls` /
// `exec kill` is the same list-and-kill pair as `forward ls` / `forward kill`,
// against the same kind of shared server-side registry, and the two surfaces
// behaving differently would be an asymmetry with no reason behind it. Before
// this existed `exec ls` had only the cmdline path, which appends into a
// FIVE-line viewport that scrolls to the bottom on every write — so a listing
// of more than four execs showed its tail and hid its own header.
//
// Like ForwardsModal and unlike ConnsModal there is no incremental ApplyEvent:
// execs have no live push subscription, so a fresh fetch is the only refresh.
type ExecsModal struct {
	open     bool
	table    table.Model
	baseCols []table.Column // natural column sizing; see fitColumns
	execs    []protocol.ExecRunInfo

	// Pending kill confirmation. confirmID == 0 means none — the server's
	// exec-id counter starts at 1, so 0 is never a real exec id.
	confirmID   uint64
	confirmTask string
	confirmArgv string
}

// NewExecsModal constructs an ExecsModal with fixed column widths.
//
// command is the flex column rather than origin — the opposite of
// ForwardsModal — because a command line has no bound while an origin is
// "<kind> <cid>" and does. bubbles/table hard-truncates a cell to its column
// width with an ellipsis (table.go renderRow), so the column that must absorb
// the leftover width is the one whose content is unbounded.
func NewExecsModal() ExecsModal {
	cols := []table.Column{
		{Title: "id", Width: 6},
		{Title: "task", Width: 12},
		{Title: "age", Width: 9},
		{Title: "origin", Width: 28},
		{Title: "command", Width: 40},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	return ExecsModal{table: t, baseCols: cols}
}

func (m *ExecsModal) IsOpen() bool { return m.open }
func (m *ExecsModal) Open()        { m.open = true }
func (m *ExecsModal) Close()       { m.open = false }

// SetSize propagates terminal dimensions into the table (full-screen overlay).
// Reserve 4 rows for border + header + footer, as ForwardsModal does.
func (m *ExecsModal) SetSize(w, h int) {
	m.table.SetWidth(w - 4)
	m.table.SetColumns(fitColumns(m.baseCols, w-4, flexColumn(m.baseCols, "command")))
	m.table.SetHeight(h - 4)
}

// ApplySnapshot replaces the rows with the given server snapshot.
func (m *ExecsModal) ApplySnapshot(es []protocol.ExecRunInfo) {
	m.execs = make([]protocol.ExecRunInfo, len(es))
	copy(m.execs, es)
	now := time.Now()
	rows := make([]table.Row, 0, len(m.execs))
	for i := range m.execs {
		rows = append(rows, execRunInfoRow(&m.execs[i], now))
	}
	m.table.SetRows(rows)
}

// ApplyEvent folds one execs.status event into the rows. Two kinds, because an
// exec has nothing that moves while it runs: started inserts, ended removes.
//
// The age column still ages only on a refetch — it is rendered into a string at
// row-build time, which is what execRunInfoRow's own comment says. That is
// unchanged by this: an event rebuilds the rows and re-ages them as a side
// effect, which is more often than before, not less.
func (m *ExecsModal) ApplyEvent(ev protocol.ExecStatusEvent) {
	selected, hadSelection := m.SelectedID()

	idx := -1
	for i := range m.execs {
		if m.execs[i].ExecId == ev.Info.ExecId {
			idx = i
			break
		}
	}

	switch ev.Kind {
	case protocol.StatusEventKind_ExecStarted:
		if idx >= 0 {
			m.execs[idx] = ev.Info
		} else {
			m.execs = append(m.execs, ev.Info)
			sort.Slice(m.execs, func(i, j int) bool {
				return m.execs[i].ExecId < m.execs[j].ExecId
			})
		}
	case protocol.StatusEventKind_ExecEnded:
		if idx < 0 {
			return
		}
		m.execs = append(m.execs[:idx], m.execs[idx+1:]...)
	default:
		return
	}

	now := time.Now()
	rows := make([]table.Row, 0, len(m.execs))
	for i := range m.execs {
		rows = append(rows, execRunInfoRow(&m.execs[i], now))
	}
	m.table.SetRows(rows)
	if hadSelection {
		for i := range m.execs {
			if m.execs[i].ExecId == selected {
				m.table.SetCursor(i)
				break
			}
		}
	}
}

// SelectedID returns the exec id under the cursor.
func (m *ExecsModal) SelectedID() (uint64, bool) {
	if len(m.execs) == 0 {
		return 0, false
	}
	i := m.table.Cursor()
	if i < 0 || i >= len(m.execs) {
		return 0, false
	}
	return m.execs[i].ExecId, true
}

// IsConfirming reports whether a kill confirmation is currently pending.
// While true, App swallows every key except y/n/Esc so the table cannot move
// under the operator mid-confirm.
func (m *ExecsModal) IsConfirming() bool { return m.confirmID != 0 }

// BeginKillConfirm arms the y/n confirmation for the row under the cursor.
// Returns false (no-op) if nothing is selected.
//
// Confirmed rather than killed outright, for ForwardsModal's reason and one of
// its own: the registry is shared, so the row under the cursor may be another
// operator's `make test` or — now that the ssh gateway maps `ssh host cmd` to
// this verb — the long-lived bootstrap holding somebody's editor session open.
func (m *ExecsModal) BeginKillConfirm() bool {
	id, ok := m.SelectedID()
	if !ok {
		return false
	}
	e := &m.execs[m.table.Cursor()]
	m.confirmID = id
	m.confirmTask = FormatTaskID(e.TaskId)
	m.confirmArgv = cli.ExecRunArgvString(e.Argv)
	return true
}

// CancelKillConfirm clears a pending confirmation without killing anything.
func (m *ExecsModal) CancelKillConfirm() {
	m.confirmID, m.confirmTask, m.confirmArgv = 0, "", ""
}

// ConfirmKill returns the pending kill's id and clears the pending state. ok is
// false if nothing was pending (defensive — App only calls this from the "y"
// branch while IsConfirming is true).
func (m *ExecsModal) ConfirmKill() (id uint64, ok bool) {
	if m.confirmID == 0 {
		return 0, false
	}
	id = m.confirmID
	m.CancelKillConfirm()
	return id, true
}

func (m ExecsModal) Update(msg tea.Msg) (ExecsModal, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m ExecsModal) View() string {
	header := HeaderStyle.Render(fmt.Sprintf("running execs (%d)", len(m.execs)))
	box := PanelStyleFocused.Padding(0, 1)
	if m.confirmID != 0 {
		prompt := fmt.Sprintf("kill exec %d (%s  %s) ? (y/n)",
			m.confirmID, pfShortID(m.confirmTask), m.confirmArgv)
		return box.Render(header + "\n" + m.table.View() + "\n" + FooterStyle.Render(prompt))
	}
	footer := FooterStyle.Render("x: kill · Esc: close")
	if len(m.execs) == 0 {
		return box.Render(header + "\n" + "no running execs" + "\n" + footer)
	}
	return box.Render(header + "\n" + m.table.View() + "\n" + footer)
}
