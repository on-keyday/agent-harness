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
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/tui"
	"github.com/on-keyday/objtrsf/objproto"
)

// The connection flags default to EMPTY rather than to their built-in values.
// A flag.String default is indistinguishable from an operator typing that same
// value, so a baked-in default would always win over the workspace config and
// `--workspace default` could never supply a server-cid. The built-in values
// are applied after resolution instead (see defaultServerCID / defaultWSPath).
const (
	defaultServerCID = "ws:127.0.0.1:8539-*"
	defaultWSPath    = "/ws"
)

var (
	serverCID  = flag.String("server-cid", "", "harness-server ConnectionID (e.g. ws:host:port-id, * for random; env HARNESS_SERVER_CID; default "+defaultServerCID+")")
	repoFlag   = flag.String("repo", "", "default repo path for submit popup; must match a runner-registered RepoPath verbatim (no client-side normalization, since runner may be on a different OS)")
	wsPath     = flag.String("ws-path", "", "WebSocket URL path (env HARNESS_WS_PATH; default "+defaultWSPath+")")
	configPath = flag.String("config", "", "workspace config file (env HARNESS_CONFIG; default ./.harness/config)")
	wsName     = flag.String("workspace", "", "workspace to apply on start and on every reconnect (see `workspace ls`)")

	persist       = flag.Bool("persist", true, "auto-reconnect on disconnect (set --no-persist to disable)")
	noPersist     = flag.Bool("no-persist", false, "shortcut for --persist=false")
	reconnectInit = flag.Duration("reconnect-initial", 500*time.Millisecond, "first backoff after disconnect")
	reconnectMax  = flag.Duration("reconnect-max", 30*time.Second, "backoff cap")
)

func main() {
	flag.Parse()

	// A config that does not parse exits HERE, before bubbletea takes the alt
	// screen — anything written after that is painted over.
	wsFile, wsFilePath, err := workspace.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	var ws *workspace.Workspace
	if *wsName != "" {
		w, ok := wsFile.Workspace(*wsName)
		if !ok {
			fmt.Fprintf(os.Stderr, "config: no workspace named %q in %s\n", *wsName, wsFilePath)
			os.Exit(2)
		}
		if err := w.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(2)
		}
		ws = w
	}
	var wsServerCID, wsWSPath, wsRepo string
	if ws != nil {
		wsServerCID, wsWSPath, wsRepo = ws.ServerCID, ws.WSPath, ws.Repo
	}

	cli.WebSocketPath = cliopts.ResolveStringWith(*wsPath, "HARNESS_WS_PATH", wsWSPath)
	if cli.WebSocketPath == "" {
		cli.WebSocketPath = defaultWSPath
	}
	resolvedCID := cliopts.ResolveStringWith(*serverCID, "HARNESS_SERVER_CID", wsServerCID)
	if resolvedCID == "" {
		resolvedCID = defaultServerCID
	}
	resolvedRepo := cliopts.ResolveStringWith(*repoFlag, "HARNESS_REPO_PATH", wsRepo)

	peerCID, err := objproto.ParseConnectionID(resolvedCID,
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
		Server:        resolvedCID,
		DefaultRepo:   resolvedRepo,
		WorkspaceFile: wsFile,
		WorkspacePath: wsFilePath,
		WorkspaceName: *wsName,
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
				// No eager snapshot here: the tasks.status subscription's
				// SubscribedMsg triggers it AFTER the join completes, so no
				// event can fall between the snapshot and the subscription.
				go tui.SubscribeTaskStatus(runCtx, handle.C, program)
				go tui.SubscribeRunnerStatus(runCtx, handle.C, program)
				go tui.SubscribeNotifications(runCtx, handle.C, program)
				go tui.SubscribeConnStatus(runCtx, handle.C, program)
				// Task log subscription is NOT re-issued here: the App owns
				// it exclusively via followTask, which its own BindClientMsg
				// handler re-triggers (see program.Send above) once a.client
				// is updated. Two subscriptions to the same topic (this
				// goroutine's and the App's) folded every chunk into the
				// pane twice.
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
