package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// execExitError is the exit code for an exec that never produced one of its
// own — killed, or a child that failed to start. 125 rather than an invented
// 127: `session exec` already uses 125 for "error", so the two verbs agree, and
// 127 is a shell's convention where there is no shell.
const execExitError = 125

// runExecAction runs the `exec <task-id> -- <cmd>` form. The ls / kill forms
// are their own declared verbs and their own dispatch methods; this is the one
// that ends in os.Exit with the child's code, which is what makes `exec`
// usable from a script.
func runExecAction(ctx context.Context, cid objproto.ConnectionID, run verb.ExecRunAction) error {
	taskID, argv, shellLine, sshdParent := run.TaskID, run.Argv, run.Shell, run.SshdParent

	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}

	// Ctrl-C has to reach the CHILD, not just this process. The exec is
	// registered server-side against this CONNECTION, and the server kills the
	// child when the connection goes away (DropExecRunsForConn) — so an
	// interrupt that killed this process outright would leave the command
	// running on the runner with nothing left to stop it but `exec kill`.
	// Cancelling the context unwinds ExecRun so the close below can happen.
	// Same shape as `forward` and `ssh-gateway`.
	ectx, stopInterrupts := interruptContext("exec", ctx)

	// stdin is forwarded only when it is NOT a terminal. An interactive
	// invocation that piped the terminal in would leave the child waiting on a
	// tty nobody is typing into, which looks exactly like a hung command.
	var stdin io.Reader
	if !isTTY(os.Stdin) {
		stdin = os.Stdin
	}
	res, runErr := c.ExecRun(ectx, taskID, argv, cli.ExecRunOpts{
		ShellLine:  shellLine,
		SshdParent: sshdParent,
		Stdin:      stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	})

	// Closed HERE, not in a defer: every path below ends in os.Exit, which runs
	// no defers. Without this the connection would be left to a ping timeout —
	// and with it the server's exec registration, and any child still running.
	stopInterrupts()
	c.Close()

	if runErr != nil {
		// The operator's own Ctrl-C is not a failure to report: the close above
		// is what stopped the child, and interruptContext already said
		// "exec: stopping…". 130 is the shell's convention for SIGINT.
		if errors.Is(runErr, context.Canceled) {
			os.Exit(130)
		}
		return runErr
	}
	switch res.Kind {
	case protocol.ExecEventKind_Exited:
		// The command's own code becomes ours: that is what makes this usable
		// from a script.
		os.Exit(int(res.ExitCode))
	default:
		fmt.Fprintf(os.Stderr, "exec: %s: %s\n", res.Kind.String(), res.Detail)
		os.Exit(execExitError)
	}
	return nil
}
