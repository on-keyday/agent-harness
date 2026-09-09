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
	rid := protocol.RunnerID{Id: [16]byte{7}}
	boardRegisterTask(nil, rid, "aa", [16]byte{1}, "claude")
	boardRevokeTask(nil, rid, "aa")
	if tk, ok := boardTaskTicket(nil, rid, protocol.TaskID{}); ok || tk != ([16]byte{}) {
		t.Fatalf("boardTaskTicket(nil) = (%v, %v), want (zero, false)", tk, ok)
	}
}

// The zero identity is refused, not registered. The identity gate rejects a
// hello carrying one, so reaching the funnel with zero means some path bypassed
// the gate — and registering anyway would file the ticket under a key every such
// runner shares, where the next dispatch overwrites it.
func TestBoardRegisterRefusesZeroIdentity(t *testing.T) {
	b := agentboard.New(agentboard.Config{})
	taskIDHex := "00000000000000000000000000000021"
	boardRegisterTask(b, protocol.RunnerID{}, taskIDHex, [16]byte{9}, "claude")
	if _, ok := boardTaskTicket(b, protocol.RunnerID{}, taskIDFromHex(taskIDHex)); ok {
		t.Fatal("a ticket was registered under the zero identity")
	}
}

// The property this whole change exists for, and the inverse of what this test
// asserted before it: a ticket is keyed by the runner PROCESS, so it stays
// findable across that runner's reconnects. The previous version pinned that a
// ticket was NOT findable under a different runner connection id — true while
// the key was derived from the connection, and the assertion whose going red
// meant the decoupling had landed.
func TestBoardKeyIsIdentityNotConnection(t *testing.T) {
	b := agentboard.New(agentboard.Config{})
	reg := NewRegistry()

	identity := protocol.RunnerID{Id: [16]byte{0xAB, 0xCD}}
	taskIDHex := "00000000000000000000000000000011"
	tid := taskIDFromHex(taskIDHex)
	want := [16]byte{9, 8, 7}

	// The runner registers on one connection and the task is dispatched to it.
	reg.Add(&RunnerEntry{ID: "ws:127.0.0.1:8539-7", Identity: identity})
	boardRegisterTask(b, identity, taskIDHex, want, "claude")

	// It reconnects: a NEW connection id, the SAME identity. The old code
	// derived the board key from the connection, so this is exactly where an
	// agent's credential used to stop validating.
	displaced := reg.Add(&RunnerEntry{ID: "ws:127.0.0.1:8539-8", Identity: identity})
	if displaced != "ws:127.0.0.1:8539-7" {
		t.Fatalf("takeover did not report the displaced connection: %q", displaced)
	}
	if got, ok := boardTaskTicket(b, identity, tid); !ok || got != want {
		t.Fatalf("after reconnect: ticket = (%v, %v), want (%v, true)", got, ok, want)
	}

	// A DIFFERENT runner must not reach it, which is the half that still holds.
	other := protocol.RunnerID{Id: [16]byte{0xAB, 0xCE}}
	if _, ok := boardTaskTicket(b, other, tid); ok {
		t.Fatal("ticket was findable under a different runner identity")
	}

	boardRevokeTask(b, identity, taskIDHex)
	if _, ok := boardTaskTicket(b, identity, tid); ok {
		t.Fatal("ticket still registered after revoke")
	}
}

// identityOfConn is the one path that still starts from a connection id, and a
// miss must yield the zero identity rather than something plausible: the entry
// is gone, so there is nothing left to revoke.
func TestIdentityOfConn(t *testing.T) {
	reg := NewRegistry()
	identity := protocol.RunnerID{Id: [16]byte{3}}
	reg.Add(&RunnerEntry{ID: "ws:127.0.0.1:8539-3", Identity: identity})

	if got := identityOfConn(reg, "ws:127.0.0.1:8539-3"); got != identity {
		t.Fatalf("identityOfConn = %s, want %s", got.Hex(), identity.Hex())
	}
	if got := identityOfConn(reg, "ws:127.0.0.1:8539-4"); !got.IsZero() {
		t.Fatalf("unknown connection yielded %s, want zero", got.Hex())
	}
	if got := identityOfConn(nil, "ws:127.0.0.1:8539-3"); !got.IsZero() {
		t.Fatalf("nil registry yielded %s, want zero", got.Hex())
	}
}
