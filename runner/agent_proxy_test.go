package runner

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/objtrsf/objproto"
)

func TestProxyHandlerCollisionDetection(t *testing.T) {
	const sharedID = 0x1234
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), sharedID)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), sharedID)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_IdCollision {
		t.Errorf("status: got %v want IdCollision", resp.Status)
	}
}

func TestProxyHandlerUnknownTask(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return false },
	}

	var taskID protocol.TaskID
	taskID.Id[0] = 0xAA
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_UnknownTask {
		t.Errorf("status: got %v want UnknownTask", resp.Status)
	}
}

func TestProxyHandlerServerNotConnected(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: false,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_ServerNotConnected {
		t.Errorf("status: got %v want ServerNotConnected", resp.Status)
	}
}

func TestProxyHandlerHappyPath(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_Ok {
		t.Errorf("status: got %v want Ok", resp.Status)
	}

	alloc := st.allocateCID(agentCID)
	if alloc.Transport != serverCID.Transport {
		t.Errorf("alloc.Transport: got %q want %q", alloc.Transport, serverCID.Transport)
	}
	if alloc.Addr != serverCID.Addr {
		t.Errorf("alloc.Addr: got %v want %v", alloc.Addr, serverCID.Addr)
	}
	if alloc.ID != agentCID.ID {
		t.Errorf("alloc.ID: got %v want %v", alloc.ID, agentCID.ID)
	}
}

// ---------------------------------------------------------------------------
// Session.BeginChainedRelay / DeliverChainedRelayResponse unit tests
// ---------------------------------------------------------------------------

// TestSessionBeginChainedRelayAndDeliver: normal round-trip — BeginChainedRelay
// returns a channel, DeliverChainedRelayResponse sends the response on it, and
// the channel becomes available for receive.
func TestSessionBeginChainedRelayAndDeliver(t *testing.T) {
	var sess Session

	ch, err := sess.BeginChainedRelay()
	if err != nil {
		t.Fatalf("BeginChainedRelay: unexpected error: %v", err)
	}
	if ch == nil {
		t.Fatal("BeginChainedRelay: returned nil channel")
	}

	want := protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_Ok}
	if !sess.DeliverChainedRelayResponse(want) {
		t.Fatal("DeliverChainedRelayResponse: returned false; expected true")
	}

	select {
	case got := <-ch:
		if got.Status != want.Status {
			t.Errorf("response status: got %v want %v", got.Status, want.Status)
		}
	default:
		t.Fatal("channel empty after DeliverChainedRelayResponse")
	}

	// After delivery, pending slot must be cleared → a new BeginChainedRelay
	// must succeed.
	ch2, err2 := sess.BeginChainedRelay()
	if err2 != nil {
		t.Errorf("second BeginChainedRelay after delivery: unexpected error: %v", err2)
	}
	if ch2 == nil {
		t.Error("second BeginChainedRelay returned nil channel")
	}
}

// TestSessionBeginChainedRelayDuplicateReturnsError: a second BeginChainedRelay
// while the first is still pending must return an error (one-at-a-time invariant,
// spec Decision 2).
func TestSessionBeginChainedRelayDuplicateReturnsError(t *testing.T) {
	var sess Session

	_, err := sess.BeginChainedRelay()
	if err != nil {
		t.Fatalf("first BeginChainedRelay: unexpected error: %v", err)
	}

	_, err2 := sess.BeginChainedRelay()
	if err2 == nil {
		t.Fatal("second BeginChainedRelay while first pending: expected error, got nil")
	}
}

// TestSessionDeliverChainedRelayResponseNoWaiter: DeliverChainedRelayResponse
// with no pending waiter must return false (server sent stale/spurious message).
func TestSessionDeliverChainedRelayResponseNoWaiter(t *testing.T) {
	var sess Session
	resp := protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_Ok}
	if sess.DeliverChainedRelayResponse(resp) {
		t.Error("DeliverChainedRelayResponse with no waiter: expected false, got true")
	}
}

// ---------------------------------------------------------------------------
// dispatchRunnerRequest ChainedRelayResponse arm
// ---------------------------------------------------------------------------

// TestDispatchChainedRelayResponseDelivers: a ChainedRelayResponse RunnerRequest
// received via dispatchRunnerRequest is delivered to the session waiter.
func TestDispatchChainedRelayResponseDelivers(t *testing.T) {
	ms := &mockSender{}
	sess := &Session{Sender: ms, Now: time.Now}

	ch, err := sess.BeginChainedRelay()
	if err != nil {
		t.Fatalf("BeginChainedRelay: %v", err)
	}

	// Build a RunnerRequest{ChainedRelayResponse{Direct}}.
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ChainedRelayResponse}
	req.SetChainedRelayResponse(protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_Direct})
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode RunnerRequest: %v", err)
	}

	dispatchRunnerRequest(context.Background(), sess, sess.logger(), appwire.AppKind_RunnerControl, payload)

	select {
	case got := <-ch:
		if got.Status != protocol.ChainedRelayStatus_Direct {
			t.Errorf("response status: got %v want Direct", got.Status)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for ChainedRelayResponse delivery")
	}
}

// TestDispatchChainedRelayResponseNoWaiterNoOp: a ChainedRelayResponse with
// no registered waiter must not panic and must not block (just a warn log).
func TestDispatchChainedRelayResponseNoWaiterNoOp(t *testing.T) {
	ms := &mockSender{}
	sess := &Session{Sender: ms, Now: time.Now}

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ChainedRelayResponse}
	req.SetChainedRelayResponse(protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_Ok})
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode RunnerRequest: %v", err)
	}

	// Must not panic, must not block.
	done := make(chan struct{})
	go func() {
		dispatchRunnerRequest(context.Background(), sess, sess.logger(), appwire.AppKind_RunnerControl, payload)
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dispatchRunnerRequest blocked on ChainedRelayResponse without waiter")
	}
}

// ---------------------------------------------------------------------------
// runAgentProxyCeremony chained-relay integration tests
//
// These tests require real peer.Conn pairs (WebSocket in-process). Each test
// spins up a Mutual-mode runner endpoint on a random port, dials an agent
// peer.Conn into it, waits for the accepted objproto.Connection, wraps it as
// a runner-side peer.Conn, and exercises the ceremony with a goroutine acting
// as the "server" (it monitors the mockSender for the RequestChainedRelay
// message and then injects a ChainedRelayResponse via
// DeliverChainedRelayResponse).
// ---------------------------------------------------------------------------

// buildRunnerEndpointForTest spins up a Mutual-mode WebSocket endpoint on a
// random port and returns it together with its accept channel (unbuffered,
// receives objproto.Connection values as agents dial in).
func buildRunnerEndpointForTest(t *testing.T) (runnerEP objproto.Endpoint, addr string) {
	t.Helper()
	mux := http.NewServeMux()
	ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   "/ws",
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatalf("buildRunnerEndpointForTest: WebSocketEndpoint: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("buildRunnerEndpointForTest: Listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ep, ln.Addr().String()
}

// ceremonyCaseRun drives a single runAgentProxyCeremony call where a goroutine
// acting as the "server" monitors the mockSender for the outgoing
// RequestChainedRelay message and then delivers deliverResp via
// DeliverChainedRelayResponse. It returns the ProxyEstablishStatus the
// runner side assigns and whether the runner endpoint has proxy entries
// installed after the ceremony.
//
// If deliverResp is nil, no response is delivered; callers use a short
// context timeout to exercise the cancellation path.
func ceremonyCaseRun(t *testing.T, ctx context.Context, deliverResp *protocol.ChainedRelayResponse) (agentRespStatus protocol.ProxyEstablishStatus, hasProxy bool) {
	t.Helper()

	runnerEP, runnerAddr := buildRunnerEndpointForTest(t)

	// Agent dials the runner endpoint.
	runnerCIDStr := "ws:" + runnerAddr + "-*"
	runnerCID, err := objproto.ParseConnectionID(runnerCIDStr,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("ceremonyCaseRun: parse CID: %v", err)
	}
	agentEP, err := cli.BuildClientEndpoint(runnerCID)
	if err != nil {
		t.Fatalf("ceremonyCaseRun: BuildClientEndpoint: %v", err)
	}
	go objproto.AutoGarbageCollect(agentEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	// Wait for the runner endpoint to accept the new connection.
	newConnCh := runnerEP.GetNewActiveConnectionChannel()

	agentPC, err := peer.Dial(ctx, agentEP, runnerCID, peer.DialConfig{})
	if err != nil {
		t.Fatalf("ceremonyCaseRun: agent peer.Dial: %v", err)
	}
	// Start the receive loop so agentPC's SetOnControl handler fires.
	agentPC.Start(ctx)
	t.Cleanup(func() { agentPC.Close() })

	// Wait for the runner side to see the new active connection.
	var runnerConn objproto.Connection
	select {
	case runnerConn = <-newConnCh:
	case <-time.After(3 * time.Second):
		t.Fatal("ceremonyCaseRun: runner did not accept connection within 3s")
	}

	// Wrap the runner-side connection as a peer.Conn (mirrors listen.go).
	runnerPC := peer.WrapAcceptedConn(ctx, runnerConn, peer.DialConfig{})
	t.Cleanup(func() { runnerPC.Close() })

	// Build a Session with a mockSender.
	ms := &mockSender{}
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	sess := &Session{Sender: ms, ServerCID: serverCID, Now: time.Now}

	// If a response is expected, spin up a goroutine that watches for
	// the outgoing RequestChainedRelay in ms.sent and delivers the response.
	if deliverResp != nil {
		go func() {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				ms.mu.Lock()
				n := len(ms.sent)
				ms.mu.Unlock()
				if n > 0 {
					sess.DeliverChainedRelayResponse(*deliverResp)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Logf("ceremonyCaseRun: goroutine: RequestChainedRelay not sent within 3s")
		}()
	}

	var taskID protocol.TaskID
	taskID.Id[0] = 0x11
	req := protocol.ProxyRequest{TaskId: taskID}

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(t protocol.TaskID) bool { return true },
	}

	// Capture the EstablishResponse the agent receives. The ceremony sends
	// it via runnerPC.Connection().SendMessage → arrives at agentPC.SetOnControl.
	var rcvStatus protocol.ProxyEstablishStatus
	var rcvMu sync.Mutex
	rcvOk := false
	agentPC.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		if kind != appwire.AppKind_AgentProxyControl {
			return
		}
		var env protocol.ProxyControl
		if _, derr := env.Decode(payload); derr != nil {
			return
		}
		if env.Kind == protocol.ProxyControlKind_EstablishResponse {
			er := env.EstablishResponse()
			if er != nil {
				rcvMu.Lock()
				rcvStatus = er.Status
				rcvOk = true
				rcvMu.Unlock()
			}
		}
	})

	// Run the ceremony synchronously (it blocks until the chained-relay
	// response arrives or ctx expires).
	_ = runAgentProxyCeremony(ctx, nil, st, runnerEP, runnerPC, req, sess)

	// Give the outbound SendMessage a moment to reach agentPC's OnControl.
	time.Sleep(150 * time.Millisecond)

	agentCID := runnerConn.ConnectionID()
	proxies := runnerEP.ListProxies()
	for _, p := range proxies {
		if p.Peer1.ID == agentCID.ID || p.Peer2.ID == agentCID.ID {
			hasProxy = true
			break
		}
	}

	rcvMu.Lock()
	if rcvOk {
		agentRespStatus = rcvStatus
	}
	rcvMu.Unlock()
	return agentRespStatus, hasProxy
}

// TestCeremonyChainedRelayDirect: server replies Direct → ceremony proceeds,
// local SetProxy is installed, agent receives Ok.
func TestCeremonyChainedRelayDirect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	status, hasProxy := ceremonyCaseRun(t, ctx, &protocol.ChainedRelayResponse{
		Status: protocol.ChainedRelayStatus_Direct,
	})
	if status != protocol.ProxyEstablishStatus_Ok {
		t.Errorf("agent status: got %v want Ok", status)
	}
	if !hasProxy {
		t.Error("expected local SetProxy to be installed; runner endpoint has no proxy entries")
	}
}

// TestCeremonyChainedRelayOk: server replies Ok → ceremony proceeds,
// local SetProxy is installed, agent receives Ok.
func TestCeremonyChainedRelayOk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	status, hasProxy := ceremonyCaseRun(t, ctx, &protocol.ChainedRelayResponse{
		Status: protocol.ChainedRelayStatus_Ok,
	})
	if status != protocol.ProxyEstablishStatus_Ok {
		t.Errorf("agent status: got %v want Ok", status)
	}
	if !hasProxy {
		t.Error("expected local SetProxy to be installed; runner endpoint has no proxy entries")
	}
}

// TestCeremonyChainedRelayHopSetupFailed: server replies HopSetupFailed →
// ceremony aborts with InternalError and no local SetProxy is installed.
func TestCeremonyChainedRelayHopSetupFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	status, hasProxy := ceremonyCaseRun(t, ctx, &protocol.ChainedRelayResponse{
		Status: protocol.ChainedRelayStatus_HopSetupFailed,
	})
	if status != protocol.ProxyEstablishStatus_InternalError {
		t.Errorf("agent status: got %v want InternalError", status)
	}
	if hasProxy {
		t.Error("expected no local SetProxy on HopSetupFailed; runner endpoint has proxy entries")
	}
}

// TestCeremonyChainedRelayTimeout: no response from server → context cancel
// aborts the ceremony with InternalError and no local SetProxy is installed.
// Uses a very short context to avoid waiting the full chainedRelayTimeout.
func TestCeremonyChainedRelayTimeout(t *testing.T) {
	// Short context: cancelled quickly so ctx.Done() fires in the ceremony's
	// select before the 10s chainedRelayTimeout fires.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer outerCancel()

	runnerEP, runnerAddr := buildRunnerEndpointForTest(t)
	runnerCIDStr := "ws:" + runnerAddr + "-*"
	runnerCID, err := objproto.ParseConnectionID(runnerCIDStr,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse CID: %v", err)
	}
	agentEP, err := cli.BuildClientEndpoint(runnerCID)
	if err != nil {
		t.Fatalf("BuildClientEndpoint: %v", err)
	}
	go objproto.AutoGarbageCollect(agentEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	newConnCh := runnerEP.GetNewActiveConnectionChannel()
	agentPC, err := peer.Dial(outerCtx, agentEP, runnerCID, peer.DialConfig{})
	if err != nil {
		t.Fatalf("agent Dial: %v", err)
	}
	agentPC.Start(outerCtx)
	t.Cleanup(func() { agentPC.Close() })

	var runnerConn objproto.Connection
	select {
	case runnerConn = <-newConnCh:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not accept connection within 3s")
	}
	runnerPC := peer.WrapAcceptedConn(outerCtx, runnerConn, peer.DialConfig{})
	t.Cleanup(func() { runnerPC.Close() })

	ms := &mockSender{}
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	sess := &Session{Sender: ms, ServerCID: serverCID, Now: time.Now}

	var taskID protocol.TaskID
	taskID.Id[0] = 0x22
	req := protocol.ProxyRequest{TaskId: taskID}
	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(t protocol.TaskID) bool { return true },
	}

	// Run ceremony; ctx expires almost immediately → ctx.Done() fires.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runAgentProxyCeremony(ctx, nil, st, runnerEP, runnerPC, req, sess)
	}()

	select {
	case <-done:
		// ceremony returned promptly after ctx cancel
	case <-time.After(3 * time.Second):
		t.Fatal("ceremony did not return within 3s after context cancel")
	}

	// No SetProxy must be installed for the agent connection.
	agentCID := runnerConn.ConnectionID()
	proxies := runnerEP.ListProxies()
	for _, p := range proxies {
		if p.Peer1.ID == agentCID.ID || p.Peer2.ID == agentCID.ID {
			t.Errorf("expected no local SetProxy on timeout; found proxy entry")
		}
	}

	// Spec Decision 2: after a cancelled ceremony the pending slot must be
	// cleared so a future BeginChainedRelay on the same session can proceed.
	ch2, err2 := sess.BeginChainedRelay()
	if err2 != nil {
		t.Errorf("BeginChainedRelay after cancelled ceremony: expected nil error, got %v", err2)
	}
	if ch2 == nil {
		t.Error("BeginChainedRelay after cancelled ceremony: returned nil channel")
	}
}
