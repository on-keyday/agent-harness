package server

import (
	"crypto/ed25519"
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
func pskDispatchIdentity(d *Dispatcher, conn ConnHandle, req *protocol.PskAuthRequest) bool {
	switch req.Role {
	case protocol.AuthRole_ClientLegacy:
		legacy := req.ClientHelloLegacy()
		if legacy == nil {
			return false
		}
		// The gate has already checked the ticket a legacy hello carries;
		// identity code past this point takes the ticket-free shape.
		hello := clientHelloFromLegacy(legacy)
		if d.RecordClientIdentity != nil {
			// Fast path: record identity directly — no TaskControlResponse emitted.
			cid := conn.ConnectionID().String()
			return d.RecordClientIdentity(cid, conn, &hello) == protocol.ClientHelloStatus_Ok
		}
		// Fallback (tests without RecordClientIdentity wired): re-dispatch via
		// the normal TaskControl wire path. This emits an extra
		// TaskControlResponse{ClientHello} but legacy clients tolerate it.
		tcReq := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
		tcReq.SetClientHello(hello)
		data, err := tcReq.Append([]byte{byte(appwire.AppKind_TaskControl)})
		if err != nil {
			slog.Error("pskDispatchIdentity: encode TaskControlRequest failed", "err", err)
			return false
		}
		d.Dispatch(conn, data)
		return true

	case protocol.AuthRole_RunnerLegacy:
		rh := req.RunnerHello()
		if rh == nil {
			return false
		}
		rmsg := protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
		rmsg.SetHello(*rh)
		data, err := rmsg.Append([]byte{byte(appwire.AppKind_RunnerControl)})
		if err != nil {
			slog.Error("pskDispatchIdentity: encode RunnerMessage failed", "err", err)
			return false
		}
		d.Dispatch(conn, data)
		return true
	}
	return false
}

// clientHelloFromLegacy drops the bearer values an AuthRole.client_legacy hello
// carries (the agent's ticket, the data-plane grant id), once the gate has
// checked them, so code past the gate handles one ClientHello shape.
func clientHelloFromLegacy(l *protocol.ClientHelloLegacy) protocol.ClientHello {
	h := protocol.ClientHello{Kind: l.Kind}
	if a := l.AgentInfo(); a != nil {
		info := protocol.AgentInfo{RunnerId: a.RunnerId, TaskId: a.TaskId}
		info.SetHostname(a.Hostname)
		h.SetAgentInfo(info)
	}
	if dp := l.DataPlaneInfo(); dp != nil {
		h.SetDataPlaneInfo(protocol.DataPlaneInfo{TaskId: dp.TaskId})
	}
	return h
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
	role   protocol.AuthRole // fixed by the accepted handshake, never by later messages
	// operatorPSK, when non-empty, is the separate secret that operator-surface
	// connections (kind=cli/tui/webui, i.e. anything but agent) must prove via
	// the binder. It is NEVER injected into agent task environments, so an
	// in-task agent — which holds only the connect psk — cannot forge an
	// operator binder and thus cannot claim operator authority by sending
	// kind=Client. Empty operatorPSK preserves the legacy behaviour (operator
	// connections validated against psk, the shared connect secret).
	operatorPSK []byte
	// serverKey signs every PskAuthResponse so a client holding the public
	// half can tell this server from an impostor. server.New guarantees one.
	serverKey      ed25519.PrivateKey
	ValidateTicket func(info *protocol.AgentInfoLegacy) protocol.PskAuthStatus
}

func newPSKGate(psk []byte) *pskGate {
	// authed is always false initially: even no-PSK connections must complete
	// the identity handshake before any other message is processed.
	return &pskGate{psk: psk, authed: false}
}

func (g *pskGate) Authed() bool { return g.authed }

// AllowsMessage binds application traffic to the role authenticated by Check.
// Both roles use pubsub (runners publish task logs) and answer telemetry, but
// only clients issue task control and only runners report task state. Hello
// messages belong exclusively to the initial handshake: accepting another one
// would let a peer replace its authenticated identity on the same connection.
func (g *pskGate) AllowsMessage(data []byte) bool {
	if !g.authed || len(data) == 0 {
		return false
	}
	switch appwire.AppKind(data[0]) {
	case appwire.AppKind_Pubsub, appwire.AppKind_Telemetry:
		return g.role == protocol.AuthRole_ClientLegacy || g.role == protocol.AuthRole_RunnerLegacy
	case appwire.AppKind_TaskControl:
		// TaskControlRequest starts with a one-byte TaskControlKind.
		return g.role == protocol.AuthRole_ClientLegacy && len(data) > 1 &&
			protocol.TaskControlKind(data[1]) != protocol.TaskControlKind_ClientHello
	case appwire.AppKind_RunnerControl:
		// RunnerMessage starts with a one-byte RunnerMessageType.
		return g.role == protocol.AuthRole_RunnerLegacy && len(data) > 1 &&
			protocol.RunnerMessageType(data[1]) != protocol.RunnerMessageType_Hello
	default:
		return false
	}
}

// binderKey selects which secret the request's binder must prove. Operator-
// surface clients (role=Client, kind != agent: cli/tui/webui) prove operatorPSK
// when it is configured; agents (kind=agent) and runners prove the shared
// connect psk. When operatorPSK is empty the operator surface falls back to the
// connect psk — the legacy behaviour — so a deployment that has not configured
// an operator secret is unchanged (and a startup warning is emitted elsewhere).
func (g *pskGate) binderKey(req *protocol.PskAuthRequest) []byte {
	if req.Role == protocol.AuthRole_ClientLegacy && len(g.operatorPSK) > 0 {
		if hello := req.ClientHelloLegacy(); hello != nil && hello.Kind != protocol.ClientKind_Agent {
			return g.operatorPSK
		}
	}
	return g.psk
}

// respond encodes and sends [AppKind_PskAuth] + PskAuthResponse{status, sig}.
// Rejections are signed too: an unsigned bad_psk is exactly what an impostor
// would send to stop a runner for good.
func (g *pskGate) respond(sendFn func([]byte), transcript []byte, status protocol.PskAuthStatus) {
	resp := protocol.PskAuthResponse{Status: status}
	if g.serverKey != nil {
		resp.SetServerSig(cli.SignServerAuth(g.serverKey, transcript, status))
	}
	out, err := resp.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
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
		g.respond(sendFn, transcript, protocol.PskAuthStatus_NoIdentity)
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
			g.respond(sendFn, transcript, protocol.PskAuthStatus_BadPsk)
			return true, true, nil
		}
	}

	// Step 3: Identity required (fail-closed).
	// At least one role+hello must be present.
	switch req.Role {
	case protocol.AuthRole_ClientLegacy:
		if req.ClientHelloLegacy() == nil {
			slog.Warn("pskGate: client role but ClientHello is nil (identity required)")
			g.respond(sendFn, transcript, protocol.PskAuthStatus_NoIdentity)
			return true, true, nil
		}
	case protocol.AuthRole_RunnerLegacy:
		rh := req.RunnerHello()
		if rh == nil {
			slog.Warn("pskGate: runner role but RunnerHello is nil (identity required)")
			g.respond(sendFn, transcript, protocol.PskAuthStatus_NoIdentity)
			return true, true, nil
		}
		// A zero runner_id is an ABSENT identity, not a valid one, and it has to
		// be refused here rather than absorbed downstream. The board keys every
		// task's ticket by (runner identity, task id) and RegisterTask
		// overwrites, so two runners both claiming the zero identity would share
		// one key namespace: dispatching to the second would invalidate the
		// credential the first one's agent is already holding.
		//
		// NoIdentity is the right status because it is RETRYABLE
		// (cli/persist.go, PskRejectedError.Retryable). The runner that sends a
		// zero id is one built before the field existed, so it reconnects and
		// self-heals once the pair is consistent instead of exiting — the
		// behaviour d4f7a5a put in place after a wire skew killed twelve slots.
		if rh.RunnerId.IsZero() {
			slog.Warn("pskGate: runner hello carries a zero runner_id (identity required)",
				"hostname", string(rh.Hostname))
			g.respond(sendFn, transcript, protocol.PskAuthStatus_NoIdentity)
			return true, true, nil
		}
	default:
		// Unknown role — fail-closed.
		slog.Warn("pskGate: unknown role", "role", req.Role)
		g.respond(sendFn, transcript, protocol.PskAuthStatus_NoIdentity)
		return true, true, nil
	}

	// Step 4: Ticket validation (client role + kind=agent only).
	// This closes the bad-ticket→operator fallback: an agent presenting an
	// invalid ticket must NOT proceed as operator.
	if req.Role == protocol.AuthRole_ClientLegacy {
		hello := req.ClientHelloLegacy()
		if hello.Kind == protocol.ClientKind_Agent {
			info := hello.AgentInfo()
			if info == nil {
				slog.Warn("pskGate: agent kind but AgentInfo is nil")
				g.respond(sendFn, transcript, protocol.PskAuthStatus_BadTicket)
				return true, true, nil
			}
			if g.ValidateTicket != nil {
				if s := g.ValidateTicket(info); s != protocol.PskAuthStatus_Ok {
					g.respond(sendFn, transcript, s)
					return true, true, nil
				}
			}
			// nil ValidateTicket degrades to Ok (test-wiring without a Board).
		}
	}

	// Step 5: Accept — mark authed, reply ok, return decoded request for dispatch.
	g.role = req.Role
	g.authed = true
	g.respond(sendFn, transcript, protocol.PskAuthStatus_Ok)
	return true, false, &req
}
