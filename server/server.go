package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/transport"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/objtrsf/objproto"
)

// Config holds the configuration for a Server instance.
type Config struct {
	Addr          string        // host:port for the WebSocket listener; empty disables the WS leg (UDPAddr must then be set)
	UDPAddr       string        // host:port for the UDP listener; empty disables the UDP leg. Combine with Addr for ws+udp dualstack.
	DataDir       string        // reserved for WAL/log persistence (Tasks 2.8 / 2.9 / 2.9b)
	TaskRetention time.Duration // if > 0, terminal tasks older than this are pruned at startup and every hour
	PruneInterval time.Duration // overrides the default 1h prune cadence (only used when TaskRetention > 0)
	Logger        *slog.Logger

	// PSK, when non-nil, requires every connecting client to present
	// a matching PskAuthRequest before any other message is accepted.
	// nil = no PSK enforcement (backward compatible).
	PSK []byte

	// WebUIFS, when non-nil, causes server.Run to register handlers on its
	// internal mux for "/" (serving "<root>/index.html") and "/static/" (serving
	// the directory tree). The fs.FS is expected to have index.html at its
	// root and static/* below. Typically supplied via //go:embed from
	// cmd/harness-server.
	WebUIFS fs.FS

	// WebUINoCache, when true, makes the server send `Cache-Control: no-cache`
	// on every WebUI response (index.html + static/*). Set by the --webui-dir
	// hot-reload path: without it browsers heuristically cache main.js /
	// main.wasm (http.FileServer sets Last-Modified but no Cache-Control) and
	// serve a stale copy that mismatches a freshly-edited index.html — which
	// silently breaks handlers wired to elements that moved/renamed. Leave
	// false for embedded assets (they change only on redeploy + restart).
	WebUINoCache bool

	// DetachRingBufferSize is the byte capacity of the per-session scrollback
	// ring buffer for detachable sessions. 0 means use the TaskHandler default
	// (1 MiB).
	DetachRingBufferSize int64

	// DetachIdleTimeout, when > 0, causes Detached sessions that have been
	// idle for longer than this duration to be automatically cancelled. 0
	// disables idle cancellation (default).
	DetachIdleTimeout time.Duration

	// NotifyHook, when non-empty, is an executable invoked once per notify
	// request: stdin receives a JSON payload, env carries HARNESS_NOTIFY_*.
	// Empty disables the egress leg (notify still records to the ring + topic).
	// Invoked directly (no shell) — text is on stdin, never an argument.
	NotifyHook string
}

// Server wires all components together and manages the main accept loop.
type Server struct {
	cfg           Config
	registry      *Registry
	tasks         *TaskStore
	sessions      *SessionRegistry
	pubsub        *pubsub.PubSub
	scheduler     *Scheduler
	runnerHandler *RunnerHandler
	taskHandler   *TaskHandler
	dispatcher    *Dispatcher

	// Board is the agentboard instance, wired in by the server binary (Task 9).
	// When nil, agent_message payloads are ignored silently.
	Board *agentboard.Board

	agentConnsMu sync.Mutex
	agentConns   map[objproto.ConnectionID]*agentConn

	// activeConnsMu / activeConns track every live wrapped connection so a
	// debug dump (DumpTrsfState, via SIGUSR1) can report each transport's trsf
	// internal state. Debug aid only.
	activeConnsMu sync.Mutex
	activeConns   map[objproto.ConnectionID]streamingConn

	// relayRespChMu / relayRespCh correlate inbound
	// RunnerMessageType_EstablishRelayResponse messages back to the goroutine
	// that sent the original EstablishRelayRequest. Keyed by the proxy_runner's
	// ConnectionID — at individual-dogfood scale we treat this as
	// at-most-one outstanding relay setup per proxy_runner; concurrent admin
	// dial-runner --via against the SAME proxy_runner would race and the
	// later request would replace the earlier waiter (the earlier blocks
	// until timeout).
	//
	// This is the simplest correlation that lines up with the wire schema:
	// EstablishRelayResponse carries no request_id, so we cannot multiplex
	// without extending the protocol.
	relayRespChMu sync.Mutex
	relayRespCh   map[objproto.ConnectionID]chan protocol.EstablishRelayResponse

	// pendingViaInfoMu / pendingViaInfo carry Phase C registration metadata from
	// DialRunnerHandler.HandleWithVia (where the via entry + target addr are in
	// scope) to RunnerHandler.Handle's Hello case (where RunnerEntry is built).
	// Keyed by the end-to-end conn's ConnectionID; consumed (deleted) when the
	// Hello arrives or when the connection closes before Hello.
	pendingViaInfoMu sync.Mutex
	pendingViaInfo   map[objproto.ConnectionID]*ViaRegistrationInfo

	// chainedRelay handles RunnerMessage{RequestChainedRelay} from runners
	// registered via Phase C. Wired in New; referenced by runnerHandler.ChainedRelay.
	chainedRelay *ChainedRelayHandler

	// connWG tracks in-flight handleConnection goroutines so serve() can wait
	// for their deferred trsf.SendClose (~50ms each) to actually leave the
	// wire before main() exits. Without this the Close packet often loses
	// the race against process termination on shutdown, and peers must wait
	// out AutoGarbageCollect's connectionTimeout (~1 min) to notice the death.
	connWG sync.WaitGroup
}

// New constructs a Server with all components wired but NOT yet listening.
// Callers in tests can inspect / inject pieces after construction.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		cfg:            cfg,
		registry:       NewRegistry(),
		tasks:          NewTaskStore(),
		sessions:       NewSessionRegistry(),
		pubsub:         pubsub.NewPubSub(cfg.Logger),
		relayRespCh:    make(map[objproto.ConnectionID]chan protocol.EstablishRelayResponse),
		pendingViaInfo: make(map[objproto.ConnectionID]*ViaRegistrationInfo),
		activeConns:    make(map[objproto.ConnectionID]streamingConn),
	}
	s.scheduler = NewScheduler(s.registry, s.tasks, s.sendAssign)
	s.runnerHandler = &RunnerHandler{
		Registry:                 s.registry,
		Tasks:                    s.tasks,
		Now:                      time.Now,
		OnChange:                 s.scheduler.Tick,
		OnEstablishRelayResponse: s.deliverEstablishRelayResponse,
	}
	s.chainedRelay = NewChainedRelayHandler(cfg.Logger, s.registry, s.sendEstablishRelayRequest)
	s.runnerHandler.ChainedRelay = s.chainedRelay
	logsDir := ""
	if s.cfg.DataDir != "" {
		logsDir = filepath.Join(s.cfg.DataDir, "logs")
	}
	s.taskHandler = &TaskHandler{
		Tasks:          s.tasks,
		Registry:       s.registry,
		Sessions:       s.sessions,
		OnChange:       s.scheduler.Tick,
		LogsDir:        logsDir,
		RingBufferSize: int(cfg.DetachRingBufferSize),
		PruneFn: func(req *protocol.PruneTasksRequest) (removed, skippedActive, skippedMissing int) {
			if req.TaskIdsLen == 0 {
				cutoff := time.Unix(0, int64(req.BeforeTs))
				return s.tasks.PruneTerminal(cutoff, logsDir), 0, 0
			}
			ids := make([]string, 0, req.TaskIdsLen)
			for i := range req.TaskIds {
				ids = append(ids, hex.EncodeToString(req.TaskIds[i].Id[:]))
			}
			return s.tasks.PruneByIDs(ids, req.Force != 0, logsDir)
		},
		// Via-relay hooks for dial-runner --via path. Endpoint + OnDialed are
		// wired later in Run (they need the constructed Endpoint), but these
		// two don't depend on the transport so we set them here.
		ResolveVia:            s.registry.GetByConnectionID,
		ViaSendEstablishRelay: s.sendEstablishRelayRequest,
	}
	// Route runner-reported remote-forward connections into the TaskHandler
	// (wired here, after taskHandler construction, since runnerHandler is built
	// just above it).
	s.runnerHandler.OnRemoteForwardConn = s.taskHandler.handleRemoteForwardConn
	s.runnerHandler.OnRemoteForwardBindResult = s.taskHandler.handleRemoteForwardBindResult
	s.dispatcher = &Dispatcher{
		OnRunnerControl: s.runnerHandler.Handle,
		OnTaskControl:   s.taskHandler.Handle,
		OnAgentMessage:  s.handleAgentMessage,
		Registry:        s.registry,
		Tasks:           s.tasks,
		// Board is wired after construction via Server.SetBoard (Task 9).
	}

	// publishTaskEvent constructs and publishes a TaskStatusEvent to the
	// global tasks.status topic and the per-task task.<id>.status topic.
	// TaskKind is looked up from the TaskStore — it is immutable for a
	// task's lifetime, so emitting it on every event lets a fresh TUI
	// subscriber tell oneshot from interactive without waiting for the
	// next List snapshot.
	publishTaskEvent := func(taskID string, kind protocol.StatusEventKind, status protocol.TaskStatus, exitCode int32) {
		var taskKind protocol.TaskKind
		if t, ok := s.tasks.Get(taskID); ok {
			taskKind = t.Kind
		}
		ev := protocol.TaskStatusEvent{
			Kind:       kind,
			Ts:         uint64(time.Now().UnixNano()),
			TaskStatus: status,
			TaskKind:   taskKind,
			ExitCode:   exitCode,
		}
		raw, err := hex.DecodeString(taskID)
		if err == nil {
			copy(ev.TaskId.Id[:], raw)
		}
		payload := ev.MustAppend(nil)
		s.pubsub.Publish("server", topics.TasksStatus(), payload)
		s.pubsub.Publish("server", topics.TaskStatus(taskID), payload)
	}

	// publishRunnerEvent constructs and publishes a RunnerStatusEvent to
	// the global runners.status topic.
	publishRunnerEvent := func(_ string, kind protocol.StatusEventKind, status protocol.RunnerStatus) {
		ev := protocol.RunnerStatusEvent{
			Kind:         kind,
			Ts:           uint64(time.Now().UnixNano()),
			RunnerStatus: status,
			RunnerId:     placeholderRunnerID(),
		}
		payload := ev.MustAppend(nil)
		s.pubsub.Publish("server", topics.RunnersStatus(), payload)
	}

	// Wire task lifecycle hooks.
	// OnCreate publishes task_queued; Run may wrap this further for log store taps.
	s.tasks.OnCreate = func(id string) {
		publishTaskEvent(id, protocol.StatusEventKind_TaskQueued, protocol.TaskStatus_Queued, 0)
	}
	s.tasks.OnAssign = func(id, runnerID, worktreeDir string) {
		publishTaskEvent(id, protocol.StatusEventKind_TaskAssigned, protocol.TaskStatus_Running, 0)
	}
	s.tasks.OnFinish = func(id string, exit int32, status protocol.TaskStatus) {
		publishTaskEvent(id, protocol.StatusEventKind_TaskEnded, status, exit)
	}
	s.tasks.OnCancel = func(id string) {
		publishTaskEvent(id, protocol.StatusEventKind_TaskEnded, protocol.TaskStatus_Cancelled, 0)
		s.dispatcher.OnCancel(id)
	}

	// Wire registry hooks.
	s.registry.OnAdd = func(entry RunnerEntry) {
		publishRunnerEvent(entry.ID, protocol.StatusEventKind_RunnerRegistered, protocol.RunnerStatus_Idle)
	}
	s.registry.OnRemove = func(id string, snap RunnerEntry) {
		// Mark all tasks that were active on the disconnected runner as Failed
		// before publishing the RunnerOffline event. MarkFailed is idempotent
		// so it is safe if TaskFinished already processed some of them.
		for taskID := range snap.ActiveTasks {
			s.tasks.MarkFailed(taskID, "runner_disconnected")
		}
		publishRunnerEvent(id, protocol.StatusEventKind_RunnerOffline, protocol.RunnerStatus_Offline)
	}

	// Wire TaskStarted hook so the runner_handler can publish the event.
	s.runnerHandler.OnTaskStarted = func(taskID string) {
		publishTaskEvent(taskID, protocol.StatusEventKind_TaskStarted, protocol.TaskStatus_Running, 0)
	}

	return s
}

// Run builds the transport stack from cfg.Addr / cfg.UDPAddr and runs the
// server until ctx is cancelled or a listener errors. Returns the first
// fatal error.
//
// Transport selection:
//   - cfg.Addr only      → single-stack WebSocket on cfg.Addr.
//   - cfg.UDPAddr only   → single-stack UDP on cfg.UDPAddr; webui is not served.
//   - both set           → ws+udp dualstack via UDPWebsocketDualStackEndpoint.
//   - neither set        → error.
//
// If cfg.DataDir is non-empty, Run creates the directory, replays any existing
// WAL into the TaskStore (rebuilding Queued tasks and marking interrupted
// Running tasks as Failed), then opens the WAL for new appends.
// If cfg.DataDir is empty, WAL setup is skipped entirely (safe for tests that
// do not need persistence).
func (s *Server) Run(ctx context.Context) error {
	ep, mux, httpAddr, err := s.buildEndpoint()
	if err != nil {
		return err
	}
	s.taskHandler.Endpoint = ep
	s.taskHandler.OnDialed = func(connCtx context.Context, conn objproto.Connection, viaInfo *ViaRegistrationInfo) {
		// connCtx is the server root context (long-lived). The ECDH-timeout
		// context lives only inside DialRunnerHandler.Handle and is already
		// cancelled by the time OnDialed fires.
		if viaInfo != nil {
			s.pendingViaInfoMu.Lock()
			s.pendingViaInfo[conn.ConnectionID()] = viaInfo
			s.pendingViaInfoMu.Unlock()
		}
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			s.handleConnection(connCtx, conn)
		}()
	}
	s.runnerHandler.TakePendingViaInfo = s.takePendingViaInfo
	return s.serve(ctx, ep, mux, httpAddr)
}

// buildEndpoint picks a transport stack based on cfg.Addr / cfg.UDPAddr.
// Returns the endpoint, the http.ServeMux to mount webui on (nil for
// UDP-only), and the http listen address (empty for UDP-only).
func (s *Server) buildEndpoint() (objproto.Endpoint, *http.ServeMux, string, error) {
	wsAddr := s.cfg.Addr
	udpAddr := s.cfg.UDPAddr

	switch {
	case wsAddr == "" && udpAddr == "":
		return nil, nil, "", fmt.Errorf("server: at least one of Config.Addr or Config.UDPAddr must be set")

	case wsAddr != "" && udpAddr == "":
		mux := http.NewServeMux()
		ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
			Logger: s.cfg.Logger,
			Path:   cli.WebSocketPath,
			Mode:   objproto.EndpointModeMutual,
		})
		if err != nil {
			return nil, nil, "", fmt.Errorf("websocket session: %w", err)
		}
		return ep, mux, wsAddr, nil

	case wsAddr == "" && udpAddr != "":
		port, err := parseListenPort(udpAddr)
		if err != nil {
			return nil, nil, "", fmt.Errorf("server: udp listen %q: %w", udpAddr, err)
		}
		ep, err := transport.UDPEndpoint(s.cfg.Logger, port, objproto.EndpointModeMutual)
		if err != nil {
			return nil, nil, "", fmt.Errorf("udp endpoint: %w", err)
		}
		return ep, nil, "", nil

	default:
		port, err := parseListenPort(udpAddr)
		if err != nil {
			return nil, nil, "", fmt.Errorf("server: udp listen %q: %w", udpAddr, err)
		}
		mux := http.NewServeMux()
		ds, err := transport.UDPWebsocketDualStackEndpoint(transport.UDPWebsocketDualStackConfig{
			Logger:  s.cfg.Logger,
			UDPPort: port,
			Mux:     mux,
			WS: transport.WebSocketConfig{
				Logger: s.cfg.Logger,
				Path:   cli.WebSocketPath,
				Mode:   objproto.EndpointModeMutual,
			},
		})
		if err != nil {
			return nil, nil, "", fmt.Errorf("dualstack endpoint: %w", err)
		}
		return ds.Endpoint, mux, wsAddr, nil
	}
}

// parseListenPort accepts "host:port" or ":port" and returns the port
// number. The host is currently informational; UDPEndpoint listens on
// IPv6 unspecified per transport/udp.go.
func parseListenPort(addr string) (uint16, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("expected host:port (got %q): %w", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("port %q: %w", portStr, err)
	}
	return uint16(port), nil
}

// serve runs the WAL / log-store / sweeper / accept loop against ep.
// Mux + httpAddr drive the webui+HTTP listener; both nil/empty for
// UDP-only setups.
func (s *Server) serve(ctx context.Context, ep objproto.Endpoint, mux *http.ServeMux, httpAddr string) error {
	// Wire the server root context into the task handler so that SessionMux
	// instances created for detachable sessions are cancelled when the server shuts down.
	s.taskHandler.Ctx = ctx

	if s.cfg.DataDir != "" {
		if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}
		walPath := filepath.Join(s.cfg.DataDir, "events.log")
		// Replay WAL if present. A corrupted WAL is logged but does not prevent
		// server startup — an empty store is recoverable.
		events, rerr := ReadWAL(walPath)
		if rerr != nil {
			s.cfg.Logger.Error("WAL replay failed", "path", walPath, "err", rerr)
		} else if events != nil {
			s.tasks.ReplayEvents(events)
		}
		// Detached survivors cannot be restored: SessionMux state was in-memory.
		// Per spec §9, mark them Cancelled.
		for _, t := range s.tasks.List(0) {
			if t.Status == protocol.TaskStatus_Detached {
				s.tasks.Cancel(t.ID)
			}
		}
		wal, err := OpenWAL(walPath)
		if err != nil {
			return fmt.Errorf("open WAL: %w", err)
		}
		defer wal.Close()
		s.tasks.SetWAL(wal)

		logStore, err := NewLogStore(filepath.Join(s.cfg.DataDir, "logs"))
		if err != nil {
			return fmt.Errorf("open log store: %w", err)
		}
		defer logStore.Close()

		// Chain log store tap into the existing OnCreate hook (which publishes task_queued).
		existingOnCreate := s.tasks.OnCreate
		s.tasks.OnCreate = func(taskID string) {
			if existingOnCreate != nil {
				existingOnCreate(taskID)
			}
			// Register log store tap for this task.
			topic := topics.TaskLog(taskID)
			s.pubsub.TapSubscribe(topic, func(_ string, msg []byte) {
				if err := logStore.Append(taskID, msg); err != nil {
					s.cfg.Logger.Error("logstore append", "task", taskID, "err", err)
				}
			})
		}

		// Register taps for tasks that survived replay and may still emit logs.
		for _, t := range s.tasks.List(0) {
			if t.Status == protocol.TaskStatus_Queued || t.Status == protocol.TaskStatus_Running {
				taskID := t.ID
				topic := topics.TaskLog(taskID)
				s.pubsub.TapSubscribe(topic, func(_ string, msg []byte) {
					logStore.Append(taskID, msg) //nolint:errcheck
				})
			}
		}

		// Auto-prune terminal tasks older than TaskRetention. Skipped when retention is 0.
		if s.cfg.TaskRetention > 0 {
			interval := s.cfg.PruneInterval
			if interval <= 0 {
				interval = time.Hour
			}
			logsDir := filepath.Join(s.cfg.DataDir, "logs")
			runPrune := func() {
				cutoff := time.Now().Add(-s.cfg.TaskRetention)
				if n := s.tasks.PruneTerminal(cutoff, logsDir); n > 0 {
					s.cfg.Logger.Info("auto-prune", "removed", n, "cutoff", cutoff)
				}
			}
			runPrune() // startup pass
			go func() {
				t := time.NewTicker(interval)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						runPrune()
					}
				}
			}()
		}
	}

	// Start idle-detach sweeper when a timeout is configured.
	if s.cfg.DetachIdleTimeout > 0 {
		go s.runDetachIdleSweeper(ctx)
	}

	const shutdownGracePeriod = 2 * time.Second

	// Mount webui handlers when the caller supplied a mux and an embed FS
	// is configured. UDP-only callers skip both.
	if mux != nil && s.cfg.WebUIFS != nil {
		if _, err := fs.ReadFile(s.cfg.WebUIFS, "index.html"); err != nil {
			return fmt.Errorf("webui: index.html not in WebUIFS: %w", err)
		}
		if _, err := fs.Stat(s.cfg.WebUIFS, "static/main.wasm"); err != nil {
			return fmt.Errorf("webui: static/main.wasm missing (did you forget `make webui-build`?): %w", err)
		}
		// noCache stamps Cache-Control on responses when hot-reload mode is on,
		// so the browser always picks up freshly-rebuilt assets instead of a
		// heuristically-cached stale copy. No-op for embedded assets.
		noCache := func(w http.ResponseWriter) {
			if s.cfg.WebUINoCache {
				w.Header().Set("Cache-Control", "no-cache")
			}
		}
		// index.html is read from WebUIFS per request (not cached at startup)
		// so that --webui-dir / HARNESS_WEBUI_DIR hot-reload also covers index
		// edits. With the embedded FS this is an in-memory byte copy — cheap,
		// and "/" is hit only on page load.
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			indexBytes, err := fs.ReadFile(s.cfg.WebUIFS, "index.html")
			if err != nil {
				http.Error(w, "index.html unavailable", http.StatusInternalServerError)
				return
			}
			noCache(w)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(indexBytes)
		})
		staticFS, err := fs.Sub(s.cfg.WebUIFS, "static")
		if err != nil {
			return fmt.Errorf("webui: fs.Sub(static): %w", err)
		}
		staticServer := http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))
		mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			noCache(w)
			staticServer.ServeHTTP(w, r)
		}))
	}

	// Spin the HTTP server only when both mux and httpAddr are present.
	// UDP-only servers skip this entirely; the connection-accept loop
	// below still runs against ep.
	var (
		httpServer *http.Server
		serverDone chan error
	)
	if mux != nil && httpAddr != "" {
		httpServer = &http.Server{Addr: httpAddr, Handler: mux}
		serverDone = make(chan error, 1)
		go func() {
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverDone <- err
				return
			}
			serverDone <- nil
		}()
	}

	shutdownHTTP := func() {
		if httpServer == nil {
			return
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		_ = httpServer.Shutdown(shutdownCtx)
		shutdownCancel()
		<-serverDone
	}

	// waitConns blocks (with a bound) for in-flight handleConnection goroutines
	// to finish their deferred trsf.SendClose+50ms-drain+Close so peers learn
	// of our exit promptly instead of waiting out AutoGarbageCollect.
	waitConns := func() {
		done := make(chan struct{})
		go func() {
			s.connWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownGracePeriod):
			s.cfg.Logger.Warn("server: connection drain timed out; some peers may need to wait for AutoGarbageCollect",
				"after", shutdownGracePeriod)
		}
	}

	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)
	ch := ep.GetNewActiveConnectionChannel()
	for {
		select {
		case <-ctx.Done():
			shutdownHTTP()
			waitConns()
			return ctx.Err()
		case serveErr := <-serverDone:
			if serveErr != nil {
				return fmt.Errorf("http server: %w", serveErr)
			}
			waitConns()
			return nil
		case session, ok := <-ch:
			if !ok {
				shutdownHTTP()
				waitConns()
				return nil
			}
			s.connWG.Add(1)
			go func() {
				defer s.connWG.Done()
				s.handleConnection(ctx, session)
			}()
		}
	}
}

// streamingConn wraps an objproto.Connection together with the trsf transport
// so handlers can both reply with single messages and create server-initiated
// streams for bulk responses (GetTaskLog, future BulkList, etc.).
type streamingConn struct {
	objproto.Connection
	trans trsf.Transport
}

// DumpTrsfState logs each active connection's trsf internal state (debug aid;
// wired to SIGUSR1 on Unix). role=runner/client makes a stuck remote-forward
// relay visible — e.g. a runner conn whose recvStreams aren't draining.
func (s *Server) DumpTrsfState() {
	s.activeConnsMu.Lock()
	conns := make([]streamingConn, 0, len(s.activeConns))
	for _, c := range s.activeConns {
		conns = append(conns, c)
	}
	s.activeConnsMu.Unlock()

	log := s.cfg.Logger
	log.Info("trsf dump: begin", "conns", len(conns))
	for _, c := range conns {
		cid := c.ConnectionID()
		role := "client"
		if _, ok := s.registry.GetByConnectionID(cid); ok {
			role = "runner"
		}
		st := c.trans.GetInternalState()
		if st == nil {
			log.Info("trsf dump: conn", "cid", cid.String(), "role", role, "state", "nil")
			continue
		}
		log.Info("trsf dump: conn",
			"cid", cid.String(), "role", role,
			"sendStreams", st.ActiveSendStreams, "recvStreams", st.ActiveReceiveStreams,
			"sendQ", st.SendQueueLength, "recvQ", st.ReceiveQueueLength,
			"sendTrig", st.SendActionCount, "updWin", st.UpdateWindowCount, "cancel", st.CancelStreamCount,
			"inflight", st.BytesInFlight, "cwnd", st.CongestionWindow, "rtt", st.SmoothedRTT, "sentPkts", len(st.SentPackets))
	}
	if s.taskHandler != nil {
		for _, rf := range s.taskHandler.rforwards().snapshot() {
			log.Info("trsf dump: remote-forward", "fwd", rf.forwardID, "task", rf.taskIDHex, "runner", rf.runnerID, "client_cid", rf.clientCID)
		}
	}
	log.Info("trsf dump: end")
}

func (s streamingConn) CreateSendStream() trsf.SendStream { return s.trans.CreateSendStream() }

func (s streamingConn) CreateBidirectionalStream() trsf.BidirectionalStream {
	return s.trans.CreateBidirectionalStream()
}

func (s streamingConn) GetReceiveStream(id trsf.StreamID) trsf.ReceiveStream {
	return s.trans.GetReceiveStream(id)
}

func (s streamingConn) GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream {
	return s.trans.GetBidirectionalStream(id)
}

// handleConnection manages a single active objproto connection for its lifetime.
func (s *Server) handleConnection(ctx context.Context, session objproto.Connection) {
	// Defer close announcement so peers (runners / clients) see an explicit
	// trsf Close instead of waiting for ping-timeout (~15s) on server
	// shutdown / connection teardown. Symmetric with peer.Conn.Close on the
	// runner side; the 50ms drain matches project_peer_close_send_drain —
	// without scheduling slack the async pktQueue may exit before the Close
	// packet leaves the wire.
	defer func() {
		_ = trsf.SendClose(session)
		time.Sleep(50 * time.Millisecond)
		_ = session.Close()
	}()
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Bridge objproto.Connection lifetime into connCtx so trsf.Streams
	// (and the AutoSend / AutoReceive loops) cancel the moment the
	// connection dies, instead of waiting for AutoReceive's natural
	// return. Without this, blocked recvStream.Read on any sub-stream
	// would only unblock once AutoReceive itself has exited.
	go func() {
		select {
		case <-session.Done():
		case <-connCtx.Done():
		}
		cancel()
	}()
	p := trsf.NewStreams(connCtx, true, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, session, s.cfg.Logger)
	subscriber := pubsub.NewSubscriber(session.ConnectionID(), p)
	defer subscriber.LeaveAll(s.pubsub)

	go trsf.AutoSend(connCtx, p, session, nil)

	wrapped := streamingConn{Connection: session, trans: p}
	s.activeConnsMu.Lock()
	s.activeConns[session.ConnectionID()] = wrapped
	s.activeConnsMu.Unlock()
	defer func() {
		s.activeConnsMu.Lock()
		delete(s.activeConns, session.ConnectionID())
		s.activeConnsMu.Unlock()
	}()

	gate := newPSKGate(s.cfg.PSK)

	trsf.AutoReceive(connCtx, p, session, func(msg *objproto.Message, err error) {
		if err != nil {
			// Includes io.EOF on peer-sent Close; AutoReceive returns next.
			s.cfg.Logger.Info("server: AutoReceive callback err", "cid", session.ConnectionID().String(), "err", err)
			return
		}
		if msg == nil || len(msg.Data) == 0 {
			return
		}
		// PSK gate: first message must be PskAuth when PSK is configured.
		if isPSKMsg, shouldClose := gate.Check(msg.Data, session.GetTranscript(), func(resp []byte) {
			session.SendMessage(resp) //nolint:errcheck
		}); isPSKMsg || !gate.Authed() {
			if shouldClose {
				trsf.SendClose(session) //nolint:errcheck
				cancel()
			}
			return
		}
		kind := appwire.AppKind(msg.Data[0])
		if kind == appwire.AppKind_PskAuth {
			return // stray PskAuth after auth complete — discard
		}
		if kind == appwire.AppKind_Pubsub {
			// HandleMessage already returns the response wire-kind prefixed.
			if resp := subscriber.HandleMessage(s.pubsub, msg.Data[1:]); resp != nil {
				session.SendMessage(resp) //nolint:errcheck
			}
			return
		}
		s.dispatcher.Dispatch(wrapped, msg.Data)
	})

	// Connection closed: clean up agent state, deregister runner, and trigger rescheduling.
	cid := session.ConnectionID().String()
	s.cfg.Logger.Info("server: connection closed, deregistering", "cid", cid)
	// Clean up any pending via-info that was stashed at OnDialed time but not yet
	// consumed by the Hello handler (connection closed before Hello arrived).
	s.takePendingViaInfo(session.ConnectionID())
	s.removeAgentConn(session.ConnectionID())
	s.registry.Remove(cid)
	s.scheduler.Tick()
}

// RegisteredRunners returns a snapshot of currently registered runners.
// Test-only accessor; production code reads via the publishRunnerEvent
// hooks. Delegates to Registry.List, which already returns value snapshots
// independent of the internal map.
func (s *Server) RegisteredRunners() []RunnerEntry {
	return s.registry.List()
}

// SetBoard wires an agentboard.Board into all handlers that participate in the
// ticket lifecycle (Dispatcher, TaskHandler, RunnerHandler). Call this after
// New and before Run. Task 9 (cmd/harness-server/main.go) is responsible for
// constructing the Board and calling this method; tests that exercise the ticket
// flow construct a Board and set it directly on the individual handler structs.
func (s *Server) SetBoard(b *agentboard.Board) {
	s.Board = b
	s.dispatcher.Board = b
	s.taskHandler.Board = b
	s.runnerHandler.Board = b
	s.wireAgentBoardWake(b)
}

// sendAssign sends an AssignTask runner-control message to the runner identified by runnerID.
// It is used as the AssignFunc supplied to the Scheduler. A fresh agentboard
// auth ticket is generated and registered before send so that the spawned
// claude can authenticate its agent_message Hello against the same Board.
func (s *Server) sendAssign(runnerID, taskID string) error {
	entry, ok := s.registry.Get(runnerID)
	if !ok || entry.Conn == nil {
		return fmt.Errorf("runner %s not connected", runnerID)
	}
	task, ok := s.tasks.Get(taskID)
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	var ticket [16]byte
	if _, err := rand.Read(ticket[:]); err != nil {
		return fmt.Errorf("ticket gen: %w", err)
	}
	if s.Board != nil {
		s.Board.Registry().Register(runnerIDFromConnID(runnerID), taskIDFromHex(taskID), ticket)
	}
	stream := entry.Conn.CreateSendStream()
	if stream == nil {
		return fmt.Errorf("CreateSendStream returned nil")
	}
	envelope, body, err := buildAssignMsg(task, ticket, uint64(stream.ID()))
	if err != nil {
		return fmt.Errorf("buildAssignMsg: %w", err)
	}
	if werr := stream.AppendData(false, body); werr != nil {
		return fmt.Errorf("stream body write: %w", werr)
	}
	if werr := stream.AppendData(true); werr != nil {
		return fmt.Errorf("stream EOF: %w", werr)
	}
	if _, _, err := entry.Conn.SendMessage(envelope); err != nil {
		return err
	}
	return nil
}

// takePendingViaInfo removes and returns the ViaRegistrationInfo stashed for the
// given conn CID. Returns nil if none was stashed (Phase A direct or reverse-dial
// registrations). Called by RunnerHandler's Hello case to populate Via + ViaDialAddr,
// and from handleConnection's defer to clean up any info that was not consumed
// (conn closed before Hello arrived).
func (s *Server) takePendingViaInfo(cid objproto.ConnectionID) *ViaRegistrationInfo {
	s.pendingViaInfoMu.Lock()
	defer s.pendingViaInfoMu.Unlock()
	info := s.pendingViaInfo[cid]
	delete(s.pendingViaInfo, cid)
	return info
}

// deliverEstablishRelayResponse routes an inbound EstablishRelayResponse to
// the goroutine that sent the matching request. Called from
// RunnerHandler.Handle (case EstablishRelayResponse) on the runner's
// registered ConnHandle. Drops the message silently if no waiter is
// registered (e.g. the request timed out before the response arrived, or
// the runner sent an unsolicited reply).
func (s *Server) deliverEstablishRelayResponse(conn ConnHandle, resp protocol.EstablishRelayResponse) {
	cid := conn.ConnectionID()
	s.relayRespChMu.Lock()
	ch, ok := s.relayRespCh[cid]
	s.relayRespChMu.Unlock()
	if !ok {
		s.cfg.Logger.Warn("server: EstablishRelayResponse without waiter",
			"runner", cid.String(), "status", resp.Status)
		return
	}
	select {
	case ch <- resp:
	default:
		// Buffer is size 1; full means the waiter already consumed and we
		// somehow saw a duplicate, OR the channel was closed by a replacement
		// (already drained by the close-path). Either way: log + drop.
		s.cfg.Logger.Warn("server: dropped EstablishRelayResponse (channel full or closed)",
			"runner", cid.String(), "status", resp.Status)
	}
}

// sendEstablishRelayRequest is the ViaSendEstablishRelay hook wired into
// TaskHandler / DialRunnerHandler. It sends an EstablishRelayRequest over
// entry.Conn (the proxy_runner's already-registered ConnHandle) and waits
// for the matching EstablishRelayResponse, routed to the per-entry response
// channel by RunnerHandler.Handle (case EstablishRelayResponse).
//
// Concurrency note: only one outstanding EstablishRelay per proxy_runner is
// supported (see relayRespCh field comment). A second concurrent dial-runner
// --via against the same proxy_runner will overwrite the first waiter; the
// first ends up blocking until ctx times out. Acceptable for individual
// dogfood; revisit if the protocol grows a request_id.
func (s *Server) sendEstablishRelayRequest(ctx context.Context, entry *RunnerEntry, req protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
	if entry == nil || entry.Conn == nil {
		return protocol.EstablishRelayResponse{}, fmt.Errorf("nil entry / Conn")
	}
	connCID := entry.Conn.ConnectionID()

	// Register the per-conn response channel BEFORE sending so we cannot miss
	// a fast reply that arrives between SendMessage return and the select below.
	respCh := make(chan protocol.EstablishRelayResponse, 1)
	s.relayRespChMu.Lock()
	prev, hadPrev := s.relayRespCh[connCID]
	s.relayRespCh[connCID] = respCh
	s.relayRespChMu.Unlock()
	if hadPrev {
		// Earlier waiter is shadowed; signal it via close so its select unblocks
		// (it'll see the zero-value status and surface as ViaRelayFailed).
		// This is a defensive path — at dogfood scale we expect no overlap.
		close(prev)
		s.cfg.Logger.Warn("server: replacing in-flight relay waiter (concurrent dial-runner --via)",
			"via", connCID.String())
	}
	defer func() {
		s.relayRespChMu.Lock()
		// Only delete if our own channel is still the registered one; a later
		// call may have already replaced it.
		if cur, ok := s.relayRespCh[connCID]; ok && cur == respCh {
			delete(s.relayRespCh, connCID)
		}
		s.relayRespChMu.Unlock()
	}()

	var rr protocol.RunnerRequest
	rr.Kind = protocol.RunnerRequestType_EstablishRelay
	rr.SetEstablishRelay(req)
	payload, err := rr.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		return protocol.EstablishRelayResponse{}, fmt.Errorf("encode EstablishRelayRequest: %w", err)
	}
	if _, _, err := entry.Conn.SendMessage(payload); err != nil {
		return protocol.EstablishRelayResponse{}, fmt.Errorf("send EstablishRelayRequest: %w", err)
	}

	select {
	case <-ctx.Done():
		return protocol.EstablishRelayResponse{}, ctx.Err()
	case resp, ok := <-respCh:
		if !ok {
			// Channel was closed by a later replacement.
			return protocol.EstablishRelayResponse{}, fmt.Errorf("relay waiter superseded")
		}
		return resp, nil
	}
}

// runDetachIdleSweeper cancels any session that has been Detached longer than
// DetachIdleTimeout. Runs until ctx is canceled. The sweep interval is set
// to a fraction of the timeout, with a sensible floor.
func (s *Server) runDetachIdleSweeper(ctx context.Context) {
	interval := s.cfg.DetachIdleTimeout / 4
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sweepIdleDetached(now)
		}
	}
}

// sweepIdleDetached cancels Detached tasks whose DetachedAt timestamp is older
// than cfg.DetachIdleTimeout relative to now.
func (s *Server) sweepIdleDetached(now time.Time) {
	cutoff := uint64(now.Add(-s.cfg.DetachIdleTimeout).UnixNano())
	for _, info := range s.tasks.List(0) {
		if info.Status != protocol.TaskStatus_Detached {
			continue
		}
		if info.DetachedAt > 0 && info.DetachedAt < cutoff {
			if mux := s.sessions.Get(info.ID); mux != nil {
				mux.Stop()
			}
			s.tasks.Cancel(info.ID)
		}
	}
}
