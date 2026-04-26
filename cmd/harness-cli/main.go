package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
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

	switch sub {
	case "submit":
		fs := flag.NewFlagSet("submit", flag.ExitOnError)
		repo := fs.String("repo", ".", "path to repo (defaults to cwd)")
		task := fs.String("task", "", "prompt text")
		fs.Parse(args)
		if *task == "" {
			fmt.Fprintln(os.Stderr, "submit: --task is required")
			os.Exit(2)
		}
		abs, err := filepath.Abs(*repo)
		if err != nil {
			die(err)
		}
		id, err := cli.Submit(ctx, parseCID(), abs, *task)
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
		repo := fs.String("repo", ".", "path to repo (defaults to cwd)")
		fs.Parse(args)
		abs, err := filepath.Abs(*repo)
		if err != nil {
			die(err)
		}
		if _, err := cli.Interactive(ctx, parseCID(), abs); err != nil {
			die(err)
		}

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli [--server-cid CID] <subcommand> [args]")
	fmt.Fprintln(os.Stderr, "  submit [--repo PATH] --task TEXT    enqueue a new task (--repo defaults to cwd)")
	fmt.Fprintln(os.Stderr, "  ls                                  list runners and recent tasks")
	fmt.Fprintln(os.Stderr, "  cancel TASK_ID                      cancel a queued/running task")
	fmt.Fprintln(os.Stderr, "  prune [--before DUR]                forget terminal tasks on the server")
	fmt.Fprintln(os.Stderr, "  prune-local [--repo PATH] [--before DUR]")
	fmt.Fprintln(os.Stderr, "                                      remove old worktrees in <repo>/.harness-worktrees/")
	fmt.Fprintln(os.Stderr, "  logs TASK_ID                        stream task log output")
	fmt.Fprintln(os.Stderr, "  watch                               stream task and runner status events")
	fmt.Fprintln(os.Stderr, "  interactive [--repo PATH]           attach an interactive PTY claude session (must be run from a tty)")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
