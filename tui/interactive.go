package tui

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	agentexec "github.com/on-keyday/agent-harness/exec"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// InteractiveReadyMsg lands in the App's Update once OpenInteractive has
// allocated a task on the server, the splice is up, and the bidi stream is
// wrapped in a CommandExecutionStream. The Update handler reacts by
// returning tea.Exec(&interactiveExec{...}) to suspend bubbletea and run
// the PTY shell.
type InteractiveReadyMsg struct {
	Stream *agentexec.CommandExecutionStream
	TaskID string
	Err    error

	// X11Cancel cancels the background -R forward goroutine when set (X11
	// sessions only). Non-x11 paths leave it nil; the Done handler calls it
	// when present so the forward stops with the session.
	X11Cancel context.CancelFunc
	// X11Warn is non-empty when forwarding WITHOUT authentication (no cookie);
	// the Ready handler surfaces it as a status line (stderr would corrupt the
	// alt-screen). Non-x11 paths leave it "".
	X11Warn string
}

// InteractiveDoneMsg lands after tea.Exec returns — claude has exited or
// the user detached. App's Update logs the result and refreshes the tasks
// snapshot so the new Succeeded/Failed status shows up.
type InteractiveDoneMsg struct {
	TaskID string
	Err    error
}

// DoOpenInteractive issues the OpenInteractive RPC against the server and
// posts InteractiveReadyMsg back to the program. The actual tea.Exec call
// happens in the App's Update when InteractiveReadyMsg arrives — Cmds run
// outside the Update loop, but tea.Exec must be returned from Update.
// caps sets RequestedCaps on the wire request; pass protocol.Capability_All
// for the inherit-all behaviour.
func DoOpenInteractive(c *cli.Client, repo string, caps protocol.Capability) tea.Cmd {
	return DoOpenInteractiveWithOpts(c, repo, "", nil, "", caps)
}

// DoOpenInteractiveWithHost is the same as DoOpenInteractive but accepts an
// optional hostname pin.
// caps sets RequestedCaps on the wire request; pass protocol.Capability_All
// for the inherit-all behaviour.
func DoOpenInteractiveWithHost(c *cli.Client, repo, host string, caps protocol.Capability) tea.Cmd {
	return DoOpenInteractiveWithOpts(c, repo, host, nil, "", caps)
}

// DoOpenDetachableSession opens a new detachable interactive session (equivalent
// to `harness-cli session new`). The session persists after the TUI detaches and
// can be re-attached via DoAttachSession / `i` on a Detached task.
// extraArgs are forwarded verbatim to claude (appended after runner-global args).
// resumeTaskID may be "" for a fresh session, or a 32-hex task id to resume
// an existing terminal interactive task's worktree and branch.
// selOpts pins the runner; zero-value SelectorOpts selects any matching runner.
// caps sets RequestedCaps on the wire request; pass protocol.Capability_All
// for the inherit-all behaviour.
func DoOpenDetachableSession(c *cli.Client, repo string, selOpts cli.SelectorOpts, extraArgs []string, resumeTaskID string, caps protocol.Capability) tea.Cmd {
	return func() tea.Msg {
		sel, err := cli.BuildSelector(selOpts)
		if err != nil {
			return InteractiveReadyMsg{Err: fmt.Errorf("selector: %w", err)}
		}
		stream, taskID, err := c.OpenInteractiveWithSelectorArgsAndCaps(context.Background(), repo, sel, extraArgs, resumeTaskID, true, caps, false)
		return InteractiveReadyMsg{Stream: stream, TaskID: taskID, Err: err}
	}
}

// DoOpenX11Session opens a new detachable interactive session with X11
// forwarding (equivalent to `harness-cli session new --x11`). It mirrors
// DoOpenDetachableSession but, on success, spawns a background goroutine that
// runs the -R remote forward (runner 127.0.0.1:6000+displayN -> the client's
// local X server) for the session's lifetime, then posts InteractiveReadyMsg so
// App.Update's existing tea.Exec path drives the PTY. The forward goroutine uses
// the BUFFERED forwardStatusLogf — a raw program.Send would block for the whole
// session because tea.Exec/RemoteShell never drains the msgs channel.
// program MUST be App's *tea.Program. The returned InteractiveReadyMsg carries
// X11Cancel (stops the forward; App stores it and calls it on InteractiveDoneMsg)
// and X11Warn (non-empty => forwarding without authentication).
func DoOpenX11Session(c *cli.Client, repo string, selOpts cli.SelectorOpts, extraArgs []string, resumeTaskID string, displayN int, program *tea.Program, caps protocol.Capability) tea.Cmd {
	return func() tea.Msg {
		sel, err := cli.BuildSelector(selOpts)
		if err != nil {
			return InteractiveReadyMsg{Err: fmt.Errorf("selector: %w", err)}
		}
		stream, taskID, sp, warn, err := c.OpenInteractiveX11(context.Background(), repo, sel, extraArgs, resumeTaskID, displayN, caps, false)
		if err != nil {
			return InteractiveReadyMsg{Stream: stream, TaskID: taskID, Err: err}
		}
		fctx, cancel := context.WithCancel(context.Background())
		go func() {
			_ = cli.RunRemoteForward(fctx, c, taskID, []cli.RemoteForwardSpec{sp}, forwardStatusLogf(fctx, program))
			// RunRemoteForward returns when fctx is cancelled (session end), by
			// which point tea.Exec has returned and the Update loop drains again,
			// so this program.Send won't block. Confirms teardown in cmdresult.
			program.Send(PortForwardStatusMsg{Line: "x11 forward stopped: " + pfShortID(taskID)})
		}()
		return InteractiveReadyMsg{Stream: stream, TaskID: taskID, X11Cancel: cancel, X11Warn: warn}
	}
}

// DoOpenInteractiveWithOpts is the full-featured form: optional hostname
// pin, per-task extraArgs (forwarded verbatim), and optional resumeTaskID
// (32-hex; "" = new task) for reusing an existing terminal interactive
// task's id and worktree branch. On AmbiguousRunner the error is surfaced
// in InteractiveReadyMsg.Err with a hint to supply a host.
// caps sets RequestedCaps on the wire request; pass protocol.Capability_All
// for the inherit-all behaviour.
func DoOpenInteractiveWithOpts(c *cli.Client, repo, host string, extraArgs []string, resumeTaskID string, caps protocol.Capability) tea.Cmd {
	return func() tea.Msg {
		sel, err := cli.BuildSelector(cli.SelectorOpts{Host: host})
		if err != nil {
			return InteractiveReadyMsg{Err: fmt.Errorf("selector: %w", err)}
		}
		stream, taskID, err := c.OpenInteractiveWithSelectorArgsAndCaps(context.Background(), repo, sel, extraArgs, resumeTaskID, false, caps, false)
		return InteractiveReadyMsg{Stream: stream, TaskID: taskID, Err: err}
	}
}

// DoAttachSession re-attaches to an existing detachable interactive task. It
// calls client.AttachSession with the given mode, then posts InteractiveReadyMsg
// so the existing tea.Exec path in App.Update can suspend the TUI and run the
// PTY splice (identical flow to DoOpenInteractiveWithOpts).
// Use protocol.AttachMode_Control for normal reattach (read/write) and
// protocol.AttachMode_View for a read-only observer attach.
func DoAttachSession(c *cli.Client, taskIDHex string, mode protocol.AttachMode) tea.Cmd {
	return func() tea.Msg {
		stream, _, err := c.AttachSession(context.Background(), taskIDHex, mode)
		if err != nil {
			return InteractiveReadyMsg{Err: fmt.Errorf("attach session: %w", err)}
		}
		return InteractiveReadyMsg{Stream: stream, TaskID: taskIDHex}
	}
}

// SessionStartedMsg is the result of `session new --detach`.
// On success TaskID holds the new task's hex id. On failure Err is non-nil.
type SessionStartedMsg struct {
	TaskID string
	Err    error
}

// DoStartDetachedSession opens a detachable interactive session, immediately
// closes the local stream, and returns SessionStartedMsg with the task id.
// Equivalent to `harness-cli session new -d`.
// selOpts pins the runner; zero-value SelectorOpts selects any matching runner.
// caps sets RequestedCaps on the wire request; pass protocol.Capability_All
// for the inherit-all behaviour.
func DoStartDetachedSession(c *cli.Client, repo string, selOpts cli.SelectorOpts, extraArgs []string, resumeTaskID string, caps protocol.Capability) tea.Cmd {
	return func() tea.Msg {
		sel, err := cli.BuildSelector(selOpts)
		if err != nil {
			return SessionStartedMsg{Err: fmt.Errorf("selector: %w", err)}
		}
		stream, taskID, err := c.OpenInteractiveWithSelectorArgsAndCaps(context.Background(), repo, sel, extraArgs, resumeTaskID, true, caps, false)
		if err != nil {
			return SessionStartedMsg{Err: err}
		}
		_ = stream.Close()
		return SessionStartedMsg{TaskID: taskID}
	}
}

// interactiveExec adapts CommandExecutionStream.RemoteShell to bubbletea's
// ExecCommand interface. SetStdin/Stdout/Stderr are no-ops because
// RemoteShell uses os.Stdin / os.Stdout directly (bubbletea's tea.Exec has
// already released the terminal by the time Run is called).
type interactiveExec struct {
	stream *agentexec.CommandExecutionStream
}

func (e *interactiveExec) SetStdin(io.Reader)  {}
func (e *interactiveExec) SetStdout(io.Writer) {}
func (e *interactiveExec) SetStderr(io.Writer) {}

func (e *interactiveExec) Run() error {
	defer e.stream.Close()
	return e.stream.RemoteShell()
}
