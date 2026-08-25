//go:build !js

package sshgw

import (
	"context"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli/sshgw/sshwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"golang.org/x/crypto/ssh"
)

// ptyDims are the four numbers pty-req and window-change both carry, narrowed
// to what SetTerminalWindowSize takes.
//
// SSH puts COLUMNS first and SetTerminalWindowSize takes ROWS first, so the
// wire layout is described in sshwire.bgn and decoded into named fields rather
// than indexed out of a byte slice: a transposed size renders a screen that
// looks plausible and is wrong.
type ptyDims struct {
	Cols, Rows, WidthPx, HeightPx uint16
}

// dims narrows the wire's uint32s to the uint16 pair the control frame carries.
// One place, so the truncation is not repeated per call site.
func dims(cols, rows, widthPx, heightPx uint32) ptyDims {
	return ptyDims{
		Cols:     uint16(cols),
		Rows:     uint16(rows),
		WidthPx:  uint16(widthPx),
		HeightPx: uint16(heightPx),
	}
}

// parseWindowChange decodes a window-change payload (RFC 4254 6.7).
func parseWindowChange(payload []byte) (ptyDims, error) {
	var wc sshwire.WindowChange
	if err := wc.DecodeExact(payload); err != nil {
		return ptyDims{}, fmt.Errorf("window-change payload: %w", err)
	}
	return dims(wc.Columns, wc.Rows, wc.WidthPx, wc.HeightPx), nil
}

// parsePtyReq decodes a pty-req payload (RFC 4254 6.2).
//
// TERM is decoded and discarded: the runner-side PTY's TERM is fixed when the
// session is created, and changing it mid-session would change what the
// already-running agent renders.
func parsePtyReq(payload []byte) (ptyDims, error) {
	var pr sshwire.PtyReq
	if err := pr.DecodeExact(payload); err != nil {
		return ptyDims{}, fmt.Errorf("pty-req payload: %w", err)
	}
	return dims(pr.Columns, pr.Rows, pr.WidthPx, pr.HeightPx), nil
}

// serveSession runs one ssh session channel against one attach stream.
func (g *Gateway) serveSession(ctx context.Context, user string, newCh ssh.NewChannel) {
	taskID, mode, err := ParseUserName(user)
	if err != nil {
		_ = newCh.Reject(ssh.Prohibited, err.Error())
		return
	}
	if !g.claim(taskID, mode) {
		_ = newCh.Reject(ssh.Prohibited, fmt.Sprintf(
			"ssh-gateway: task %s already has a control session through this gateway; connect without .control to co-write, or with .view to watch",
			taskID))
		return
	}
	defer g.release(taskID, mode)

	ch, requests, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	// Wait for pty-req/shell before attaching: the initial size rides pty-req,
	// and attaching first would paint the replay at whatever size the session
	// happened to have.
	var dims ptyDims
	var haveDims bool
	started := false
	for req := range requests {
		switch req.Type {
		case "pty-req":
			d, perr := parsePtyReq(req.Payload)
			if perr != nil {
				fmt.Fprintf(ch.Stderr(), "ssh-gateway: %v\r\n", perr)
				_ = req.Reply(false, nil)
				continue
			}
			dims, haveDims = d, true
			_ = req.Reply(true, nil)
		case "shell":
			_ = req.Reply(true, nil)
			started = true
		case "exec":
			// Accepted and then answered, rather than refused. A refused
			// request is reported by the client as a generic failure and the
			// session is torn down without the stderr ever being read — the
			// explanation would be written and never seen. Measured against
			// x/crypto/ssh as the client; `ssh host ls` shows nothing useful
			// either way, so the useful shape is: accept, say why, exit 1.
			_ = req.Reply(true, nil)
			fmt.Fprint(ch.Stderr(),
				"ssh-gateway: exec is not served here — this gateway attaches to a session's PTY, it runs no commands of its own. For files use `harness-cli file push` / `file pull`.\r\n")
			sendExit(ch, 1)
			return
		case "subsystem":
			// Refused rather than accepted: an sftp client that got an accept
			// would sit waiting for a protocol that is never coming, which is
			// worse than the "subsystem request failed" it prints on a refusal.
			fmt.Fprint(ch.Stderr(),
				"ssh-gateway: sftp/scp are not served here — use `harness-cli file push` / `file pull`.\r\n")
			_ = req.Reply(false, nil)
		default:
			_ = req.Reply(false, nil)
		}
		if started {
			break
		}
	}
	if !started {
		return
	}

	g.attachAndPump(ctx, ch, requests, taskID, mode, dims, haveDims)
}

// attachAndPump attaches to the task and splices the ssh channel to it until
// one end stops.
func (g *Gateway) attachAndPump(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request,
	taskID string, mode protocol.AttachMode, dims ptyDims, haveDims bool) {

	stream, replayBytes, kind, err := g.client.AttachSessionWithReplayLimit(ctx, taskID, mode, 0)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "ssh-gateway: %v\r\n", err)
		sendExit(ch, 1)
		return
	}
	defer stream.Close()

	if kind == protocol.TaskKind_Stream {
		fmt.Fprintf(ch.Stderr(),
			"ssh-gateway: task %s is an event-stream session (structured events, no terminal): use `harness-cli session stream attach %s`\r\n",
			taskID, taskID)
		sendExit(ch, 1)
		return
	}

	// Both-or-nothing, matching applyInitialWindowSize: a PTY sized 40x0 is not
	// a smaller terminal, it is a broken one, and filling in the missing half
	// would hide a client that sent only one.
	if haveDims && dims.Rows != 0 && dims.Cols != 0 {
		if serr := stream.SetTerminalWindowSize(dims.Rows, dims.Cols, dims.WidthPx, dims.HeightPx); serr != nil {
			// This is the first frame on a stream that was just opened, so a
			// failure means the stream is dead and the session is unusable.
			fmt.Fprintf(ch.Stderr(), "ssh-gateway: send initial size: %v\r\n", serr)
			sendExit(ch, 1)
			return
		}
	} else {
		fmt.Fprintf(ch.Stderr(),
			"ssh-gateway: no usable terminal size from pty-req; the session keeps the size it had (full-screen apps may mis-render)\r\n")
	}
	fmt.Fprintf(ch.Stderr(), "ssh-gateway: attached to %s as %s (replay %d bytes; Ctrl+] detaches, ~. disconnects)\r\n",
		taskID, mode, replayBytes)

	// window-change arrives while the pump runs, so it needs its own reader.
	// A size the server declines to apply is not an error here: an observer's
	// resize is honoured only while the control seat is empty, which is the
	// server's rule to enforce and not something this end can see.
	go func() {
		for req := range requests {
			if req.Type != "window-change" {
				_ = req.Reply(false, nil)
				continue
			}
			d, perr := parseWindowChange(req.Payload)
			if perr != nil || d.Rows == 0 || d.Cols == 0 {
				continue
			}
			_ = stream.SetTerminalWindowSize(d.Rows, d.Cols, d.WidthPx, d.HeightPx)
		}
	}()

	// The agent's stderr rides its own frame type; an undrained demux side
	// backpressures the whole stream, so copy it concurrently and verbatim.
	go func() { _, _ = io.Copy(ch.Stderr(), stream.Stderr()) }()

	// The pump owns detach-key interception and the half-close the server reads
	// as a detach.
	err = stream.PumpTerminalIO(ch, ch)

	// Reset the client's terminal before the channel closes. This is one of the
	// two endings the gateway is present for; a client that disconnects on its
	// own (~., a closed window, a dropped link) is already gone by the time we
	// get here, and nothing can be delivered to it.
	agentexec.WriteTerminalReset(ch)

	if err != nil {
		fmt.Fprintf(ch.Stderr(), "ssh-gateway: session ended: %v\r\n", err)
		sendExit(ch, 1)
		return
	}
	sendExit(ch, 0)
}

// sendExit reports the session's exit status, which is what `ssh` exits with.
func sendExit(ch ssh.Channel, code uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}
