package tui

import (
	"context"
	"fmt"
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

// RawConnectModal prompts for a host:port and then shows the bytes coming back
// from a forward whose client endpoint is this TUI process. Mirrors
// PortForwardModal's shape (open/close/input/View) so the app's modal handling
// needs no new idiom.
type RawConnectModal struct {
	open   bool
	taskID string
	input  textinput.Model
	out    []byte
	live   bool
	// connecting is true from the moment Enter dispatches a connect attempt
	// until that attempt resolves (SetConn on success, MarkClosed on
	// failure). rawGen scopes a SESSION, not an ATTEMPT: two connect attempts
	// dispatched back-to-back under one open session share one Gen, so the
	// Gen guard alone cannot tell them apart — the loser's eventual
	// RawForwardClosedMsg would still pass the guard and tear down whatever
	// the winner just set up. connecting makes the Enter handler refuse a
	// second dispatch while one is outstanding, so at most one attempt (and
	// therefore at most one Gen-indistinguishable reply) is ever in flight.
	connecting bool
	note       string
	// conn is the live connection this modal owns, and cancel stops its pump.
	// Kept on the modal rather than in a package-level global so closing the
	// modal cannot leave a connection nobody references.
	conn   *cli.RawConn
	cancel context.CancelFunc
}

func NewRawConnectModal() RawConnectModal {
	in := textinput.New()
	in.Placeholder = "host:port"
	in.CharLimit = 128
	return RawConnectModal{input: in}
}

func (m *RawConnectModal) IsOpen() bool       { return m.open }
func (m *RawConnectModal) TaskID() string     { return m.taskID }
func (m *RawConnectModal) IsLive() bool       { return m.live }
func (m *RawConnectModal) IsConnecting() bool { return m.connecting }
func (m *RawConnectModal) Output() []byte     { return m.out }

func (m *RawConnectModal) Open(taskID string) {
	m.CloseConn() // a re-open must not orphan a previous connection
	m.open = true
	m.taskID = taskID
	m.live = false
	m.connecting = false
	m.note = ""
	m.out = nil
	m.input.SetValue("")
	m.input.Focus()
}

// Close hides the modal AND drops its connection: the registration must not
// outlive the only UI that can read it.
func (m *RawConnectModal) Close() {
	m.CloseConn()
	m.open = false
	m.live = false
	m.connecting = false
	m.input.Blur()
}

// SetConn adopts the connection opened by DoStartRawForward, closing any
// previous one so a double-connect cannot orphan a live registration.
func (m *RawConnectModal) SetConn(rc *cli.RawConn, cancel context.CancelFunc) {
	m.CloseConn()
	m.conn = rc
	m.cancel = cancel
	m.connecting = false
}

// MarkConnecting records that a connect attempt was just dispatched. Cleared
// by SetConn (attempt succeeded) or MarkClosed (attempt failed) — see the
// connecting field's doc comment for why this must gate the Enter handler
// rather than rely on rawGen alone.
func (m *RawConnectModal) MarkConnecting() { m.connecting = true }

// CloseConn closes the live connection and stops its pump. Idempotent.
func (m *RawConnectModal) CloseConn() {
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// SendLine writes the given text plus CRLF. The TUI has no newline selector —
// CRLF is what the line-oriented protocols this is for (HTTP, Redis, SMTP)
// expect; the WebUI pane is where the selector lives.
func (m *RawConnectModal) SendLine(s string) error {
	if m.conn == nil {
		return fmt.Errorf("raw connect: not connected")
	}
	return m.conn.Send([]byte(s + "\r\n"))
}

func (m *RawConnectModal) MarkLive(note string) { m.live = true; m.note = note }

// MarkClosed records why the connection ended and releases it. The pump has
// already closed the connection by the time this runs; CloseConn is idempotent
// and is what drops the reference and stops the sink goroutine.
func (m *RawConnectModal) MarkClosed(note string) {
	m.live = false
	m.connecting = false
	m.note = note
	m.CloseConn()
}

// AppendOutput adds received bytes, trimming the front so the buffer stays
// bounded and the NEWEST bytes are the ones kept.
func (m *RawConnectModal) AppendOutput(b []byte) {
	m.out = append(m.out, b...)
	if len(m.out) > rawTUIRingBytes {
		m.out = append([]byte(nil), m.out[len(m.out)-rawTUIRingBytes:]...)
	}
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
	state := "enter host:port, Enter to connect"
	if m.connecting {
		state = "connecting…"
	}
	if m.live {
		state = "connected — type bytes, Enter sends (esc closes)"
	}
	if m.note != "" {
		state = m.note
	}
	return head + "\n" + state + "\n" + m.input.View() + "\n\n" + string(m.out)
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
