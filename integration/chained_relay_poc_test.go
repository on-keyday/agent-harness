//go:build integration

// Chained relay POC. Verifies the wire-level mechanics of multi-hop
// SetProxy chains (3-hop and 4-hop). Uses the synthetic-owned SetProxy
// relaxation directly (no per-hop initial ECDH dance).
//
// The chain:
//
//   initiator → P1 → P2 → ... → target
//
// Each Pn has a SetProxy:
//   owned    = (lower-hop's src addr toward Pn, slot)
//   allocate = (next-hop's listen addr from Pn, slot)
//
// "lower-hop's src addr toward Pn" is the ephemeral outbound port the
// lower hop uses when it dials Pn. We learn it by establishing each
// inter-hop WS conn first (single throwaway Handshake per pair), reading
// the resulting activeConn's CID at the upper end, and using its addr
// component for the next SetProxy's owned.
//
// Once all SetProxy entries are in place, initiator does SendHandshake
// at (P1.Addr, slot). The packet is forwarded through every Pn's
// proxySettings and arrives at target. Target processes the Handshake
// locally (Mutual mode), ECDH's with initiator's pubkey, sends
// HandshakeAck back along the chain. Initiator's sentHandshake completes
// with an end-to-end peer.Conn to target.
//
// To prove end-to-end:
//   - Send an AgentMessage from initiator on the new conn.
//   - Receive at target, verify the AEAD decrypts correctly (= shared
//     key is initiator↔target, not initiator↔any-Pn).

package integration

import (
	"context"
	"crypto/ecdh"
	"log/slog"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// chainEndpoint bundles one endpoint with the listen-addr the next-lower
// hop can dial.
type chainEndpoint struct {
	name   string
	addr   string // listen host:port for proxies/target; empty for initiator
	ep     objproto.Endpoint
	cancel func()
}

// startMutualEndpoint builds a Mutual-mode WS endpoint listening on addr.
// Returns the endpoint and a cleanup func.
func startMutualEndpoint(t *testing.T, name, addr, wsPath string) chainEndpoint {
	t.Helper()
	mux := http.NewServeMux()
	ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
		Logger: slog.Default().With("ep", name),
		Path:   wsPath,
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatalf("startMutualEndpoint(%s): %v", name, err)
	}
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	cancel := func() {
		c, cancelFn := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancelFn()
		_ = srv.Shutdown(c)
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)
	return chainEndpoint{name: name, addr: addr, ep: ep, cancel: cancel}
}

// startClientEndpoint builds a Mutual-mode endpoint with no listener
// (acts as the initiator). Used for the leftmost endpoint in the chain.
// Path must match the upper endpoints' listen path so outbound dials
// hit the right WS handler.
func startClientEndpoint(t *testing.T, name, wsPath string) chainEndpoint {
	t.Helper()
	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: slog.Default().With("ep", name),
		Path:   wsPath,
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatalf("startClientEndpoint(%s): %v", name, err)
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)
	return chainEndpoint{name: name, ep: ep, cancel: func() {}}
}

// throwawayDial opens a WS conn lower→upper by doing one ECDH Handshake.
// Returns upper's view CID (= the lower's src addr from upper, plus the
// throwaway slot). The activeConn at upper end gets closed so it does not
// interfere with the chain SetProxy entries we install later.
func throwawayDial(t *testing.T, lower, upper chainEndpoint, throwawaySlot uint16) objproto.ConnectionID {
	t.Helper()
	upperCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort(upper.addr), throwawaySlot)
	priv, hs, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatalf("throwawayDial(%s→%s) NewECDHHandshake: %v", lower.name, upper.name, err)
	}
	ch, err := lower.ep.SendHandshake(upperCID, priv, hs)
	if err != nil {
		t.Fatalf("throwawayDial(%s→%s) SendHandshake: %v", lower.name, upper.name, err)
	}
	var lowerConn objproto.Connection
	select {
	case lowerConn = <-ch.C:
	case <-time.After(5 * time.Second):
		t.Fatalf("throwawayDial(%s→%s) lower-side completion timeout", lower.name, upper.name)
	}
	var upperConn objproto.Connection
	select {
	case upperConn = <-upper.ep.GetNewActiveConnectionChannel():
	case <-time.After(5 * time.Second):
		t.Fatalf("throwawayDial(%s→%s) upper-side accept timeout", lower.name, upper.name)
	}
	t.Logf("throwawayDial(%s→%s): upper-view cid=%v lower-view cid=%v",
		lower.name, upper.name, upperConn.ConnectionID(), lowerConn.ConnectionID())
	// Close the activeConns we just made — chain uses synthetic SetProxy,
	// these were only to learn the inter-hop addrs and populate connMap.
	_ = lowerConn.Close()
	_ = upperConn.Close()
	return upperConn.ConnectionID()
}

func TestChainedRelayPOC_3hop(t *testing.T) {
	if testing.Short() {
		t.Skip("POC")
	}
	const (
		p1Addr     = "127.0.0.1:18700"
		p2Addr     = "127.0.0.1:18701"
		targetAddr = "127.0.0.1:18702"
		chainSlot  = uint16(0xC001)
		thrSlot1   = uint16(0xD001)
		thrSlot2   = uint16(0xD002)
		thrSlot3   = uint16(0xD003)
		wsPath     = "/ws"
	)

	// Endpoints
	target := startMutualEndpoint(t, "target", targetAddr, wsPath)
	defer target.cancel()
	p2 := startMutualEndpoint(t, "P2", p2Addr, wsPath)
	defer p2.cancel()
	p1 := startMutualEndpoint(t, "P1", p1Addr, wsPath)
	defer p1.cancel()
	initiator := startClientEndpoint(t, "initiator", wsPath)
	defer initiator.cancel()

	time.Sleep(300 * time.Millisecond) // listeners bind

	// Throwaway dials to establish inter-hop WS conns + learn src addrs.
	// Each returns the upper-side view CID — the addr field is the
	// lower-hop's outbound ephemeral port toward this upper hop.
	initiatorFromP1Cid := throwawayDial(t, initiator, p1, thrSlot1)
	p1FromP2Cid := throwawayDial(t, p1, p2, thrSlot2)
	p2FromTargetCid := throwawayDial(t, p2, target, thrSlot3)

	// Set up synthetic SetProxy entries at each intermediate hop.
	// Each owned uses the addr learned from the throwaway dial, with the
	// REAL chain slot_id. Each allocate is synthetic (next hop's listen
	// addr, REAL chain slot_id).
	p1Owned := objproto.NewConnectionID("ws", initiatorFromP1Cid.Addr, chainSlot)
	p1Allocate := objproto.NewConnectionID("ws", netip.MustParseAddrPort(p2Addr), chainSlot)
	if err := p1.ep.SetProxy(p1Owned, p1Allocate); err != nil {
		t.Fatalf("P1.SetProxy: %v", err)
	}
	t.Logf("P1.SetProxy: owned=%v allocate=%v", p1Owned, p1Allocate)

	p2Owned := objproto.NewConnectionID("ws", p1FromP2Cid.Addr, chainSlot)
	p2Allocate := objproto.NewConnectionID("ws", netip.MustParseAddrPort(targetAddr), chainSlot)
	if err := p2.ep.SetProxy(p2Owned, p2Allocate); err != nil {
		t.Fatalf("P2.SetProxy: %v", err)
	}
	t.Logf("P2.SetProxy: owned=%v allocate=%v", p2Owned, p2Allocate)
	_ = p2FromTargetCid // unused here; target is the endpoint, no proxy setup

	// Initiator does SendHandshake at (P1.Addr, chainSlot). The packet is
	// forwarded by P1's proxySettings → P2 → target. Target processes the
	// Handshake locally (Mutual mode) and ECDHs with initiator's pubkey,
	// then sends HandshakeAck back through the chain.
	p1Dst := objproto.NewConnectionID("ws", netip.MustParseAddrPort(p1Addr), chainSlot)
	priv, hs, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatalf("NewECDHHandshake initiator: %v", err)
	}
	ch, err := initiator.ep.SendHandshake(p1Dst, priv, hs)
	if err != nil {
		t.Fatalf("initiator.SendHandshake: %v", err)
	}
	var initEnd2EndConn objproto.Connection
	select {
	case initEnd2EndConn = <-ch.C:
	case <-time.After(10 * time.Second):
		t.Fatal("initiator: end-to-end Handshake completion timeout (= chain broken)")
	}
	t.Logf("initiator: end-to-end conn ready: cid=%v", initEnd2EndConn.ConnectionID())

	// Target should have accepted the conn from its perspective.
	var targetEnd2EndConn objproto.Connection
	select {
	case targetEnd2EndConn = <-target.ep.GetNewActiveConnectionChannel():
	case <-time.After(2 * time.Second):
		t.Fatal("target: did not receive end-to-end conn (= ECDH did not reach target)")
	}
	t.Logf("target: end-to-end conn ready: cid=%v", targetEnd2EndConn.ConnectionID())

	// Send a small Application payload initiator → target. AEAD must
	// validate at target with initiator's keys (proves end-to-end ECDH,
	// not relayed via local decrypt at any hop).
	payload := []byte{byte(wire.ApplicationPayloadKind_AgentMessage), 0xDE, 0xAD, 0xBE, 0xEF}
	if _, _, err := initEnd2EndConn.SendMessage(payload); err != nil {
		t.Fatalf("initiator SendMessage on end-to-end conn: %v", err)
	}
	msg, err := targetEnd2EndConn.ReceiveMessage()
	if err != nil {
		t.Fatalf("target ReceiveMessage: %v", err)
	}
	if len(msg.Data) < 5 ||
		msg.Data[0] != byte(wire.ApplicationPayloadKind_AgentMessage) ||
		msg.Data[1] != 0xDE || msg.Data[2] != 0xAD || msg.Data[3] != 0xBE || msg.Data[4] != 0xEF {
		t.Fatalf("unexpected payload at target after chain: % x", msg.Data)
	}
	t.Log("3-hop chained relay: end-to-end roundtrip CONFIRMED")
}

func TestChainedRelayPOC_4hop(t *testing.T) {
	if testing.Short() {
		t.Skip("POC")
	}
	const (
		p1Addr     = "127.0.0.1:18710"
		p2Addr     = "127.0.0.1:18711"
		p3Addr     = "127.0.0.1:18712"
		targetAddr = "127.0.0.1:18713"
		chainSlot  = uint16(0xC002)
		wsPath     = "/ws"
	)
	target := startMutualEndpoint(t, "target", targetAddr, wsPath)
	defer target.cancel()
	p3 := startMutualEndpoint(t, "P3", p3Addr, wsPath)
	defer p3.cancel()
	p2 := startMutualEndpoint(t, "P2", p2Addr, wsPath)
	defer p2.cancel()
	p1 := startMutualEndpoint(t, "P1", p1Addr, wsPath)
	defer p1.cancel()
	initiator := startClientEndpoint(t, "initiator", wsPath)
	defer initiator.cancel()

	time.Sleep(300 * time.Millisecond)

	initiatorFromP1Cid := throwawayDial(t, initiator, p1, 0xE001)
	p1FromP2Cid := throwawayDial(t, p1, p2, 0xE002)
	p2FromP3Cid := throwawayDial(t, p2, p3, 0xE003)
	_ = throwawayDial(t, p3, target, 0xE004)

	for _, setup := range []struct {
		name  string
		ep    objproto.Endpoint
		owned objproto.ConnectionID
		alloc objproto.ConnectionID
	}{
		{"P1", p1.ep,
			objproto.NewConnectionID("ws", initiatorFromP1Cid.Addr, chainSlot),
			objproto.NewConnectionID("ws", netip.MustParseAddrPort(p2Addr), chainSlot)},
		{"P2", p2.ep,
			objproto.NewConnectionID("ws", p1FromP2Cid.Addr, chainSlot),
			objproto.NewConnectionID("ws", netip.MustParseAddrPort(p3Addr), chainSlot)},
		{"P3", p3.ep,
			objproto.NewConnectionID("ws", p2FromP3Cid.Addr, chainSlot),
			objproto.NewConnectionID("ws", netip.MustParseAddrPort(targetAddr), chainSlot)},
	} {
		if err := setup.ep.SetProxy(setup.owned, setup.alloc); err != nil {
			t.Fatalf("%s.SetProxy: %v", setup.name, err)
		}
		t.Logf("%s.SetProxy: owned=%v allocate=%v", setup.name, setup.owned, setup.alloc)
	}

	p1Dst := objproto.NewConnectionID("ws", netip.MustParseAddrPort(p1Addr), chainSlot)
	priv, hs, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatalf("NewECDHHandshake: %v", err)
	}
	ch, err := initiator.ep.SendHandshake(p1Dst, priv, hs)
	if err != nil {
		t.Fatalf("initiator.SendHandshake: %v", err)
	}
	var initEnd2EndConn objproto.Connection
	select {
	case initEnd2EndConn = <-ch.C:
	case <-time.After(10 * time.Second):
		t.Fatal("initiator: end-to-end Handshake completion timeout (4-hop chain broken)")
	}
	var targetEnd2EndConn objproto.Connection
	select {
	case targetEnd2EndConn = <-target.ep.GetNewActiveConnectionChannel():
	case <-time.After(2 * time.Second):
		t.Fatal("target: did not receive end-to-end conn (4-hop)")
	}

	payload := []byte{byte(wire.ApplicationPayloadKind_AgentMessage), 0x12, 0x34, 0x56, 0x78}
	if _, _, err := initEnd2EndConn.SendMessage(payload); err != nil {
		t.Fatalf("initiator SendMessage: %v", err)
	}
	msg, err := targetEnd2EndConn.ReceiveMessage()
	if err != nil {
		t.Fatalf("target ReceiveMessage: %v", err)
	}
	if len(msg.Data) < 5 ||
		msg.Data[0] != byte(wire.ApplicationPayloadKind_AgentMessage) ||
		msg.Data[1] != 0x12 || msg.Data[2] != 0x34 || msg.Data[3] != 0x56 || msg.Data[4] != 0x78 {
		t.Fatalf("unexpected payload at target after 4-hop chain: % x", msg.Data)
	}
	t.Log("4-hop chained relay: end-to-end roundtrip CONFIRMED")
}
