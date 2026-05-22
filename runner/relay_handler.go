package runner

import (
	"context"
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
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
// the endpoint or expectedRelays map. Pure function; safe to test in isolation.
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

// expectedRelays tracks slot_ids the runner has agreed to relay for.
// Keyed by slot_id; value is the target ConnectionID used to construct the
// SetProxy allocate-side CID when the server's slot_id dial arrives.
//
// One-shot: Take removes the entry, so a single EstablishRelay grants
// exactly one relay setup. Written by the EstablishRelay dispatch
// goroutine, read by the listen accept loop.
type expectedRelays struct {
	mu sync.Mutex
	m  map[uint16]objproto.ConnectionID
}

func newExpectedRelays() *expectedRelays {
	return &expectedRelays{m: make(map[uint16]objproto.ConnectionID)}
}

func (e *expectedRelays) Put(slotID uint16, target objproto.ConnectionID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.m[slotID] = target
}

// Take returns the target CID for slotID and deletes the entry. The bool is
// false when no entry exists. One-shot semantics — subsequent Takes miss.
func (e *expectedRelays) Take(slotID uint16) (objproto.ConnectionID, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	target, ok := e.m[slotID]
	if ok {
		delete(e.m, slotID)
	}
	return target, ok
}

// handleEstablishRelay processes an EstablishRelayRequest from the server,
// invoked from dispatchRunnerRequest. Validates, records the expected slot
// (on Ok), and sends the response back via sendResponse.
//
// NOTE: this does NOT dial target or set up SetProxy. Those happen later
// when the server's slot_id dial arrives at the accept handler — see
// completeRelaySetup, invoked from listen.go's handleAcceptedConn.
func handleEstablishRelay(
	ctx context.Context,
	logger *slog.Logger,
	st *relayHandlerState,
	expected *expectedRelays,
	req protocol.EstablishRelayRequest,
	sendResponse func(protocol.EstablishRelayResponse) error,
) {
	_ = ctx // reserved for future cancellation hooks
	resp := st.validate(req)
	if resp.Status == protocol.EstablishRelayStatus_Ok {
		targetCID := protocol.RunnerIDToConnID(req.Target)
		expected.Put(req.SlotId, targetCID)
		if logger != nil {
			logger.Info("relay: expecting server dial",
				"slot_id", req.SlotId,
				"target", targetCID.String())
		}
	}
	if err := sendResponse(resp); err != nil && logger != nil {
		logger.Warn("relay: send response failed", "err", err)
	}
}

// completeRelaySetup runs when the listen accept loop sees a slot_id dial
// that matches an expectedRelays entry. The activeConn at
// (server.Addr, slot_id) is already established (initial ECDH server↔
// proxy_runner completed). This function:
//
//  1. SetProxy(owned=activeConn.CID, allocate=synthetic (target.Transport,
//     target.Addr, slot_id)) — allocate-side is purely synthetic because
//     proxy_runner does not have an activeConn for target.
//  2. Close the underlying objproto connection silently (NOT via peer.Conn.Close,
//     which would send a trsf.Close wire message to the server and cause
//     the server's endToEndConn to receive EOF prematurely). Using
//     pc.Connection().Close() removes the activeConn entry from the endpoint
//     and leaves the proxySettings entry in place so subsequent packets at
//     this CID are forwarded raw to target.Addr.
//
// proxy_runner does NOT ECDH target and does NOT send DialGreeting — those
// are the server's responsibility after RehandshakeForProxy (Task 3).
func completeRelaySetup(
	logger *slog.Logger,
	ep objproto.Endpoint,
	pc *peer.Conn,
	target objproto.ConnectionID,
	slotID uint16,
) {
	// Close the raw connection without sending a trsf.Close wire message.
	// peer.Conn.Close() would send Close (causing server's endToEndConn to
	// receive EOF). We only need to release the activeConn slot; the
	// proxySettings entry must survive.
	defer pc.Connection().Close() //nolint:errcheck

	ownedCID := pc.Connection().ConnectionID()
	allocCID := objproto.NewConnectionID(target.Transport, target.Addr, slotID)

	if err := ep.SetProxy(ownedCID, allocCID); err != nil {
		if logger != nil {
			logger.Error("relay: SetProxy failed",
				"owned", ownedCID.String(),
				"allocate", allocCID.String(),
				"err", err)
		}
		return
	}
	if logger != nil {
		logger.Info("relay: SetProxy established",
			"owned", ownedCID.String(),
			"allocate", allocCID.String(),
			"slot_id", slotID)
	}
}
