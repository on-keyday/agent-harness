package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/on-keyday/agent-harness/runner"
)

var (
	server    = flag.String("server", "localhost:8539", "server host:port")
	repo      = flag.String("repo", ".", "absolute path to the repo this runner serves")
	claudeBin = flag.String("claude-bin", "claude", "path to the claude binary")
)

func main() {
	flag.Parse()
	abs, err := filepath.Abs(*repo)
	if err != nil {
		slog.Error("repo abs", "err", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := runner.Run(ctx, runner.Config{
		ServerAddr: *server,
		RepoPath:   abs,
		ClaudeBin:  *claudeBin,
		Logger:     slog.Default(),
	}); err != nil {
		slog.Error("runner exit", "err", err)
		os.Exit(1)
	}
}
