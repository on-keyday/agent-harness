package tui

import (
	"context"
	"errors"
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

// TrsfStateMsg carries one transport reading for the modal's trsf mode.
// SampledAt is when the ANSWERER walked its connections, by its own clock.
type TrsfStateMsg struct {
	Conns     []protocol.TrsfConnState
	SampledAt int64
	Err       error
}

// trsfTickMsg asks for the next reading. Only the modal's trsf mode arms it,
// and only while it is open.
//
// gen identifies the poll it belongs to, the way rawGenSeq identifies a raw
// pane's connection attempt. Leaving the mode does not cancel a tick already
// in flight, so without this a toggle off and back on inside one interval
// would leave the old chain rescheduling alongside the new one and the reading
// would poll at twice the rate the header claims — and again per round trip.
type trsfTickMsg struct{ gen int }

// errNotConnected is what the reading shows in place of numbers when the TUI
// holds no client. It goes in the modal's own header rather than through
// cmdresult, which is a full-screen overlay away from being visible.
var errNotConnected = errors.New("not connected")

// DoTrsfState reads congestion state from one end of the fleet. runnerCID
// empty asks the SERVER about its own connections.
//
// On a.client, the connection this TUI already holds — the pattern every other
// Do* here follows, and the reason TrsfStateOn is a method on the client rather
// than a dial-and-close helper.
func DoTrsfState(c *cli.Client, runnerCID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conns, sampledAt, err := c.TrsfStateOn(ctx, runnerCID)
		return TrsfStateMsg{Conns: conns, SampledAt: sampledAt, Err: err}
	}
}

// trsfTick schedules the next poll of generation gen.
func trsfTick(every time.Duration, gen int) tea.Cmd {
	return tea.Tick(every, func(time.Time) tea.Msg { return trsfTickMsg{gen: gen} })
}

// DefaultTrsfInterval is how often the modal re-reads when the operator did not
// say. Fast enough that BLOCK% describes something recent, slow enough that
// watching it is not itself load.
const DefaultTrsfInterval = time.Second

// connsMode is which question the modal is asking about the same connections:
// which exist, or what their transport is doing.
type connsMode int

const (
	connsIdentity connsMode = iota
	connsTrsf
)

// ConnsModal is a scrollable overlay that renders the live connections table
// over the main TUI. It is opened via a key binding ('C') and closed with Esc.
// Initial population is via ConnListWith (DoConnSnapshot); live updates arrive
// as ConnStatusMsg events from SubscribeConnStatus.
//
// Rows are keyed by the CID string (ConnInfo.Cid): ConnOpened adds a row,
// ConnIdentified updates role, ConnClosed removes the row.
//
// 't' switches it to the trsf reading — the same connections, answering what
// each one's transport is doing. It shares the modal rather than opening a
// second one for the reason `conns --trsf` shares the verb: it is a different
// question about the same rows, and the selected row is keyed by cid in both
// modes so it survives the switch.
type ConnsModal struct {
	open     bool
	table    table.Model
	baseCols []table.Column      // natural column sizing; see fitColumns
	rowConns []protocol.ConnInfo // parallel slice: rowConns[i] = full info for row i
	byCID    map[string]int      // cid string → index in rowConns; rebuilt on ApplySnapshot / on event

	mode  connsMode
	width int

	// trsf mode. sampler holds the previous reading, which is what the delta
	// columns are a delta OF; trsfRows is what it rendered.
	sampler  cli.TrsfSampler
	trsfRows []cli.TrsfRow
	// Empty means the SERVER's own connections; otherwise a runner is being
	// asked about its own, which needs the global view.
	target   string
	every    time.Duration
	trsfErr  string
	readings int // how many have landed; the first has no deltas by construction

	// The connection the cursor should be on once a row set carrying it
	// arrives. Switching INTO the reading finds it empty — its first rows are
	// a round trip away — so restoring at the moment of the switch always
	// missed, and the cursor snapped to whatever landed first.
	pendingSelect string
}

// identityColumns is the "which connections exist" column set.
func identityColumns() []table.Column {
	return []table.Column{
		{Title: "CID", Width: 30},
		{Title: "Role", Width: 11},
		{Title: "Principal", Width: 9},
		{Title: "Runner", Width: 9},
		{Title: "Age", Width: 8},
		{Title: "State", Width: 7},
	}
}

// trsfColumns is the "what is each connection's transport doing" column set —
// the same reading `conns --trsf` prints, in the same order, because it is the
// same rows from the same cli.TrsfSampler.
//
// All twelve, rather than a subset chosen to fit: fitColumns squeezes to the
// terminal, and a column dropped here would make the TUI a different answer
// from the CLI's for no reason an operator could see.
func trsfColumns() []table.Column {
	return []table.Column{
		{Title: "CID", Width: 30},
		{Title: "Role", Width: 8},
		{Title: "Task", Width: 8},
		{Title: "Cwnd", Width: 8},
		{Title: "Inflight", Width: 8},
		{Title: "Srtt", Width: 8},
		{Title: "Queue", Width: 7},
		{Title: "Loss+", Width: 6},
		{Title: "Spur+", Width: 6},
		{Title: "Loop+", Width: 6},
		{Title: "Block%", Width: 6},
		{Title: "Wait", Width: 11},
	}
}

// NewConnsModal constructs a ConnsModal with fixed column widths.
func NewConnsModal() ConnsModal {
	cols := identityColumns()
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	return ConnsModal{
		table:    t,
		baseCols: cols,
		byCID:    make(map[string]int),
		every:    DefaultTrsfInterval,
	}
}

func (m *ConnsModal) IsOpen() bool { return m.open }

func (m *ConnsModal) Open() { m.open = true }

// Close leaves the modal in identity mode and forgets the reading. Reopening
// starts a fresh series rather than showing a delta across the gap the modal
// was shut for — the counters advanced during it and nothing measured that
// interval.
func (m *ConnsModal) Close() {
	m.open = false
	m.setMode(connsIdentity)
	m.resetTrsf()
}

// IsTrsf reports whether the trsf reading is showing, which is also whether
// the poll should keep running.
func (m *ConnsModal) IsTrsf() bool { return m.mode == connsTrsf }

// TrsfTarget is the runner whose own transport is being read; empty is the
// server's.
func (m *ConnsModal) TrsfTarget() string { return m.target }

// TrsfEvery is the poll interval in force.
func (m *ConnsModal) TrsfEvery() time.Duration { return m.every }

// SetTrsfEvery overrides the interval (`conns --trsf --watch 200ms`). A
// non-positive duration keeps the default rather than spinning.
func (m *ConnsModal) SetTrsfEvery(d time.Duration) {
	if d > 0 {
		m.every = d
	}
}

// EnterTrsf switches to the reading, aimed at runnerCID (empty = the server).
func (m *ConnsModal) EnterTrsf(runnerCID string) {
	m.setTarget(runnerCID)
	m.setMode(connsTrsf)
}

// ToggleTrsf flips between the two questions and reports which is now showing.
func (m *ConnsModal) ToggleTrsf() bool {
	if m.mode == connsTrsf {
		m.setMode(connsIdentity)
		return false
	}
	m.setMode(connsTrsf)
	return true
}

// setTarget aims the reading at a different answerer, discarding the series so
// far. The counters of one host are not the counters of another, and the
// elapsed between the last reading and the first from the new target measures
// nothing that happened on it — carrying either over would render a delta and
// a BLOCK% over an interval the new answerer never lived through.
func (m *ConnsModal) setTarget(runnerCID string) {
	if m.target == runnerCID {
		return
	}
	m.target = runnerCID
	m.resetTrsf()
}

func (m *ConnsModal) resetTrsf() {
	m.sampler = cli.TrsfSampler{}
	m.trsfRows = nil
	m.trsfErr = ""
	m.readings = 0
	// Both callers are "start over" moments — a new answerer, or the modal
	// closing — and a note naming a connection neither will carry would block
	// the next switch's own from ever being made.
	m.pendingSelect = ""
}

// SelectedRunnerCID is the cid of the highlighted row when that row is a
// RUNNER connection — what 'enter' retargets the reading to. Any other role
// has no separate transport to ask about: it is one end of a connection the
// server is already reporting.
func (m *ConnsModal) SelectedRunnerCID() (string, bool) {
	i := m.table.Cursor()
	if m.mode == connsTrsf {
		if i < 0 || i >= len(m.trsfRows) {
			return "", false
		}
		if !strings.EqualFold(m.trsfRows[i].Role, protocol.ConnRole_Runner.String()) {
			return "", false
		}
		return m.trsfRows[i].CID, true
	}
	if i < 0 || i >= len(m.rowConns) {
		return "", false
	}
	if m.rowConns[i].Role != protocol.ConnRole_Runner {
		return "", false
	}
	return string(m.rowConns[i].Cid), true
}

// TargetSelectedRunner aims the reading at the highlighted runner row.
func (m *ConnsModal) TargetSelectedRunner() bool {
	cid, ok := m.SelectedRunnerCID()
	if !ok {
		return false
	}
	m.setTarget(cid)
	return true
}

// TargetServer aims the reading back at the server's own connections.
func (m *ConnsModal) TargetServer() { m.setTarget("") }

// setMode swaps the column set and the rows together, keeping the highlighted
// CONNECTION rather than the highlighted row index — the two modes can hold
// different rows, since one is fed by conns.status events and the other by a
// poll.
func (m *ConnsModal) setMode(mode connsMode) {
	if m.mode == mode {
		return
	}
	m.rememberSelection()
	m.mode = mode
	m.baseCols = identityColumns()
	if mode == connsTrsf {
		m.baseCols = trsfColumns()
	}
	m.swapColumnsAndRebuild(m.fittedColumns())
	m.restoreSelection()
}

// rememberSelection notes the highlighted connection so a later row set can
// put the cursor back on it. An outstanding note wins: it was made by a switch
// that has not been honoured yet, and overwriting it with wherever the cursor
// happens to sit in the meantime is how the selection gets lost.
func (m *ConnsModal) rememberSelection() {
	if m.pendingSelect != "" {
		return
	}
	if cid := m.selectedCID(); cid != "" {
		m.pendingSelect = cid
	}
}

// restoreSelection honours the note once a row set carrying that connection
// exists, and keeps waiting otherwise.
func (m *ConnsModal) restoreSelection() {
	if m.pendingSelect == "" {
		return
	}
	if m.selectCID(m.pendingSelect) {
		m.pendingSelect = ""
	}
}

// fittedColumns sizes the current base set to the width the modal was given.
// Before the first SetSize there is no width to fit to, so the natural widths
// stand.
func (m *ConnsModal) fittedColumns() []table.Column {
	if m.width <= 0 {
		return m.baseCols
	}
	return fitColumns(m.baseCols, m.width-4, flexColumn(m.baseCols, "CID"))
}

// swapColumnsAndRebuild swaps columns and rows together. The order is
// load-bearing: bubbles re-renders on each of SetRows and SetColumns and
// indexes row[i] per column, so a moment holding N columns against
// M-cell rows panics. Empty first, then the columns, then rebuild.
//
// Same shape and same reason as RunnersModel.swapColumnsAndRebuild — that pane
// hit the panic within minutes of gaining a conditional column set, and this
// one swaps twelve columns for six on a keystroke.
func (m *ConnsModal) swapColumnsAndRebuild(cols []table.Column) {
	m.table.SetRows(nil)
	m.table.SetColumns(cols)
	m.rebuildRows()
}

// selectedCID is the cid of the highlighted row in whichever mode is showing.
func (m *ConnsModal) selectedCID() string {
	i := m.table.Cursor()
	if m.mode == connsTrsf {
		if i >= 0 && i < len(m.trsfRows) {
			return m.trsfRows[i].CID
		}
		return ""
	}
	if i >= 0 && i < len(m.rowConns) {
		return string(m.rowConns[i].Cid)
	}
	return ""
}

// selectCID puts the cursor on a connection by cid and reports whether the
// current mode carries it. A cid it does not leaves the cursor where
// setTableRows put it.
func (m *ConnsModal) selectCID(cid string) bool {
	if cid == "" {
		return false
	}
	if m.mode == connsTrsf {
		for i := range m.trsfRows {
			if m.trsfRows[i].CID == cid {
				m.table.SetCursor(i)
				return true
			}
		}
		return false
	}
	if i, ok := m.byCID[cid]; ok {
		m.table.SetCursor(i)
		return true
	}
	return false
}

// SetSize propagates terminal dimensions into the table (full-screen overlay).
func (m *ConnsModal) SetSize(w, h int) {
	// Reserve 4 rows for border + header + footer lines.
	m.width = w
	m.table.SetWidth(w - 4)
	m.table.SetColumns(m.fittedColumns())
	m.table.SetHeight(h - 4)
}

// ApplySnapshot replaces all rows with the given slice and rebuilds the CID index.
func (m *ConnsModal) ApplySnapshot(conns []protocol.ConnInfo) {
	m.rememberSelection()
	m.rowConns = make([]protocol.ConnInfo, len(conns))
	copy(m.rowConns, conns)
	m.byCID = make(map[string]int, len(conns))
	for i := range m.rowConns {
		m.byCID[connCIDKey(&m.rowConns[i])] = i
	}
	m.rebuildRows()
	m.restoreSelection()
}

// ApplyTrsf records one reading and renders it. The sampler holds the previous
// one, so the delta columns are a delta over the interval the ANSWERER
// measured — its sampled-at minus its last one, never this process's clock.
func (m *ConnsModal) ApplyTrsf(conns []protocol.TrsfConnState, sampledAt int64) {
	m.rememberSelection()
	m.trsfErr = ""
	m.trsfRows = m.sampler.Observe(conns, sampledAt)
	m.readings++
	m.rebuildRows()
	m.restoreSelection()
}

// SetTrsfError records why a reading did not arrive. The previous rows stay on
// screen underneath it: a refused or failed read says nothing about whether the
// numbers already shown were true when they were taken.
func (m *ConnsModal) SetTrsfError(err error) {
	if err == nil {
		m.trsfErr = ""
		return
	}
	m.trsfErr = err.Error()
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

// rebuildRows is the ONLY place cells are produced, so the cell count cannot
// disagree with the column count the current mode declared.
func (m *ConnsModal) rebuildRows() {
	if m.mode == connsTrsf {
		rows := make([]table.Row, 0, len(m.trsfRows))
		for i := range m.trsfRows {
			rows = append(rows, trsfRowToRow(&m.trsfRows[i]))
		}
		setTableRows(&m.table, rows)
		return
	}
	rows := make([]table.Row, 0, len(m.rowConns))
	for i := range m.rowConns {
		rows = append(rows, connInfoToRow(&m.rowConns[i]))
	}
	setTableRows(&m.table, rows)
}

// connInfoToRow maps a ConnInfo to a table.Row (6 columns).
// The cid is "transport:ip:port-id" — it already carries the remote ip:port.
// Runner is the runner PROCESS a runner conn belongs to; see
// cli.connInfoTextLine for why it is a column of its own and not folded into
// Principal.
func connInfoToRow(ci *protocol.ConnInfo) table.Row {
	cid := string(ci.Cid)
	role := strings.ToLower(ci.Role.String())
	principal := cli.PrincipalShort(ci.PrincipalTask.Id[:])
	runner := cli.PrincipalShort(ci.PrincipalRunner.Id[:])
	age := cli.ConnAge(ci.ConnectedAt)
	state := "ok"
	if !ci.Identified() {
		state = "unident"
	}
	return table.Row{cid, role, principal, runner, age, state}
}

// trsfRowToRow maps one already-rendered reading to a table.Row (12 columns).
// Every cell arrives as a string, absences included, so nothing here decides
// what "-" means. Only the role is restyled, to the lowercase this modal's
// identity rows already use.
func trsfRowToRow(r *cli.TrsfRow) table.Row {
	return table.Row{
		r.CID, strings.ToLower(r.Role), r.Task,
		r.Cwnd, r.InFlight, r.SRTT, r.Queue,
		r.LossD, r.SpurD, r.LoopD, r.BlockPct, r.Wait,
	}
}

// connCIDKey returns the CID as a string for use as a map key.
func connCIDKey(ci *protocol.ConnInfo) string {
	return string(ci.Cid)
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
	box := PanelStyleFocused.Padding(0, 1)
	if m.mode != connsTrsf {
		header := HeaderStyle.Render(fmt.Sprintf("connections (%d)", len(m.rowConns)))
		footer := FooterStyle.Render("Esc: close · t: trsf")
		return box.Render(header + "\n" + m.table.View() + "\n" + footer)
	}

	target := "server"
	if m.target != "" {
		target = m.target
	}
	head := fmt.Sprintf("connections · trsf (%s) every %v — %d conn(s)", target, m.every, len(m.trsfRows))
	header := HeaderStyle.Render(head)
	if m.trsfErr != "" {
		header += "\n" + ErrorStyle.Render("trsf: "+m.trsfErr)
	} else if m.readings == 1 {
		// Say why the right-hand columns are empty, rather than letting a
		// screen of "-" read as "this end reports nothing".
		header += "\n" + FooterStyle.Render("(deltas appear on the next reading — they need two)")
	}
	footer := FooterStyle.Render("Esc: close · t: back · enter: read the selected runner · s: read the server")
	return box.Render(header + "\n" + m.table.View() + "\n" + footer)
}
