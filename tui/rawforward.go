package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// rawTUIRingBytes caps the modal's output buffer. Same reasoning as the WebUI
// pane's ring: this is a debugging window, not a transcript.
const rawTUIRingBytes = 64 * 1024

// rawHeadKeepBytes is the front of a connection's output that is never
// dropped. A pure keep-the-newest ring is right for a log and wrong here: what
// arrives FIRST on a raw connection is the status line and the headers — the
// part worth reading — and a response larger than the ring used to push
// exactly that out, leaving a body with no idea what it answered. Keeping both
// ends and saying how much was dropped in between costs one line of output.
const rawHeadKeepBytes = 16 * 1024

// rawConnectTimeout bounds the connect RPC. Every other Do* in this package
// bounds its RPC; without it a wedged open leaves the operator with a modal
// that says "connecting" forever and no way to cancel the attempt.
const rawConnectTimeout = 30 * time.Second

// rawPane is one live (or ended) raw connection. The WebUI has held several of
// these since it shipped (rawSlots in cli/raw_forward_wasm.go); the TUI held
// exactly one, which is what made esc destructive and a second target
// impossible.
type rawPane struct {
	taskID string
	host   string
	port   int
	// gen tags every message this pane's pump sends. It scopes a PANE: a
	// message whose gen matches no pane belongs to a pane the operator has
	// already closed, and is dropped.
	gen    uint64
	conn   *cli.RawConn
	cancel context.CancelFunc
	// head/tail bracket the output: the front is never dropped, the newest
	// bytes are, and elided counts what fell out between them.
	head   []byte
	tail   []byte
	elided int
	// inBytes/outBytes are totals, not what the ring retained: the list needs
	// to say how much actually crossed the connection.
	inBytes    int
	outBytes   int
	live       bool
	connecting bool
	note       string
}

func (p *rawPane) target() string { return fmt.Sprintf("%s:%d", p.host, p.port) }

// sanitizeOutput makes remote bytes safe to draw inside a bordered panel. The
// far end sends arbitrary bytes; an ESC sequence among them repositions the
// cursor, and the frame is then drawn over — the panel visibly disintegrates.
// The WebUI pane already replaces control bytes for the same reason, and this
// keeps one extra: CR moves the cursor to column 0 in a terminal (harmless in
// the WebUI's <pre>), so CRLF collapses to LF and a lone CR becomes a dot
// rather than overwriting the line and the left border with it.
func sanitizeOutput(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f, r >= 0x80 && r <= 0x9f:
			return '.'
		}
		return r
	}, s)
}

// AppendOutput adds received bytes, keeping the head and the newest tail and
// counting what fell out between them. See rawHeadKeepBytes.
func (p *rawPane) AppendOutput(b []byte) {
	p.inBytes += len(b)
	if n := rawHeadKeepBytes - len(p.head); n > 0 {
		if n > len(b) {
			n = len(b)
		}
		p.head = append(p.head, b[:n]...)
		b = b[n:]
	}
	if len(b) == 0 {
		return
	}
	p.tail = append(p.tail, b...)
	if cap := rawTUIRingBytes - rawHeadKeepBytes; len(p.tail) > cap {
		drop := len(p.tail) - cap
		p.elided += drop
		p.tail = append([]byte(nil), p.tail[drop:]...)
	}
}

// output renders what the pane has, with an explicit marker for the middle it
// dropped — silence there would read as "this is the whole response".
func (p *rawPane) output() []byte {
	if p.elided == 0 {
		return append(append([]byte(nil), p.head...), p.tail...)
	}
	marker := fmt.Sprintf("\n… %d bytes elided …\n", p.elided)
	out := append([]byte(nil), p.head...)
	out = append(out, marker...)
	return append(out, p.tail...)
}

// closeConn closes the connection and stops its pump. Idempotent. Closing is
// what deregisters the forward server-side.
func (p *rawPane) closeConn() {
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

// rawModalMode is which of the modal's three screens is showing. The list is
// home: a tab strip put the panes and one pane's output on the same screen, so
// the output had nowhere to grow and the panes had nowhere to report. Modelled
// on ForwardsModal (a table you select in) plus a viewer, rather than inventing
// a third idiom.
type rawModalMode int

const (
	rawModeList rawModalMode = iota // table of panes
	rawModeNew                      // target prompt
	rawModeView                     // one pane: output + entry line
)

// RawConnectModal lists a task's raw connections and opens one at a time.
type RawConnectModal struct {
	open   bool
	mode   rawModalMode
	taskID string
	input  textinput.Model
	table  table.Model
	// baseCols is the natural sizing; see RunnersModel.baseCols.
	baseCols []table.Column
	vp       viewport.Model
	// newErr is the target prompt's own error line. It used to go to the
	// cmdresult panel, which the modal is covering — so the operator got no
	// feedback at all until they closed the modal and found it behind.
	newErr string
	panes  []rawPane
	active int
	// width/height are the terminal's, set by App like every other modal's
	// SetSize. Without them the view sized itself to its longest line and
	// lipgloss.Place centred that narrow blob, which is why this modal looked
	// nothing like the forwards list or the grid.
	width  int
	height int
	// form is a MODE of the active pane, not a separate modal: the request it
	// builds is sent on that pane's connection. ctrl+t toggles it, because the
	// input below consumes every printable key.
	form     httpForm
	formMode bool
	// hexMode reads the entry line as hex instead of text. The WebUI pane has
	// had this toggle since it shipped; without it the TUI could not send a
	// byte it has no key for.
	hexMode bool
	// newline is the terminator appended in text mode. CRLF is what the
	// line-oriented protocols this pane is for expect, but not every server
	// does: an LF-only listener treats the CR as part of the payload, and a
	// length-prefixed one wants neither.
	newline rawNewline
}

// rawNewline is the terminator appended to a text-mode entry. The zero value
// is CRLF, so a modal built without touching this behaves as it always has.
type rawNewline int

const (
	rawNewlineCRLF rawNewline = iota
	rawNewlineLF
	rawNewlineNone
)

func (n rawNewline) label() string {
	switch n {
	case rawNewlineLF:
		return "LF"
	case rawNewlineNone:
		return "none"
	}
	return "CRLF"
}

func (n rawNewline) suffix() string {
	switch n {
	case rawNewlineLF:
		return "\n"
	case rawNewlineNone:
		return ""
	}
	return "\r\n"
}

func NewRawConnectModal() RawConnectModal {
	in := textinput.New()
	in.Placeholder = "host:port"
	in.CharLimit = 128
	cols := []table.Column{
		{Title: "target", Width: 24},
		{Title: "state", Width: 8},
		{Title: "in", Width: 9},
		{Title: "out", Width: 9},
		{Title: "note", Width: 30},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	return RawConnectModal{input: in, table: t, baseCols: cols, vp: viewport.New(80, 10)}
}

// Mode reports which screen is showing.
func (m *RawConnectModal) Mode() rawModalMode { return m.mode }

// OpenSelected enters the viewer for the highlighted row.
func (m *RawConnectModal) OpenSelected() {
	if len(m.panes) == 0 {
		return
	}
	m.active = m.table.Cursor() + 1
	m.mode = rawModeView
	m.syncViewport()
	m.input.Focus()
}

// BeginNew shows the target prompt.
func (m *RawConnectModal) BeginNew() {
	m.mode = rawModeNew
	m.newErr = ""
	m.input.SetValue("")
	m.input.Focus()
}

// TargetOrError parses the entered target, reporting inside the modal. The
// shared parser's message names the `-W` flag it was written for, which means
// nothing here, so the wording is replaced at this boundary — the parsing
// itself stays shared so the CLI and the TUI cannot disagree about what a
// target looks like.
func (m *RawConnectModal) TargetOrError() (string, int, bool) {
	if strings.TrimSpace(m.input.Value()) == "" {
		m.newErr = "enter a target as host:port"
		return "", 0, false
	}
	host, port, err := m.Target()
	if err != nil {
		m.newErr = fmt.Sprintf("%q is not host:port", m.input.Value())
		return "", 0, false
	}
	m.newErr = ""
	return host, port, true
}

// BackToList leaves the viewer or the target prompt. The pane stays connected.
func (m *RawConnectModal) BackToList() {
	m.mode = rawModeList
	m.formMode = false
	m.input.SetValue("")
	m.input.Blur()
	m.syncRows()
}

// syncRows rebuilds the table from the panes, keeping the cursor in range.
func (m *RawConnectModal) syncRows() {
	rows := make([]table.Row, 0, len(m.panes))
	for i := range m.panes {
		p := &m.panes[i]
		state := "closed"
		switch {
		case p.live:
			state = "live"
		case p.connecting:
			state = "opening"
		}
		rows = append(rows, table.Row{
			p.target(), state,
			formatByteCount(p.inBytes), formatByteCount(p.outBytes), p.note,
		})
	}
	m.table.SetRows(rows)
	if c := m.table.Cursor(); c >= len(rows) && len(rows) > 0 {
		m.table.SetCursor(len(rows) - 1)
	}
}

// syncViewport refreshes the viewer's content, sticking to the bottom only
// while the reader is already there — the same rule the log pane and the WebUI
// output use, so following live output does not fight scrolling back.
func (m *RawConnectModal) syncViewport() {
	p := m.ActivePane()
	if p == nil {
		m.vp.SetContent("")
		return
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(sanitizeOutput(p.output()))
	if atBottom {
		m.vp.GotoBottom()
	}
}

// ScrollViewport forwards a key to the output viewport (↑/↓, PgUp/PgDn).
func (m *RawConnectModal) ScrollViewport(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd
}

// UpdateList forwards a key to the pane table (row movement).
func (m *RawConnectModal) UpdateList(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return cmd
}

// formatByteCount keeps the list's columns narrow.
func formatByteCount(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// SetSize records the terminal size. Mirrors ForwardsModal.SetSize — App calls
// it on WindowSizeMsg and when the modal opens.
func (m *RawConnectModal) SetSize(w, h int) {
	m.width, m.height = w, h
	if w > 8 {
		m.table.SetWidth(w - 4)
		m.table.SetColumns(fitColumns(m.baseCols, w-4, flexColumn(m.baseCols, "note")))
		m.vp.Width = w - 4
	}
	if h > 8 {
		m.table.SetHeight(h - 4)
		// header, target line, entry line, footer, and the box's two rows.
		m.vp.Height = h - 6
	}
}

func (m *RawConnectModal) IsOpen() bool   { return m.open }
func (m *RawConnectModal) TaskID() string { return m.taskID }
func (m *RawConnectModal) PaneCount() int { return len(m.panes) }

// Show reveals the modal for taskID. Unlike the Open it replaces, it keeps
// existing panes: those are connections, not view state.
func (m *RawConnectModal) Show(taskID string) {
	m.open = true
	m.taskID = taskID
	m.mode = rawModeList
	m.input.SetValue("")
	m.input.Blur()
	m.syncRows()
}

// Hide closes the view only. Every pane stays connected and stays registered —
// `x` is what closes one.
func (m *RawConnectModal) Hide() {
	m.open = false
	m.input.Blur()
}

// SendActive's byte accounting lives here so every path through it — the entry
// line, the HTTP form — reports what it wrote.
func (m *RawConnectModal) ActivePane() *rawPane {
	if m.active <= 0 || m.active > len(m.panes) {
		return nil
	}
	return &m.panes[m.active-1]
}

// PaneForGen resolves the pane a generation-tagged message belongs to, or nil
// when its pane is gone.
func (m *RawConnectModal) PaneForGen(gen uint64) *rawPane {
	for i := range m.panes {
		if m.panes[i].gen == gen {
			return &m.panes[i]
		}
	}
	return nil
}

// AddPane appends a connecting pane and selects it.
func (m *RawConnectModal) AddPane(taskID, host string, port int, gen uint64) {
	m.panes = append(m.panes, rawPane{
		taskID: taskID, host: host, port: port, gen: gen,
		connecting: true, note: "connecting…",
	})
	m.active = len(m.panes)
	m.mode = rawModeView
	m.input.SetValue("")
	m.input.Focus()
	m.syncRows()
	m.table.SetCursor(m.active - 1)
	m.syncViewport()
}

// CloseActivePane drops the selected pane and closes its connection.
// CloseSelectedPane closes whichever pane the list has under its cursor.
func (m *RawConnectModal) CloseSelectedPane() {
	if m.mode == rawModeList {
		if len(m.panes) == 0 {
			return
		}
		m.active = m.table.Cursor() + 1
	}
	m.CloseActivePane()
	if m.mode == rawModeView {
		m.BackToList()
	}
}

func (m *RawConnectModal) CloseActivePane() {
	p := m.ActivePane()
	if p == nil {
		return
	}
	p.closeConn()
	i := m.active - 1
	m.panes = append(m.panes[:i], m.panes[i+1:]...)
	if m.active > len(m.panes) {
		m.active = len(m.panes)
	}
	m.syncRows()
}

// CloseAllPanes is what quitting must call: a RawConn whose process is gone
// leaves a registration nobody can reach.
func (m *RawConnectModal) CloseAllPanes() {
	for i := range m.panes {
		m.panes[i].closeConn()
	}
	m.panes = nil
	m.active = 0
}

// SendActive writes bytes verbatim — nothing is appended. SendLine is the
// line-oriented convenience; a built HTTP request must reach the wire exactly
// as built or its Content-Length stops matching what arrives.
func (m *RawConnectModal) SendActive(b []byte) error {
	p := m.ActivePane()
	if p == nil || p.conn == nil {
		return fmt.Errorf("raw connect: not connected")
	}
	if err := p.conn.Send(b); err != nil {
		return err
	}
	p.outBytes += len(b)
	return nil
}

// hexToBytes accepts "48 65 6c" / "48656c" and rejects anything else, so a
// typo sends nothing rather than sending garbage. Same rules as the WebUI's
// hexToBytes, deliberately: an operator who learned one pane should not have
// to relearn the other.
func hexToBytes(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	if clean == "" {
		return nil, fmt.Errorf("hex: nothing to send")
	}
	if len(clean)%2 != 0 {
		return nil, fmt.Errorf("hex: %d digits, want an even count", len(clean))
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("hex: %w", err)
	}
	return b, nil
}

// entryBytes is exactly what an Enter in byte-entry mode puts on the wire. It
// is a pure function so the terminator rule is testable without a connection:
// text gets the selected terminator, hex gets nothing added regardless — hex
// exists to send an exact byte sequence, and appending to it would defeat the
// only reason to reach for it.
func entryBytes(entry string, hexMode bool, nl rawNewline) ([]byte, error) {
	if hexMode {
		return hexToBytes(entry)
	}
	return []byte(entry + nl.suffix()), nil
}

// ToggleHex switches the entry line between text and hex.
func (m *RawConnectModal) ToggleHex() { m.hexMode = !m.hexMode }

// CycleNewline walks CRLF → LF → none, matching the WebUI's selector.
func (m *RawConnectModal) CycleNewline() {
	m.newline = (m.newline + 1) % 3
}

func (m *RawConnectModal) NewlineLabel() string { return m.newline.label() }

func (m *RawConnectModal) InHex() bool { return m.hexMode }

// SendEntry sends the entry line under the current mode and clears it. A hex
// parse error is reported on the pane and nothing is written.
func (m *RawConnectModal) SendEntry() error {
	b, err := entryBytes(m.input.Value(), m.hexMode, m.newline)
	if err != nil {
		return err
	}
	if err := m.SendActive(b); err != nil {
		return err
	}
	m.input.SetValue("")
	return nil
}

// SetConn adopts the connection opened for gen. A pane that vanished while its
// open was in flight resolves to nil here, and App closes the connection rather
// than leaving it registered with no UI reference.
func (m *RawConnectModal) SetConn(gen uint64, rc *cli.RawConn, cancel context.CancelFunc, note string) {
	p := m.PaneForGen(gen)
	if p == nil {
		return
	}
	p.closeConn()
	p.conn, p.cancel = rc, cancel
	p.connecting, p.live, p.note = false, true, note
}

// MarkClosed records why a pane's connection ended and releases it. The pump has
// already closed the connection by the time this runs; closeConn is idempotent
// and is what drops the reference and stops the sink goroutine.
func (m *RawConnectModal) MarkClosed(gen uint64, note string) {
	p := m.PaneForGen(gen)
	if p == nil {
		return
	}
	p.live, p.connecting, p.note = false, false, note
	p.closeConn()
}

// SetActiveNote reports something about the selected pane (a build error, a
// send failure) without touching its connection.
func (m *RawConnectModal) SetActiveNote(note string) {
	if p := m.ActivePane(); p != nil {
		p.note = note
	}
}

// ToggleForm switches the active pane between raw byte entry and the HTTP
// form. There is nothing to send from the [+ new] slot, so it stays inert
// there.
func (m *RawConnectModal) ToggleForm() {
	if m.ActivePane() == nil {
		return
	}
	if !m.formMode {
		m.form = newHTTPForm()
	}
	m.formMode = !m.formMode
}

func (m *RawConnectModal) InForm() bool { return m.formMode && m.ActivePane() != nil }

// FormNextField / FormCycleMethod / UpdateForm keep app.go out of the form's
// fields: the key handler names intents, the form owns its own state.
func (m *RawConnectModal) FormNextField()        { m.form.NextField() }
func (m *RawConnectModal) FormCycleMethod(d int) { m.form.CycleMethod(d) }
func (m *RawConnectModal) UpdateForm(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.form, cmd = m.form.Update(msg)
	return cmd
}

// SetFormForTest fills the form without going through key events.
func (m *RawConnectModal) SetFormForTest(method, path, headers, body string) {
	m.form.setForTest(method, path, headers, body)
}

// SendForm builds the request for the active pane's own target and writes it in
// ONE Send. Nothing may append to those bytes — SendLine's CRLF would stop
// Content-Length matching what the far end reads.
func (m *RawConnectModal) SendForm() error {
	p := m.ActivePane()
	if p == nil {
		return fmt.Errorf("raw connect: not connected")
	}
	req, err := cli.BuildHTTPRequest(m.form.Spec(), p.host, p.port)
	if err != nil {
		return err
	}
	return m.SendActive(req)
}

func (m *RawConnectModal) SetSpec(s string) { m.input.SetValue(s) }
func (m *RawConnectModal) Spec() string     { return m.input.Value() }

// Target parses the entered spec. Reuses the CLI parser so -W and the TUI
// cannot disagree about what a target looks like.
func (m *RawConnectModal) Target() (string, int, error) {
	return cli.ParseStdioForwardSpec(m.input.Value())
}

func (m *RawConnectModal) Update(msg tea.Msg) (RawConnectModal, tea.Cmd) {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return *m, cmd
}

// syncEntry makes the entry line describe what Enter will do. One textinput
// serves two jobs — a target on [+ new], bytes to send on a pane — and it used
// to keep the "host:port" placeholder either way, so a connected pane invited
// the operator to type a target into what is actually the send line. Derived
// state, recomputed in View so it cannot drift from the mode it describes.
func (m *RawConnectModal) syncEntry() {
	p := m.ActivePane()
	switch {
	case p == nil:
		m.input.Prompt = "target > "
		m.input.Placeholder = "host:port"
	case !p.live:
		m.input.Prompt = "closed  > "
		m.input.Placeholder = "ctrl+x removes this pane"
	case m.hexMode:
		m.input.Prompt = "send hex > "
		m.input.Placeholder = "48 65 6c 6c 6f"
	default:
		m.input.Prompt = "send " + m.newline.label() + " > "
		m.input.Placeholder = "bytes to send"
	}
}

// Refresh re-derives the list and the viewer from the panes. Called whenever a
// message mutates one, so the counters and the output stay current without the
// App reaching into either widget.
func (m *RawConnectModal) Refresh() {
	m.syncRows()
	if m.mode == rawModeView {
		m.syncViewport()
	}
}

func (m *RawConnectModal) View() string {
	box := PanelStyleFocused.Padding(0, 1)
	if m.width > 4 {
		box = box.Width(m.width - 4)
	}

	var lines []string
	switch m.mode {
	case rawModeNew:
		m.input.Prompt = "target > "
		m.input.Placeholder = "host:port"
		// Padded to the same height as the other two screens: a modal that
		// shrinks when you press `n` reads as a different window opening.
		lines = []string{
			HeaderStyle.Render(fmt.Sprintf("raw connect — task %s", pfShortID(m.taskID))),
			MutedStyle.Render("connect to a host:port the runner can reach"),
			m.input.View(),
		}
		if m.newErr != "" {
			lines = append(lines, WarnStyle.Render(m.newErr))
		}
		for i := len(lines); i < m.height-3; i++ {
			lines = append(lines, "")
		}
		lines = append(lines, FooterStyle.Render("Enter: connect · esc: back"))

	case rawModeView:
		p := m.ActivePane()
		if p == nil {
			m.mode = rawModeList
			return m.View()
		}
		state := p.note
		if state == "" {
			if p.live {
				state = "connected"
			} else {
				state = "closed"
			}
		}
		m.syncEntry()
		entry := m.input.View()
		foot := "↑↓ scroll · Enter send · ctrl+r hex · ctrl+o " + m.newline.label() +
			" · ctrl+t HTTP · ctrl+x close · esc: list"
		if m.InForm() {
			entry = m.form.View()
			foot = "ctrl+t back · ctrl+x close · esc: list"
		}
		lines = []string{
			HeaderStyle.Render("raw connect — " + p.target()),
			MutedStyle.Render(fmt.Sprintf("%s · in %s / out %s",
				state, formatByteCount(p.inBytes), formatByteCount(p.outBytes))),
			m.vp.View(),
			entry,
			FooterStyle.Render(foot),
		}

	default: // rawModeList
		body := m.table.View()
		if len(m.panes) == 0 {
			body = MutedStyle.Render("no connections yet")
		}
		lines = []string{
			HeaderStyle.Render(fmt.Sprintf("raw connect — task %s  (%d pane%s)",
				pfShortID(m.taskID), len(m.panes), plural(len(m.panes)))),
			body,
			FooterStyle.Render("Enter: open · n: new · ctrl+x: close · esc: hide"),
		}
	}
	return box.Render(strings.Join(lines, "\n"))
}

// plural is the "s" in "1 pane" / "2 panes".
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// RawForwardOpenedMsg reports a successful connect. It carries the connection
// and its cancel func so the modal can own both.
type RawForwardOpenedMsg struct {
	Gen       uint64
	TaskID    string
	ForwardID uint64
	Conn      *cli.RawConn
	Cancel    context.CancelFunc
}

// RawForwardDataMsg carries bytes received from the far end.
type RawForwardDataMsg struct {
	Gen  uint64
	Data []byte
}

// RawForwardClosedMsg reports the connection ending, for any reason. Exactly one
// is sent per connection.
type RawForwardClosedMsg struct {
	Gen    uint64
	Reason string
}

// rawNote lets the control-stream callback record WHY a forward ended while the
// pump remains the single sender of RawForwardClosedMsg. Two senders would show
// the operator two closes with different reasons for one remote kill — the same
// defect the wasm pane had to fix.
type rawNote struct {
	mu sync.Mutex
	s  string
}

func (n *rawNote) set(s string) { n.mu.Lock(); n.s = s; n.mu.Unlock() }
func (n *rawNote) get() string  { n.mu.Lock(); defer n.mu.Unlock(); return n.s }

// rawSink funnels a raw connection's messages to the UI through a background
// goroutine that owns program.Send. Modelled on forwardStatusLogf
// (tui/portforward.go:238), which exists because a goroutine blocking in
// program.Send hangs for as long as the event loop is not draining. One deliberate
// difference: this sink must NOT drop on a full buffer the way cosmetic status
// lines may — dropping received bytes would silently corrupt what the operator
// reads — so a full buffer blocks the pump and ctx cancellation is what
// releases it.
func rawSink(ctx context.Context, program *tea.Program) func(tea.Msg) bool {
	ch := make(chan tea.Msg, 256)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-ch:
				program.Send(m)
			}
		}
	}()
	return func(m tea.Msg) bool {
		select {
		case ch <- m:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

// DoStartRawForward opens the connection on the app's existing long-lived client
// — never a fresh Dial, matching every other Do* in this package — and pumps
// received bytes back as messages tagged with gen, so a reply that arrives after
// the operator moved on is ignored rather than applied to a different session.
func DoStartRawForward(c *cli.Client, taskID, host string, port int, gen uint64, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		send := rawSink(ctx, program)
		note := &rawNote{}

		openCtx, openCancel := context.WithTimeout(ctx, rawConnectTimeout)
		rc, err := cli.OpenRawForward(openCtx, c, taskID, host, port, protocol.ClientEndpointKind_InProcessPane, note.set)
		openCancel()
		if err != nil {
			cancel()
			return RawForwardClosedMsg{Gen: gen, Reason: "raw connect: " + err.Error()}
		}

		go func() {
			// Closing is what deregisters the forward server-side, so the pump
			// closes it on the way out — the same rule spliceStdio and the wasm
			// pump follow. Without it an ordinary remote hangup leaves a row in
			// `forward ls` until the operator presses esc.
			defer rc.Close()
			defer cancel()
			for {
				data, eof, rerr := rc.Recv(ctx)
				if len(data) > 0 {
					if !send(RawForwardDataMsg{Gen: gen, Data: append([]byte(nil), data...)}) {
						return
					}
				}
				if eof || rerr != nil {
					reason := note.get()
					if reason == "" {
						reason = "connection closed"
					}
					send(RawForwardClosedMsg{Gen: gen, Reason: reason})
					return
				}
			}
		}()
		return RawForwardOpenedMsg{Gen: gen, TaskID: taskID, ForwardID: rc.ForwardID(), Conn: rc, Cancel: cancel}
	}
}
