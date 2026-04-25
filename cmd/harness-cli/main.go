package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/on-keyday/agent-harness/cli"
)

func main() {
	server := flag.String("server", "localhost:8539", "server host:port")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	sub := flag.Arg(0)
	args := flag.Args()[1:]
	ctx := context.Background()

	switch sub {
	case "submit":
		fs := flag.NewFlagSet("submit", flag.ExitOnError)
		repo := fs.String("repo", "", "absolute path to repo")
		task := fs.String("task", "", "prompt text")
		fs.Parse(args)
		if *repo == "" || *task == "" {
			fmt.Fprintln(os.Stderr, "submit: --repo and --task are required")
			os.Exit(2)
		}
		abs, err := filepath.Abs(*repo)
		if err != nil {
			die(err)
		}
		id, err := cli.Submit(ctx, *server, abs, *task)
		if err != nil {
			die(err)
		}
		fmt.Println(id)

	case "ls":
		if err := cli.List(ctx, *server, os.Stdout); err != nil {
			die(err)
		}

	case "cancel":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "cancel: missing task id")
			os.Exit(2)
		}
		if err := cli.Cancel(ctx, *server, args[0]); err != nil {
			die(err)
		}

	case "prune":
		fs := flag.NewFlagSet("prune", flag.ExitOnError)
		repo := fs.String("repo", ".", "repo to prune")
		before := fs.Duration("before", 7*24*time.Hour, "remove worktrees older than this")
		fs.Parse(args)
		abs, err := filepath.Abs(*repo)
		if err != nil {
			die(err)
		}
		if err := cli.Prune(abs, *before, os.Stdout); err != nil {
			die(err)
		}

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli [--server HOST:PORT] <subcommand> [args]")
	fmt.Fprintln(os.Stderr, "  submit --repo PATH --task TEXT     enqueue a new task")
	fmt.Fprintln(os.Stderr, "  ls                                  list runners and recent tasks")
	fmt.Fprintln(os.Stderr, "  cancel TASK_ID                      cancel a queued/running task")
	fmt.Fprintln(os.Stderr, "  prune [--repo PATH] [--before DUR]  remove old harness worktrees")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
