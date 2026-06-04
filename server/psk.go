package server

import (
	"crypto/subtle"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// pskGate enforces a PSK handshake on each connection.
// authed starts true when no PSK is configured so callers need no extra branch.
type pskGate struct {
	psk    []byte
	authed bool
}

func newPSKGate(psk []byte) *pskGate {
	return &pskGate{psk: psk, authed: len(psk) == 0}
}

func (g *pskGate) Authed() bool { return g.authed }

// Check examines one incoming message against the PSK gate.
// sendFn writes response bytes back to the connection (may be called zero or one times).
// Returns (isPSKMessage, shouldClose).
// When authed is already true, returns (false, false) for every message — the gate is open.
//
// transcript is this connection's objproto handshake transcript
// (Connection.GetTranscript()). The client sends a transcript-bound binder
// rather than the raw PSK; the gate recomputes the expected binder over its own
// transcript and compares. Because an active MITM's two legs have different
// transcripts, a binder relayed from the client leg fails this check.
func (g *pskGate) Check(data, transcript []byte, sendFn func([]byte)) (isPSKMsg bool, shouldClose bool) {
	if g.authed {
		return false, false
	}
	if len(data) == 0 {
		return false, true
	}
	kind := wire.ApplicationPayloadKind(data[0])
	if kind != wire.ApplicationPayloadKind_PskAuth {
		return false, true
	}
	status := wire.PskAuthStatus_BadPsk
	if expected, err := objproto.ComputePSKBinder(g.psk, transcript); err == nil &&
		subtle.ConstantTimeCompare(data[1:], expected) == 1 {
		status = wire.PskAuthStatus_Ok
	}
	sendFn([]byte{byte(wire.ApplicationPayloadKind_PskAuth), byte(status)})
	if status == wire.PskAuthStatus_Ok {
		g.authed = true
		return true, false
	}
	return true, true
}
