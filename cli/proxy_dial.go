package cli

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Sentinel errors returned by DialViaProxy. Callers can errors.Is-check to
// decide whether to retry (ErrProxyIdCollision) or fail fast (others).
var (
	ErrProxyIdCollision        = errors.New("proxy: connection_id collision with runner's server conn (retry with different id)")
	ErrProxyServerNotConnected = errors.New("proxy: runner has no live server conn")
	ErrProxyUnknownTask        = errors.New("proxy: runner does not have this task")
	ErrProxyInternalError      = errors.New("proxy: runner-side internal error (e.g. SetProxy failed); do not retry")
	ErrProxyUnexpectedStatus   = errors.New("proxy: runner returned unexpected status")
)

// DialViaProxy establishes an end-to-end peer.Conn to the harness server,
// going through a runner that acts as an objproto-level packet relay.
//
//   - proxyCID:   the runner's listen-side ConnectionID (e.g. ws:host:port-*)
//   - taskID:     the task this agent is bound to (server validates via AuthTicket
//     later; runner validates that the task exists in its session)
//
// On IdCollision, the function retries up to 3 times with fresh random IDs.
// Other non-Ok statuses are returned as typed errors so the caller can
// distinguish (e.g. surface to the user).
func DialViaProxy(ctx context.Context, proxyCID objproto.ConnectionID, taskID protocol.TaskID) (*peer.Conn, error) {
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		proxyCID.ID = uint16(rand.Uint32() & 0xFFFF) // #nosec G404 — collision retry, not crypto

		pc, err := dialViaProxyAttempt(ctx, proxyCID, taskID)
		if err == nil {
			return pc, nil
		}
		if errors.Is(err, ErrProxyIdCollision) && attempt < maxRetries-1 {
			continue
		}
		return nil, err
	}
	return nil, ErrProxyIdCollision
}

func dialViaProxyAttempt(ctx context.Context, proxyCID objproto.ConnectionID, taskID protocol.TaskID) (*peer.Conn, error) {
	ep, err := BuildClientEndpoint(proxyCID)
	if err != nil {
		return nil, fmt.Errorf("build client endpoint: %w", err)
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	localConn, err := peer.Dial(ctx, ep, proxyCID, peer.DialConfig{
		Logger:       slog.Default(),
		PingInterval: 15 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("peer.Dial(proxy): %w", err)
	}
	// localConn becomes a "child" of the new conn after RehandshakeForProxy;
	// do NOT defer Close here in the happy path. We Close explicitly on error
	// paths below.

	respCh := make(chan protocol.ProxyEstablishResponse, 1)
	localConn.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		if kind != wire.ApplicationPayloadKind_AgentProxyControl {
			return
		}
		var envelope protocol.ProxyControl
		if _, err := envelope.Decode(payload); err != nil {
			return
		}
		if envelope.Kind != protocol.ProxyControlKind_EstablishResponse {
			return
		}
		if resp := envelope.EstablishResponse(); resp != nil {
			select {
			case respCh <- *resp:
			default:
			}
		}
	})
	localConn.Start(ctx)

	// Send ProxyRequest.
	var req protocol.ProxyControl
	req.Kind = protocol.ProxyControlKind_Request
	req.SetRequest(protocol.ProxyRequest{TaskId: taskID})
	if _, _, err := localConn.Connection().SendMessage(req.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentProxyControl)})); err != nil {
		localConn.Close()
		return nil, fmt.Errorf("send ProxyRequest: %w", err)
	}

	// Wait for EstablishResponse.
	respCtx, respCancel := context.WithTimeout(ctx, 10*time.Second)
	defer respCancel()
	var resp protocol.ProxyEstablishResponse
	select {
	case <-respCtx.Done():
		localConn.Close()
		return nil, fmt.Errorf("waiting for EstablishResponse: %w", respCtx.Err())
	case resp = <-respCh:
	}

	switch resp.Status {
	case protocol.ProxyEstablishStatus_Ok:
		// fall through to rehandshake
	case protocol.ProxyEstablishStatus_IdCollision:
		localConn.Close()
		return nil, ErrProxyIdCollision
	case protocol.ProxyEstablishStatus_ServerNotConnected:
		localConn.Close()
		return nil, ErrProxyServerNotConnected
	case protocol.ProxyEstablishStatus_UnknownTask:
		localConn.Close()
		return nil, ErrProxyUnknownTask
	case protocol.ProxyEstablishStatus_InternalError:
		localConn.Close()
		return nil, ErrProxyInternalError
	default:
		localConn.Close()
		return nil, fmt.Errorf("%w: %v", ErrProxyUnexpectedStatus, resp.Status)
	}

	// SetProxy is in effect on the runner. Rehandshake to derive new keys
	// with the actual server (handshake packets are forwarded by the runner).
	newKey, newHS, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		localConn.Close()
		return nil, fmt.Errorf("new ECDH handshake: %w", err)
	}
	rh, err := localConn.Connection().RehandshakeForProxy(newKey, newHS)
	if err != nil {
		localConn.Close()
		return nil, fmt.Errorf("RehandshakeForProxy: %w", err)
	}
	rhCtx, rhCancel := context.WithTimeout(ctx, 10*time.Second)
	defer rhCancel()
	var newConn objproto.Connection
	select {
	case <-rhCtx.Done():
		return nil, fmt.Errorf("waiting for rehandshake: %w", rhCtx.Err())
	case newConn = <-rh.C:
	}

	// Wrap the new objproto.Connection in a peer.Conn ready for PSK +
	// AgentBridgeHello. The old localConn is auto-closed via the
	// proxyConnection field in handshakeInfo when newConn closes.
	return peer.WrapAcceptedConn(ctx, newConn, peer.DialConfig{
		Logger:       slog.Default(),
		PingInterval: 15 * time.Second,
	}), nil
}
