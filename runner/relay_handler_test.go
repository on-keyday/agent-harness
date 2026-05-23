package runner

import (
	"net/netip"
	"testing"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestRelayHandlerValidateSlotCollision: slot_id collides with the runner's
// own server-conn CID ID → SlotCollision.
func TestRelayHandlerValidateSlotCollision(t *testing.T) {
	const sharedID = 0xABCD
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8549"), sharedID)

	st := &relayHandlerState{serverCID: serverCID}
	var req protocol.EstablishRelayRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 5})
	req.Target.Port = 8540
	req.SlotId = sharedID

	resp := st.validate(req)
	if resp.Status != protocol.EstablishRelayStatus_SlotCollision {
		t.Errorf("status: got %v want SlotCollision", resp.Status)
	}
}

// TestRelayHandlerValidateInvalidTarget: empty transport on target → InvalidTarget.
func TestRelayHandlerValidateInvalidTarget(t *testing.T) {
	st := &relayHandlerState{
		serverCID: objproto.NewConnectionID("ws",
			netip.MustParseAddrPort("127.0.0.1:8549"), 0x0001),
	}
	var req protocol.EstablishRelayRequest
	// req.Target.Transport intentionally empty
	req.SlotId = 0x4242

	resp := st.validate(req)
	if resp.Status != protocol.EstablishRelayStatus_InvalidTarget {
		t.Errorf("status: got %v want InvalidTarget", resp.Status)
	}
}

// TestRelayHandlerValidateHappyPath: valid target + non-colliding slot → Ok.
func TestRelayHandlerValidateHappyPath(t *testing.T) {
	st := &relayHandlerState{
		serverCID: objproto.NewConnectionID("ws",
			netip.MustParseAddrPort("127.0.0.1:8549"), 0x0001),
	}
	var req protocol.EstablishRelayRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 5})
	req.Target.Port = 8540
	req.SlotId = 0x4242 // different from serverCID.ID

	resp := st.validate(req)
	if resp.Status != protocol.EstablishRelayStatus_Ok {
		t.Errorf("status: got %v want Ok", resp.Status)
	}
}

// TestExpectedRelaysPutTake: basic Put/Take round-trip on the expected-relays map.
func TestExpectedRelaysPutTake(t *testing.T) {
	er := newExpectedRelays()
	targetCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("10.0.0.5:8540"), 0xABCD)

	er.Put(0x4242, targetCID)

	got, ok := er.Take(0x4242)
	if !ok {
		t.Fatal("Take(0x4242) returned false")
	}
	if got != targetCID {
		t.Errorf("Take returned %v want %v", got, targetCID)
	}

	// Subsequent Take should miss (one-shot)
	_, ok = er.Take(0x4242)
	if ok {
		t.Errorf("Take(0x4242) second call returned true, want false (one-shot)")
	}
}

// TestExpectedRelaysTakeMissing: Take of a slot never Put returns false.
func TestExpectedRelaysTakeMissing(t *testing.T) {
	er := newExpectedRelays()
	_, ok := er.Take(0x9999)
	if ok {
		t.Errorf("Take(0x9999) on empty: got true, want false")
	}
}
