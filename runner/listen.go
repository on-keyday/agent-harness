package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/transport"
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
			go handleAcceptedConn(ctx, cfg.Config, pc)
		}
	}
}

// handleAcceptedConn drives a freshly-accepted peer.Conn through the same
// PSK + Hello + dispatch lifecycle as the outbound Connect/OnConnect path.
// Errors are logged and the conn is closed; the listen loop continues.
func handleAcceptedConn(ctx context.Context, cfg Config, pc *peer.Conn) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	h, err := driveAfterConn(ctx, cfg, pc)
	if err != nil {
		cfg.Logger.Error("accepted conn: PSK/setup failed", "err", err)
		pc.Close()
		return
	}
	defer h.Close()
	if err := OnConnect(ctx, h); err != nil {
		cfg.Logger.Error("accepted conn: OnConnect failed", "err", err)
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
