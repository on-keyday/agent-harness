package server

import (
	"crypto/subtle"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// pskDispatchIdentity records identity from the embedded hello in an accepted
// PskAuthRequest without sending a redundant wire response.
//
// Client role: calls d.RecordClientIdentity directly (no TaskControlResponse —
// the gate's PskAuthResponse{ok} is the sole client handshake ack). If
// RecordClientIdentity is not wired (nil), falls back to re-dispatch via the
// TaskControl wire path (old behaviour; still correct for tests that do not wire
// the field, though it emits an extra TaskControlResponse which legacy clients tolerate).
//
// Runner role: re-dispatches via [0x43]+RunnerMessage{Hello} unchanged. The
// runner NEEDS the RunnerHelloResponse (it carries YourRunnerId consumed at
// runner/connect.go:340) so this path must NOT be suppressed.
//
// Wire encoding used for the runner path:
//
//	runner_hello → [0x43] + RunnerMessage{Kind:Hello, hello}
func pskDispatchIdentity(d *Dispatcher, conn ConnHandle, req *protocol.PskAuthRequest) {
	switch req.Role {
	case protocol.AuthRole_Client:
		hello := req.ClientHello()
		if hello == nil {
			return
		}
		if d.RecordClientIdentity != nil {
			// Fast path: record identity directly — no TaskControlResponse emitted.
			cid := conn.ConnectionID().String()
			d.RecordClientIdentity(cid, conn, hello)
			return
		}
		// Fallback (tests without RecordClientIdentity wired): re-dispatch via
		// the normal TaskControl wire path. This emits an extra
		// TaskControlResponse{ClientHello} but legacy clients tolerate it.
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
	psk    []byte
	authed bool
	// operatorPSK, when non-empty, is the separate secret that operator-surface
	// connections (kind=cli/tui/webui, i.e. anything but agent) must prove via
	// the binder. It is NEVER injected into agent task environments, so an
	// in-task agent — which holds only the connect psk — cannot forge an
	// operator binder and thus cannot claim operator authority by sending
	// kind=Client. Empty operatorPSK preserves the legacy behaviour (operator
	// connections validated against psk, the shared connect secret).
	operatorPSK    []byte
	ValidateTicket func(info *protocol.AgentInfo) protocol.PskAuthStatus
}

func newPSKGate(psk []byte) *pskGate {
	// authed is always false initially: even no-PSK connections must complete
	// the identity handshake before any other message is processed.
	return &pskGate{psk: psk, authed: false}
}

func (g *pskGate) Authed() bool { return g.authed }

// binderKey selects which secret the request's binder must prove. Operator-
// surface clients (role=Client, kind != agent: cli/tui/webui) prove operatorPSK
// when it is configured; agents (kind=agent) and runners prove the shared
// connect psk. When operatorPSK is empty the operator surface falls back to the
// connect psk — the legacy behaviour — so a deployment that has not configured
// an operator secret is unchanged (and a startup warning is emitted elsewhere).
func (g *pskGate) binderKey(req *protocol.PskAuthRequest) []byte {
	if req.Role == protocol.AuthRole_Client && len(g.operatorPSK) > 0 {
		if hello := req.ClientHello(); hello != nil && hello.Kind != protocol.ClientKind_Agent {
			return g.operatorPSK
		}
	}
	return g.psk
}

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
	// The binder must prove the secret appropriate to the role: operator-surface
	// clients (kind != agent) prove operatorPSK when it is configured; agents and
	// runners prove the shared connect psk. binderKey resolves this; an in-task
	// agent holds only the connect psk, so it cannot forge an operator binder.
	// When the resolved key is empty (no PSK configured for that role) the
	// compare is skipped — but the identity handshake still runs.
	if key := g.binderKey(&req); len(key) > 0 {
		expected, err := cli.ComputePSKBinder(key, transcript)
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
