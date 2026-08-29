//go:build !js

package sshgw

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/cli"
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

// parseExecReq decodes an exec payload (RFC 4254 6.5) into the command line
// `ssh host <cmd>` sent.
func parseExecReq(payload []byte) (string, error) {
	var er sshwire.ExecReq
	if err := er.DecodeExact(payload); err != nil {
		return "", fmt.Errorf("exec payload: %w", err)
	}
	return string(er.Command), nil
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
	taskID, uopts, err := ParseUserName(user)
	if err != nil {
		_ = newCh.Reject(ssh.Prohibited, err.Error())
		return
	}
	if !g.claim(taskID, uopts.Mode) {
		_ = newCh.Reject(ssh.Prohibited, fmt.Sprintf(
			"ssh-gateway: task %s already has a control session through this gateway; connect without .control to co-write, or with .view to watch",
			taskID))
		return
	}
	// Released exactly once, and possibly early: an `exec` never attaches, so it
	// must not sit on the control seat for the length of the command. Calling
	// g.release twice would be worse than harmless — between the two calls
	// another session can claim the seat, and the second call would free THAT
	// session's.
	released := false
	release := func() {
		if !released {
			released = true
			g.release(taskID, uopts.Mode)
		}
	}
	defer release()

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
			// `ssh host cmd` runs the command in the task's WORKTREE, as its own
			// process. It used to be accepted-then-refused, because the only
			// command surface then was `session exec`, which types into the
			// session's foreground shell and merges the two output streams —
			// nothing like what ssh promises. `exec` is that missing thing:
			// separate stdout and stderr, the command's own exit code, and a
			// session that is never touched.
			cmdline, perr := parseExecReq(req.Payload)
			if perr != nil {
				// Accepted first, then answered. A REFUSED request is reported
				// by the client as a generic failure and the channel is torn
				// down without the stderr ever being read, so the explanation
				// would be written and never seen (measured against
				// x/crypto/ssh).
				_ = req.Reply(true, nil)
				fmt.Fprintf(ch.Stderr(), "ssh-gateway: %v\r\n", perr)
				sendExit(ch, 1)
				return
			}
			_ = req.Reply(true, nil)
			// The seat goes back now: this never attaches, so holding a
			// controller's seat for the length of `make test` would lock a real
			// attach out for no reason.
			release()
			// An exec dies with the SSH SESSION, not with this gateway. The
			// request channel closes when the client's channel goes away, which
			// is the only signal this end gets that `ssh host cmd` was
			// interrupted — Ctrl-C at the far terminal kills the ssh client, and
			// nothing else here would notice.
			ectx, cancelExec := context.WithCancel(ctx)
			go func() {
				defer cancelExec()
				for r := range requests {
					if r.WantReply {
						_ = r.Reply(false, nil)
					}
				}
			}()
			g.runExec(ectx, ch, taskID, cmdline, uopts)
			cancelExec()
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

	g.attachAndPump(ctx, ch, requests, taskID, uopts.Mode, dims, haveDims)
}

// runExec runs one command line in the task's worktree and reports its exit
// status as the ssh session's.
//
// The command line goes to the runner AS A LINE (ExecRunOpts.ShellLine), for
// its own shell to interpret. That is what `ssh host cmd` means everywhere
// else: the client sends ONE string it expects a shell to interpret, so
// re-splitting it here on whitespace would break every quote and redirection
// the operator typed.
//
// And the shell is not this end's to choose. Sending ["sh","-c",line] — which
// this did — is right on unix and wrong on a Windows runner, where it worked
// only because Git for Windows had put sh on PATH. The runner knows its own
// platform; nothing here does.
//
// The user-name suffix (.control / .view / bare) does not gate this. It selects
// how a SHELL session ATTACHES, and an exec never attaches — treating .view as
// read-only here would advertise an authority boundary the gateway does not
// have, since reaching it at all already means holding the operator's
// credentials.
func (g *Gateway) runExec(ctx context.Context, ch ssh.Channel, taskID, cmdline string, uopts UserOpts) {
	// The kill is by ID, and it is not belt-and-braces: cancelling ctx only
	// unwinds THIS end. The server drops an exec when its client's CONNECTION
	// goes away, and one ssh client leaving is not this gateway's harness
	// connection leaving — so without naming the id, an interrupted
	// `ssh host cmd` leaves the command running with nobody watching it.
	// Measured before this existed: the ssh client gone, `exec ls` still
	// listing it, the child still alive on the runner.
	var execID atomic.Uint64
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		id := execID.Load()
		if id == 0 {
			return // never started; there is nothing to name
		}
		// A fresh context: ctx is the cancelled one, and the kill has to
		// outlive whatever cancelled it.
		kctx, kcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer kcancel()
		if err := g.client.ExecRunKillWith(kctx, id); err != nil {
			slog.Info("ssh-gateway: could not stop the exec its client abandoned",
				"exec_id", id, "task", taskID, "err", err)
		}
	}()

	res, err := g.client.ExecRun(ctx, taskID, []string{cmdline}, cli.ExecRunOpts{
		// Both come from the ssh USER NAME, so they are a property of the
		// connection rather than of one command: a client that opens several
		// execs over one connection — which is what a remote editor's
		// bootstrap does — gets the same treatment on each without having to
		// say so per command, and it has nowhere to say so anyway.
		Detached:   uopts.Detach,
		SshdParent: uopts.SshdParent,
		// The RUNNER picks the shell. This used to send ["sh","-c",line], which
		// is right on unix and wrong on Windows — and only appeared to work
		// there because Git for Windows had put sh on PATH.
		ShellLine: true,
		OnStarted: func(id uint64) { execID.Store(id) },
		// Separate ends, all the way out to the ssh client: keeping them apart
		// is the whole reason this is wired to `exec` and not to `session exec`.
		Stdin:  ch,
		Stdout: ch,
		Stderr: ch.Stderr(),
	})
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "ssh-gateway: %v\r\n", err)
		sendExit(ch, 1)
		return
	}
	if res.Kind != protocol.ExecEventKind_Exited {
		// The command never ran, or was killed. 125 rather than an invented
		// 127, matching `harness-cli exec`: 127 is a shell's convention and the
		// failure here is that no shell ever started.
		fmt.Fprintf(ch.Stderr(), "ssh-gateway: exec %s: %s\r\n", res.Kind.String(), res.Detail)
		sendExit(ch, 125)
		return
	}
	sendExit(ch, uint32(res.ExitCode))
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
