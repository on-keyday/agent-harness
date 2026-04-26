package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"

	"github.com/on-keyday/agent-harness/server"
)

var (
	listen     = flag.String("listen", "127.0.0.1:8539", "listen host:port (use :8539 to dual-stack on all interfaces; loopback by default)")
	dataDir    = flag.String("data-dir", "./harness-data", "persistent data dir")
	taskRetain = flag.Duration("task-retain", 0, "auto-prune terminal tasks older than this (0 = keep forever)")
)

func main() {
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	s := server.New(server.Config{
		Addr:          *listen,
		DataDir:       *dataDir,
		TaskRetention: *taskRetain,
		Logger:        slog.Default(),
	})
	if err := s.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
