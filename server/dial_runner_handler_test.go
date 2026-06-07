package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/objtrsf/objproto"
)

// TestDialRunnerViaInvalidTarget covers the early-return guard on the via-relay
// path: an empty-transport target should produce InvalidTarget BEFORE we ever
// attempt via resolution or any handshake setup.
func TestDialRunnerViaInvalidTarget(t *testing.T) {
	resolved := false
	h := &DialRunnerHandler{
		Logger: slog.Default(),
		ResolveVia: func(_ objproto.ConnectionID) (*RunnerEntry, bool) {
			resolved = true
			return nil, false
		},
		ViaSendEstablishRelay: func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			t.Fatal("ViaSendEstablishRelay must not be called on InvalidTarget path")
			return protocol.EstablishRelayResponse{}, nil
		},
	}
	var target protocol.RunnerID // empty Transport
	var via protocol.RunnerID
	via.SetTransport([]byte("ws"))
	via.SetIpAddr([]byte{1, 2, 3, 4})
	via.Port = 1234
	via.UniqueNumber = 12345

	resp := h.HandleWithVia(context.Background(), target, via)
	if resp.Status != protocol.DialRunnerStatus_InvalidTarget {
		t.Errorf("status: got %v want InvalidTarget", resp.Status)
	}
	if resolved {
		t.Error("ResolveVia must not be called when target is invalid")
	}
}

// TestDialRunnerViaNotFound: when via CID doesn't match any registered runner,
// the handler returns ViaNotFound without attempting any relay setup.
func TestDialRunnerViaNotFound(t *testing.T) {
	sentRelay := false
	h := &DialRunnerHandler{
		Logger:   slog.Default(),
		Endpoint: nil, // not reached
		ResolveVia: func(_ objproto.ConnectionID) (*RunnerEntry, bool) {
			return nil, false
		},
		ViaSendEstablishRelay: func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			sentRelay = true
			return protocol.EstablishRelayResponse{}, nil
		},
	}
	var target protocol.RunnerID
	target.SetTransport([]byte("ws"))
	target.SetIpAddr([]byte{10, 0, 0, 5})
	target.Port = 8540

	var via protocol.RunnerID
	via.SetTransport([]byte("ws"))
	via.SetIpAddr([]byte{1, 2, 3, 4})
	via.Port = 9999
	via.UniqueNumber = 12345

	resp := h.HandleWithVia(context.Background(), target, via)
	if resp.Status != protocol.DialRunnerStatus_ViaNotFound {
		t.Errorf("status: got %v want ViaNotFound", resp.Status)
	}
	if sentRelay {
		t.Error("ViaSendEstablishRelay must not be called when via is unresolved")
	}
}

// TestDialRunnerViaRelayFailed: when proxy_runner returns non-Ok
// EstablishRelayResponse, the handler returns ViaRelayFailed without
// attempting any handshake.
func TestDialRunnerViaRelayFailed(t *testing.T) {
	fakeEntry := &RunnerEntry{
		ID: objproto.NewConnectionID("ws",
			netip.MustParseAddrPort("192.168.1.10:8540"), 12345).String(),
	}
	h := &DialRunnerHandler{
		Logger:   slog.Default(),
		Endpoint: nil, // not reached because we short-circuit before SendHandshake
		ResolveVia: func(_ objproto.ConnectionID) (*RunnerEntry, bool) {
			return fakeEntry, true
		},
		ViaSendEstablishRelay: func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_SlotCollision}, nil
		},
	}
	var target protocol.RunnerID
	target.SetTransport([]byte("ws"))
	target.SetIpAddr([]byte{10, 0, 0, 5})
	target.Port = 8540

	var via protocol.RunnerID
	via.SetTransport([]byte("ws"))
	via.SetIpAddr([]byte{192, 168, 1, 10})
	via.Port = 8540
	via.UniqueNumber = 12345

	resp := h.HandleWithVia(context.Background(), target, via)
	if resp.Status != protocol.DialRunnerStatus_ViaRelayFailed {
		t.Errorf("status: got %v want ViaRelayFailed", resp.Status)
	}
}

// TestDialRunnerViaEmptyVia: when via.transport_len == 0, HandleWithVia must
// fall through to the direct-dial path (Handle). This is the "via not
// specified" case from the CLI side, where the same envelope can carry both
// direct and relay dials.
func TestDialRunnerViaEmptyVia(t *testing.T) {
	h := &DialRunnerHandler{
		Logger:   slog.Default(),
		Endpoint: nil, // not reached: target.Transport empty triggers InvalidTarget first
	}
	var target protocol.RunnerID // empty Transport → InvalidTarget via Handle
	var via protocol.RunnerID    // empty Transport — triggers direct fallback

	resp := h.HandleWithVia(context.Background(), target, via)
	if resp.Status != protocol.DialRunnerStatus_InvalidTarget {
		t.Errorf("status: got %v want InvalidTarget (direct path)", resp.Status)
	}
}

// TestDialRunnerInvalidTargetTransport covers empty-transport early return.
func TestDialRunnerInvalidTargetTransport(t *testing.T) {
	h := &DialRunnerHandler{
		Logger:   slog.Default(),
		Endpoint: nil, // not reached when validation fails first
	}
	var bad protocol.RunnerID
	// Leave transport empty (SetTransport with empty slice produces TransportLen=0)
	bad.SetTransport([]byte{})
	bad.SetIpAddr([]byte{127, 0, 0, 1})
	bad.Port = 8540

	resp := h.Handle(context.Background(), bad)
	if resp.Status != protocol.DialRunnerStatus_InvalidTarget {
		t.Errorf("status: got %v, want InvalidTarget", resp.Status)
	}
}

// TestDialRunnerDialFailsUnboundedPort covers a real DoECDHHandshake that hits a
// nothing-listening port. Uses a real WebSocket Client endpoint so we exercise
// the actual dial path. The test must complete quickly via DialTimeout.
func TestDialRunnerDialFailsUnboundedPort(t *testing.T) {
	ep := buildTestClientEndpoint(t)
	h := &DialRunnerHandler{
		Logger:      slog.Default(),
		Endpoint:    ep,
		DialTimeout: 500 * time.Millisecond,
	}
	var target protocol.RunnerID
	target.SetTransport([]byte("ws"))
	target.SetIpAddr([]byte{127, 0, 0, 1})
	target.Port = 1 // nothing listens here

	resp := h.Handle(context.Background(), target)
	if resp.Status != protocol.DialRunnerStatus_DialFailed {
		t.Errorf("status: got %v, want DialFailed", resp.Status)
	}
}

// buildTestClientEndpoint returns a WS-only Client endpoint for tests.
// Client mode is sufficient because DialRunnerHandler only initiates outbound
// ECDH handshakes; it never accepts inbound connections.
func buildTestClientEndpoint(t *testing.T) objproto.Endpoint {
	t.Helper()
	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   "/ws",
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		t.Fatalf("buildTestClientEndpoint: %v", err)
	}
	return ep
}

// TestDialRunnerTaskHandlerCase verifies that the TaskHandler dispatches
// DialRunner correctly: when Endpoint is nil the handler returns InvalidTarget
// for a bad target (before reaching any dial), and the response is encoded/decoded
// correctly.
func TestDialRunnerTaskHandlerCase(t *testing.T) {
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9001-50")}

	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
		// Endpoint is nil — the handler will fail on validation before using it
	}

	var req protocol.TaskControlRequest
	req.Kind = protocol.TaskControlKind_DialRunner
	// Target has empty transport → InvalidTarget
	var dr protocol.DialRunnerRequest
	dr.Target.SetIpAddr([]byte{127, 0, 0, 1})
	dr.Target.Port = 9001
	req.SetDialRunner(dr)

	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	h.Handle(fc, payload)

	if len(fc.sent) == 0 {
		t.Fatal("expected a response message, got none")
	}
	// Strip the ApplicationPayloadKind byte
	raw := fc.sent[0]
	if len(raw) < 1 {
		t.Fatal("response too short")
	}
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(raw[1:]); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_DialRunner {
		t.Errorf("kind: got %v, want DialRunner", resp.Kind)
	}
	dr2 := resp.DialRunner()
	if dr2 == nil {
		t.Fatal("DialRunner response variant is nil")
	}
	if dr2.Status != protocol.DialRunnerStatus_InvalidTarget {
		t.Errorf("status: got %v, want InvalidTarget", dr2.Status)
	}
}

// TestTaskControlDialRunnerRequestRoundTrip verifies wire encode/decode of
// DialRunnerRequest when wrapped in a TaskControlRequest envelope. The bare
// DialRunnerRequest round-trip is in runner/protocol/dial_runner_test.go;
// this test specifically exercises the TaskControlKind_DialRunner match-arm
// codegen.
func TestTaskControlDialRunnerRequestRoundTrip(t *testing.T) {
	var req protocol.TaskControlRequest
	req.Kind = protocol.TaskControlKind_DialRunner
	req.RequestId = 42
	var dr protocol.DialRunnerRequest
	dr.Target.SetTransport([]byte("ws"))
	dr.Target.SetIpAddr([]byte{10, 0, 0, 1})
	dr.Target.Port = 8540
	dr.Target.UniqueNumber = 7
	req.SetDialRunner(dr)

	encoded, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var got protocol.TaskControlRequest
	if err := got.DecodeExact(encoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != protocol.TaskControlKind_DialRunner {
		t.Errorf("kind: got %v", got.Kind)
	}
	if got.RequestId != 42 {
		t.Errorf("request_id: got %d", got.RequestId)
	}
	drGot := got.DialRunner()
	if drGot == nil {
		t.Fatal("DialRunner variant nil after decode")
	}
	if string(drGot.Target.Transport) != "ws" {
		t.Errorf("transport: got %q", drGot.Target.Transport)
	}
	if drGot.Target.Port != 8540 {
		t.Errorf("port: got %d", drGot.Target.Port)
	}
	if drGot.Target.UniqueNumber != 7 {
		t.Errorf("unique_number: got %d", drGot.Target.UniqueNumber)
	}
}

// TestTaskControlDialRunnerResponseRoundTrip verifies wire encode/decode of
// DialRunnerResponse when wrapped in a TaskControlResponse envelope. See
// the TestTaskControlDialRunnerRequestRoundTrip note for the layering
// rationale.
func TestTaskControlDialRunnerResponseRoundTrip(t *testing.T) {
	var resp protocol.TaskControlResponse
	resp.Kind = protocol.TaskControlKind_DialRunner
	resp.RequestId = 99
	resp.SetDialRunner(protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_Ok})

	encoded, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var got protocol.TaskControlResponse
	if err := got.DecodeExact(encoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != protocol.TaskControlKind_DialRunner {
		t.Errorf("kind: got %v", got.Kind)
	}
	if got.RequestId != 99 {
		t.Errorf("request_id: got %d", got.RequestId)
	}
	dr := got.DialRunner()
	if dr == nil {
		t.Fatal("DialRunner variant nil after decode")
	}
	if dr.Status != protocol.DialRunnerStatus_Ok {
		t.Errorf("status: got %v", dr.Status)
	}
}

// TestDialRunnerSendsGreeting verifies that after a successful ECDH
// handshake the server emits a DialGreeting payload as the first outbound
// app message on the new conn.
func TestDialRunnerSendsGreeting(t *testing.T) {
	listenAddr := "127.0.0.1:18570"
	firstKind := make(chan appwire.AppKind, 1)
	firstPayload := make(chan []byte, 1)
	stop := startFakeListenRunner(t, listenAddr, firstKind, firstPayload)
	defer stop()

	ep := buildTestClientEndpoint(t)
	dialedCh := make(chan struct{}, 1)
	h := &DialRunnerHandler{
		Logger:      slog.Default(),
		Endpoint:    ep,
		DialTimeout: 2 * time.Second,
		OnDialed: func(_ context.Context, conn objproto.Connection, _ *ViaRegistrationInfo) {
			// Drop the conn — we only care that the greeting reached
			// the fake listener.
			_ = conn.Close()
			dialedCh <- struct{}{}
		},
	}
	var target protocol.RunnerID
	target.SetTransport([]byte("ws"))
	target.SetIpAddr([]byte{127, 0, 0, 1})
	target.Port = 18570

	resp := h.Handle(context.Background(), target)
	if resp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("dial status: got %v want Ok", resp.Status)
	}

	select {
	case k := <-firstKind:
		if k != appwire.AppKind_DialGreeting {
			t.Fatalf("first inbound kind: got %v want DialGreeting", k)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound message at the fake listener within 2s")
	}

	payload := <-firstPayload
	var g protocol.DialGreeting
	if _, err := g.Decode(payload); err != nil {
		t.Fatalf("decode DialGreeting: %v", err)
	}
	if g.Version != 1 {
		t.Errorf("greeting version: got %d want 1", g.Version)
	}
}

// TestDialRunnerViaWithUpstreamChain verifies the N-hop chain walk in Step 3b
// of HandleWithVia. Topology:
//
//	server → Q (direct) → P (via Q) → L (target, being registered via P)
//
// ViaSendEstablishRelay must be called exactly twice:
//  1. Step 3: entry=P, Target=L_addr (target RunnerID), SlotId=slotID
//  2. Step 3b: entry=Q, Target=P.ViaDialAddr, SlotId=slotID
//
// After both calls succeed, h.Endpoint==nil triggers DialFailed — the test
// verifies the chain walk, not the final dial.
func TestDialRunnerViaWithUpstreamChain(t *testing.T) {
	reg := NewRegistry()

	// Q: directly registered (no Via).
	qCID := buildTestCID("ws:127.0.0.1:9100-1")
	qEntry := addEntry(reg, qCID.String(), nil, objproto.ConnectionID{})

	// P: registered via Q. P.ViaDialAddr is the addr Q uses for SetProxy.allocate → P.
	pViaDialAddr := buildTestCID("ws:10.0.0.20:8540-0")
	pCID := buildTestCID("ws:127.0.0.1:9100-2")
	pEntry := addEntry(reg, pCID.String(), qEntry, pViaDialAddr)

	// Build the RunnerIDs for via=P and target=L.
	var pRunnerID protocol.RunnerID
	pRunnerID.SetTransport([]byte("ws"))
	pRunnerID.SetIpAddr([]byte{127, 0, 0, 1})
	pRunnerID.Port = 9100
	pRunnerID.UniqueNumber = 2

	const slotID uint16 = 77 // = target.UniqueNumber below
	var targetRunnerID protocol.RunnerID
	targetRunnerID.SetTransport([]byte("ws"))
	targetRunnerID.SetIpAddr([]byte{10, 0, 0, 99})
	targetRunnerID.Port = 8541
	targetRunnerID.UniqueNumber = slotID

	type callRecord struct {
		entry  *RunnerEntry
		target protocol.RunnerID
		slotID uint16
	}
	var (
		mu      sync.Mutex
		calls   []callRecord
		callCnt int32
	)

	h := &DialRunnerHandler{
		Logger:   slog.Default(),
		Endpoint: nil, // intentionally nil — causes DialFailed after Step 3b succeeds
		ResolveVia: func(cid objproto.ConnectionID) (*RunnerEntry, bool) {
			if cid.String() == pCID.String() {
				return pEntry, true
			}
			return nil, false
		},
		ViaSendEstablishRelay: func(_ context.Context, entry *RunnerEntry, req protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			mu.Lock()
			calls = append(calls, callRecord{
				entry:  entry,
				target: req.Target,
				slotID: req.SlotId,
			})
			mu.Unlock()
			atomic.AddInt32(&callCnt, 1)
			return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_Ok}, nil
		},
	}

	resp := h.HandleWithVia(context.Background(), targetRunnerID, pRunnerID)
	// DialFailed is expected: Endpoint is nil, which the handler returns
	// after all EstablishRelay calls succeed. Any other status means the
	// chain walk didn't complete.
	if resp.Status != protocol.DialRunnerStatus_DialFailed {
		t.Fatalf("expected DialFailed (endpoint nil after successful chain walk), got %v", resp.Status)
	}

	mu.Lock()
	gotCalls := make([]callRecord, len(calls))
	copy(gotCalls, calls)
	mu.Unlock()

	if n := len(gotCalls); n != 2 {
		t.Fatalf("expected ViaSendEstablishRelay called 2 times, got %d", n)
	}

	// Verify slot IDs.
	for i, c := range gotCalls {
		if c.slotID != slotID {
			t.Errorf("call[%d]: SlotId = %d, want %d", i, c.slotID, slotID)
		}
	}

	// Identify which call was for P (Step 3) and which for Q (Step 3b).
	// Step 3 is synchronous and fires before Step 3b's goroutines, so calls[0] = P,
	// calls[1] = Q. Verify by entry pointer.
	if gotCalls[0].entry != pEntry {
		t.Errorf("call[0]: entry = %v, want pEntry", gotCalls[0].entry.ID)
	}
	if gotCalls[1].entry != qEntry {
		t.Errorf("call[1]: entry = %v, want qEntry", gotCalls[1].entry.ID)
	}

	// call[0] target must equal targetRunnerID (L's address).
	targetBytes, _ := targetRunnerID.Append(nil)
	call0Bytes, _ := gotCalls[0].target.Append(nil)
	if !bytes.Equal(targetBytes, call0Bytes) {
		t.Errorf("call[0]: Target = %v, want targetRunnerID", gotCalls[0].target)
	}

	// call[1] target must equal ConnIDToRunnerID(pViaDialAddr) (P's ViaDialAddr).
	wantQ := protocol.ConnIDToRunnerID(pViaDialAddr)
	wantQBytes, _ := wantQ.Append(nil)
	call1Bytes, _ := gotCalls[1].target.Append(nil)
	if !bytes.Equal(wantQBytes, call1Bytes) {
		t.Errorf("call[1]: Target = %v, want ConnIDToRunnerID(pViaDialAddr)", gotCalls[1].target)
	}
}

// TestDialRunnerViaLoopDetected verifies that a cyclic Via chain is detected
// in Step 3b and causes HandleWithVia to return ViaRelayFailed.
//
// Topology: P.Via = Q, Q.Via = P (cycle).
// ViaSendEstablishRelay is called exactly once (Step 3, for P with the
// target address) before Step 3b detects the cycle and short-circuits.
// No upstream-hop dispatch goroutine is ever launched.
func TestDialRunnerViaLoopDetected(t *testing.T) {
	// Build the cyclic entries without using the registry helper (we need
	// to wire Via pointers before insertion).
	pCID := buildTestCID("ws:127.0.0.1:9101-1")
	qCID := buildTestCID("ws:127.0.0.1:9101-2")

	pEntry := &RunnerEntry{
		ID:          pCID.String(),
		ActiveTasks: make(map[string]struct{}),
		ViaDialAddr: buildTestCID("ws:10.0.0.30:8550-0"),
	}
	qEntry := &RunnerEntry{
		ID:          qCID.String(),
		ActiveTasks: make(map[string]struct{}),
		ViaDialAddr: buildTestCID("ws:10.0.0.31:8551-0"),
	}
	// Create the cycle: P → Q → P.
	pEntry.Via = qEntry
	qEntry.Via = pEntry

	// Build the RunnerID for via=P.
	var pRunnerID protocol.RunnerID
	pRunnerID.SetTransport([]byte("ws"))
	pRunnerID.SetIpAddr([]byte{127, 0, 0, 1})
	pRunnerID.Port = 9101
	pRunnerID.UniqueNumber = 1

	var targetRunnerID protocol.RunnerID
	targetRunnerID.SetTransport([]byte("ws"))
	targetRunnerID.SetIpAddr([]byte{10, 0, 0, 50})
	targetRunnerID.Port = 8542
	targetRunnerID.UniqueNumber = 55

	var callCnt int32

	h := &DialRunnerHandler{
		Logger:   slog.Default(),
		Endpoint: nil,
		ResolveVia: func(_ objproto.ConnectionID) (*RunnerEntry, bool) {
			return pEntry, true
		},
		ViaSendEstablishRelay: func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			atomic.AddInt32(&callCnt, 1)
			return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_Ok}, nil
		},
	}

	resp := h.HandleWithVia(context.Background(), targetRunnerID, pRunnerID)
	if resp.Status != protocol.DialRunnerStatus_ViaRelayFailed {
		t.Errorf("expected ViaRelayFailed on cyclic Via chain, got %v", resp.Status)
	}

	// Step 3 calls ViaSendEstablishRelay once for the direct via (P).
	// Step 3b detects the loop before dispatching any goroutine (zero additional calls).
	// Total = 1.
	if n := atomic.LoadInt32(&callCnt); n != 1 {
		t.Errorf("expected ViaSendEstablishRelay called 1 time (Step 3 only, Step 3b loop-detected), got %d", n)
	}
}

// startFakeListenRunner builds a Mutual WS endpoint on listenAddr, accepts
// one peer.Conn via GetNewActiveConnectionChannel, wraps it, and records
// the first inbound app-payload kind + payload-body bytes (i.e. excluding
// the kind byte prefix). Returns a cleanup function.
//
// Mirrors the pattern in runner/listen.go (buildListenEndpoint, WS-only).
func startFakeListenRunner(t *testing.T, listenAddr string, firstKind chan<- appwire.AppKind, firstPayload chan<- []byte) (cleanup func()) {
	t.Helper()
	mux := http.NewServeMux()
	ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   "/ws",
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatalf("ws endpoint: %v", err)
	}

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	// Allow the listener to bind.
	time.Sleep(150 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-ctx.Done():
			return
		case conn := <-ep.GetNewActiveConnectionChannel():
			if conn == nil {
				return
			}
			// Wrap into peer.Conn so we can SetOnControl + Start.
			pc := peer.WrapAcceptedConn(ctx, conn, peer.DialConfig{
				Logger: slog.Default(),
			})
			pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
				select {
				case firstKind <- kind:
				default:
				}
				// peer.Conn.dispatch already stripped the kind prefix byte;
				// payload is the raw body after the kind byte.
				select {
				case firstPayload <- append([]byte(nil), payload...):
				default:
				}
			})
			pc.Start(ctx)
			<-ctx.Done()
			pc.Close()
		}
	}()

	return func() {
		cancel()
		shutdownCtx, c := context.WithTimeout(context.Background(), 1*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}
}
