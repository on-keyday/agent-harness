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
func DoOpenInteractive(c *cli.Client, repo string) tea.Cmd {
	return DoOpenInteractiveWithOpts(c, repo, "", nil, "")
}

// DoOpenInteractiveWithHost is the same as DoOpenInteractive but accepts an
// optional hostname pin.
func DoOpenInteractiveWithHost(c *cli.Client, repo, host string) tea.Cmd {
	return DoOpenInteractiveWithOpts(c, repo, host, nil, "")
}

// DoOpenDetachableSession opens a new detachable interactive session (equivalent
// to `harness-cli session new`). The session persists after the TUI detaches and
// can be re-attached via DoAttachSession / `i` on a Detached task.
// extraArgs are forwarded verbatim to claude (appended after runner-global args).
// resumeTaskID may be "" for a fresh session, or a 32-hex task id to resume
// an existing terminal interactive task's worktree and branch.
// selOpts pins the runner; zero-value SelectorOpts selects any matching runner.
func DoOpenDetachableSession(c *cli.Client, repo string, selOpts cli.SelectorOpts, extraArgs []string, resumeTaskID string) tea.Cmd {
	return func() tea.Msg {
		sel, err := cli.BuildSelector(selOpts)
		if err != nil {
			return InteractiveReadyMsg{Err: fmt.Errorf("selector: %w", err)}
		}
		stream, taskID, err := c.OpenInteractiveWithSelectorAndArgs(context.Background(), repo, sel, extraArgs, resumeTaskID, true)
		return InteractiveReadyMsg{Stream: stream, TaskID: taskID, Err: err}
	}
}

// DoOpenInteractiveWithOpts is the full-featured form: optional hostname
// pin, per-task extraArgs (forwarded verbatim), and optional resumeTaskID
// (32-hex; "" = new task) for reusing an existing terminal interactive
// task's id and worktree branch. On AmbiguousRunner the error is surfaced
// in InteractiveReadyMsg.Err with a hint to supply a host.
func DoOpenInteractiveWithOpts(c *cli.Client, repo, host string, extraArgs []string, resumeTaskID string) tea.Cmd {
	return func() tea.Msg {
		sel, err := cli.BuildSelector(cli.SelectorOpts{Host: host})
		if err != nil {
			return InteractiveReadyMsg{Err: fmt.Errorf("selector: %w", err)}
		}
		stream, taskID, err := c.OpenInteractiveWithSelectorAndArgs(context.Background(), repo, sel, extraArgs, resumeTaskID, false)
		return InteractiveReadyMsg{Stream: stream, TaskID: taskID, Err: err}
	}
}

// DoAttachSession re-attaches to an existing detachable interactive task. It
// calls client.AttachSession, then posts InteractiveReadyMsg so the existing
// tea.Exec path in App.Update can suspend the TUI and run the PTY splice
// (identical flow to DoOpenInteractiveWithOpts).
func DoAttachSession(c *cli.Client, taskIDHex string) tea.Cmd {
	return func() tea.Msg {
		stream, _, err := c.AttachSession(context.Background(), taskIDHex, protocol.AttachMode_Control)
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
func DoStartDetachedSession(c *cli.Client, repo string, selOpts cli.SelectorOpts, extraArgs []string, resumeTaskID string) tea.Cmd {
	return func() tea.Msg {
		sel, err := cli.BuildSelector(selOpts)
		if err != nil {
			return SessionStartedMsg{Err: fmt.Errorf("selector: %w", err)}
		}
		stream, taskID, err := c.OpenInteractiveWithSelectorAndArgs(context.Background(), repo, sel, extraArgs, resumeTaskID, true)
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
