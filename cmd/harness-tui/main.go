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
	logFile    = flag.String("log-file", "", "if set, mirror slog output to this file (in addition to the in-screen [log] tail)")
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
	// panel with a dim "[log]" prefix. When --log-file is given, every
	// record is also written to that file for offline tailing.
	slogHandler := tui.NewSlogTailHandler(slog.LevelInfo)
	slog.SetDefault(slog.New(slogHandler))
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open log file:", err)
			os.Exit(1)
		}
		defer f.Close()
		slogHandler.SetMirror(f)
	}

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
		conn, p, err := tui.Connect(ctx, *serverAddr)
		if err != nil {
			program.Send(tui.ConnectionMsg{Connected: false, Err: err})
			return
		}
		app.BindTransport(conn, p)
		program.Send(tui.ConnectionMsg{Connected: true})
		program.Send(tui.RefreshSnapshot(*serverAddr)())
		go tui.SubscribeTaskStatus(ctx, conn, p, program)
		go tui.SubscribeRunnerStatus(ctx, conn, p, program)
		<-ctx.Done()
	}()

	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	time.Sleep(50 * time.Millisecond) // brief drain for goroutines
}
