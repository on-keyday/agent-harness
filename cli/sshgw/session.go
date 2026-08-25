//go:build !js

package sshgw

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"golang.org/x/crypto/ssh"
)

// ptyDims are the four numbers pty-req and window-change both carry.
//
// SSH puts COLUMNS first; SetTerminalWindowSize takes ROWS first. Decoding into
// named fields is what stops the two orders from being silently swapped — a
// transposed size renders a screen that looks plausible and is wrong.
type ptyDims struct {
	Cols, Rows, WidthPx, HeightPx uint16
}

// parseWindowChange decodes a window-change payload: four uint32s.
func parseWindowChange(payload []byte) (ptyDims, error) {
	if len(payload) < 16 {
		return ptyDims{}, fmt.Errorf("window-change payload is %d bytes, want 16", len(payload))
	}
	return ptyDims{
		Cols:     uint16(binary.BigEndian.Uint32(payload[0:4])),
		Rows:     uint16(binary.BigEndian.Uint32(payload[4:8])),
		WidthPx:  uint16(binary.BigEndian.Uint32(payload[8:12])),
		HeightPx: uint16(binary.BigEndian.Uint32(payload[12:16])),
	}, nil
}

// parsePtyReq decodes a pty-req payload: a TERM string, the same four uint32s,
// then encoded modes.
//
// TERM is read and discarded. The runner-side PTY's TERM is fixed when the
// session is created, and changing it mid-session would change what the
// already-running agent renders.
func parsePtyReq(payload []byte) (ptyDims, error) {
	if len(payload) < 4 {
		return ptyDims{}, fmt.Errorf("pty-req payload is %d bytes, too short for the TERM length", len(payload))
	}
	termLen := binary.BigEndian.Uint32(payload[0:4])
	rest := payload[4:]
	if uint32(len(rest)) < termLen {
		return ptyDims{}, fmt.Errorf("pty-req TERM length %d exceeds the payload", termLen)
	}
	return parseWindowChange(rest[termLen:])
}

// serveSession runs one ssh session channel against one attach stream.
func (g *gateway) serveSession(ctx context.Context, user string, newCh ssh.NewChannel) {
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
		case "exec", "subsystem":
			// Refused with a reason on stderr, because an ssh client prints
			// that and prints nothing useful otherwise.
			fmt.Fprintf(ch.Stderr(),
				"ssh-gateway: %s is not served here — this gateway attaches to a session's PTY. For files use `harness-cli file push` / `file pull`.\r\n",
				req.Type)
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
func (g *gateway) attachAndPump(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request,
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
