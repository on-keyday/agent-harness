//go:build integration

// Relay POC: verify the REVISED relay protocol mechanics (no proxy↔target ECDH;
// SetProxy with synthetic allocate; server sends DialGreeting post-rehandshake).
// Mirrors ksdk's TestWebSocketNegotiatedProxy pattern strictly.
//
// Flow:
//   1. Server SendHandshake(proxy.Addr, slotID) — initial ECDH server↔proxy.
//   2. Proxy SetProxy(owned=proxy's view of server activeConn,
//                     allocate=synthetic (target.Addr, slotID)). NO ECDH proxy↔target.
//   3. Proxy closes its server-side activeConn (proxySettings entry persists).
//   4. Server RehandshakeForProxy on the (proxy.Addr, slotID) peer.Conn.
//      New Handshake flows through proxy's proxySettings → target.
//      Target ECDH's with server's new key → activeConn at (proxy.Addr, slotID) [target's view].
//   5. Server's rh.C delivers the new end-to-end conn.
//   6. Server SendMessage(DialGreeting) on the new conn → proxy forwards → target receives.
//
// AEAD validation in Step 6 proves the keys are end-to-end (server↔target),
// not relayed via proxy decrypt/re-encrypt.

package integration

import (
	"context"
	"crypto/ecdh"
	"log/slog"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
)

func TestRelayPOC(t *testing.T) {
	if testing.Short() {
		t.Skip("POC")
	}

	const (
		proxyAddr        = "127.0.0.1:18620"
		targetAddr       = "127.0.0.1:18621"
		slotID           = uint16(0x1234)
		wsPath           = "/ws"
		handshakeTimeout = 5 * time.Second
	)

	// --- proxy endpoint (Mutual, listens on proxyAddr) ---
	proxyMux := http.NewServeMux()
	proxyEP, err := transport.WebSocketEndpoint(proxyMux, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   wsPath,
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatalf("proxy endpoint: %v", err)
	}
	proxyHTTP := &http.Server{Addr: proxyAddr, Handler: proxyMux}
	go func() { _ = proxyHTTP.ListenAndServe() }()
	defer func() {
		c, cancelFn := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancelFn()
		_ = proxyHTTP.Shutdown(c)
	}()
	go objproto.AutoGarbageCollect(proxyEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	// --- target endpoint (Mutual, listens on targetAddr) ---
	targetMux := http.NewServeMux()
	targetEP, err := transport.WebSocketEndpoint(targetMux, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   wsPath,
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatalf("target endpoint: %v", err)
	}
	targetHTTP := &http.Server{Addr: targetAddr, Handler: targetMux}
	go func() { _ = targetHTTP.ListenAndServe() }()
	defer func() {
		c, cancelFn := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancelFn()
		_ = targetHTTP.Shutdown(c)
	}()
	go objproto.AutoGarbageCollect(targetEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	// --- server endpoint (Client mode is enough — only dials out) ---
	serverEP, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   wsPath,
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		t.Fatalf("server endpoint: %v", err)
	}
	go objproto.AutoGarbageCollect(serverEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	time.Sleep(300 * time.Millisecond) // listeners bind

	// === Step 1: server dials proxy at slotID (initial ECDH server↔proxy) ===
	proxyCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort(proxyAddr), slotID)
	priv1, hs1, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatalf("NewECDHHandshake server↔proxy: %v", err)
	}
	ch1, err := serverEP.SendHandshake(proxyCID, priv1, hs1)
	if err != nil {
		t.Fatalf("server.SendHandshake: %v", err)
	}
	var serverProxyConn objproto.Connection
	select {
	case serverProxyConn = <-ch1.C:
	case <-time.After(handshakeTimeout):
		t.Fatal("Step 1: server↔proxy handshake timeout")
	}
	var proxyServerConn objproto.Connection
	select {
	case proxyServerConn = <-proxyEP.GetNewActiveConnectionChannel():
	case <-time.After(handshakeTimeout):
		t.Fatal("Step 1: proxy did not receive server connection")
	}
	t.Logf("Step 1: server↔proxy ECDH done: server cid=%v proxy cid=%v",
		serverProxyConn.ConnectionID(), proxyServerConn.ConnectionID())

	// === Step 2: proxy SetProxy with SYNTHETIC allocate CID ===
	// owned    = proxy's view of server conn (real activeConn at proxy).
	// allocate = synthetic (ws, targetAddr, slotID); NO activeConn at proxy
	//            for this CID — proxy never ECDH's with target.
	allocCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort(targetAddr), slotID)
	if err := proxyEP.SetProxy(proxyServerConn.ConnectionID(), allocCID); err != nil {
		t.Fatalf("Step 2: SetProxy: %v", err)
	}
	t.Logf("Step 2: SetProxy(owned=%v, allocate=%v synthetic) ok",
		proxyServerConn.ConnectionID(), allocCID)

	// === Step 3: proxy closes its server-side activeConn ===
	// (proxySettings entry persists; subsequent inbound packets at
	//  proxyServerConn.ConnectionID() go through the proxy-forward path.)
	if err := proxyServerConn.Close(); err != nil {
		t.Logf("Step 3: proxyServerConn.Close: %v (non-fatal)", err)
	}
	t.Log("Step 3: proxy closed its server-side activeConn")

	// === Step 4: server rehandshakes on the existing peer.Conn ===
	priv2, hs2, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatalf("Step 4: NewECDHHandshake rehandshake: %v", err)
	}
	rh, err := serverProxyConn.RehandshakeForProxy(priv2, hs2)
	if err != nil {
		t.Fatalf("Step 4: RehandshakeForProxy: %v", err)
	}
	t.Log("Step 4: server initiated RehandshakeForProxy")

	// === Step 5: receive new end-to-end conn at server ===
	var newServerConn objproto.Connection
	select {
	case newServerConn = <-rh.C:
	case <-time.After(handshakeTimeout):
		t.Fatal("Step 5: rehandshake completion timeout (server side)")
	}
	t.Logf("Step 5: server end-to-end conn ready: cid=%v",
		newServerConn.ConnectionID())

	// === Step 6: receive new end-to-end conn at target ===
	newTargetConn, err := targetEP.WaitNewActiveConnection(handshakeTimeout)
	if err != nil {
		t.Fatalf("Step 6: target.WaitNewActiveConnection: %v", err)
	}
	t.Logf("Step 6: target end-to-end conn ready: cid=%v",
		newTargetConn.ConnectionID())

	// === Step 7: server sends DialGreeting on the new end-to-end conn ===
	greeting := protocol.DialGreeting{Version: 1}
	greetingPayload := greeting.MustAppend([]byte{byte(appwire.AppKind_DialGreeting)})
	if _, _, err := newServerConn.SendMessage(greetingPayload); err != nil {
		t.Fatalf("Step 7: server send DialGreeting: %v", err)
	}
	t.Log("Step 7: server sent DialGreeting on end-to-end conn")

	// === Step 8: target receives DialGreeting (AEAD validates end-to-end keys) ===
	msg, err := newTargetConn.ReceiveMessage()
	if err != nil {
		t.Fatalf("Step 8: target receive DialGreeting: %v", err)
	}
	if len(msg.Data) < 1 || msg.Data[0] != byte(appwire.AppKind_DialGreeting) {
		t.Fatalf("Step 8: expected DialGreeting kind, got: % x", msg.Data)
	}
	var receivedGreeting protocol.DialGreeting
	if _, err := receivedGreeting.Decode(msg.Data[1:]); err != nil {
		t.Fatalf("Step 8: decode DialGreeting: %v", err)
	}
	if receivedGreeting.Version != 1 {
		t.Fatalf("Step 8: greeting.Version: got %d want 1", receivedGreeting.Version)
	}
	t.Log("Relay POC: end-to-end conn server↔target through proxy CONFIRMED")
}
