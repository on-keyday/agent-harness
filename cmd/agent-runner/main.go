package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
)

var (
	serverCID  = flag.String("server-cid", "ws:localhost:8539-*", "server ConnectionID (e.g. ws:host:port-id, * for random)")
	repo       = flag.String("repo", ".", "absolute path to the repo this runner serves")
	claudeBin  = flag.String("claude-bin", "claude", "path to the claude binary")
	claudeArgs = flag.String("claude-args", "", "extra args passed to claude before -p (whitespace-separated, e.g. \"--dangerously-skip-permissions\")")
)

func main() {
	flag.Parse()
	abs, err := filepath.Abs(*repo)
	if err != nil {
		slog.Error("repo abs", "err", err)
		os.Exit(1)
	}
	peerCID, err := objproto.ParseConnectionID(*serverCID,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		slog.Error("server-cid", "err", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := runner.Run(ctx, runner.Config{
		ServerCID:       peerCID,
		RepoPath:        abs,
		ClaudeBin:       *claudeBin,
		ExtraClaudeArgs: strings.Fields(*claudeArgs),
		Logger:          slog.Default(),
	}); err != nil {
		slog.Error("runner exit", "err", err)
		os.Exit(1)
	}
}
