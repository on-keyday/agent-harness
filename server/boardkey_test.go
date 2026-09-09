package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A nil Board must be a no-op rather than a panic. Before the funnel every
// caller carried its own `if X.Board != nil` guard for this; the guard now
// lives once, so a caller that forgets one cannot reintroduce the crash — and
// nothing else pins that.
func TestBoardKeyHelpersNilBoardAreNoOps(t *testing.T) {
	boardRegisterTask(nil, "ws:127.0.0.1:8539-1", "aa", [16]byte{1}, "claude")
	boardRevokeTask(nil, "ws:127.0.0.1:8539-1", "aa")
	if tk, ok := boardTaskTicket(nil, "ws:127.0.0.1:8539-1", protocol.TaskID{}); ok || tk != ([16]byte{}) {
		t.Fatalf("boardTaskTicket(nil) = (%v, %v), want (zero, false)", tk, ok)
	}
}

// The funnel must round-trip through whatever key the board uses: a ticket
// registered under a runner CONNECTION id has to be findable by that same
// connection id, and gone after a revoke. This is the property a change to the
// key derivation must preserve, so it is asserted through the helpers rather
// than against the (rid, tid) pair they currently build.
func TestBoardKeyRegisterLookupRevokeRoundTrip(t *testing.T) {
	b := agentboard.New(agentboard.Config{})
	const runnerConnID = "ws:127.0.0.1:8539-7"
	taskIDHex := "00000000000000000000000000000011"
	tid := taskIDFromHex(taskIDHex)
	want := [16]byte{9, 8, 7}

	boardRegisterTask(b, runnerConnID, taskIDHex, want, "claude")
	got, ok := boardTaskTicket(b, runnerConnID, tid)
	if !ok || got != want {
		t.Fatalf("after register: ticket = (%v, %v), want (%v, true)", got, ok, want)
	}

	// A different runner connection is a different key — this is exactly what
	// makes a reconnect lose the credential, so pin it: the day it stops being
	// true is the day the decoupling landed, and this test should be the one
	// that says so.
	if _, ok := boardTaskTicket(b, "ws:127.0.0.1:8539-8", tid); ok {
		t.Fatal("ticket was findable under a different runner connection id")
	}

	boardRevokeTask(b, runnerConnID, taskIDHex)
	if _, ok := boardTaskTicket(b, runnerConnID, tid); ok {
		t.Fatal("ticket still registered after revoke")
	}
}
