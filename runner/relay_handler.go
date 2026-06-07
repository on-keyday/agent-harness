package runner

import (
	"context"
	"log/slog"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// relayHandlerState bundles the inputs needed by EstablishRelay validation.
// Extracted as a struct so the validation logic is pure-function-testable
// without spinning up real endpoints / sessions.
type relayHandlerState struct {
	// serverCID is the runner's view of its server peer.Conn (from
	// Session.ServerCID). slot_id must not collide with this conn's ID,
	// otherwise the inbound dial at (proxy_runner.Addr, slot_id) would
	// resolve to the existing server-conn entry rather than producing a
	// new activeConn for the relay-setup path.
	serverCID objproto.ConnectionID
}

// validate computes the EstablishRelayResponse for a request without touching
// the endpoint. Pure function; safe to test in isolation.
func (s *relayHandlerState) validate(req protocol.EstablishRelayRequest) protocol.EstablishRelayResponse {
	if len(req.Target.Transport) == 0 {
		return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_InvalidTarget}
	}
	switch len(req.Target.IpAddr) {
	case 0, 4, 16:
		// 0 is allowed by the schema constraint (matches DialRunner path).
	default:
		return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_InvalidTarget}
	}
	if req.SlotId == s.serverCID.ID {
		return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_SlotCollision}
	}
	return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_Ok}
}

// handleEstablishRelay processes an EstablishRelayRequest from the server,
// invoked from dispatchRunnerRequest. Validates, then immediately installs
// a SetProxy entry (eager SetProxy) on Ok. This is safe because
// objproto.SetProxy no longer requires owned to be a live activeConn
// (synthetic-owned relaxation landed in commit f2b0f5c).
//
// Eager SetProxy is required for chained relay: the agent's rehandshake at
// the slot_id is forwarded raw by the proxySettings entry without ever
// creating a local activeConn at proxy_runner. Lazy deferred SetProxy (the
// pre-Task-4 path that required a matching activeConn) would never fire in
// that case.
//
// For existing Phase C (direct server --via): the server's SendHandshake at
// slot_id hits proxy_runner's eager proxySettings entry and is forwarded raw
// to target. Target ECDH's it; the resulting activeConn IS the end-to-end
// conn — server's HandleWithVia uses this directly (Task 4 also drops the
// old RehandshakeForProxy step from dial_runner_handler.go).
func handleEstablishRelay(
	ctx context.Context,
	logger *slog.Logger,
	st *relayHandlerState,
	ep objproto.Endpoint,
	req protocol.EstablishRelayRequest,
	sendResponse func(protocol.EstablishRelayResponse) error,
) {
	_ = ctx // reserved for future cancellation hooks
	resp := st.validate(req)
	if resp.Status == protocol.EstablishRelayStatus_Ok {
		targetCID := protocol.RunnerIDToConnID(req.Target)
		ownedCID := objproto.NewConnectionID(st.serverCID.Transport, st.serverCID.Addr, req.SlotId)
		allocCID := objproto.NewConnectionID(targetCID.Transport, targetCID.Addr, req.SlotId)
		if err := ep.SetProxy(ownedCID, allocCID); err != nil {
			if logger != nil {
				logger.Warn("relay: eager SetProxy failed",
					"owned", ownedCID.String(),
					"allocate", allocCID.String(),
					"err", err)
			}
			// SetProxyFailed is the schema-defined status for this case (message.bgn:222).
			resp.Status = protocol.EstablishRelayStatus_SetProxyFailed
		} else {
			if logger != nil {
				logger.Info("relay: eager SetProxy installed",
					"owned", ownedCID.String(),
					"allocate", allocCID.String(),
					"slot_id", req.SlotId)
			}
		}
	}
	if err := sendResponse(resp); err != nil && logger != nil {
		logger.Warn("relay: send response failed", "err", err)
	}
}
