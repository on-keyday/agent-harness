package server

import (
	"context"
	"encoding/hex"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// makeEventServer builds a minimal Server with New() so onConnEvent is wired,
// but without starting a listener. The pubsub tap is used to capture
// conns.status events without a real wire subscriber.
func makeEventServer(t *testing.T) *Server {
	t.Helper()
	s := New(Config{})
	// Reset pubsub so OnSubscribe isn't nil — it is wired in New for
	// notification replay and is not needed here; the tap approach works fine.
	return s
}

// captureConnEvents returns a function that captures ConnStatusEvent payloads
// published to conns.status via a pubsub Tap, and a getter that returns them.
func captureConnEvents(s *Server) (getEvents func() []protocol.ConnStatusEvent) {
	var mu sync.Mutex
	var events []protocol.ConnStatusEvent
	s.pubsub.TapSubscribe(topics.ConnsStatus(), func(_ string, msg []byte) {
		var ev protocol.ConnStatusEvent
		if err := ev.DecodeExact(msg); err != nil {
			return
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	return func() []protocol.ConnStatusEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]protocol.ConnStatusEvent, len(events))
		copy(out, events)
		return out
	}
}

// TestConnEvents_OpenIdentifyClose verifies the full open→identify→close lifecycle
// for a client (CLI) connection:
//
//   - conn_opened fires immediately after register: role Unspecified, Identified=false
//   - conn_identified fires after RecordClientIdentity: role Cli, Identified=true
//   - conn_closed fires in the teardown defer before activeConns delete
//   - all three events carry the same Cid string
func TestConnEvents_OpenIdentifyClose(t *testing.T) {
	s := makeEventServer(t)
	getEvents := captureConnEvents(s)

	now := time.Now()
	cidStr := "ws:127.0.0.1:9500-1"
	cid := objproto.MustParseConnectionID(cidStr)

	// Build the streamingConn as handleConnection would.
	wrapped := streamingConn{
		Connection:     &fakeRawConn{cid: cid},
		connectedSince: now,
	}

	// Register into activeConns (mirrors handleConnection's register step).
	s.activeConnsMu.Lock()
	s.activeConns[cid] = wrapped
	s.activeConnsMu.Unlock()

	// Emit conn_opened (mirrors what handleConnection does after register).
	if s.onConnEvent != nil {
		s.onConnEvent(protocol.StatusEventKind_ConnOpened, wrapped)
	}

	// Assert: 1 event, kind=conn_opened, Identified=false, role=Unspecified.
	evs := getEvents()
	if len(evs) != 1 {
		t.Fatalf("after conn_opened: expected 1 event, got %d", len(evs))
	}
	openEv := evs[0]
	if openEv.Kind != protocol.StatusEventKind_ConnOpened {
		t.Errorf("event[0].Kind = %v, want ConnOpened", openEv.Kind)
	}
	if openEv.Info.Identified() {
		t.Errorf("conn_opened: Info.Identified should be false (unauthed)")
	}
	if openEv.Info.Role != protocol.ConnRole_Unspecified {
		t.Errorf("conn_opened: Info.Role = %v, want Unspecified", openEv.Info.Role)
	}
	openCID := string(openEv.Info.Cid)
	if openCID != cidStr {
		t.Errorf("conn_opened: Cid = %q, want %q", openCID, cidStr)
	}

	// Record client identity (CLI kind). This calls OnConnIdentified which
	// publishes conn_identified.
	hello := &protocol.ClientHello{Kind: protocol.ClientKind_Cli}
	fc := &fakeConn{id: cid}
	status := s.taskHandler.RecordClientIdentity(cidStr, fc, hello)
	if status != protocol.ClientHelloStatus_Ok {
		t.Fatalf("RecordClientIdentity: status=%v", status)
	}

	// Assert: 2 events now; event[1] = conn_identified, role=Cli, Identified=true.
	evs = getEvents()
	if len(evs) != 2 {
		t.Fatalf("after conn_identified: expected 2 events, got %d", len(evs))
	}
	identEv := evs[1]
	if identEv.Kind != protocol.StatusEventKind_ConnIdentified {
		t.Errorf("event[1].Kind = %v, want ConnIdentified", identEv.Kind)
	}
	if !identEv.Info.Identified() {
		t.Errorf("conn_identified: Info.Identified should be true")
	}
	if identEv.Info.Role != protocol.ConnRole_Cli {
		t.Errorf("conn_identified: Info.Role = %v, want Cli", identEv.Info.Role)
	}
	identCID := string(identEv.Info.Cid)
	if identCID != cidStr {
		t.Errorf("conn_identified: Cid = %q, want %q (all 3 must match)", identCID, cidStr)
	}

	// Emit conn_closed (mirrors the defer in handleConnection).
	if s.onConnEvent != nil {
		s.onConnEvent(protocol.StatusEventKind_ConnClosed, wrapped)
	}
	s.activeConnsMu.Lock()
	delete(s.activeConns, cid)
	s.activeConnsMu.Unlock()

	// Assert: 3 events; event[2] = conn_closed, same Cid.
	evs = getEvents()
	if len(evs) != 3 {
		t.Fatalf("after conn_closed: expected 3 events, got %d", len(evs))
	}
	closeEv := evs[2]
	if closeEv.Kind != protocol.StatusEventKind_ConnClosed {
		t.Errorf("event[2].Kind = %v, want ConnClosed", closeEv.Kind)
	}
	closeCID := string(closeEv.Info.Cid)
	if closeCID != cidStr {
		t.Errorf("conn_closed: Cid = %q, want %q", closeCID, cidStr)
	}
}

// TestConnEvents_RunnerIdentified verifies that a runner conn emits
// conn_opened (Unspecified) then conn_identified (Runner) when RunnerHello
// is processed via runnerHandler.Handle.
func TestConnEvents_RunnerIdentified(t *testing.T) {
	s := makeEventServer(t)
	getEvents := captureConnEvents(s)

	now := time.Now()
	cidStr := "ws:127.0.0.1:9501-1"
	cid := objproto.MustParseConnectionID(cidStr)

	wrapped := streamingConn{
		Connection:     &fakeRawConn{cid: cid},
		connectedSince: now,
	}

	s.activeConnsMu.Lock()
	s.activeConns[cid] = wrapped
	s.activeConnsMu.Unlock()

	// conn_opened.
	if s.onConnEvent != nil {
		s.onConnEvent(protocol.StatusEventKind_ConnOpened, wrapped)
	}

	evs := getEvents()
	if len(evs) != 1 || evs[0].Kind != protocol.StatusEventKind_ConnOpened {
		t.Fatalf("expected 1 conn_opened event, got %d events", len(evs))
	}
	if evs[0].Info.Role != protocol.ConnRole_Unspecified || evs[0].Info.Identified() {
		t.Errorf("conn_opened for runner: expected Unspecified+false, got role=%v identified=%v",
			evs[0].Info.Role, evs[0].Info.Identified())
	}

	// Simulate RunnerHello via runnerHandler (populates registry, calls OnConnIdentified).
	fc := &fakeConn{id: cid}
	helloMsg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	hello := protocol.RunnerHello{MaxTasks: 1}
	hello.SetHostname([]byte("runner-host"))
	helloMsg.SetHello(hello)
	payload, err := helloMsg.Append(nil)
	if err != nil {
		t.Fatalf("encode RunnerHello: %v", err)
	}
	// RunnerHandler.Handle needs a non-nil Now.
	s.runnerHandler.Now = func() time.Time { return now }
	s.runnerHandler.Handle(fc, payload)

	evs = getEvents()
	if len(evs) != 2 {
		t.Fatalf("after RunnerHello: expected 2 events, got %d", len(evs))
	}
	identEv := evs[1]
	if identEv.Kind != protocol.StatusEventKind_ConnIdentified {
		t.Errorf("event[1].Kind = %v, want ConnIdentified", identEv.Kind)
	}
	if !identEv.Info.Identified() {
		t.Errorf("runner conn_identified: Identified should be true")
	}
	if identEv.Info.Role != protocol.ConnRole_Runner {
		t.Errorf("runner conn_identified: Role = %v, want Runner", identEv.Info.Role)
	}
	if string(identEv.Info.Cid) != cidStr {
		t.Errorf("runner conn_identified: Cid = %q, want %q", string(identEv.Info.Cid), cidStr)
	}
}

// TestConnEvents_SubtreeGating verifies that a confined subscriber (non-zero
// principal, no InfoGlobal) only receives conn_opened/closed events for agent
// conns in its subtree, not for CLI conns or unidentified conns.
func TestConnEvents_SubtreeGating(t *testing.T) {
	s := makeEventServer(t)

	// Create parent task P and child task C.
	// P must NOT have InfoGlobal so it is treated as a confined viewer.
	var pidTask, cidTask protocol.TaskID
	pHex := s.tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)
	copyHexToID(t, pHex, &pidTask)
	cHex := s.tasks.Create("/r", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, pidTask, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)
	copyHexToID(t, cHex, &cidTask)

	// Create a subscriber conn that identifies as agent with principal=P.
	subscriberCIDStr := "ws:127.0.0.1:9502-99"
	subscriberCID := objproto.MustParseConnectionID(subscriberCIDStr)
	// Record the subscriber's identity in the task handler.
	recordAgent(s, subscriberCIDStr, pidTask)

	// Use a tap to capture raw events, then decode each and check delivery.
	// We simulate per-subscriber filtering by calling the publishConnEvent logic
	// directly: register the subscriber into pubsub and assert which events arrive.
	//
	// Since PublishFiltered uses the subscriber's CID to check visibility, we
	// need the subscriber to be registered in pubsub. We do this by creating a
	// minimal pubsub.Subscriber with the agent's CID and subscribing it to
	// conns.status via a fake transport.
	//
	// For test simplicity: use TapSubscribe to capture ALL events (operator-level),
	// then verify that connInfoFor returns nil for non-subtree conns when called
	// with the confined viewer's allowed set.

	// Verify the visibility predicate directly for the 3 conn types:
	// (i) Agent conn with principal=C (child of P) → visible to P.
	now := time.Now()
	agentCCIDStr := "ws:127.0.0.1:9502-1"
	agentCCID := objproto.MustParseConnectionID(agentCCIDStr)
	addActiveConn(s, agentCCIDStr, now)
	recordAgent(s, agentCCIDStr, cidTask)
	agentCSC, _ := func() (streamingConn, bool) {
		s.activeConnsMu.Lock()
		defer s.activeConnsMu.Unlock()
		sc, ok := s.activeConns[agentCCID]
		return sc, ok
	}()

	// (ii) CLI conn → NOT visible to confined subscriber P.
	cliCIDStr := "ws:127.0.0.1:9502-2"
	addActiveConn(s, cliCIDStr, now)
	if s.taskHandler.clientKinds == nil {
		s.taskHandler.clientKinds = make(map[string]protocol.ClientKind)
	}
	s.taskHandler.clientKinds[cliCIDStr] = protocol.ClientKind_Cli
	cliCID := objproto.MustParseConnectionID(cliCIDStr)
	cliSC, _ := func() (streamingConn, bool) {
		s.activeConnsMu.Lock()
		defer s.activeConnsMu.Unlock()
		sc, ok := s.activeConns[cliCID]
		return sc, ok
	}()

	// Build the confined viewer's allowed set (subtree of P).
	_, allowed := s.taskHandler.visibleToCaller(subscriberCIDStr)

	// Agent conn with principal=C (child of P): should be visible.
	agentInfo := s.connInfoFor(agentCSC, allowed, false)
	if agentInfo == nil {
		t.Errorf("agent conn with principal=C (child of P): expected visible to confined viewer P, got nil")
	}

	// CLI conn: NOT visible to confined viewer.
	cliInfo := s.connInfoFor(cliSC, allowed, false)
	if cliInfo != nil {
		t.Errorf("CLI conn: expected NOT visible to confined viewer P, got non-nil ConnInfo")
	}

	// Operator / InfoGlobal viewer sees all.
	allInfos := s.ConnList(protocol.TaskID{}, true)
	// At this point we have: subscriberCID (not in activeConns; just in clientKinds),
	// agentCCID, cliCID. Check that global view sees both in activeConns.
	foundAgent, foundCLI := false, false
	for _, info := range allInfos {
		switch string(info.Cid) {
		case agentCCIDStr:
			foundAgent = true
		case cliCIDStr:
			foundCLI = true
		}
	}
	if !foundAgent {
		t.Errorf("global view: agent conn %s not found", agentCCIDStr)
	}
	if !foundCLI {
		t.Errorf("global view: CLI conn %s not found", cliCIDStr)
	}

	_ = subscriberCID // only used via its string in visibleToCaller
}

// TestConnEvents_SameCIDAllThree verifies that the CID field is identical
// across all three events emitted for one connection lifetime.
func TestConnEvents_SameCIDAllThree(t *testing.T) {
	s := makeEventServer(t)
	getEvents := captureConnEvents(s)

	now := time.Now()
	cidStr := "ws:127.0.0.1:9503-1"
	cid := objproto.MustParseConnectionID(cidStr)

	wrapped := streamingConn{
		Connection:     &fakeRawConn{cid: cid},
		connectedSince: now,
	}
	s.activeConnsMu.Lock()
	s.activeConns[cid] = wrapped
	s.activeConnsMu.Unlock()

	if s.onConnEvent != nil {
		s.onConnEvent(protocol.StatusEventKind_ConnOpened, wrapped)
	}

	// Record identity as agent with a task.
	var principalID protocol.TaskID
	principalBytes, _ := hex.DecodeString("0102030405060708090a0b0c0d0e0f10")
	copy(principalID.Id[:], principalBytes)
	s.tasks.Create("/r", "t", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	recordAgent(s, cidStr, principalID)
	// Manually trigger OnConnIdentified (RecordClientIdentity already records via clientKinds,
	// but in this test we bypassed the hello path so call the hook directly).
	if s.taskHandler.OnConnIdentified != nil {
		s.taskHandler.OnConnIdentified(cidStr)
	}

	if s.onConnEvent != nil {
		s.onConnEvent(protocol.StatusEventKind_ConnClosed, wrapped)
	}

	evs := getEvents()
	if len(evs) != 3 {
		t.Fatalf("expected 3 events (open, identified, closed), got %d", len(evs))
	}
	for i, ev := range evs {
		got := string(ev.Info.Cid)
		if got != cidStr {
			t.Errorf("event[%d].Cid = %q, want %q (all three must carry the same Cid)", i, got, cidStr)
		}
	}
	if evs[0].Kind != protocol.StatusEventKind_ConnOpened {
		t.Errorf("event[0].Kind = %v, want ConnOpened", evs[0].Kind)
	}
	if evs[1].Kind != protocol.StatusEventKind_ConnIdentified {
		t.Errorf("event[1].Kind = %v, want ConnIdentified", evs[1].Kind)
	}
	if evs[2].Kind != protocol.StatusEventKind_ConnClosed {
		t.Errorf("event[2].Kind = %v, want ConnClosed", evs[2].Kind)
	}
}

// TestConnEvents_PublishFilteredDelivery is an end-to-end test of the real
// PublishFiltered → allow() dispatch path. Unlike TestConnEvents_SubtreeGating
// (which calls connInfoFor directly), this registers REAL pubsub subscribers on
// the conns.status topic via the same broker Subscribe path the server uses,
// and asserts that PublishFiltered's allow(p.id) callback (CID-string conversion,
// taskHandler.visibleToCaller wiring) actually gates delivery per subscriber.
//
// Delivery is observed via the subscriber's send stream HasSendData(): the
// PublishFiltered path calls AppendData on the subscriber's stream only when
// allow() returns true. The transport is a real trsf.Streams not backed by a
// wire, so buffered send data stays observable without a round trip.
func TestConnEvents_PublishFilteredDelivery(t *testing.T) {
	s := makeEventServer(t)

	// Build a confined-caller subtree: leaf task L (no info_global). The confined
	// subscriber's principal is L, so it may see only agent conns in L's subtree.
	var leafTask protocol.TaskID
	lHex := s.tasks.Create("/r", "leaf", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)
	copyHexToID(t, lHex, &leafTask)

	// A separate unrelated task U whose agent conn is OUTSIDE L's subtree.
	var otherTask protocol.TaskID
	uHex := s.tasks.Create("/r", "other", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)
	copyHexToID(t, uHex, &otherTask)

	// --- Register the CONFINED subscriber on conns.status ---
	// Its CID's principal is leafTask, with no info_global.
	confinedCIDStr := "ws:127.0.0.1:9600-1"
	confinedCID := objproto.MustParseConnectionID(confinedCIDStr)
	recordAgent(s, confinedCIDStr, leafTask) // principal=L, caps=Spawn (no InfoGlobal)
	confinedStream := subscribeConnsStatus(t, s, confinedCID)

	// --- Register the OPERATOR subscriber on conns.status ---
	// Operator = zero principal (no clientKinds/principals entry) → sees all.
	operatorCIDStr := "ws:127.0.0.1:9600-2"
	operatorCID := objproto.MustParseConnectionID(operatorCIDStr)
	operatorStream := subscribeConnsStatus(t, s, operatorCID)

	now := time.Now()

	// --- (1) Event for a conn OUTSIDE the confined caller's subtree ---
	// Agent conn whose principal is otherTask (U), unrelated to L.
	outsideCIDStr := "ws:127.0.0.1:9600-10"
	outsideCID := objproto.MustParseConnectionID(outsideCIDStr)
	addActiveConn(s, outsideCIDStr, now)
	recordAgent(s, outsideCIDStr, otherTask)
	outsideSC := lookupSC(t, s, outsideCID)

	// Drain any data buffered by Subscribe handshake before measuring.
	drainStream(confinedStream)
	drainStream(operatorStream)

	s.onConnEvent(protocol.StatusEventKind_ConnIdentified, outsideSC)

	// Operator MUST receive it; confined MUST NOT.
	if !hasPendingData(operatorStream) {
		t.Errorf("outside-subtree event: operator subscriber should have received it (HasSendData=false)")
	}
	if hasPendingData(confinedStream) {
		t.Errorf("outside-subtree event: confined subscriber should NOT have received it (got data)")
	}

	// Drain before the next measurement.
	drainStream(confinedStream)
	drainStream(operatorStream)

	// --- (2) Event for a conn INSIDE the confined caller's subtree ---
	// Agent conn whose principal IS leafTask (the confined caller's own task).
	insideCIDStr := "ws:127.0.0.1:9600-11"
	insideCID := objproto.MustParseConnectionID(insideCIDStr)
	addActiveConn(s, insideCIDStr, now)
	recordAgent(s, insideCIDStr, leafTask)
	insideSC := lookupSC(t, s, insideCID)

	s.onConnEvent(protocol.StatusEventKind_ConnIdentified, insideSC)

	// Both confined AND operator MUST receive it.
	if !hasPendingData(confinedStream) {
		t.Errorf("in-subtree event: confined subscriber should have received it (HasSendData=false)")
	}
	if !hasPendingData(operatorStream) {
		t.Errorf("in-subtree event: operator subscriber should have received it (HasSendData=false)")
	}
}

// subscribeConnsStatus registers a real pubsub.Subscriber for cid on the
// conns.status topic via the broker's Subscribe path (the same path
// pubsub.Subscriber.HandleMessage invokes on a JOIN). The subscriber is keyed
// by the real ConnectionID, so PublishFiltered's allow(p.id) callback receives
// the genuine CID. The transport is a recordingTransport whose
// CreateBidirectionalStream returns a recordingStream that captures AppendData
// calls deterministically (no background pacing goroutine), so delivery vs skip
// is observable without a wire round trip.
func subscribeConnsStatus(t *testing.T, s *Server, cid objproto.ConnectionID) *recordingStream {
	t.Helper()
	tp := &recordingTransport{}
	sub := pubsub.NewSubscriber(cid, tp)
	resp := s.pubsub.Subscribe(1, topics.ConnsStatus(), "test", sub)
	if uint8(resp.Status) != 0 { // pubsub/protocol.Status_Ok == 0
		t.Fatalf("Subscribe(conns.status) status=%v", resp.Status)
	}
	if tp.created == nil {
		t.Fatal("Subscribe did not call CreateBidirectionalStream")
	}
	return tp.created
}

// lookupSC fetches the streamingConn registered under cid.
func lookupSC(t *testing.T, s *Server, cid objproto.ConnectionID) streamingConn {
	t.Helper()
	s.activeConnsMu.Lock()
	defer s.activeConnsMu.Unlock()
	sc, ok := s.activeConns[cid]
	if !ok {
		t.Fatalf("conn %s not in activeConns", cid.String())
	}
	return sc
}

// hasPendingData reports whether PublishFiltered's AppendData was called for
// this subscriber's stream since the last reset.
func hasPendingData(stream *recordingStream) bool {
	return stream.appendCount() > 0
}

// drainStream resets the recorded AppendData count so the next publish can be
// measured in isolation.
func drainStream(stream *recordingStream) {
	stream.reset()
}

// recordingTransport is a minimal trsf.Transport for the PublishFiltered
// delivery test. Only CreateBidirectionalStream is meaningful (it records the
// returned stream so the test can inspect deliveries); every other method is a
// stub never exercised by the pubsub.Subscribe path.
type recordingTransport struct {
	created *recordingStream
}

func (r *recordingTransport) CreateBidirectionalStream() trsf.BidirectionalStream {
	r.created = &recordingStream{}
	return r.created
}
func (r *recordingTransport) CreateSendStream() trsf.SendStream { return nil }
func (r *recordingTransport) AcceptBidirectionalStream(_ context.Context) (trsf.BidirectionalStream, error) {
	return nil, nil
}
func (r *recordingTransport) AcceptReceiveStream(_ context.Context) (trsf.ReceiveStream, error) {
	return nil, nil
}
func (r *recordingTransport) GetInternalState() *trsf.InternalState               { return nil }
func (r *recordingTransport) GetSendStream(_ trsf.StreamID) trsf.SendStream       { return nil }
func (r *recordingTransport) GetReceiveStream(_ trsf.StreamID) trsf.ReceiveStream { return nil }
func (r *recordingTransport) GetBidirectionalStream(_ trsf.StreamID) trsf.BidirectionalStream {
	return r.created
}
func (r *recordingTransport) Send(_ *objproto.Message)        {}
func (r *recordingTransport) Recv(_ context.Context) *trsf.SendAction { return nil }

// recordingStream is a trsf.BidirectionalStream that counts AppendData calls
// (the delivery the broker performs) and returns EOF from ReadDirect so the
// Subscribe read goroutine terminates immediately instead of spinning.
type recordingStream struct {
	mu      sync.Mutex
	appends int
}

func (s *recordingStream) appendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appends
}
func (s *recordingStream) reset() {
	s.mu.Lock()
	s.appends = 0
	s.mu.Unlock()
}
func (s *recordingStream) AppendData(_ bool, _ ...[]byte) error {
	s.mu.Lock()
	s.appends++
	s.mu.Unlock()
	return nil
}
func (s *recordingStream) AppendDataContext(_ context.Context, eof bool, p ...[]byte) error {
	return s.AppendData(eof, p...)
}
func (s *recordingStream) ID() trsf.StreamID                       { return 1 }
func (s *recordingStream) Write(p []byte) (int, error)             { return len(p), nil }
func (s *recordingStream) WriteContext(_ context.Context, p []byte) (int, error) {
	return len(p), nil
}
func (s *recordingStream) Close() error      { return nil }
func (s *recordingStream) HasSendData() bool { return false }
func (s *recordingStream) Completed() bool   { return true }
func (s *recordingStream) Read([]byte) (int, error)                            { return 0, io.EOF }
func (s *recordingStream) ReadContext(_ context.Context, _ []byte) (int, error) { return 0, io.EOF }
func (s *recordingStream) ReadDirect(_ uint64) ([]byte, bool, error)           { return nil, true, nil }
func (s *recordingStream) ReadDirectContext(_ context.Context, _ uint64) ([]byte, bool, error) {
	return nil, true, nil
}
func (s *recordingStream) HasRecvData() bool { return false }
func (s *recordingStream) EOF() bool         { return true }
func (s *recordingStream) Cancel()           {}
func (s *recordingStream) CloseBoth() error  { return nil }
