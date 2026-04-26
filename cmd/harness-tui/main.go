package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/tui"
)

var (
	serverCID = flag.String("server-cid", "ws:127.0.0.1:8539-*", "harness-server ConnectionID (e.g. ws:host:port-id, * for random)")
	repoFlag  = flag.String("repo", ".", "default repo path for submit popup")
	wsPath    = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
)

func main() {
	flag.Parse()
	cli.WebSocketPath = *wsPath
	repoAbs, err := filepath.Abs(*repoFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo:", err)
		os.Exit(1)
	}
	peerCID, err := objproto.ParseConnectionID(*serverCID,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "server-cid:", err)
		os.Exit(1)
	}

	// Route slog away from stderr — the bubbletea alt screen shares the
	// terminal, so any direct stderr write scribbles over the TUI.
	// SlogTailHandler buffers records until BindProgram is called, then
	// dispatches each as a LogTailMsg that app.go renders into the cmdresult
	// panel with a dim "[log]" prefix.
	slogHandler := tui.NewSlogTailHandler(slog.LevelInfo)
	slog.SetDefault(slog.New(slogHandler))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	app := tui.New(tui.Config{
		Server:      *serverCID,
		DefaultRepo: repoAbs,
	})
	program := tea.NewProgram(app, tea.WithAltScreen())
	app.BindProgram(program)
	app.BindContext(ctx)
	slogHandler.BindProgram(program)

	go func() {
		c, err := cli.Dial(ctx, peerCID)
		if err != nil {
			program.Send(tui.ConnectionMsg{Connected: false, Err: err})
			return
		}
		if err := c.SayHello(ctx, protocol.ClientKind_Tui); err != nil {
			c.Close()
			program.Send(tui.ConnectionMsg{Connected: false, Err: err})
			return
		}
		app.BindClient(c)
		program.Send(tui.ConnectionMsg{Connected: true})
		program.Send(tui.RefreshSnapshot(c)())
		go tui.SubscribeTaskStatus(ctx, c, program)
		go tui.SubscribeRunnerStatus(ctx, c, program)
		<-ctx.Done()
		c.Close()
	}()

	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	time.Sleep(50 * time.Millisecond) // brief drain for goroutines
}
