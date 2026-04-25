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
	"github.com/on-keyday/agent-harness/tui"
)

var (
	serverAddr = flag.String("server", "localhost:8539", "harness-server host:port")
	repoFlag   = flag.String("repo", ".", "default repo path for submit popup")
)

func main() {
	flag.Parse()
	repoAbs, err := filepath.Abs(*repoFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo:", err)
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
		Server:      *serverAddr,
		DefaultRepo: repoAbs,
	})
	program := tea.NewProgram(app, tea.WithAltScreen())
	app.BindProgram(program)
	app.BindContext(ctx)
	slogHandler.BindProgram(program)

	go func() {
		conn, p, pubClient, err := tui.Connect(ctx, *serverAddr)
		if err != nil {
			program.Send(tui.ConnectionMsg{Connected: false, Err: err})
			return
		}
		app.BindTransport(conn, p, pubClient)
		program.Send(tui.ConnectionMsg{Connected: true})
		program.Send(tui.RefreshSnapshot(*serverAddr)())
		go tui.SubscribeTaskStatus(ctx, conn, p, pubClient, program)
		go tui.SubscribeRunnerStatus(ctx, conn, p, pubClient, program)
		<-ctx.Done()
	}()

	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	time.Sleep(50 * time.Millisecond) // brief drain for goroutines
}
