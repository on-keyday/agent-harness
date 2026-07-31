package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
)

// rawTUIRingBytes caps the modal's output buffer. Same reasoning as the WebUI
// pane's ring: this is a debugging window, not a transcript.
const rawTUIRingBytes = 64 * 1024

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
	gen        uint64
	conn       *cli.RawConn
	cancel     context.CancelFunc
	out        []byte
	live       bool
	connecting bool
	note       string
}

func (p *rawPane) target() string { return fmt.Sprintf("%s:%d", p.host, p.port) }

// AppendOutput adds received bytes, trimming the front so the buffer stays
// bounded and the NEWEST bytes are the ones kept.
func (p *rawPane) AppendOutput(b []byte) {
	p.out = append(p.out, b...)
	if len(p.out) > rawTUIRingBytes {
		p.out = append([]byte(nil), p.out[len(p.out)-rawTUIRingBytes:]...)
	}
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

// RawConnectModal shows a [+ new] target prompt plus one tab per pane, mirroring
// the WebUI's raw panel so the two surfaces behave the same way. active == 0 is
// the [+ new] slot; pane i is active-1.
type RawConnectModal struct {
	open   bool
	taskID string
	input  textinput.Model
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
	return RawConnectModal{input: in}
}

// SetSize records the terminal size. Mirrors ForwardsModal.SetSize — App calls
// it on WindowSizeMsg and when the modal opens.
func (m *RawConnectModal) SetSize(w, h int) { m.width, m.height = w, h }

func (m *RawConnectModal) IsOpen() bool    { return m.open }
func (m *RawConnectModal) TaskID() string  { return m.taskID }
func (m *RawConnectModal) PaneCount() int  { return len(m.panes) }
func (m *RawConnectModal) OnNewSlot() bool { return m.active == 0 }

// Show reveals the modal for taskID. Unlike the Open it replaces, it keeps
// existing panes: those are connections, not view state.
func (m *RawConnectModal) Show(taskID string) {
	m.open = true
	m.taskID = taskID
	m.input.SetValue("")
	m.input.Focus()
}

// Hide closes the view only. Every pane stays connected and stays registered —
// `x` is what closes one.
func (m *RawConnectModal) Hide() {
	m.open = false
	m.input.Blur()
}

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

// MovePane walks the tab strip. It clamps rather than wraps: wrapping past
// [+ new] would make "go left until you reach new" a guessing game.
func (m *RawConnectModal) MovePane(delta int) {
	m.active += delta
	if m.active < 0 {
		m.active = 0
	}
	if m.active > len(m.panes) {
		m.active = len(m.panes)
	}
	m.input.SetValue("")
}

// AddPane appends a connecting pane and selects it.
func (m *RawConnectModal) AddPane(taskID, host string, port int, gen uint64) {
	m.panes = append(m.panes, rawPane{
		taskID: taskID, host: host, port: port, gen: gen,
		connecting: true, note: "connecting…",
	})
	m.active = len(m.panes)
	m.input.SetValue("")
}

// CloseActivePane drops the selected pane and closes its connection.
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
	return p.conn.Send(b)
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

func (m *RawConnectModal) View() string {
	m.syncEntry()
	box := PanelStyleFocused.Padding(0, 1)
	header := HeaderStyle.Render(fmt.Sprintf("raw connect — task %s  (%d pane%s)",
		pfShortID(m.taskID), len(m.panes), plural(len(m.panes))))

	tabs := make([]string, 0, len(m.panes)+1)
	label := "[+ new]"
	if m.active == 0 {
		label = FocusedStyle.Render(label)
	}
	tabs = append(tabs, label)
	for i := range m.panes {
		t := "[" + m.panes[i].target() + "]"
		switch {
		case m.active == i+1:
			t = FocusedStyle.Render(t)
		case !m.panes[i].live:
			t = MutedStyle.Render(t)
		}
		tabs = append(tabs, t)
	}

	state := "enter host:port, Enter to connect"
	body := ""
	if p := m.ActivePane(); p != nil {
		state = p.note
		if state == "" {
			state = "connected"
		}
		body = string(p.out)
	}

	entry := m.input.View()
	foot := "←/→ tab · Enter send · ctrl+r hex · ctrl+o " + m.newline.label() +
		" · ctrl+t HTTP · ctrl+x close · esc hide"
	if m.InForm() {
		entry = m.form.View()
		foot = "ctrl+x close · esc hide"
	}

	head := []string{header, strings.Join(tabs, " "), MutedStyle.Render(state), entry}
	tail := FooterStyle.Render(foot)

	// Fill the box the way the forwards list and the grid do, so the output
	// area is where the eye lands instead of a floating three-line note.
	out := strings.Split(body, "\n")
	if avail := m.height - 4 - lipgloss.Height(strings.Join(head, "\n")) - 1; avail > 0 {
		if len(out) > avail {
			out = out[len(out)-avail:] // newest, matching the ring's own rule
		}
		for len(out) < avail {
			out = append(out, "")
		}
	}

	view := strings.Join(head, "\n") + "\n" + strings.Join(out, "\n") + "\n" + tail
	if m.width > 4 {
		box = box.Width(m.width - 4)
	}
	return box.Render(view)
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
// program.Send while tea.Exec holds the event loop hangs. One deliberate
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
		rc, err := cli.OpenRawForward(openCtx, c, taskID, host, port, note.set)
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
