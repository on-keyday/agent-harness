package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ConnSnapshotMsg carries the initial snapshot from ConnListWith, dispatched
// as the first step of opening the connections view.
type ConnSnapshotMsg struct {
	Conns []protocol.ConnInfo
	Err   error
}

// DoConnSnapshot fetches the live connection snapshot via the long-lived
// client (ConnListWith — no fresh dial) and returns a ConnSnapshotMsg.
func DoConnSnapshot(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conns, err := c.ConnListWith(ctx)
		return ConnSnapshotMsg{Conns: conns, Err: err}
	}
}

// ConnsModal is a scrollable overlay that renders the live connections table
// over the main TUI. It is opened via a key binding ('C') and closed with Esc.
// Initial population is via ConnListWith (DoConnSnapshot); live updates arrive
// as ConnStatusMsg events from SubscribeConnStatus.
//
// Rows are keyed by the CID string (ConnInfo.Cid): ConnOpened adds a row,
// ConnIdentified updates role, ConnClosed removes the row.
type ConnsModal struct {
	open     bool
	table    table.Model
	rowConns []protocol.ConnInfo     // parallel slice: rowConns[i] = full info for row i
	byCID    map[string]int           // cid string → index in rowConns; rebuilt on ApplySnapshot / on event
}

// NewConnsModal constructs a ConnsModal with fixed column widths.
func NewConnsModal() ConnsModal {
	cols := []table.Column{
		{Title: "Remote-Addr", Width: 22},
		{Title: "Role", Width: 11},
		{Title: "Principal", Width: 9},
		{Title: "Age", Width: 8},
		{Title: "State", Width: 7},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	return ConnsModal{
		table: t,
		byCID: make(map[string]int),
	}
}

func (m *ConnsModal) IsOpen() bool { return m.open }

func (m *ConnsModal) Open() { m.open = true }

func (m *ConnsModal) Close() { m.open = false }

// SetSize propagates terminal dimensions into the table (full-screen overlay).
func (m *ConnsModal) SetSize(w, h int) {
	// Reserve 4 rows for border + header + footer lines.
	m.table.SetWidth(w - 4)
	m.table.SetHeight(h - 4)
}

// ApplySnapshot replaces all rows with the given slice and rebuilds the CID index.
func (m *ConnsModal) ApplySnapshot(conns []protocol.ConnInfo) {
	m.rowConns = make([]protocol.ConnInfo, len(conns))
	copy(m.rowConns, conns)
	m.byCID = make(map[string]int, len(conns))
	for i := range m.rowConns {
		m.byCID[connCIDKey(&m.rowConns[i])] = i
	}
	m.rebuildRows()
}

// ApplyEvent processes a ConnStatusEvent: ConnOpened adds, ConnIdentified
// updates, ConnClosed removes.
func (m *ConnsModal) ApplyEvent(ev protocol.ConnStatusEvent) {
	key := connCIDKey(&ev.Info)
	switch ev.Kind {
	case protocol.StatusEventKind_ConnOpened:
		if _, ok := m.byCID[key]; ok {
			// Already present (e.g. snapshot race): update in place.
			idx := m.byCID[key]
			m.rowConns[idx] = ev.Info
		} else {
			m.byCID[key] = len(m.rowConns)
			m.rowConns = append(m.rowConns, ev.Info)
		}
	case protocol.StatusEventKind_ConnIdentified:
		if idx, ok := m.byCID[key]; ok {
			m.rowConns[idx] = ev.Info
		} else {
			// Identified event without a prior Opened (e.g. replay catchup);
			// treat as an insert so the conn is visible.
			m.byCID[key] = len(m.rowConns)
			m.rowConns = append(m.rowConns, ev.Info)
		}
	case protocol.StatusEventKind_ConnClosed:
		idx, ok := m.byCID[key]
		if !ok {
			return
		}
		last := len(m.rowConns) - 1
		if idx != last {
			// Swap with tail so we avoid an O(n) slice copy.
			m.rowConns[idx] = m.rowConns[last]
			m.byCID[connCIDKey(&m.rowConns[idx])] = idx
		}
		m.rowConns = m.rowConns[:last]
		delete(m.byCID, key)
	}
	m.rebuildRows()
}

// rebuildRows translates rowConns into bubbles/table rows.
func (m *ConnsModal) rebuildRows() {
	rows := make([]table.Row, 0, len(m.rowConns))
	for i := range m.rowConns {
		rows = append(rows, connInfoToRow(&m.rowConns[i]))
	}
	m.table.SetRows(rows)
}

// connInfoToRow maps a ConnInfo to a table.Row (5 columns).
func connInfoToRow(ci *protocol.ConnInfo) table.Row {
	addr := string(ci.RemoteAddr)
	role := strings.ToLower(ci.Role.String())
	principal := principalShortTUI(ci.PrincipalTask.Id[:])
	age := connAgeTUI(ci.ConnectedAt)
	state := "ok"
	if !ci.Identified() {
		state = "unident"
	}
	return table.Row{addr, role, principal, age, state}
}

// connCIDKey returns the CID as a string for use as a map key.
func connCIDKey(ci *protocol.ConnInfo) string {
	return string(ci.Cid)
}

// principalShortTUI returns the first 8 hex characters of a task id, or "-".
// Mirrors cli.principalShort but lives in the tui package.
func principalShortTUI(b []byte) string {
	allZ := true
	for _, v := range b {
		if v != 0 {
			allZ = false
			break
		}
	}
	if allZ {
		return "-"
	}
	hex := fmt.Sprintf("%x", b)
	if len(hex) > 8 {
		return hex[:8]
	}
	return hex
}

// connAgeTUI returns a human-readable age string for a ConnInfo.
// Mirrors cli.connAge but lives in the tui package.
func connAgeTUI(connectedAtNano uint64) string {
	if connectedAtNano == 0 {
		return "0s"
	}
	since := time.Since(time.Unix(0, int64(connectedAtNano)))
	if since < 0 {
		since = 0
	}
	secs := int64(since.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	m := secs / 60
	s := secs % 60
	return fmt.Sprintf("%dm%ds", m, s)
}

func (m ConnsModal) Update(msg tea.Msg) (ConnsModal, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m ConnsModal) View() string {
	count := len(m.rowConns)
	header := HeaderStyle.Render(fmt.Sprintf("connections (%d)", count))
	footer := FooterStyle.Render("Esc: close")
	box := PanelStyleFocused.Padding(0, 1)
	return box.Render(header + "\n" + m.table.View() + "\n" + footer)
}
