package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The sweep publishes only what MOVED. An idle server must emit nothing at all
// — that is the property that makes a push cheaper than every client polling,
// not merely tidier.
func TestForwardStatsSweepIsSilentWhenNothingMoved(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore()}
	var kinds []protocol.StatusEventKind
	h.OnForwardEvent = func(kind protocol.StatusEventKind, _ *portForward) {
		kinds = append(kinds, kind)
	}
	pf := &portForward{taskIDHex: "aa", conns: map[uint64]connBytes{}}
	h.pforwards().add(pf)

	// First sweep publishes the initial state (nothing has been published yet).
	h.sweepForwardStats()
	if len(kinds) != 1 || kinds[0] != protocol.StatusEventKind_ForwardStats {
		t.Fatalf("first sweep: %v", kinds)
	}
	// Nothing crossed since; three more sweeps must be silent.
	h.sweepForwardStats()
	h.sweepForwardStats()
	h.sweepForwardStats()
	if len(kinds) != 1 {
		t.Fatalf("an idle forward published %d events, want 1", len(kinds))
	}

	// A byte crosses: exactly one more event, however many sweeps follow.
	pf.noteBytes(protocol.ForwardTapDirection_ToTarget, 10)
	h.sweepForwardStats()
	h.sweepForwardStats()
	if len(kinds) != 2 {
		t.Fatalf("after one change: %d events, want 2", len(kinds))
	}
}

// Many bytes between two sweeps are one event, not one per byte. This is the
// whole reason the counters ride a clock instead of the change itself.
func TestForwardStatsCoalescesABurst(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore()}
	n := 0
	h.OnForwardEvent = func(protocol.StatusEventKind, *portForward) { n++ }
	pf := &portForward{taskIDHex: "aa", conns: map[uint64]connBytes{}}
	h.pforwards().add(pf)
	h.sweepForwardStats() // initial
	n = 0

	for i := 0; i < 10000; i++ {
		pf.noteBytes(protocol.ForwardTapDirection_ToTarget, 1)
	}
	h.sweepForwardStats()
	if n != 1 {
		t.Fatalf("10000 byte changes produced %d events, want 1", n)
	}
}

// taps= moves without a byte crossing, and the listing reports it, so the sweep
// has to notice it too — otherwise a row shows taps=0 while someone reads it.
func TestForwardStatsNoticesTapCountChanges(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore()}
	n := 0
	h.OnForwardEvent = func(protocol.StatusEventKind, *portForward) { n++ }
	pf := &portForward{taskIDHex: "aa", conns: map[uint64]connBytes{}}
	h.pforwards().add(pf)
	h.sweepForwardStats()
	n = 0

	tap := newForwardTap(nil, protocol.ForwardTapFilter_Both, 0)
	pf.addTap(tap)
	h.sweepForwardStats()
	if n != 1 {
		t.Fatalf("a new tap produced %d events, want 1", n)
	}
	pf.removeTap(tap)
	h.sweepForwardStats()
	if n != 2 {
		t.Fatalf("a departing tap produced no event (total %d)", n)
	}
}

// Registered and closed bracket a forward's life on the stream, so a subscriber
// upserts and drops without ever refetching.
func TestForwardRegisteredAndClosedAreAnnounced(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	var kinds []protocol.StatusEventKind
	h.OnForwardEvent = func(kind protocol.StatusEventKind, _ *portForward) {
		kinds = append(kinds, kind)
	}
	pf := &portForward{taskIDHex: "aa", conns: map[uint64]connBytes{}}
	h.pforwards().add(pf)
	h.emitForwardEvent(protocol.StatusEventKind_ForwardRegistered, pf)
	h.teardownPortForward(pf, protocol.PortForwardCloseReason_Killed, false)

	if len(kinds) != 2 ||
		kinds[0] != protocol.StatusEventKind_ForwardRegistered ||
		kinds[1] != protocol.StatusEventKind_ForwardClosed {
		t.Fatalf("kinds = %v", kinds)
	}
}
