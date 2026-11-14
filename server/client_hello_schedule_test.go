package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// S-D18: no ClientHello may claim ClientKind.schedule. The value is the
// server's own mark on a task a schedule created or resumed. Both hello paths
// (the standalone ClientHello and the merged PSK gate) end in
// RecordClientIdentity, so one refusal there covers both.
func TestClientHelloRefusesScheduleKind(t *testing.T) {
	h := &TaskHandler{}
	cidStr := "ws:127.0.0.1:8539-91"
	fc := &fakeConn{id: objproto.MustParseConnectionID(cidStr)}
	st := h.RecordClientIdentity(cidStr, fc, &protocol.ClientHello{Kind: protocol.ClientKind_Schedule})
	if st != protocol.ClientHelloStatus_NotPermitted {
		t.Fatalf("status = %v, want not_permitted", st)
	}
	if k := h.lookupClientKind(cidStr); k != protocol.ClientKind_Unspecified {
		t.Fatalf("a refused hello was recorded as kind %v", k)
	}
	// Control: a legitimate kind on the same handler is still accepted.
	if st := h.RecordClientIdentity(cidStr, fc, &protocol.ClientHello{Kind: protocol.ClientKind_Cli}); st != protocol.ClientHelloStatus_Ok {
		t.Fatalf("cli hello refused: %v", st)
	}
}
