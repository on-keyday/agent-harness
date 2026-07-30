package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

// rawTUIRingBytes caps the modal's output buffer. Same reasoning as the WebUI
// pane's ring: this is a debugging window, not a transcript.
const rawTUIRingBytes = 64 * 1024

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
	note   string
	// conn is the live connection this modal owns. Kept on the modal rather
	// than in a package-level global so closing the modal cannot leave a
	// connection nobody references, and so two app instances in one test
	// binary do not share it.
	conn *cli.RawConn
}

func NewRawConnectModal() RawConnectModal {
	in := textinput.New()
	in.Placeholder = "host:port"
	in.CharLimit = 128
	return RawConnectModal{input: in}
}

func (m *RawConnectModal) IsOpen() bool   { return m.open }
func (m *RawConnectModal) TaskID() string { return m.taskID }
func (m *RawConnectModal) IsLive() bool   { return m.live }
func (m *RawConnectModal) Output() []byte { return m.out }

func (m *RawConnectModal) Open(taskID string) {
	m.CloseConn() // a re-open must not orphan a previous connection
	m.open = true
	m.taskID = taskID
	m.live = false
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
	m.input.Blur()
}

// SetConn adopts the connection opened by DoStartRawForward.
func (m *RawConnectModal) SetConn(rc *cli.RawConn) { m.conn = rc }

// CloseConn closes the live connection, if any. Idempotent.
func (m *RawConnectModal) CloseConn() {
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
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

func (m *RawConnectModal) SetSpec(s string) { m.input.SetValue(s) }
func (m *RawConnectModal) Spec() string     { return m.input.Value() }

// Target parses the entered spec. Reuses the CLI parser so -W and the TUI cannot
// disagree about what a target looks like.
func (m *RawConnectModal) Target() (string, int, error) {
	return cli.ParseStdioForwardSpec(m.input.Value())
}

func (m *RawConnectModal) MarkLive(note string)   { m.live = true; m.note = note }
func (m *RawConnectModal) MarkClosed(note string) { m.live = false; m.note = note }

// AppendOutput adds received bytes, trimming the front so the buffer stays
// bounded.
func (m *RawConnectModal) AppendOutput(b []byte) {
	m.out = append(m.out, b...)
	if len(m.out) > rawTUIRingBytes {
		m.out = append([]byte(nil), m.out[len(m.out)-rawTUIRingBytes:]...)
	}
}

func (m *RawConnectModal) Update(msg tea.Msg) (RawConnectModal, tea.Cmd) {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return *m, cmd
}

func (m *RawConnectModal) View() string {
	head := fmt.Sprintf("raw connect — task %s", pfShortID(m.taskID))
	state := "enter host:port, Enter to connect"
	if m.live {
		state = "connected — type bytes, Enter sends (esc closes)"
	}
	if m.note != "" {
		state = m.note
	}
	body := string(m.out)
	return head + "\n" + state + "\n" + m.input.View() + "\n\n" + body
}

// RawForwardOpenedMsg reports a successful connect. It carries the connection
// itself so the modal can own it; the controller stores it via SetConn.
type RawForwardOpenedMsg struct {
	TaskID    string
	ForwardID uint64
	Conn      *cli.RawConn
}

// RawForwardDataMsg carries bytes received from the far end.
type RawForwardDataMsg struct{ Data []byte }

// RawForwardClosedMsg reports the connection ending, for any reason.
type RawForwardClosedMsg struct{ Reason string }

// DoStartRawForward opens the connection on the app's existing long-lived client
// — never a fresh Dial, matching every other Do* in this package — and pumps
// received bytes back as messages.
func DoStartRawForward(c *cli.Client, taskID, host string, port int, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		rc, err := cli.OpenRawForward(ctx, c, taskID, host, port, func(line string) {
			if program != nil {
				program.Send(RawForwardClosedMsg{Reason: line})
			}
		})
		if err != nil {
			return RawForwardClosedMsg{Reason: "raw connect: " + err.Error()}
		}
		go func() {
			for {
				data, eof, rerr := rc.Recv(ctx)
				if len(data) > 0 && program != nil {
					program.Send(RawForwardDataMsg{Data: append([]byte(nil), data...)})
				}
				if eof || rerr != nil {
					if program != nil {
						program.Send(RawForwardClosedMsg{Reason: "connection closed"})
					}
					return
				}
			}
		}()
		return RawForwardOpenedMsg{TaskID: taskID, ForwardID: rc.ForwardID(), Conn: rc}
	}
}
