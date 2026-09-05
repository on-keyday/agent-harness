package cli

import (
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/appwire"
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

// The hello has to say data_plane and carry the grant verbatim, because that is
// the only thing the runner has to match against what the server pushed it.
func TestDataPlaneHelloCarriesTheGrant(t *testing.T) {
	var req protocol.PskAuthRequest
	req.Role = protocol.AuthRole_Client
	var hello protocol.ClientHello
	hello.Kind = protocol.ClientKind_DataPlane
	hello.SetDataPlaneInfo(protocol.DataPlaneInfo{
		GrantId: [16]uint8{7, 7},
		TaskId:  protocol.TaskID{Id: [16]uint8{3}},
	})
	req.SetClientHello(hello)
	data := req.MustAppend([]byte{byte(appwire.AppKind_PskAuth)})

	if appwire.AppKind(data[0]) != appwire.AppKind_PskAuth {
		t.Fatalf("wrong envelope kind: %d", data[0])
	}
	var back protocol.PskAuthRequest
	if err := back.DecodeExact(data[1:]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	h := back.ClientHello()
	if h == nil || h.Kind != protocol.ClientKind_DataPlane {
		t.Fatalf("hello kind lost: %+v", h)
	}
	info := h.DataPlaneInfo()
	if info == nil || info.GrantId != ([16]uint8{7, 7}) || info.TaskId.Id != ([16]uint8{3}) {
		t.Fatalf("grant lost: %+v", info)
	}
}
