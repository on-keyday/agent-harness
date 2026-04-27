package tui

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	agentexec "github.com/on-keyday/agent-harness/exec"
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
	return DoOpenInteractiveWithHost(c, repo, "")
}

// DoOpenInteractiveWithHost is the same as DoOpenInteractive but accepts an
// optional hostname pin. On AmbiguousRunner the error is surfaced in
// InteractiveReadyMsg.Err with a hint to supply a host.
func DoOpenInteractiveWithHost(c *cli.Client, repo, host string) tea.Cmd {
	return func() tea.Msg {
		sel, err := cli.BuildSelector(cli.SelectorOpts{Host: host})
		if err != nil {
			return InteractiveReadyMsg{Err: fmt.Errorf("selector: %w", err)}
		}
		stream, taskID, err := c.OpenInteractiveWithSelector(context.Background(), repo, sel)
		return InteractiveReadyMsg{Stream: stream, TaskID: taskID, Err: err}
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
