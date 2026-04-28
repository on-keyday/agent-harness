//go:build !js

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func main() {
	serverCID := flag.String("server-cid", "ws:127.0.0.1:8539-*",
		"server ConnectionID (e.g. ws:host:port-id, * for random)")
	wsPath := flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
	flag.Usage = usage
	flag.Parse()
	cli.WebSocketPath = *wsPath

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	sub := flag.Arg(0)
	args := flag.Args()[1:]
	ctx := context.Background()

	parseCID := func() objproto.ConnectionID {
		peerCID, err := objproto.ParseConnectionID(*serverCID,
			objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
		if err != nil {
			die(fmt.Errorf("server-cid: %w", err))
		}
		return peerCID
	}

	// addSelectorFlags registers --runner/--host/--ip on fs and returns a
	// function that resolves them to a RunnerSelector after fs.Parse.
	addSelectorFlags := func(fs *flag.FlagSet) func() protocol.RunnerSelector {
		runner := fs.String("runner", "", "pin to a specific runner by ConnectionID hex")
		host := fs.String("host", "", "pin to runner by hostname")
		ip := fs.String("ip", "", "pin to runner by IP address")
		return func() protocol.RunnerSelector {
			opts := cli.SelectorOpts{Runner: *runner, Host: *host, IP: *ip}
			if err := opts.ValidateSelector(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			sel, err := cli.BuildSelector(opts)
			if err != nil {
				die(err)
			}
			return sel
		}
	}

	switch sub {
	case "submit":
		fs := flag.NewFlagSet("submit", flag.ExitOnError)
		repo := fs.String("repo", "", "repo identifier; must match a runner-registered RepoPath verbatim")
		task := fs.String("task", "", "prompt text")
		resolveSelector := addSelectorFlags(fs)
		fs.Parse(args)
		if *task == "" {
			fmt.Fprintln(os.Stderr, "submit: --task is required")
			os.Exit(2)
		}
		if *repo == "" {
			fmt.Fprintln(os.Stderr, "submit: --repo is required (must match a runner's RepoPath verbatim)")
			os.Exit(2)
		}
		sel := resolveSelector()
		// Hand-rolled Dial→SayHello→Submit→Close so the server records
		// kind=cli on this connection. Used by ii (origin tracking) so
		// the resulting task is attributed to "cli" in `harness-cli ls`.
		c, err := cli.Dial(ctx, parseCID())
		if err != nil {
			die(err)
		}
		defer c.Close()
		if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
			die(err)
		}
		id, err := c.SubmitWithSelector(ctx, *repo, *task, sel)
		if err != nil {
			die(err)
		}
		fmt.Println(id)

	case "ls":
		if err := cli.List(ctx, parseCID(), os.Stdout); err != nil {
			die(err)
		}

	case "cancel":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "cancel: missing task id")
			os.Exit(2)
		}
		if err := cli.Cancel(ctx, parseCID(), args[0]); err != nil {
			die(err)
		}

	case "prune":
		fs := flag.NewFlagSet("prune", flag.ExitOnError)
		before := fs.Duration("before", 7*24*time.Hour, "forget terminal tasks older than this")
		fs.Parse(args)
		if err := cli.Prune(ctx, parseCID(), *before, os.Stdout); err != nil {
			die(err)
		}

	case "prune-local":
		fs := flag.NewFlagSet("prune-local", flag.ExitOnError)
		repo := fs.String("repo", ".", "repo to prune")
		before := fs.Duration("before", 7*24*time.Hour, "remove worktrees older than this")
		fs.Parse(args)
		abs, err := filepath.Abs(*repo)
		if err != nil {
			die(err)
		}
		if err := cli.PruneLocal(ctx, abs, *before, os.Stdout); err != nil {
			die(err)
		}

	case "logs":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "logs: missing task id")
			os.Exit(2)
		}
		if err := cli.Logs(ctx, parseCID(), args[0], os.Stdout); err != nil {
			die(err)
		}

	case "watch":
		if err := cli.Watch(ctx, parseCID(), os.Stdout); err != nil {
			die(err)
		}

	case "interactive":
		fs := flag.NewFlagSet("interactive", flag.ExitOnError)
		repo := fs.String("repo", "", "repo identifier; must match a runner-registered RepoPath verbatim")
		resolveSelector := addSelectorFlags(fs)
		fs.Parse(args)
		if *repo == "" {
			fmt.Fprintln(os.Stderr, "interactive: --repo is required (must match a runner's RepoPath verbatim)")
			os.Exit(2)
		}
		sel := resolveSelector()
		// Hand-rolled Dial→SayHello→Interactive→Close so the server
		// records kind=cli on this connection (origin attribution).
		c, err := cli.Dial(ctx, parseCID())
		if err != nil {
			die(err)
		}
		defer c.Close()
		if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
			die(err)
		}
		if _, err := c.InteractiveWithSelector(ctx, *repo, sel); err != nil {
			die(err)
		}

	case "agent":
		if len(args) == 0 {
			agentUsage()
			os.Exit(2)
		}
		asub := args[0]
		rest := args[1:]
		var err error
		switch asub {
		case "send":
			err = agent.Send(ctx, rest, os.Stdin, os.Stdout)
		case "wait":
			err = agent.Wait(ctx, rest, os.Stdout)
		case "inbox":
			err = agent.Inbox(ctx, rest, os.Stdout)
		case "subscribe":
			err = agent.Subscribe(ctx, rest, os.Stdout)
		case "unsubscribe":
			err = agent.Unsubscribe(ctx, rest, os.Stdout)
		case "dispatch":
			err = agent.Dispatch(ctx, rest, os.Stdin, os.Stdout)
		default:
			agentUsage()
			os.Exit(2)
		}
		if err != nil {
			die(err)
		}

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli [--server-cid CID] <subcommand> [args]")
	fmt.Fprintln(os.Stderr, "  submit --repo REPO --task TEXT [--runner HEX | --host NAME | --ip ADDR]")
	fmt.Fprintln(os.Stderr, "                                      enqueue a new task (optionally pin to a runner)")
	fmt.Fprintln(os.Stderr, "  ls                                  list runners and recent tasks")
	fmt.Fprintln(os.Stderr, "  cancel TASK_ID                      cancel a queued/running task")
	fmt.Fprintln(os.Stderr, "  prune [--before DUR]                forget terminal tasks on the server")
	fmt.Fprintln(os.Stderr, "  prune-local [--repo PATH] [--before DUR]")
	fmt.Fprintln(os.Stderr, "                                      remove old worktrees in <repo>/.harness-worktrees/ (local fs; PATH is client-side)")
	fmt.Fprintln(os.Stderr, "  logs TASK_ID                        stream task log output")
	fmt.Fprintln(os.Stderr, "  watch                               stream task and runner status events")
	fmt.Fprintln(os.Stderr, "  interactive --repo REPO [--runner HEX | --host NAME | --ip ADDR]")
	fmt.Fprintln(os.Stderr, "                                      attach an interactive PTY claude session")
	fmt.Fprintln(os.Stderr, "  agent {send|wait|inbox|subscribe|unsubscribe|dispatch}")
	fmt.Fprintln(os.Stderr, "                                      agent-to-agent message ops (env-primary; HARNESS_AUTH_TICKET required)")
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli agent <subcommand> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Env-primary (HARNESS_*): SERVER_CID, TASK_ID, RUNNER_ID, HOSTNAME, WS_PATH, REPO_PATH")
	fmt.Fprintln(os.Stderr, "HARNESS_AUTH_TICKET is env-only (no flag accepted).")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  send --topic T --data D|-           publish a message")
	fmt.Fprintln(os.Stderr, "  wait --topic T [--since-last] [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       block until next message")
	fmt.Fprintln(os.Stderr, "  inbox [--since-last]                 non-blocking dump (used by hook)")
	fmt.Fprintln(os.Stderr, "  subscribe --topic T                  register a subscription")
	fmt.Fprintln(os.Stderr, "  unsubscribe --topic T                remove a subscription")
	fmt.Fprintln(os.Stderr, "  dispatch --topic T --reply-topic R --data D|- [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       send + wait for reply (sugar)")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
