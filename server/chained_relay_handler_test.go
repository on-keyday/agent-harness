package server

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// buildTestCID is a helper that returns a ConnectionID from a plain string,
// using the same format as the rest of the server package ("ws:host:port-id").
func buildTestCID(s string) objproto.ConnectionID {
	return objproto.MustParseConnectionID(s)
}

// addEntry adds a RunnerEntry directly to the registry for test setup.
func addEntry(reg *Registry, id string, via *RunnerEntry, viaDialAddr objproto.ConnectionID) *RunnerEntry {
	e := &RunnerEntry{
		ID:          id,
		Via:         via,
		ViaDialAddr: viaDialAddr,
		ActiveTasks: make(map[string]struct{}),
	}
	reg.Add(e)
	// Return the live pointer that was stored (not a copy).
	livePtr, _ := reg.GetByConnectionID(buildTestCID(id))
	return livePtr
}

// noopSendEstablishRelay is a stub that never gets called; fails the test if called.
func noopSendEstablishRelay(t *testing.T) func(context.Context, *RunnerEntry, protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
	return func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
		t.Helper()
		t.Fatal("SendEstablishRelay must not be called in this test")
		return protocol.EstablishRelayResponse{}, nil
	}
}

// okSendEstablishRelay is a stub that always returns Ok.
func okSendEstablishRelay() func(context.Context, *RunnerEntry, protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
	return func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
		return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_Ok}, nil
	}
}

// TestChainedRelay_Direct: runner with Via==nil returns Direct without calling SendEstablishRelay.
func TestChainedRelay_Direct(t *testing.T) {
	reg := NewRegistry()
	// L has no Via.
	lCID := buildTestCID("ws:127.0.0.1:9001-1")
	addEntry(reg, lCID.String(), nil, objproto.ConnectionID{})

	h := &ChainedRelayHandler{
		Logger:             slog.Default(),
		Registry:           reg,
		SendEstablishRelay: noopSendEstablishRelay(t),
	}

	conn := &fakeConn{id: lCID}
	resp := h.Handle(context.Background(), conn, protocol.RequestChainedRelay{SlotId: 42})
	if resp.Status != protocol.ChainedRelayStatus_Direct {
		t.Errorf("expected Direct, got %v", resp.Status)
	}
}

// TestChainedRelay_2Hop: L → P → server. SendEstablishRelay called once for P
// with Target=L.ViaDialAddr and SlotId=42.
func TestChainedRelay_2Hop(t *testing.T) {
	reg := NewRegistry()

	// P: directly registered (no Via).
	pCID := buildTestCID("ws:127.0.0.1:9002-1")
	pEntry := addEntry(reg, pCID.String(), nil, objproto.ConnectionID{})

	// L.ViaDialAddr is the address P uses to forward to L.
	lDialAddr := buildTestCID("ws:10.0.0.1:8540-0")
	lCID := buildTestCID("ws:127.0.0.1:9002-2")
	addEntry(reg, lCID.String(), pEntry, lDialAddr)

	var (
		callCount  int32
		calledEntry *RunnerEntry
		calledSlot  uint16
		calledTarget protocol.RunnerID
		mu          sync.Mutex
	)

	h := &ChainedRelayHandler{
		Logger:   slog.Default(),
		Registry: reg,
		SendEstablishRelay: func(_ context.Context, entry *RunnerEntry, req protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			mu.Lock()
			atomic.AddInt32(&callCount, 1)
			calledEntry = entry
			calledSlot = req.SlotId
			calledTarget = req.Target
			mu.Unlock()
			return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_Ok}, nil
		},
	}

	conn := &fakeConn{id: lCID}
	resp := h.Handle(context.Background(), conn, protocol.RequestChainedRelay{SlotId: 42})
	if resp.Status != protocol.ChainedRelayStatus_Ok {
		t.Fatalf("expected Ok, got %v", resp.Status)
	}
	if n := atomic.LoadInt32(&callCount); n != 1 {
		t.Errorf("expected SendEstablishRelay called 1 time, got %d", n)
	}
	if calledEntry != pEntry {
		t.Errorf("expected SendEstablishRelay called for P entry, got %v", calledEntry)
	}
	if calledSlot != 42 {
		t.Errorf("expected SlotId=42, got %d", calledSlot)
	}
	expectedTarget := protocol.ConnIDToRunnerID(lDialAddr)
	// RunnerID contains slices — compare by encoded form.
	expectedBytes, _ := expectedTarget.Append(nil)
	calledBytes, _ := calledTarget.Append(nil)
	if !bytes.Equal(expectedBytes, calledBytes) {
		t.Errorf("expected Target=%v, got %v", expectedTarget, calledTarget)
	}
}

// TestChainedRelay_3Hop_Parallel: L → P → Q → server. Both P and Q receive
// an EstablishRelayRequest. Stubs delay 200ms each. Total time should be
// ~200ms (parallel), not ~400ms (sequential).
func TestChainedRelay_3Hop_Parallel(t *testing.T) {
	reg := NewRegistry()

	// Q: directly registered.
	qCID := buildTestCID("ws:127.0.0.1:9003-1")
	qEntry := addEntry(reg, qCID.String(), nil, objproto.ConnectionID{})

	// P via Q. P.ViaDialAddr is what Q uses for SetProxy.allocate → P's addr.
	pDialAddr := buildTestCID("ws:10.0.0.2:8541-0")
	pCID := buildTestCID("ws:127.0.0.1:9003-2")
	pEntry := addEntry(reg, pCID.String(), qEntry, pDialAddr)

	// L via P. L.ViaDialAddr is what P uses for SetProxy.allocate → L's addr.
	lDialAddr := buildTestCID("ws:10.0.0.3:8542-0")
	lCID := buildTestCID("ws:127.0.0.1:9003-3")
	addEntry(reg, lCID.String(), pEntry, lDialAddr)

	var callCount int32

	h := &ChainedRelayHandler{
		Logger:   slog.Default(),
		Registry: reg,
		HopTimeout: 5 * time.Second,
		SendEstablishRelay: func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			time.Sleep(200 * time.Millisecond)
			atomic.AddInt32(&callCount, 1)
			return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_Ok}, nil
		},
	}

	conn := &fakeConn{id: lCID}
	start := time.Now()
	resp := h.Handle(context.Background(), conn, protocol.RequestChainedRelay{SlotId: 7})
	elapsed := time.Since(start)

	if resp.Status != protocol.ChainedRelayStatus_Ok {
		t.Fatalf("expected Ok, got %v", resp.Status)
	}
	if n := atomic.LoadInt32(&callCount); n != 2 {
		t.Errorf("expected SendEstablishRelay called 2 times, got %d", n)
	}
	// Parallel: should complete in ~200ms, not ~400ms.
	if elapsed >= 350*time.Millisecond {
		t.Errorf("expected parallel dispatch (~200ms), but took %v (suggests sequential)", elapsed)
	}
}

// TestChainedRelay_HopFailure: 2-hop chain where the hop returns non-Ok.
// Handle should return HopSetupFailed.
func TestChainedRelay_HopFailure(t *testing.T) {
	reg := NewRegistry()

	pCID := buildTestCID("ws:127.0.0.1:9004-1")
	pEntry := addEntry(reg, pCID.String(), nil, objproto.ConnectionID{})

	lDialAddr := buildTestCID("ws:10.0.0.1:8543-0")
	lCID := buildTestCID("ws:127.0.0.1:9004-2")
	addEntry(reg, lCID.String(), pEntry, lDialAddr)

	h := &ChainedRelayHandler{
		Logger:   slog.Default(),
		Registry: reg,
		SendEstablishRelay: func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_SlotCollision}, nil
		},
	}

	conn := &fakeConn{id: lCID}
	resp := h.Handle(context.Background(), conn, protocol.RequestChainedRelay{SlotId: 10})
	if resp.Status != protocol.ChainedRelayStatus_HopSetupFailed {
		t.Errorf("expected HopSetupFailed, got %v", resp.Status)
	}
}

// TestChainedRelay_LoopDetection: cyclic Via chain A ↔ B. Handle returns ChainUnwalkable.
func TestChainedRelay_LoopDetection(t *testing.T) {
	reg := NewRegistry()

	// Build two entries with a cycle: A.Via = B, B.Via = A.
	// We need to construct them as pointers before adding to the registry
	// so we can wire the cycle.
	dialAddrA := buildTestCID("ws:10.0.1.1:8550-0")
	dialAddrB := buildTestCID("ws:10.0.1.2:8551-0")

	aCID := buildTestCID("ws:127.0.0.1:9005-1")
	bCID := buildTestCID("ws:127.0.0.1:9005-2")

	entryA := &RunnerEntry{
		ID:          aCID.String(),
		ActiveTasks: make(map[string]struct{}),
		ViaDialAddr: dialAddrA,
	}
	entryB := &RunnerEntry{
		ID:          bCID.String(),
		ActiveTasks: make(map[string]struct{}),
		ViaDialAddr: dialAddrB,
	}
	// Create the cycle.
	entryA.Via = entryB
	entryB.Via = entryA

	reg.Add(entryA)
	reg.Add(entryB)

	h := &ChainedRelayHandler{
		Logger:             slog.Default(),
		Registry:           reg,
		SendEstablishRelay: noopSendEstablishRelay(t),
	}

	// Request from A — its Via chain loops through B → A → ...
	conn := &fakeConn{id: aCID}
	resp := h.Handle(context.Background(), conn, protocol.RequestChainedRelay{SlotId: 99})
	if resp.Status != protocol.ChainedRelayStatus_ChainUnwalkable {
		t.Errorf("expected ChainUnwalkable, got %v", resp.Status)
	}
}

// TestChainedRelay_AnotherInFlight: a second concurrent Handle from the same
// runner returns AnotherInFlight immediately. After the first completes, a
// third call works normally.
func TestChainedRelay_AnotherInFlight(t *testing.T) {
	reg := NewRegistry()

	pCID := buildTestCID("ws:127.0.0.1:9006-1")
	pEntry := addEntry(reg, pCID.String(), nil, objproto.ConnectionID{})

	lDialAddr := buildTestCID("ws:10.0.0.1:8560-0")
	lCID := buildTestCID("ws:127.0.0.1:9006-2")
	addEntry(reg, lCID.String(), pEntry, lDialAddr)

	// blockCh holds the first Handle call until the test releases it.
	blockCh := make(chan struct{})
	var firstDone atomic.Bool

	h := &ChainedRelayHandler{
		Logger:   slog.Default(),
		Registry: reg,
		SendEstablishRelay: func(_ context.Context, _ *RunnerEntry, _ protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error) {
			<-blockCh // block until test releases
			return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_Ok}, nil
		},
	}

	conn := &fakeConn{id: lCID}

	// Start the first call in the background.
	firstRespCh := make(chan protocol.ChainedRelayResponse, 1)
	go func() {
		r := h.Handle(context.Background(), conn, protocol.RequestChainedRelay{SlotId: 1})
		firstDone.Store(true)
		firstRespCh <- r
	}()

	// Give the goroutine time to enter Handle and register in-flight.
	time.Sleep(20 * time.Millisecond)

	// Second call from the same conn should return AnotherInFlight immediately.
	second := h.Handle(context.Background(), conn, protocol.RequestChainedRelay{SlotId: 2})
	if second.Status != protocol.ChainedRelayStatus_AnotherInFlight {
		t.Errorf("expected AnotherInFlight for second call, got %v", second.Status)
	}
	if firstDone.Load() {
		t.Error("first call should still be in flight when second is checked")
	}

	// Release the first call.
	close(blockCh)
	first := <-firstRespCh
	if first.Status != protocol.ChainedRelayStatus_Ok {
		t.Errorf("expected Ok for first call after release, got %v", first.Status)
	}

	// Third call should work now (no in-flight guard blocking it).
	// Wire a fresh stub that returns Ok immediately.
	h.SendEstablishRelay = okSendEstablishRelay()
	third := h.Handle(context.Background(), conn, protocol.RequestChainedRelay{SlotId: 3})
	if third.Status != protocol.ChainedRelayStatus_Ok {
		t.Errorf("expected Ok for third call, got %v", third.Status)
	}
}
