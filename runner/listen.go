package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/transport"
	"github.com/on-keyday/objtrsf/objproto"
)

// ListenConfig extends Config with listen-side fields. WSListen / UDPListen
// follow the same convention as cmd/harness-server/main.go: either may be
// empty; at least one must be non-empty.
type ListenConfig struct {
	Config

	// WSListen is the WebSocket listen host:port (e.g. "0.0.0.0:8540").
	// Empty disables the WS leg.
	WSListen string

	// UDPListen is the UDP listen host:port (e.g. "0.0.0.0:8541").
	// Empty disables the UDP leg. Combine with WSListen for dualstack.
	UDPListen string

	// WSPath overrides the default WS mount path; empty → cli.WebSocketPath
	// ("/ws"), which matches what cli.BuildClientEndpoint / peer.Dial
	// expect on the client side.
	WSPath string
}

// ListenAndServe builds a Mutual endpoint, accepts incoming peer dials, and
// drives each one through the same PSK + Hello + dispatch lifecycle that
// runner.Connect uses for outbound dials. Returns when ctx is cancelled or
// a fatal listen error occurs.
func ListenAndServe(ctx context.Context, cfg ListenConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.WSListen == "" && cfg.UDPListen == "" {
		return fmt.Errorf("ListenAndServe: at least one of WSListen / UDPListen is required")
	}

	wsPath := cfg.WSPath
	if wsPath == "" {
		wsPath = cli.WebSocketPath
	}

	ep, httpServer, httpServerDone, err := buildListenEndpoint(cfg, wsPath)
	if err != nil {
		return err
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	// Mirror server.go: shut the HTTP server down with a short grace
	// period when ctx ends so the listening port is released promptly
	// (otherwise the goroutine outlives ListenAndServe and the next
	// caller — including tests — hits EADDRINUSE).
	const shutdownGracePeriod = 2 * time.Second
	shutdownHTTP := func() {
		if httpServer == nil {
			return
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		_ = httpServer.Shutdown(shutdownCtx)
		shutdownCancel()
		<-httpServerDone
	}

	cfg.Logger.Info("runner listening",
		"ws", cfg.WSListen,
		"udp", cfg.UDPListen,
		"path", wsPath)

	// Phase B: tell driveAfterConn (and through it, BuildAgentEnv) where agents
	// should dial for the proxy. Prefer WSListen if set; UDP-only listen uses
	// the UDPListen addr with "udp" transport.
	if cfg.Config.ProxyVia == "" {
		switch {
		case cfg.WSListen != "":
			cfg.Config.ProxyVia = "ws:" + cfg.WSListen + "-*"
		case cfg.UDPListen != "":
			cfg.Config.ProxyVia = "udp:" + cfg.UDPListen + "-*"
		}
	}

	// sessionRef is shared across all accepted conns so the agent-proxy
	// handler (for agent dials) can look up the live server-conn session
	// (set by the server-dial handler). atomic.Pointer is sufficient: at
	// most one server conn is alive at a time, and agent conns only read.
	// Backed by lastListenSession (package-scope) so test hooks can read
	// the established Session — see runner/test_hooks.go.
	sessionRef := &lastListenSession

	connCh := ep.GetNewActiveConnectionChannel()
	for {
		select {
		case <-ctx.Done():
			shutdownHTTP()
			return ctx.Err()
		case err := <-httpServerDone:
			if err != nil {
				return fmt.Errorf("http server: %w", err)
			}
			return nil
		case conn, ok := <-connCh:
			if !ok {
				shutdownHTTP()
				return nil
			}
			pc := peer.WrapAcceptedConn(ctx, conn, peer.DialConfig{
				Logger:       cfg.Logger,
				PingInterval: cfg.PingInterval,
			})
			go handleAcceptedConn(ctx, cfg.Config, sessionRef, ep, pc)
		}
	}
}

// firstMsgT is the first inbound app payload captured by the OnControl
// shim installed in handleAcceptedConn, used to dispatch by wire kind.
type firstMsgT struct {
	kind    appwire.AppKind
	payload []byte
}

// lastListenSession holds the most recently established Session from
// ListenAndServe's accept path. Updated when a server conn completes
// driveAfterConn + OnConnect; cleared via CompareAndSwap on disconnect.
// Test-only access point (see runner/test_hooks.go). Production code does
// not depend on this — it's read by the agent-proxy handler via the
// sessionRef parameter passed into handleAcceptedConn.
var lastListenSession atomic.Pointer[Session]

// handleAcceptedConn peeks the first inbound payload on a freshly-accepted
// peer.Conn and routes to the server-dial path (DialGreeting) or the
// agent-proxy path (AgentProxyControl). Other kinds are logged and the
// conn closed. peer.Conn.Start is idempotent so driveAfterConn (server
// path) re-calling it is a no-op.
//
// With eager SetProxy (Task 4), relay-destined conns at a slot_id never
// reach handleAcceptedConn: objproto.receive forwards the rehandshake packet
// raw via proxySettings before the accept channel is notified. No deferred
// relay short-circuit is needed here.
func handleAcceptedConn(ctx context.Context, cfg Config, sessionRef *atomic.Pointer[Session], ep objproto.Endpoint, pc *peer.Conn) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	firstMsg := make(chan firstMsgT, 1)
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		// Copy: peer.Conn does not promise the slice is retained beyond
		// this callback (it sits over a pooled receive buffer).
		select {
		case firstMsg <- firstMsgT{kind: kind, payload: append([]byte(nil), payload...)}:
		default:
		}
	})
	pc.Start(ctx)

	select {
	case <-ctx.Done():
		pc.Close()
		return
	case msg := <-firstMsg:
		switch msg.kind {
		case appwire.AppKind_DialGreeting:
			// Best-effort: log the greeting version. Decode failure is
			// non-fatal — server-conn path proceeds regardless.
			var g protocol.DialGreeting
			if _, err := g.Decode(msg.payload); err == nil {
				cfg.Logger.Info("server greeting received", "version", g.Version)
			}
			handleServerConn(ctx, cfg, sessionRef, ep, pc)
		case appwire.AppKind_AgentProxyControl:
			handleAgentProxyConn(ctx, cfg, sessionRef, ep, pc, msg)
		default:
			cfg.Logger.Warn("accepted conn sent unexpected first payload",
				"kind", msg.kind,
				"remote", pc.Connection().ConnectionID().String())
			pc.Close()
		}
	}
}

// handleServerConn drives the server-dial path (DialGreeting first). It
// invokes driveAfterConn — which replaces the OnControl shim, re-calls
// Start (no-op due to idempotence), and performs the PSK exchange — then
// publishes the resulting session to sessionRef so concurrent agent-proxy
// dials can read it. ep is stored on the session so dispatchRunnerRequest
// can call SetProxy for EstablishRelay (eager SetProxy, Task 4).
func handleServerConn(ctx context.Context, cfg Config, sessionRef *atomic.Pointer[Session], ep objproto.Endpoint, pc *peer.Conn) {
	h, err := driveAfterConn(ctx, cfg, pc)
	if err != nil {
		// driveAfterConn already closes pc on PSK failure; do not double-close
		// (peer.Conn.Close re-sends a wire Close + sleeps 50ms each call).
		cfg.Logger.Error("server conn: PSK/setup failed", "err", err)
		return
	}
	h.session.Endpoint = ep
	sessionRef.Store(h.session)
	defer func() {
		sessionRef.CompareAndSwap(h.session, nil)
		h.Close()
	}()
	if err := OnConnect(ctx, h); err != nil {
		cfg.Logger.Error("server conn: OnConnect failed", "err", err)
	}
}

// handleAgentProxyConn drives the agent-proxy ceremony for an agent-dialed
// peer.Conn. Closes pc unconditionally on return (per spec ordering: SetProxy
// → ack → Close, all inside runAgentProxyCeremony, then this defer fires).
func handleAgentProxyConn(ctx context.Context, cfg Config, sessionRef *atomic.Pointer[Session], ep objproto.Endpoint, pc *peer.Conn, first firstMsgT) {
	defer pc.Close()

	var envelope protocol.ProxyControl
	if _, err := envelope.Decode(first.payload); err != nil {
		cfg.Logger.Warn("agent proxy: decode ProxyControl failed", "err", err)
		return
	}
	if envelope.Kind != protocol.ProxyControlKind_Request {
		cfg.Logger.Warn("agent proxy: first message is not Request", "kind", envelope.Kind)
		return
	}
	req := envelope.Request()
	if req == nil {
		cfg.Logger.Warn("agent proxy: Request variant nil")
		return
	}

	sess := sessionRef.Load()
	var serverCID objproto.ConnectionID
	hasServerConn := sess != nil
	if hasServerConn {
		serverCID = sess.ServerCIDForProxyAllocate()
	}
	taskExists := func(t protocol.TaskID) bool {
		if sess == nil {
			return false
		}
		return sess.HasTask(t)
	}

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: hasServerConn,
		taskExists:    taskExists,
	}

	if err := runAgentProxyCeremony(ctx, cfg.Logger, st, ep, pc, *req, sess); err != nil {
		cfg.Logger.Error("agent proxy ceremony failed", "err", err)
	}
}

// buildListenEndpoint constructs a Mutual-mode endpoint matching the
// WSListen / UDPListen split, in parallel with server.buildEndpoint. The
// returned httpServer is non-nil when the WS leg is active; callers use
// it to invoke Shutdown on ctx cancel so the port releases promptly.
// httpServerDone receives the http.Server's terminal error (or nil) and
// is also the channel ListenAndServe waits on inside shutdownHTTP.
func buildListenEndpoint(cfg ListenConfig, wsPath string) (objproto.Endpoint, *http.Server, chan error, error) {
	httpServerDone := make(chan error, 1)

	switch {
	case cfg.WSListen != "" && cfg.UDPListen == "":
		mux := http.NewServeMux()
		ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
			Logger: cfg.Logger,
			Path:   wsPath,
			Mode:   objproto.EndpointModeMutual,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("ws endpoint: %w", err)
		}
		httpServer := &http.Server{Addr: cfg.WSListen, Handler: mux}
		go func() {
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				httpServerDone <- err
				return
			}
			httpServerDone <- nil
		}()
		return ep, httpServer, httpServerDone, nil

	case cfg.WSListen == "" && cfg.UDPListen != "":
		port, err := parseListenPortRunner(cfg.UDPListen)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("runner: udp listen %q: %w", cfg.UDPListen, err)
		}
		ep, err := transport.UDPEndpoint(cfg.Logger, port, objproto.EndpointModeMutual)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("udp endpoint: %w", err)
		}
		// UDP-only: no HTTP server to wait on. Leave the channel open so
		// the select arm in ListenAndServe never fires until shutdown.
		return ep, nil, httpServerDone, nil

	default: // both set → dualstack
		port, err := parseListenPortRunner(cfg.UDPListen)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("runner: udp listen %q: %w", cfg.UDPListen, err)
		}
		mux := http.NewServeMux()
		ds, err := transport.UDPWebsocketDualStackEndpoint(transport.UDPWebsocketDualStackConfig{
			Logger:  cfg.Logger,
			UDPPort: port,
			Mux:     mux,
			WS: transport.WebSocketConfig{
				Logger: cfg.Logger,
				Path:   wsPath,
				Mode:   objproto.EndpointModeMutual,
			},
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("dualstack endpoint: %w", err)
		}
		httpServer := &http.Server{Addr: cfg.WSListen, Handler: mux}
		go func() {
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				httpServerDone <- err
				return
			}
			httpServerDone <- nil
		}()
		return ds.Endpoint, httpServer, httpServerDone, nil
	}
}

// parseListenPortRunner accepts "host:port" or ":port" and returns the port.
// Named with a Runner suffix to avoid colliding with server.parseListenPort
// at the symbol level (different package, but keeps grep results unambiguous).
func parseListenPortRunner(addr string) (uint16, error) {
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
