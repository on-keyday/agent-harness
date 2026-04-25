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
	port       = flag.String("port", "8539", "listen port")
	dataDir    = flag.String("data-dir", "./harness-data", "persistent data dir")
	taskRetain = flag.Duration("task-retain", 0, "auto-prune terminal tasks older than this (0 = keep forever)")
)

func main() {
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	s := server.New(server.Config{
		Addr:          "localhost:" + *port,
		DataDir:       *dataDir,
		TaskRetention: *taskRetain,
		Logger:        slog.Default(),
	})
	if err := s.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
