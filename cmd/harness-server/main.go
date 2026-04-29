package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/agent-harness/webui"
)

var (
	listen                = flag.String("listen", "127.0.0.1:8539", "listen host:port (use :8539 to dual-stack on all interfaces; loopback by default)")
	dataDir               = flag.String("data-dir", "./harness-data", "persistent data dir")
	taskRetain            = flag.Duration("task-retain", 0, "auto-prune terminal tasks older than this (0 = keep forever)")
	wsPath                = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
	agentboardRing        = flag.Int("agentboard-ring", 64, "agentboard ring buffer entries per topic")
	agentboardTTL         = flag.Duration("agentboard-ttl", 30*time.Minute, "agentboard topic TTL after last publish")
	agentboardMaxTopics   = flag.Int("agentboard-max-topics", 1024, "agentboard max active topics")
	agentboardMaxPayload  = flag.Int("agentboard-max-payload", 64*1024, "agentboard max payload bytes per message")
)

func main() {
	flag.Parse()
	cli.WebSocketPath = *wsPath
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	s := server.New(server.Config{
		Addr:          *listen,
		DataDir:       *dataDir,
		TaskRetention: *taskRetain,
		Logger:        slog.Default(),
		WebUIFS:       webui.FS,
	})
	board := agentboard.New(agentboard.Config{
		RingN:      *agentboardRing,
		TopicTTL:   *agentboardTTL,
		MaxTopics:  *agentboardMaxTopics,
		MaxPayload: *agentboardMaxPayload,
	})
	defer board.Close()
	s.SetBoard(board)
	if err := s.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
