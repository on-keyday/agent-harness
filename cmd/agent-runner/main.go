package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
)

var (
	serverCID  = flag.String("server-cid", "ws:127.0.0.1:8539-*", "server ConnectionID (e.g. ws:host:port-id, * for random)")
	roots      = flag.String("roots", ".", "comma-separated list of absolute repo root paths this runner serves")
	maxTasks   = flag.Int("max-tasks", 1, "maximum number of concurrent tasks (>= 1)")
	claudeBin  = flag.String("claude-bin", "claude", "path to the claude binary")
	claudeArgs = flag.String("claude-args", "", "extra args passed to claude before -p (whitespace-separated, e.g. \"--dangerously-skip-permissions\")")
	wsPath     = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
)

func main() {
	flag.Parse()
	cli.WebSocketPath = *wsPath

	if *maxTasks < 1 {
		fmt.Fprintf(os.Stderr, "agent-runner: --max-tasks must be >= 1, got %d\n", *maxTasks)
		os.Exit(1)
	}

	rawRoots := strings.Split(*roots, ",")
	var abs []string
	for _, r := range rawRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		a, err := filepath.Abs(r)
		if err != nil {
			slog.Error("roots abs", "root", r, "err", err)
			os.Exit(1)
		}
		abs = append(abs, filepath.Clean(a))
	}
	if len(abs) < 1 {
		fmt.Fprintf(os.Stderr, "agent-runner: --roots must contain at least one non-empty path\n")
		os.Exit(1)
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
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
		AllowedRoots:    abs,
		MaxTasks:        *maxTasks,
		Hostname:        hostname,
		ClaudeBin:       *claudeBin,
		ExtraClaudeArgs: strings.Fields(*claudeArgs),
		Logger:          slog.Default(),
	}); err != nil {
		slog.Error("runner exit", "err", err)
		os.Exit(1)
	}
}
