package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

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

	taskID, argv, err := splitExecArgv(args)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return fmt.Errorf("usage: harness-cli exec <task-id> -- <command> [args...]")
	}

	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	// stdin is forwarded only when it is NOT a terminal. An interactive
	// invocation that piped the terminal in would leave the child waiting on a
	// tty nobody is typing into, which looks exactly like a hung command.
	var stdin io.Reader
	if !isTTY(os.Stdin) {
		stdin = os.Stdin
	}
	res, err := c.ExecRun(ctx, taskID, argv, cli.ExecRunOpts{
		Stdin:  stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		return err
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
