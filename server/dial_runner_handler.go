package server

import (
	"context"
	"crypto/ecdh"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// DialRunnerHandler handles a single TaskControlKind_DialRunner request:
// converts the embedded RunnerID into an objproto.ConnectionID, calls
// objproto.DoECDHHandshake on the server's existing Endpoint, and reports
// the dial outcome.
//
// Design note: we call objproto.DoECDHHandshake directly rather than
// peer.Dial so the raw objproto.Connection is delivered to OnDialed without
// a peer.Conn wrapper. handleConnection builds its own trsf layer on top;
// double-wrapping would create two competing AutoSend/AutoPing goroutines on
// the same underlying connection.
//
// Outbound-Dial connections are delivered via the hsDone channel inside
// objproto (not GetNewActiveConnectionChannel, which is inbound-only).
// OnDialed bridges the gap: the server wires it to
//
//	func(ctx, conn) { go s.handleConnection(ctx, conn) }
//
// so the new connection enters the standard registration path (PSK gate →
// RunnerHello → Registry insert).
type DialRunnerHandler struct {
	Logger      *slog.Logger
	Endpoint    objproto.Endpoint
	DialTimeout time.Duration // 0 → 10s default

	// OnDialed, when non-nil, is called with the server root context and the
	// raw objproto.Connection produced by a successful ECDH handshake.
	// If nil, the connection is closed immediately (useful in tests that only
	// check status codes without wanting a live connection).
	OnDialed func(ctx context.Context, conn objproto.Connection)
}

// Handle performs the dial and returns the response struct. Does NOT wait
// for PSK / Hello to complete — those happen asynchronously in the goroutine
// spawned by OnDialed.
func (h *DialRunnerHandler) Handle(ctx context.Context, target protocol.RunnerID) protocol.DialRunnerResponse {
	if len(target.Transport) == 0 {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner: invalid target: empty transport")
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
	}
	switch len(target.IpAddr) {
	case 0, 4, 16:
		// 0 is allowed by the schema constraint (ip_addr_len == 0 || 4 || 16)
	default:
		if h.Logger != nil {
			h.Logger.Warn("dial-runner: invalid target: bad ip_addr_len", "len", len(target.IpAddr))
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
	}

	cid := protocol.RunnerIDToConnID(target)

	timeout := h.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := objproto.DoECDHHandshake(dialCtx, h.Endpoint, cid, ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner: ECDH handshake failed", "target", fmt.Sprintf("%v", cid), "err", err)
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	}

	if h.OnDialed != nil {
		h.OnDialed(ctx, conn)
	} else {
		conn.Close() //nolint:errcheck
	}

	return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_Ok}
}
