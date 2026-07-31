package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

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
	// form is a MODE of the active pane, not a separate modal: the request it
	// builds is sent on that pane's connection. ctrl+t toggles it, because the
	// input below consumes every printable key.
	form     httpForm
	formMode bool
}

func NewRawConnectModal() RawConnectModal {
	in := textinput.New()
	in.Placeholder = "host:port"
	in.CharLimit = 128
	return RawConnectModal{input: in}
}

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

// SendLine writes the given text plus CRLF. The TUI has no newline selector —
// CRLF is what the line-oriented protocols this is for (HTTP, Redis, SMTP)
// expect; the WebUI pane is where the selector lives.
func (m *RawConnectModal) SendLine(s string) error {
	return m.SendActive([]byte(s + "\r\n"))
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

func (m *RawConnectModal) View() string {
	head := fmt.Sprintf("raw connect — task %s", pfShortID(m.taskID))

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
	foot := "←/→ tab · Enter send · ctrl+t HTTP · x close pane · esc hide"
	if m.InForm() {
		entry = m.form.View()
		foot = "x close pane · esc hide"
	}
	return head + "\n" + strings.Join(tabs, " ") + "\n" + state + "\n" +
		entry + "\n\n" + body + "\n" + FooterStyle.Render(foot)
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
