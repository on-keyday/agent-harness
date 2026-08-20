// Command harness-stream-adapter runs an event-stream agent behind the neutral
// adapter protocol.
//
// The runner resolves the agent argv (bin, sandbox wrapper, profile) and hands
// it here; this process execs it, appends the vendor protocol flags, and speaks
// neutral NDJSON on its own stdio. Everything vendor-specific stops at this
// binary.
//
//	harness-stream-adapter [--dir D] [--prompt P] [--resume-conversation] -- claude
//
// stdout  neutral NDJSON: hello, events, requests, exit
// stdin   neutral NDJSON: responses, user turns
// stderr  the agent's stderr, verbatim
//
// The `--` is required: everything after it is the agent command, so an agent
// flag can never be mistaken for one of this program's.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/on-keyday/agent-harness/runner/streamagent"
)

func main() {
	fs := flag.NewFlagSet("harness-stream-adapter", flag.ContinueOnError)
	dir := fs.String("dir", "", "working directory for the agent (the task worktree)")
	prompt := fs.String("prompt", "", "first user turn; empty leaves the agent idle until one is sent")
	resume := fs.Bool("resume-conversation", false,
		"resume the agent's own conversation. An INTENT, not a flag: the adapter "+
			"picks the vendor spelling, so the runner names no vendor flags")
	version := fs.Bool("version", false, "print the adapter protocol version and exit")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] -- <agent argv...>\n\n", fs.Name())
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nSpeaks adapter protocol v%d on stdio.\n", streamagent.ProtocolVersion)
	}

	argv, agentArgv := splitAtDashDash(os.Args[1:])
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if *version {
		fmt.Println(streamagent.ProtocolVersion)
		return
	}
	if len(agentArgv) == 0 {
		fmt.Fprintln(os.Stderr, "harness-stream-adapter: no agent command; expected `-- <agent argv...>`")
		fs.Usage()
		os.Exit(2)
	}

	// SIGINT/SIGTERM cancel the context, which kills the agent process group
	// through CommandContext. The runner already owns process lifetime; this
	// only makes a hand-run adapter behave.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := streamagent.RunClaude(ctx, streamagent.ClaudeOpts{
		Argv:               agentArgv,
		Dir:                *dir,
		Prompt:             *prompt,
		ResumeConversation: *resume,
		Out:                os.Stdout,
		In:                 os.Stdin,
		ErrOut:             os.Stderr,
	})
	if err != nil {
		// The failure was already reported as an `exit` line on stdout, which
		// is what the runner reads; stderr is for a human running this by hand.
		fmt.Fprintf(os.Stderr, "harness-stream-adapter: %v\n", err)
		os.Exit(1)
	}
}

// splitAtDashDash returns the args before the first `--` and those after it.
// A missing `--` puts everything in the first half, which fs.Parse then
// rejects or accepts on its own terms — the caller checks for an empty agent
// argv rather than this function guessing.
func splitAtDashDash(args []string) (mine, theirs []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}
