//go:build !js

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runSession dispatches session sub-verbs: new / attach / ls / kill.
// cid is the already-resolved server ConnectionID from main()'s parseCID().
func runSession(cid objproto.ConnectionID, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: harness-cli session <new|attach|ls|kill> [args]")
		os.Exit(2)
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "new":
		return runSessionNew(cid, rest)
	case "attach":
		return runSessionAttach(cid, rest)
	case "ls":
		return runSessionLs(cid, rest)
	case "kill":
		return runSessionKill(cid, rest)
	default:
		return fmt.Errorf("unknown session verb %q", verb)
	}
}

// runSessionNew opens a new detachable interactive PTY session on a runner
// and blocks until the session ends (Ctrl+D / exit / detach).
// With -d / --detach the stream is closed immediately after open and the task
// id is printed — mirroring `docker run -d`.
func runSessionNew(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session new", flag.ExitOnError)
	repo := fs.String("repo", "", "repo path (required; env: HARNESS_REPO_PATH)")
	runner := fs.String("runner", "", "pin to runner by ConnectionID hex")
	host := fs.String("host", "", "pin to runner by hostname")
	ip := fs.String("ip", "", "pin to runner by IP address")
	resume := fs.String("resume", "", "task id (32 hex) of a terminal interactive task to resume into a new detachable session; --repo is ignored")
	var extraArgs repeatableStrings
	fs.Var(&extraArgs, "claude-arg", "extra CLI arg to forward to claude (repeatable; appended after runner-global --claude-args)")
	detach := false
	fs.BoolVar(&detach, "detach", false, "start the session and immediately detach (run in background, print task id, exit)")
	fs.BoolVar(&detach, "d", false, "shorthand for --detach")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repoVal := *repo
	if repoVal == "" {
		repoVal = os.Getenv("HARNESS_REPO_PATH")
	}
	if repoVal == "" && *resume == "" {
		return fmt.Errorf("session new: --repo required (or set HARNESS_REPO_PATH) — except when --resume is set, which uses the existing task's repo")
	}

	opts := cli.SelectorOpts{Runner: *runner, Host: *host, IP: *ip}
	if err := opts.ValidateSelector(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sel, err := cli.BuildSelector(opts)
	if err != nil {
		return err
	}

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
		return err
	}

	if detach {
		stream, taskIDHex, err := c.OpenInteractiveWithSelectorAndArgs(ctx, repoVal, sel, []string(extraArgs), *resume, true)
		if err != nil {
			return err
		}
		_ = stream.Close() // immediately detach → server transitions Running -> Detached
		fmt.Println(taskIDHex)
		return nil
	}

	id, err := c.InteractiveWithSelectorAndArgs(ctx, repoVal, sel, []string(extraArgs), *resume, true /*detachable*/)
	if err != nil {
		return err
	}
	fmt.Printf("session %s ended\n", id)
	return nil
}

// runSessionAttach re-attaches to a detachable interactive session by id.
func runSessionAttach(cid objproto.ConnectionID, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: session attach <id>")
	}
	taskIDHex := args[0]

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
		return err
	}

	if _, err := c.SessionAttach(ctx, taskIDHex); err != nil {
		return err
	}
	return nil
}

// runSessionLs lists detachable interactive sessions as JSON Lines.
func runSessionLs(cid objproto.ConnectionID, _ []string) error {
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid)
	if err != nil {
		return err
	}
	defer c.Close()

	lr, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	for _, t := range lr.Tasks {
		if t.Kind != protocol.TaskKind_Interactive || !t.Detachable() {
			continue
		}
		_ = enc.Encode(map[string]any{
			"id":                hex.EncodeToString(t.Id.Id[:]),
			"status":            t.Status.String(),
			"is_attached":       t.IsAttached(),
			"repo":              string(t.RepoPath),
			"runner":            protocol.RunnerIDToConnID(t.AssignedTo).String(),
			"created_at":        t.CreatedAt,
			"started_at":        t.StartedAt,
			"ring_buffer_bytes": t.RingBufferBytes,
		})
	}
	return nil
}

// runSessionKill cancels a session (alias of 'harness-cli cancel <id>').
func runSessionKill(cid objproto.ConnectionID, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: session kill <id>")
	}
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Cancel(ctx, args[0])
}
