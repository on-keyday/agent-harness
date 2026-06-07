package runner

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"testing"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
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

// buildTestEndpoint creates a Mutual-mode WebSocket endpoint on a random
// port and returns it together with the address it is listening on.
func buildTestEndpoint(t *testing.T) (objproto.Endpoint, string) {
	t.Helper()
	mux := http.NewServeMux()
	ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   "/ws",
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatalf("buildTestEndpoint: WebSocketEndpoint: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("buildTestEndpoint: Listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ep, ln.Addr().String()
}

// TestHandleEstablishRelay_EagerSetProxy_Ok: on a valid request, the eager
// path must install a proxySettings entry before sendResponse is called.
func TestHandleEstablishRelay_EagerSetProxy_Ok(t *testing.T) {
	ctx := context.Background()
	ep, addr := buildTestEndpoint(t)

	ap := netip.MustParseAddrPort(addr)
	serverCID := objproto.NewConnectionID("ws", ap, 0x0001)
	st := &relayHandlerState{serverCID: serverCID}

	var req protocol.EstablishRelayRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 5})
	req.Target.Port = 8540
	const slot = uint16(0x4242)
	req.SlotId = slot

	var gotResp protocol.EstablishRelayResponse
	handleEstablishRelay(ctx, nil, st, ep, req, func(resp protocol.EstablishRelayResponse) error {
		gotResp = resp
		return nil
	})

	if gotResp.Status != protocol.EstablishRelayStatus_Ok {
		t.Fatalf("response status: got %v want Ok", gotResp.Status)
	}

	// Verify proxySettings entry was installed eagerly.
	proxies := ep.ListProxies()
	ownedCID := objproto.NewConnectionID("ws", ap, slot)
	targetAddr := netip.MustParseAddrPort("10.0.0.5:8540")
	allocCID := objproto.NewConnectionID("ws", targetAddr, slot)
	found := false
	for _, p := range proxies {
		if (p.Peer1 == ownedCID && p.Peer2 == allocCID) ||
			(p.Peer1 == allocCID && p.Peer2 == ownedCID) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("proxySettings: expected entry (owned=%s, alloc=%s) not found; proxies: %v",
			ownedCID, allocCID, proxies)
	}
}

// TestHandleEstablishRelay_SlotCollision_NoProxy: slot_id collision must not
// install a SetProxy entry and must return SlotCollision status.
func TestHandleEstablishRelay_SlotCollision_NoProxy(t *testing.T) {
	ctx := context.Background()
	ep, addr := buildTestEndpoint(t)

	ap := netip.MustParseAddrPort(addr)
	const slot = uint16(0xABCD)
	serverCID := objproto.NewConnectionID("ws", ap, slot)
	st := &relayHandlerState{serverCID: serverCID}

	var req protocol.EstablishRelayRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 5})
	req.Target.Port = 8540
	req.SlotId = slot // same as serverCID.ID → collision

	var gotResp protocol.EstablishRelayResponse
	handleEstablishRelay(ctx, nil, st, ep, req, func(resp protocol.EstablishRelayResponse) error {
		gotResp = resp
		return nil
	})

	if gotResp.Status != protocol.EstablishRelayStatus_SlotCollision {
		t.Fatalf("response status: got %v want SlotCollision", gotResp.Status)
	}

	proxies := ep.ListProxies()
	if len(proxies) != 0 {
		t.Errorf("proxySettings: expected no entries on SlotCollision, got %v", proxies)
	}
}
