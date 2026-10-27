package cli

import (
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A zero grant is how the server says "I spliced this one", and the client must
// take the stream the server allocated rather than dialing anything.
func TestDataPlaneTargetUse(t *testing.T) {
	if (dataPlaneTarget{}).use() {
		t.Fatalf("a zero target must not route")
	}
	if (dataPlaneTarget{GrantID: [16]uint8{1}}).use() {
		t.Fatalf("a grant with no slot must not route")
	}
	if (dataPlaneTarget{SlotID: 5}).use() {
		t.Fatalf("a slot with no grant must not route")
	}
	if !(dataPlaneTarget{GrantID: [16]uint8{1}, SlotID: 5}).use() {
		t.Fatalf("a grant and a slot should route")
	}
}

// A withdrawn or stale authority is not a network fault and must not read as
// one: each refusal maps to an error a person can act on.
func TestDataPlaneStatusErrorDistinguishesRefusals(t *testing.T) {
	if err := dataPlaneStatusError(protocol.PskAuthStatus_Ok); err != nil {
		t.Fatalf("ok should be no error: %v", err)
	}
	if err := dataPlaneStatusError(protocol.PskAuthStatus_Expired); !errors.Is(err, ErrDataPlaneExpired) {
		t.Fatalf("expired: %v", err)
	}
	if err := dataPlaneStatusError(protocol.PskAuthStatus_NotPermitted); !errors.Is(err, ErrDataPlaneRevoked) {
		t.Fatalf("not_permitted: %v", err)
	}
	if err := dataPlaneStatusError(protocol.PskAuthStatus_BadPsk); !errors.Is(err, ErrDataPlaneRefused) {
		t.Fatalf("bad_psk: %v", err)
	}
	if err := dataPlaneStatusError(protocol.PskAuthStatus_BadTicket); !errors.Is(err, ErrDataPlaneRefused) {
		t.Fatalf("bad_ticket: %v", err)
	}
	// The three must not collapse into one another.
	if errors.Is(dataPlaneStatusError(protocol.PskAuthStatus_Expired), ErrDataPlaneRevoked) {
		t.Fatalf("expired and revoked are the same error")
	}
}
