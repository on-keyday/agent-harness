package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/tui"
)

var (
	serverCID = flag.String("server-cid", "ws:127.0.0.1:8539-*", "harness-server ConnectionID (e.g. ws:host:port-id, * for random)")
	repoFlag  = flag.String("repo", "", "default repo path for submit popup; must match a runner-registered RepoPath verbatim (no client-side normalization, since runner may be on a different OS)")
	wsPath    = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")

	persist       = flag.Bool("persist", true, "auto-reconnect on disconnect (set --no-persist to disable)")
	noPersist     = flag.Bool("no-persist", false, "shortcut for --persist=false")
	reconnectInit = flag.Duration("reconnect-initial", 500*time.Millisecond, "first backoff after disconnect")
	reconnectMax  = flag.Duration("reconnect-max", 30*time.Second, "backoff cap")
)

func main() {
	flag.Parse()
	cli.WebSocketPath = *wsPath
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
		DefaultRepo: *repoFlag,
	})
	program := tea.NewProgram(app, tea.WithAltScreen())
	app.BindProgram(program)
	app.BindContext(ctx)
	slogHandler.BindProgram(program)

	go func() {
		enabled := *persist && !*noPersist
		err := cli.PersistLoop(ctx,
			func(dialCtx context.Context) (cli.PersistHandle, error) {
				c, err := cli.Dial(dialCtx, peerCID, protocol.ClientKind_Tui)
				if err != nil {
					return nil, err
				}
				return cli.NewClientHandle(c), nil
			},
			func(runCtx context.Context, h cli.PersistHandle) error {
				handle := h.(*cli.ClientHandle)
				program.Send(tui.BindClientMsg{Client: handle.C})
				program.Send(tui.RefreshSnapshot(handle.C)())
				go tui.SubscribeTaskStatus(runCtx, handle.C, program)
				go tui.SubscribeRunnerStatus(runCtx, handle.C, program)
				go tui.SubscribeNotifications(runCtx, handle.C, program)
				if id := app.FollowingTaskID(); id != "" {
					go tui.SubscribeTaskLog(runCtx, handle.C, program, id)
				}
				<-runCtx.Done()
				return nil
			},
			cli.PersistConfig{
				Enabled:        enabled,
				InitialBackoff: *reconnectInit,
				MaxBackoff:     *reconnectMax,
				OnState: func(s cli.PersistState) {
					program.Send(tui.ConnectionMsg{
						Connected:    s.Phase == cli.PersistPhaseConnected,
						Reconnecting: s.Phase == cli.PersistPhaseReconnecting,
						Attempt:      s.Attempt,
						NextRetry:    s.NextRetry,
						Err:          s.LastError,
					})
				},
			})
		if err != nil {
			program.Send(tui.ConnectionMsg{Connected: false, Err: err})
		}
	}()

	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	time.Sleep(50 * time.Millisecond) // brief drain for goroutines
}

