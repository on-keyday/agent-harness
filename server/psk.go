package server

import (
	"crypto/subtle"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// pskDispatchIdentity re-dispatches the embedded hello from an accepted
// PskAuthRequest to the normal wire handlers so they record identity.
//
// Choice: re-dispatch via Dispatcher.Dispatch (dispatch.go:58) with re-encoded
// wire bytes, rather than calling handler internals directly. This reuses the
// existing TaskHandler.Handle (task_handler.go:148) for client role and
// RunnerHandler.Handle (runner_handler.go:64) for runner role unchanged.
//
// Wire encoding mirrors how each hello normally arrives:
//
//	client_hello → [0x41] + TaskControlRequest{Kind:ClientHello, RequestId:0, hello}
//	runner_hello → [0x43] + RunnerMessage{Kind:Hello, hello}
//
// The TaskHandler will also send a TaskControlResponse{ClientHello} back to the
// conn (request_id=0). In the new merged handshake the status has already been
// communicated via PskAuthResponse{ok}, so this extra response is a no-op for
// the caller (Tasks 3-5 will update the client to not send a separate hello at all).
func pskDispatchIdentity(d *Dispatcher, conn ConnHandle, req *protocol.PskAuthRequest) {
	switch req.Role {
	case protocol.AuthRole_Client:
		hello := req.ClientHello()
		if hello == nil {
			return
		}
		tcReq := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
		tcReq.SetClientHello(*hello)
		data, err := tcReq.Append([]byte{byte(appwire.AppKind_TaskControl)})
		if err != nil {
			slog.Error("pskDispatchIdentity: encode TaskControlRequest failed", "err", err)
			return
		}
		d.Dispatch(conn, data)

	case protocol.AuthRole_Runner:
		rh := req.RunnerHello()
		if rh == nil {
			return
		}
		rmsg := protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
		rmsg.SetHello(*rh)
		data, err := rmsg.Append([]byte{byte(appwire.AppKind_RunnerControl)})
		if err != nil {
			slog.Error("pskDispatchIdentity: encode RunnerMessage failed", "err", err)
			return
		}
		d.Dispatch(conn, data)
	}
}

// pskGate enforces a merged PSK + identity handshake on each connection.
// The first message must be [0x45]+PskAuthRequest (brgen-encoded):
//   - binder is verified (when PSK is configured; skipped with binder_len=0 when none)
//   - identity (ClientHello or RunnerHello) is required regardless of PSK config
//   - for client role + kind=agent, the agent ticket is validated before acceptance
//
// authed starts false until Check succeeds. When no PSK is configured the binder
// compare is skipped but the identity handshake is still required (identity is
// mandatory in dev too).
//
// ValidateTicket is called by Check (before marking authed) to validate a
// kind=agent ClientHello ticket. It must return PskAuthStatus_Ok on success or
// PskAuthStatus_BadTicket on failure. Nil ValidateTicket degrades to Ok (safe
// for tests that do not wire a Board).
type pskGate struct {
	psk            []byte
	authed         bool
	ValidateTicket func(info *protocol.AgentInfo) protocol.PskAuthStatus
}

func newPSKGate(psk []byte) *pskGate {
	// authed is always false initially: even no-PSK connections must complete
	// the identity handshake before any other message is processed.
	return &pskGate{psk: psk, authed: false}
}

func (g *pskGate) Authed() bool { return g.authed }

// sendPskResponse encodes and sends [AppKind_PskAuth] + PskAuthResponse{status} via sendFn.
func sendPskResponse(sendFn func([]byte), status protocol.PskAuthStatus) {
	resp := protocol.PskAuthResponse{Status: status}
	out, err := resp.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
		// Encoding cannot fail for a single-byte enum; log defensively.
		slog.Error("pskGate: failed to encode PskAuthResponse", "err", err)
		return
	}
	sendFn(out)
}

// Check examines one incoming message against the PSK + identity gate.
// sendFn writes response bytes back to the connection.
// Returns:
//
//	isPSKMsg  — true when the message carried the PskAuth kind byte (0x45)
//	shouldClose — true when the connection must be closed
//	accepted — non-nil *PskAuthRequest when auth succeeded; caller must
//	            dispatch the embedded hello to record identity.
//
// Processing order (load-bearing per spec):
//  1. Decode PskAuthRequest
//  2. Binder verification (before any identity action)
//  3. Identity required (fail-closed)
//  4. Ticket validation (client+agent role only)
//  5. Accept: mark authed, reply ok, return decoded request for caller dispatch
//
// transcript is this connection's objproto handshake transcript
// (Connection.GetTranscript()). ComputePSKBinder inputs are UNCHANGED.
func (g *pskGate) Check(
	data, transcript []byte,
	sendFn func([]byte),
) (isPSKMsg bool, shouldClose bool, accepted *protocol.PskAuthRequest) {
	if g.authed {
		// Gate already open; pass through silently.
		return false, false, nil
	}
	if len(data) == 0 {
		return false, true, nil
	}
	kind := appwire.AppKind(data[0])
	if kind != appwire.AppKind_PskAuth {
		// Non-PSK message before auth — fail-closed.
		return false, true, nil
	}

	// Step 1: Decode PskAuthRequest from data[1:].
	// Decode is parse-only; safe before binder verification (spec: "decode is
	// parse-only and the brgen decoder is robust to malformed input").
	var req protocol.PskAuthRequest
	if err := req.DecodeExact(data[1:]); err != nil {
		slog.Warn("pskGate: failed to decode PskAuthRequest", "err", err)
		sendPskResponse(sendFn, protocol.PskAuthStatus_NoIdentity)
		return true, true, nil
	}

	// Step 2: Binder verification (before any identity action).
	// ComputePSKBinder call is BYTE-IDENTICAL to the previous gate:
	//   cli.ComputePSKBinder(g.psk, transcript)
	// When no PSK is configured (g.psk is nil/empty), binder_len is expected 0
	// and the compare is skipped — but the identity handshake still runs.
	if len(g.psk) > 0 {
		expected, err := cli.ComputePSKBinder(g.psk, transcript)
		if err != nil || subtle.ConstantTimeCompare(req.Binder, expected) != 1 {
			sendPskResponse(sendFn, protocol.PskAuthStatus_BadPsk)
			return true, true, nil
		}
	}

	// Step 3: Identity required (fail-closed).
	// At least one role+hello must be present.
	switch req.Role {
	case protocol.AuthRole_Client:
		if req.ClientHello() == nil {
			slog.Warn("pskGate: client role but ClientHello is nil (identity required)")
			sendPskResponse(sendFn, protocol.PskAuthStatus_NoIdentity)
			return true, true, nil
		}
	case protocol.AuthRole_Runner:
		if req.RunnerHello() == nil {
			slog.Warn("pskGate: runner role but RunnerHello is nil (identity required)")
			sendPskResponse(sendFn, protocol.PskAuthStatus_NoIdentity)
			return true, true, nil
		}
	default:
		// Unknown role — fail-closed.
		slog.Warn("pskGate: unknown role", "role", req.Role)
		sendPskResponse(sendFn, protocol.PskAuthStatus_NoIdentity)
		return true, true, nil
	}

	// Step 4: Ticket validation (client role + kind=agent only).
	// This closes the bad-ticket→operator fallback: an agent presenting an
	// invalid ticket must NOT proceed as operator.
	if req.Role == protocol.AuthRole_Client {
		hello := req.ClientHello()
		if hello.Kind == protocol.ClientKind_Agent {
			info := hello.AgentInfo()
			if info == nil {
				slog.Warn("pskGate: agent kind but AgentInfo is nil")
				sendPskResponse(sendFn, protocol.PskAuthStatus_BadTicket)
				return true, true, nil
			}
			if g.ValidateTicket != nil {
				if s := g.ValidateTicket(info); s != protocol.PskAuthStatus_Ok {
					sendPskResponse(sendFn, s)
					return true, true, nil
				}
			}
			// nil ValidateTicket degrades to Ok (test-wiring without a Board).
		}
	}

	// Step 5: Accept — mark authed, reply ok, return decoded request for dispatch.
	g.authed = true
	sendPskResponse(sendFn, protocol.PskAuthStatus_Ok)
	return true, false, &req
}
