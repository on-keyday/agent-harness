package server

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/hex"
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
	case protocol.AuthRole_Client:
		hello := req.ClientHello()
		if hello == nil {
			return false
		}
		if d.RecordClientIdentity != nil {
			return d.RecordClientIdentity(conn.ConnectionID().String(), conn, hello) == protocol.ClientHelloStatus_Ok
		}
		tcReq := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
		tcReq.SetClientHello(*hello)
		data, err := tcReq.Append([]byte{byte(appwire.AppKind_TaskControl)})
		if err != nil {
			slog.Error("pskDispatchIdentity: encode TaskControlRequest failed", "err", err)
			return false
		}
		d.Dispatch(conn, data)
		return true

	case protocol.AuthRole_Runner:
		a := req.RunnerAuth()
		if a == nil {
			return false
		}
		rmsg := protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
		rmsg.SetHello(a.Hello)
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

// pskGate enforces the merged authentication + identity handshake on each
// connection. The first message must be [0x45]+PskAuthRequest (brgen-encoded),
// and what it must prove depends on the role and kind:
//   - an operator kind (cli/tui/webui): a binder keyed by the operator PSK
//     (nothing when none is configured: --dangerously-permit-no-operator-psk);
//   - role=client, kind=agent: a binder keyed by the task's ticket (TicketFor);
//   - role=runner: an Ed25519 signature by an enrolled key (RunnerAuth).
//
// The two roles every binary spoke before runner enrollment (AuthRole 0 and 1)
// are gone: a request carrying one decodes to no arm and is refused
// no_identity, which a runner retries.
//
// An identity is required in every case.
//
// authed starts false until Check succeeds.
type pskGate struct {
	authed bool
	// role is fixed by the accepted handshake, never by later messages.
	role protocol.AuthRole
	// runnerKey is the enrolled key a role=runner connection signed with;
	// nil for every other connection.
	runnerKey ed25519.PublicKey
	// runnerKeys is the set of keys allowed to register under role=runner.
	runnerKeys *RunnerKeyStore
	// cid names the connection in the gate's own log lines.
	cid string
	// operatorPSK is the secret operator-surface connections (kind=cli/tui/
	// webui, i.e. anything but agent) must prove via the binder. It is NEVER
	// injected into agent task environments, so an in-task agent cannot forge
	// an operator binder and thus cannot claim operator authority by sending
	// an operator kind. Empty only under --dangerously-permit-no-operator-psk.
	operatorPSK []byte
	// serverKey signs every PskAuthResponse so a client holding the public
	// half can tell this server from an impostor. server.New guarantees one.
	serverKey ed25519.PrivateKey
	// TicketFor is the ticket a role=client agent's binder must be keyed by.
	// nil (test wiring without a Board) refuses every role=client agent.
	TicketFor func(rid protocol.RunnerID, tid protocol.TaskID) ([16]byte, bool)
}

func newPSKGate() *pskGate {
	// authed is always false initially: every connection must complete the
	// identity handshake before any other message is processed.
	return &pskGate{}
}

func (g *pskGate) Authed() bool { return g.authed }

// Role is the authenticated role, AuthRole_Client or AuthRole_Runner.
// Meaningful only once Authed.
func (g *pskGate) Role() protocol.AuthRole { return g.role }

// agentBinderOK checks a role=client agent's binder against the ticket the
// board holds for its (runner, task).
func (g *pskGate) agentBinderOK(hello *protocol.ClientHello, binder, transcript []byte) bool {
	info := hello.AgentInfo()
	if info == nil || g.TicketFor == nil {
		return false
	}
	ticket, ok := g.TicketFor(info.RunnerId, info.TaskId)
	if !ok {
		return false
	}
	want, err := cli.ComputePSKBinder(ticket[:], transcript)
	return err == nil && subtle.ConstantTimeCompare(binder, want) == 1
}

func (g *pskGate) keyEnrolled(key ed25519.PublicKey) bool {
	if g.runnerKeys == nil {
		return false
	}
	_, ok := g.runnerKeys.Lookup(key)
	return ok
}

// RunnerKey is the enrolled key a role=runner connection proved; nil otherwise.
func (g *pskGate) RunnerKey() ed25519.PublicKey { return g.runnerKey }

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
		return g.role == protocol.AuthRole_Client || g.role == protocol.AuthRole_Runner
	case appwire.AppKind_TaskControl:
		// TaskControlRequest starts with a one-byte TaskControlKind.
		return g.role == protocol.AuthRole_Client && len(data) > 1 &&
			protocol.TaskControlKind(data[1]) != protocol.TaskControlKind_ClientHello
	case appwire.AppKind_RunnerControl:
		// RunnerMessage starts with a one-byte RunnerMessageType.
		return g.role == protocol.AuthRole_Runner && len(data) > 1 &&
			protocol.RunnerMessageType(data[1]) != protocol.RunnerMessageType_Hello
	default:
		return false
	}
}

// binderKey selects the secret a binder must prove: the operator PSK, for
// operator-surface clients (kind cli/tui/webui). Nothing else proves a shared
// secret: an agent's binder is keyed by its ticket (agentBinderOK) and a
// runner signs (RunnerAuth). nil = no binder to check; with no operator PSK
// configured (--dangerously-permit-no-operator-psk) operator surfaces prove
// nothing.
func (g *pskGate) binderKey(req *protocol.PskAuthRequest) []byte {
	if req.Role == protocol.AuthRole_Client {
		if h := req.ClientHello(); h != nil && h.Kind != protocol.ClientKind_Agent {
			return g.operatorPSK
		}
	}
	return nil
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
//  2. Binder verification (before any identity action); for kind=agent this
//     is the ticket check, for role=runner the signature and enrollment (2b)
//  3. Identity required (fail-closed)
//  4. Accept: mark authed, reply ok, return decoded request for caller dispatch
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

	// Step 2: Binder verification (before any identity action). binderKey
	// names the secret an operator kind proves (the operator PSK, which no
	// agent holds); nil means there is no binder to check, and the identity
	// handshake still runs.
	// role=client, kind=agent: the binder is keyed by the task's ticket, so
	// proving it IS the ticket check. A wrong or missing ticket is bad_ticket.
	if hello := req.ClientHello(); req.Role == protocol.AuthRole_Client && hello != nil && hello.Kind == protocol.ClientKind_Agent {
		if !g.agentBinderOK(hello, req.Binder, transcript) {
			g.respond(sendFn, transcript, protocol.PskAuthStatus_BadTicket)
			return true, true, nil
		}
	} else if key := g.binderKey(&req); len(key) > 0 {
		expected, err := cli.ComputePSKBinder(key, transcript)
		if err != nil || subtle.ConstantTimeCompare(req.Binder, expected) != 1 {
			g.respond(sendFn, transcript, protocol.PskAuthStatus_BadPsk)
			return true, true, nil
		}
	}

	// Step 2b: an enrolled runner key (role=runner). The signature is checked
	// before the key is looked up, so the not_enrolled answer is given only to
	// a peer that holds the private half of the key it names.
	if req.Role == protocol.AuthRole_Runner {
		auth := req.RunnerAuth()
		if auth == nil {
			g.respond(sendFn, transcript, protocol.PskAuthStatus_NoIdentity)
			return true, true, nil
		}
		key := ed25519.PublicKey(auth.Key[:])
		if !cli.VerifyRunnerAuth(key, transcript, auth.Sig[:]) {
			g.respond(sendFn, transcript, protocol.PskAuthStatus_BadPsk)
			return true, true, nil
		}
		if !g.keyEnrolled(key) {
			slog.Warn("runner key not enrolled", "key", hex.EncodeToString(key),
				"hostname", string(auth.Hello.Hostname), "cid", g.cid,
				"hint", "harness-cli runner enroll "+hex.EncodeToString(key))
			g.respond(sendFn, transcript, protocol.PskAuthStatus_NotEnrolled)
			return true, true, nil
		}
		g.runnerKey = append(ed25519.PublicKey(nil), key...)
	}

	// Step 3: Identity required (fail-closed).
	// At least one role+hello must be present.
	switch req.Role {
	case protocol.AuthRole_Client:
		if req.ClientHello() == nil {
			slog.Warn("pskGate: client role but ClientHello is nil (identity required)")
			g.respond(sendFn, transcript, protocol.PskAuthStatus_NoIdentity)
			return true, true, nil
		}
	case protocol.AuthRole_Runner:
		// Step 2b has refused a request with no RunnerAuth.
		rh := &req.RunnerAuth().Hello
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

	// Step 4: Accept — mark authed, reply ok, return decoded request for dispatch.
	g.role = req.Role
	g.authed = true
	g.respond(sendFn, transcript, protocol.PskAuthStatus_Ok)
	return true, false, &req
}
