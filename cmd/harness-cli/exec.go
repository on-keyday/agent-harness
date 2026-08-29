package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// execExitError is the exit code for an exec that never produced one of its
// own — killed, or a child that failed to start. 125 rather than an invented
// 127: `session exec` already uses 125 for "error", so the two verbs agree, and
// 127 is a shell's convention where there is no shell.
const execExitError = 125

// splitExecArgv peels the command off the tail.
//
// A bare "--" ends the flags, exactly as `git`'s pathspec separator does here,
// and everything after it is the argv VERBATIM — never re-joined and re-split,
// because an argument containing a space is one argument and splitting it would
// silently change the command. Without a "--", everything after the task id is
// the argv.
func splitExecArgv(args []string) (taskID string, argv []string, err error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("exec: missing task id")
	}
	taskID = args[0]
	rest := args[1:]
	for i, a := range rest {
		if a == "--" {
			return taskID, rest[i+1:], nil
		}
	}
	return taskID, rest, nil
}

func runExec(ctx context.Context, cid objproto.ConnectionID, args []string) error {
	switch args[0] {
	case "ls":
		fs := flag.NewFlagSet("exec ls", flag.ExitOnError)
		taskFilter := fs.String("task", "", "only execs against this task id")
		asJSON := fs.Bool("json", false, "one JSON object per exec")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		execs, err := cli.ExecRunList(ctx, cid, *taskFilter)
		if err != nil {
			return err
		}
		if *asJSON {
			for i := range execs {
				fmt.Println(cli.ExecRunInfoJSONLine(&execs[i]))
			}
			return nil
		}
		for _, line := range cli.ExecRunInfoLines(execs) {
			fmt.Println(line)
		}
		return nil

	case "kill":
		if len(args) < 2 {
			return fmt.Errorf("usage: harness-cli exec kill <exec-id> [<exec-id> ...]")
		}
		for _, raw := range args[1:] {
			id, perr := strconv.ParseUint(raw, 10, 64)
			if perr != nil {
				return fmt.Errorf("exec kill: bad exec id %q", raw)
			}
			if err := cli.ExecRunKill(ctx, cid, id); err != nil {
				return err
			}
			fmt.Printf("killed exec %d\n", id)
		}
		return nil
	}

	// These are scanned BEFORE the task id, because everything after the id is
	// the argv VERBATIM: re-scanning that for flags is how a command whose own
	// first word is `--shell` would be eaten.
	shellLine, sshdParent := false, false
scan:
	for len(args) > 0 {
		switch args[0] {
		case "--shell":
			shellLine = true
		case "--sshd-parent":
			sshdParent = true
		default:
			break scan
		}
		args = args[1:]
	}
	taskID, argv, err := splitExecArgv(args)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return fmt.Errorf("usage: harness-cli exec [--shell] [--sshd-parent] <task-id> -- <command> [args...]")
	}
	if shellLine {
		// Joining is right here and ONLY here: the operator asked for shell
		// interpretation, so these words were never an argv to preserve. The
		// runner picks sh -c or cmd /c from its own platform.
		argv = []string{strings.Join(argv, " ")}
	}

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
