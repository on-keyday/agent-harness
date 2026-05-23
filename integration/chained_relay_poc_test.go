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

// ============================================================================
// Realistic-addr variants below
// ============================================================================
//
// The two tests above (3hop / 4hop) use throwawayDial to learn inter-hop
// ephemeral addrs. That's artificial — in production, each hop's serverCID
// (= the addr the upstream uses for SetProxy.owned in chained-relay setup)
// is derived from the activeConn established during the hop's Phase C
// registration, NOT from a separate throwaway. The two tests below
// reproduce the full production-shaped sequence:
//
//   1. Register each hop via real Phase C ceremony (server-driven ECDH
//      forwarded through the chain, each hop's serverCID populated from
//      pc.Connection().ConnectionID() like driveAfterConn does).
//   2. Set up chained-relay SetProxy at each hop using the REAL
//      serverCID.Addr that registration captured. Allocate.addr =
//      target's DialedAddr (= the LISTEN_ADDR that server passed as the
//      target field when registering the runner; in production, this is
//      stored in RunnerEntry.DialedAddr per the spec's prerequisite).
//   3. Initiator does full Phase B agent-proxy ceremony with the leaf
//      runner (real initial ECDH, real SetProxy via the leaf's
//      runAgentProxyCeremony pattern, real RehandshakeForProxy).
//   4. Verify end-to-end roundtrip.
//
// What this validates: the addr-propagation chain works through real
// pc.Connection().ConnectionID() values, with no synthetic shortcuts.

// hopInfo captures the post-registration state of one hop in the chain.
//
// Knowledge-domain note:
//
//   - `serverCID` is the hop's OWN view of its upstream peer (= what its
//     `runner.Session.serverCID` would be in production). Only the hop
//     itself reads this directly. Server-side orchestrator code MUST NOT
//     access this field — it has no production way to know the value.
//
//   - `dialedAddr` is what server passed when registering this hop (= the
//     CLI argument to `dial-runner`, = the LISTEN_ADDR the upstream proxy
//     dialed). Server stores this in `RunnerEntry.DialedAddr` per the spec.
//     Server-side orchestrator code CAN read this — it's server's own state.
//
// To enforce the boundary in code, we wrap each role's logic in dedicated
// helpers (`hopComputeSetProxy` for hop-side, the test bodies for server-side
// orchestration). The hopInfo struct itself is shared, but cross-role
// access is restricted by which helper reads which field.
type hopInfo struct {
	endpoint   chainEndpoint
	serverCID  objproto.ConnectionID // HOP-PRIVATE — only the hop itself reads
	dialedAddr netip.AddrPort        // SERVER-VISIBLE — DialedAddr in RunnerEntry
}

// hopComputeSetProxy is the hop-side role: simulates what
// `handleEstablishRelay` does on a runner when it receives an
// EstablishRelay (or chained-relay) request from server. The hop reads its
// OWN serverCID, combines with the slot + target.DialedAddr received over
// the wire (= passed as targetDialedAddr here), and calls SetProxy on its
// own endpoint.
//
// Inputs that match the production `handleEstablishRelay` signature:
//   - self: the hop running the handler (reads self.serverCID, self.endpoint.ep)
//   - slot: from the request wire
//   - targetDialedAddr: from the request wire (= server-passed target.DialedAddr)
//
// Server-side code passes targetDialedAddr but does NOT touch self.serverCID.
func hopComputeSetProxy(t *testing.T, self *hopInfo, slot uint16, targetDialedAddr netip.AddrPort) {
	t.Helper()
	// self.serverCID.Addr is hop's OWN view of upstream — known to hop only.
	owned := objproto.NewConnectionID("ws", self.serverCID.Addr, slot)
	allocate := objproto.NewConnectionID("ws", targetDialedAddr, slot)
	if err := self.endpoint.ep.SetProxy(owned, allocate); err != nil {
		t.Fatalf("hopComputeSetProxy on %s: %v", self.endpoint.name, err)
	}
	t.Logf("hop %s computed SetProxy: slot=%d owned=%v allocate=%v",
		self.endpoint.name, slot, owned, allocate)
}

// leafComputeAgentProxySetProxy is the leaf-side role: what
// `runAgentProxyCeremony` does after an agent's initial ECDH lands and the
// ProxyRequest is received. Leaf reads its OWN serverCID + the live incoming
// activeConn's CID, then SetProxy. Matches Phase B's pattern.
func leafComputeAgentProxySetProxy(t *testing.T, leaf *hopInfo, agentSlot uint16, incomingConn objproto.Connection) {
	t.Helper()
	owned := incomingConn.ConnectionID()                                           // leaf knows from the accepted conn
	allocate := objproto.NewConnectionID("ws", leaf.serverCID.Addr, agentSlot)     // leaf knows own serverCID
	if err := leaf.endpoint.ep.SetProxy(owned, allocate); err != nil {
		t.Fatalf("leafComputeAgentProxySetProxy on %s: %v", leaf.endpoint.name, err)
	}
	t.Logf("leaf %s agent-proxy SetProxy: owned=%v allocate=%v",
		leaf.endpoint.name, owned, allocate)
}

// chainRegister walks an existing chain (server → intermediates... → target)
// and sets up the per-hop SetProxy + drives server's SendHandshake so the
// target gets registered via the chain. After completion, target.serverCID
// is populated from the real activeConn at target's end.
//
// chainAbove contains the existing intermediates from server-side to
// target-adjacent. Each must already be registered (serverCID populated).
//
// regSlot is the slot_id to use for the registration handshake.
//
// Wire-level flow:
//   - For each intermediate above target, set up SetProxy at regSlot:
//       owned    = (intermediate.serverCID.Addr, regSlot)
//       allocate = (next-hop-down.dialedAddr, regSlot)
//     The last intermediate's allocate.addr = target's listen-addr.
//   - server.SendHandshake to (top-intermediate.LISTEN_ADDR, regSlot).
//     With synthetic-owned + eager SetProxy, the Handshake is forwarded
//     raw through every hop's proxySettings to target.
//   - target.ep accepts the Handshake (Mutual mode), ECDHs with server's
//     pubkey, activeConn appears on newActiveSess.
//   - Read pc.Connection().ConnectionID() from target's side; that's the
//     real serverCID the runner would record in production.
// chainRegister drives a runner registration through (possibly empty) chain.
// Role separation:
//   - SERVER role: orchestrate per-hop EstablishRelay (each intermediate is
//     told the next downstream's DialedAddr), then SendHandshake to top.
//     Server NEVER reads hop.serverCID directly.
//   - HOP role: each intermediate's SetProxy is computed via
//     hopComputeSetProxy(self, ...), where the hop reads its OWN serverCID.
//   - TARGET role: receives the forwarded handshake, ECDHs with server's
//     pubkey, captures pc.Connection().ConnectionID() as its new serverCID.
func chainRegister(
	t *testing.T,
	server chainEndpoint,
	chainAbove []*hopInfo,
	target chainEndpoint,
	regSlot uint16,
) *hopInfo {
	t.Helper()

	// SERVER role: iterate the chain, for each intermediate compute downstream's
	// DialedAddr (= server's own knowledge from CLI / RunnerEntry.DialedAddr),
	// then ask that hop to run its handler. Server passes ONLY targetDialedAddr.
	for i, hop := range chainAbove {
		var downstreamDialedAddr netip.AddrPort
		if i == len(chainAbove)-1 {
			// Last intermediate's downstream is the target being registered now.
			// Server knows target's listen addr from the CLI's dial-runner arg.
			downstreamDialedAddr = netip.MustParseAddrPort(target.addr)
		} else {
			// Server reads next hop's DialedAddr from its own RunnerEntry registry.
			downstreamDialedAddr = chainAbove[i+1].dialedAddr
		}
		hopComputeSetProxy(t, hop, regSlot, downstreamDialedAddr)
	}

	// SERVER role: determine SendHandshake destination.
	//   - With intermediates: send to the top intermediate's LISTEN_ADDR (= server's
	//     knowledge of its first hop, the direct-registered runner's DialedAddr).
	//   - Without intermediates: send directly to target's LISTEN_ADDR.
	var sendAddr string
	if len(chainAbove) == 0 {
		sendAddr = target.addr // = target.DialedAddr from server's perspective
	} else {
		sendAddr = chainAbove[0].dialedAddr.String() // = top-hop.DialedAddr
	}
	destCID := objproto.NewConnectionID("ws", netip.MustParseAddrPort(sendAddr), regSlot)
	priv, hs, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatalf("chainRegister: NewECDHHandshake: %v", err)
	}
	ch, err := server.ep.SendHandshake(destCID, priv, hs)
	if err != nil {
		t.Fatalf("chainRegister: server.SendHandshake: %v", err)
	}
	select {
	case <-ch.C:
	case <-time.After(10 * time.Second):
		t.Fatalf("chainRegister: server-side completion timeout for %s", target.name)
	}

	// TARGET role: the new runner accepts the forwarded handshake and reads its
	// own activeConn's CID as its serverCID. This is exactly what driveAfterConn
	// does on the runner side: serverCID = pc.Connection().ConnectionID().
	var targetConn objproto.Connection
	select {
	case targetConn = <-target.ep.GetNewActiveConnectionChannel():
	case <-time.After(10 * time.Second):
		t.Fatalf("chainRegister: target-side accept timeout for %s", target.name)
	}
	result := &hopInfo{
		endpoint:   target,
		serverCID:  targetConn.ConnectionID(),                    // TARGET's own knowledge
		dialedAddr: netip.MustParseAddrPort(target.addr),          // SERVER stores this (= what server dialed)
	}
	t.Logf("chainRegister: %s registered, serverCID=%v (private to %s) dialedAddr=%v (server-visible)",
		target.name, result.serverCID, target.name, result.dialedAddr)
	return result
}

// chainedRelayPhaseB simulates the leaf runner's runAgentProxyCeremony
// for an incoming agent dial, then drives the agent-side RehandshakeForProxy.
// Returns the initiator-side end-to-end Connection.
//
// Realistic sequence:
//   1. initiator.SendHandshake to (leaf.LISTEN_ADDR, agentSlot) — initial
//      ECDH agent↔leaf. leaf has activeConn with cid=(initiator.SRC-from-leaf, agentSlot).
//   2. leaf.runAgentProxyCeremony equivalent: SetProxy(
//        owned = leafIncomingConn.CID,
//        allocate = (leaf.serverCID.Addr, agentSlot),
//      ). Close leafIncomingConn.
//   3. initiator.RehandshakeForProxy(newKey) — packet flows through leaf's
//      proxySettings → all intermediates → server. server ECDHs with initiator.
//   4. Both ends have end-to-end peer.Conn.
//
// Pre-condition: chained relay setup has already been applied at every
// intermediate hop for agentSlot.
// chainedRelayPhaseB drives Phase B from the agent's perspective with full role
// separation:
//   - AGENT role: knows only leaf's LISTEN_ADDR (= HARNESS_PROXY_VIA_RUNNER env)
//     + its own crypto material. Does NOT read leaf.serverCID. Calls SendHandshake
//     then RehandshakeForProxy on its own endpoint.
//   - LEAF role: accepts the incoming conn, computes SetProxy using its own
//     serverCID (via leafComputeAgentProxySetProxy).
func chainedRelayPhaseB(
	t *testing.T,
	initiator chainEndpoint,
	leaf *hopInfo,
	agentSlot uint16,
) objproto.Connection {
	t.Helper()

	// AGENT role: initial ECDH to leaf's LISTEN_ADDR (= env value).
	leafListenAddr := leaf.dialedAddr // = LISTEN_ADDR (server-visible field; agent knows from env injection)
	leafDestCID := objproto.NewConnectionID("ws", leafListenAddr, agentSlot)
	priv1, hs1, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatalf("phaseB: NewECDHHandshake initial: %v", err)
	}
	ch1, err := initiator.ep.SendHandshake(leafDestCID, priv1, hs1)
	if err != nil {
		t.Fatalf("phaseB: initiator.SendHandshake: %v", err)
	}
	var initLeafConn objproto.Connection
	select {
	case initLeafConn = <-ch1.C:
	case <-time.After(5 * time.Second):
		t.Fatal("phaseB: initiator-side initial ECDH timeout")
	}

	// LEAF role: accept the incoming conn, compute SetProxy. Reads its OWN
	// serverCID via leafComputeAgentProxySetProxy (which lives in the
	// hop-side knowledge domain).
	var leafIncomingConn objproto.Connection
	select {
	case leafIncomingConn = <-leaf.endpoint.ep.GetNewActiveConnectionChannel():
	case <-time.After(5 * time.Second):
		t.Fatal("phaseB: leaf-side initial ECDH accept timeout")
	}
	leafComputeAgentProxySetProxy(t, leaf, agentSlot, leafIncomingConn)
	_ = leafIncomingConn.Close() // proxySettings persists; agent rehandshake will forward

	// AGENT role: rehandshake with fresh key. Goes through the SetProxy chain.
	priv2, hs2, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatalf("phaseB: NewECDHHandshake rehandshake: %v", err)
	}
	rh, err := initLeafConn.RehandshakeForProxy(priv2, hs2)
	if err != nil {
		t.Fatalf("phaseB: RehandshakeForProxy: %v", err)
	}
	var initE2EConn objproto.Connection
	select {
	case initE2EConn = <-rh.C:
	case <-time.After(10 * time.Second):
		t.Fatal("phaseB: rehandshake completion timeout (chain broken)")
	}
	return initE2EConn
}

func TestChainedRelayPOC_Realistic_3hop(t *testing.T) {
	if testing.Short() {
		t.Skip("POC")
	}
	const (
		serverAddr = "127.0.0.1:18800"
		pAddr      = "127.0.0.1:18801"
		lAddr      = "127.0.0.1:18802"
		regSlotP   = uint16(0x1001)
		regSlotL   = uint16(0x1002)
		agentSlot  = uint16(0xA001)
		wsPath     = "/ws"
	)

	// Server is Mutual: it both accepts (initiator end-to-end) and dials
	// (Phase C reverse-dial via SendHandshake).
	server := startMutualEndpoint(t, "server", serverAddr, wsPath)
	defer server.cancel()
	p := startMutualEndpoint(t, "P", pAddr, wsPath)
	defer p.cancel()
	l := startMutualEndpoint(t, "L", lAddr, wsPath)
	defer l.cancel()
	initiator := startClientEndpoint(t, "initiator", wsPath)
	defer initiator.cancel()

	time.Sleep(300 * time.Millisecond)

	// 1. P registered direct with server (Phase A reverse-dial analog).
	pInfo := chainRegister(t, server, nil, p, regSlotP)
	// 2. L registered via P (Phase C).
	lInfo := chainRegister(t, server, []*hopInfo{pInfo}, l, regSlotL)

	// 3. Chained-relay setup for agent slot.
	//    SERVER role: walk L.Via=P, P.Via=nil. Send "EstablishRelay" to P
	//    with target=L.DialedAddr (server's own knowledge).
	//    HOP role: hopComputeSetProxy reads P's own serverCID internally.
	//    Server never accesses pInfo.serverCID.
	hopComputeSetProxy(t, pInfo, agentSlot, lInfo.dialedAddr)

	// 4. Initiator runs Phase B agent ceremony at L.
	initE2EConn := chainedRelayPhaseB(t, initiator, lInfo, agentSlot)

	var serverE2EConn objproto.Connection
	select {
	case serverE2EConn = <-server.ep.GetNewActiveConnectionChannel():
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive end-to-end conn (3-hop chain broken)")
	}
	t.Logf("server end-to-end conn: cid=%v", serverE2EConn.ConnectionID())

	payload := []byte{byte(wire.ApplicationPayloadKind_AgentMessage), 0xAA, 0xBB, 0xCC, 0xDD}
	if _, _, err := initE2EConn.SendMessage(payload); err != nil {
		t.Fatalf("initiator SendMessage: %v", err)
	}
	msg, err := serverE2EConn.ReceiveMessage()
	if err != nil {
		t.Fatalf("server ReceiveMessage: %v", err)
	}
	if len(msg.Data) < 5 ||
		msg.Data[0] != byte(wire.ApplicationPayloadKind_AgentMessage) ||
		msg.Data[1] != 0xAA || msg.Data[2] != 0xBB || msg.Data[3] != 0xCC || msg.Data[4] != 0xDD {
		t.Fatalf("unexpected payload at server: % x", msg.Data)
	}
	t.Log("3-hop realistic-addr chained relay: end-to-end roundtrip CONFIRMED")
}

func TestChainedRelayPOC_Realistic_4hop(t *testing.T) {
	if testing.Short() {
		t.Skip("POC")
	}
	const (
		serverAddr = "127.0.0.1:18810"
		qAddr      = "127.0.0.1:18811"
		pAddr      = "127.0.0.1:18812"
		lAddr      = "127.0.0.1:18813"
		regSlotQ   = uint16(0x2001)
		regSlotP   = uint16(0x2002)
		regSlotL   = uint16(0x2003)
		agentSlot  = uint16(0xA002)
		wsPath     = "/ws"
	)

	server := startMutualEndpoint(t, "server", serverAddr, wsPath)
	defer server.cancel()
	q := startMutualEndpoint(t, "Q", qAddr, wsPath)
	defer q.cancel()
	p := startMutualEndpoint(t, "P", pAddr, wsPath)
	defer p.cancel()
	l := startMutualEndpoint(t, "L", lAddr, wsPath)
	defer l.cancel()
	initiator := startClientEndpoint(t, "initiator", wsPath)
	defer initiator.cancel()

	time.Sleep(300 * time.Millisecond)

	// Topology: server → Q (direct) → P (via Q) → L (via P-via-Q).
	qInfo := chainRegister(t, server, nil, q, regSlotQ)
	pInfo := chainRegister(t, server, []*hopInfo{qInfo}, p, regSlotP)
	lInfo := chainRegister(t, server, []*hopInfo{qInfo, pInfo}, l, regSlotL)

	// Chained-relay setup: walk L.Via=P, P.Via=Q, Q.Via=nil.
	// SERVER role: for each intermediate, pass downstream's DialedAddr.
	// Server reads only its own knowledge (dialedAddr fields from registry).
	// In production these are dispatched concurrently; the test does them
	// sequentially for clarity — same SetProxy outcome at each hop.
	hopComputeSetProxy(t, qInfo, agentSlot, pInfo.dialedAddr)
	hopComputeSetProxy(t, pInfo, agentSlot, lInfo.dialedAddr)

	initE2EConn := chainedRelayPhaseB(t, initiator, lInfo, agentSlot)

	var serverE2EConn objproto.Connection
	select {
	case serverE2EConn = <-server.ep.GetNewActiveConnectionChannel():
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive end-to-end conn (4-hop chain broken)")
	}

	payload := []byte{byte(wire.ApplicationPayloadKind_AgentMessage), 0x11, 0x22, 0x33, 0x44}
	if _, _, err := initE2EConn.SendMessage(payload); err != nil {
		t.Fatalf("initiator SendMessage: %v", err)
	}
	msg, err := serverE2EConn.ReceiveMessage()
	if err != nil {
		t.Fatalf("server ReceiveMessage: %v", err)
	}
	if len(msg.Data) < 5 ||
		msg.Data[0] != byte(wire.ApplicationPayloadKind_AgentMessage) ||
		msg.Data[1] != 0x11 || msg.Data[2] != 0x22 || msg.Data[3] != 0x33 || msg.Data[4] != 0x44 {
		t.Fatalf("unexpected payload at server: % x", msg.Data)
	}
	t.Log("4-hop realistic-addr chained relay: end-to-end roundtrip CONFIRMED")
}
