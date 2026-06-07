package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/transport"
	"github.com/on-keyday/objtrsf/objproto"
)

// TestDialViaProxyHandlesIdCollision: the runner replies with IdCollision;
// DialViaProxy retries up to 3 times then returns ErrProxyIdCollision.
func TestDialViaProxyHandlesIdCollision(t *testing.T) {
	const listenAddr = "127.0.0.1:18580"
	stop := startFakeProxyRunner(t, listenAddr, func(req protocol.ProxyRequest) protocol.ProxyEstablishResponse {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_IdCollision}
	})
	defer stop()

	proxyCID, err := objproto.ParseConnectionID("ws:"+listenAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse CID: %v", err)
	}

	var taskID protocol.TaskID
	taskID.Id[0] = 0xAB

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = DialViaProxy(ctx, proxyCID, taskID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrProxyIdCollision) {
		t.Errorf("expected ErrProxyIdCollision, got %v", err)
	}
}

// TestDialViaProxyServerNotConnected: typed error pass-through.
func TestDialViaProxyServerNotConnected(t *testing.T) {
	const listenAddr = "127.0.0.1:18581"
	stop := startFakeProxyRunner(t, listenAddr, func(req protocol.ProxyRequest) protocol.ProxyEstablishResponse {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_ServerNotConnected}
	})
	defer stop()

	proxyCID, err := objproto.ParseConnectionID("ws:"+listenAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse CID: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var taskID protocol.TaskID
	_, err = DialViaProxy(ctx, proxyCID, taskID)
	if !errors.Is(err, ErrProxyServerNotConnected) {
		t.Errorf("got %v want ErrProxyServerNotConnected", err)
	}
}

// TestDialViaProxyUnknownTask: typed error pass-through.
func TestDialViaProxyUnknownTask(t *testing.T) {
	const listenAddr = "127.0.0.1:18582"
	stop := startFakeProxyRunner(t, listenAddr, func(req protocol.ProxyRequest) protocol.ProxyEstablishResponse {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_UnknownTask}
	})
	defer stop()

	proxyCID, err := objproto.ParseConnectionID("ws:"+listenAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse CID: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var taskID protocol.TaskID
	taskID.Id[0] = 0x42
	_, err = DialViaProxy(ctx, proxyCID, taskID)
	if !errors.Is(err, ErrProxyUnknownTask) {
		t.Errorf("got %v want ErrProxyUnknownTask", err)
	}
}

// TestDialViaProxyInternalError: runner replies InternalError (e.g. SetProxy
// failed); DialViaProxy returns ErrProxyInternalError without retry.
func TestDialViaProxyInternalError(t *testing.T) {
	const listenAddr = "127.0.0.1:18583"
	stop := startFakeProxyRunner(t, listenAddr, func(req protocol.ProxyRequest) protocol.ProxyEstablishResponse {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_InternalError}
	})
	defer stop()

	proxyCID, err := objproto.ParseConnectionID("ws:"+listenAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse CID: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var taskID protocol.TaskID
	taskID.Id[0] = 0x42
	_, err = DialViaProxy(ctx, proxyCID, taskID)
	if !errors.Is(err, ErrProxyInternalError) {
		t.Errorf("got %v want ErrProxyInternalError", err)
	}
}

// startFakeProxyRunner builds a Mutual WS endpoint, accepts ONE peer.Conn,
// waits for the first agent_proxy_control payload, calls respond(req), then
// sends back ProxyControl{EstablishResponse{respond's status}}.
//
// Mirrors the pattern in runner/listen.go (Mutual endpoint setup) but simpler:
// no PSK, no driveAfterConn, just the ceremony reply.
func startFakeProxyRunner(t *testing.T, listenAddr string, respond func(protocol.ProxyRequest) protocol.ProxyEstablishResponse) (cleanup func()) {
	t.Helper()

	mux := http.NewServeMux()
	ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   "/ws",
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatalf("ws endpoint: %v", err)
	}

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	time.Sleep(200 * time.Millisecond) // bind

	ctx, cancel := context.WithCancel(context.Background())
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case conn := <-ep.GetNewActiveConnectionChannel():
				if conn == nil {
					return
				}
				pc := peer.WrapAcceptedConn(ctx, conn, peer.DialConfig{
					Logger: slog.Default(),
				})
				// Process exactly one conn: wait for ProxyRequest, reply, return.
				go func(pc *peer.Conn) {
					respCh := make(chan protocol.ProxyRequest, 1)
					pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
						if kind != appwire.AppKind_AgentProxyControl {
							return
						}
						var envelope protocol.ProxyControl
						if _, err := envelope.Decode(payload); err != nil {
							return
						}
						if envelope.Kind != protocol.ProxyControlKind_Request {
							return
						}
						if req := envelope.Request(); req != nil {
							select {
							case respCh <- *req:
							default:
							}
						}
					})
					pc.Start(ctx)

					select {
					case <-ctx.Done():
						pc.Close()
						return
					case req := <-respCh:
						resp := respond(req)
						var envelope protocol.ProxyControl
						envelope.Kind = protocol.ProxyControlKind_EstablishResponse
						envelope.SetEstablishResponse(resp)
						out := envelope.MustAppend([]byte{byte(appwire.AppKind_AgentProxyControl)})
						_, _, _ = pc.Connection().SendMessage(out)
						// Give the message a moment to flush, then close.
						time.Sleep(100 * time.Millisecond)
						pc.Close()
					}
				}(pc)
			}
		}
	}()

	return func() {
		cancel()
		shutCtx, c := context.WithTimeout(context.Background(), 1*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	}
}
