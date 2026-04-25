package main

import (
	"context"
	"flag"
	"fmt"
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	app := tui.New(tui.Config{
		Server:      *serverAddr,
		DefaultRepo: repoAbs,
	})
	program := tea.NewProgram(app, tea.WithAltScreen())
	app.BindProgram(program)
	app.BindContext(ctx)

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
