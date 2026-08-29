package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// registerTapTestForward puts a registration for taskHex into the registry and
// returns it. It bypasses handleRegisterPortForward on purpose: these tests are
// about the tap's gates, not about registration.
func registerTapTestForward(t *testing.T, h *TaskHandler, taskHex string) *portForward {
	t.Helper()
	pf := &portForward{
		direction:  protocol.PortForwardDirection_Local,
		taskIDHex:  taskHex,
		targetHost: "localhost",
		targetPort: 3000,
		conns:      map[uint64]connBytes{},
	}
	h.pforwards().add(pf)
	return pf
}

func tapConn(cid string) *fakeConn {
	return &fakeConn{id: objproto.MustParseConnectionID(cid)}
}

func TestOpenForwardTapUnknownIDIsNoSuchForward(t *testing.T) {
	h, p, _, _, _ := scopeFixture(t)
	conn := tapConn("ws:127.0.0.1:9970-1")
	_ = p

	got := h.handleOpenForwardTap(conn, &protocol.OpenForwardTapRequest{ForwardId: 999}, conn.ConnectionID().String())
	if got.Status != protocol.OpenForwardTapStatus_NoSuchForward {
		t.Fatalf("status %v", got.Status)
	}
	if got.StreamId != 0 {
		t.Fatal("a refused tap must not leave a stream behind")
	}
}

// An id belonging to a task the caller cannot see answers exactly what an
// unknown id answers. Forward ids come from a dense next++ counter, so a
// distinguishable refusal would be an enumeration oracle.
func TestOpenForwardTapInvisibleForwardIsNotAnOracle(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	pf := registerTapTestForward(t, h, u) // owned by a task outside c's subtree
	cid := bindPrincipal(t, h, c)
	conn := tapConn("ws:127.0.0.1:9971-1")

	got := h.handleOpenForwardTap(conn, &protocol.OpenForwardTapRequest{ForwardId: pf.forwardID}, cid)
	if got.Status != protocol.OpenForwardTapStatus_NoSuchForward {
		t.Fatalf("an invisible forward must answer no_such_forward, got %v", got.Status)
	}
	if pf.tapCount() != 0 {
		t.Fatal("a refused tap was still attached")
	}
}

// Visible through the visibility axis, but the ACTION scope for forward_tap
// does not contain the task: still refused. This is the distinction
// kill_port_forward draws, and the reason the bit is classified as resolved by
// name rather than as naming no task.
func TestOpenForwardTapRefusesOutOfActionScope(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	pf := registerTapTestForward(t, h, u)
	cid := bindPrincipal(t, h, c)
	// See everything, but act on nothing outside self.
	setScope(t, h, c, Scope{
		Base:    protocol.ScopeBase_None,
		VisBase: protocol.ScopeBase_Global, VisBasePresent: true,
	})
	conn := tapConn("ws:127.0.0.1:9972-1")

	got := h.handleOpenForwardTap(conn, &protocol.OpenForwardTapRequest{ForwardId: pf.forwardID}, cid)
	if got.Status != protocol.OpenForwardTapStatus_NoSuchForward {
		t.Fatalf("a forward visible but out of action scope must be refused, got %v", got.Status)
	}
}

func TestOpenForwardTapAttachesAndIsCountedOnTheListing(t *testing.T) {
	h, p, _, _, _ := scopeFixture(t)
	pf := registerTapTestForward(t, h, p)
	// An operator connection: no principal, so Capability_All and full scope.
	conn := tapConn("ws:127.0.0.1:9973-1")
	conn.nextStreamID = 77 // the handler needs a real stream to hand back

	got := h.handleOpenForwardTap(conn, &protocol.OpenForwardTapRequest{
		ForwardId:       pf.forwardID,
		DirectionFilter: protocol.ForwardTapFilter_Both,
	}, conn.ConnectionID().String())
	if got.Status != protocol.OpenForwardTapStatus_Ok {
		t.Fatalf("status %v", got.Status)
	}
	if pf.tapCount() != 1 {
		t.Fatalf("tapCount = %d after a successful attach", pf.tapCount())
	}
	if portForwardInfo(pf).Taps != 1 {
		t.Fatal("taps= must report the open tap: a forward must not be watchable invisibly")
	}
}

func TestOpenForwardTapNeedsForwardTapCap(t *testing.T) {
	want, gated := requiredCap[protocol.TaskControlKind_OpenForwardTap]
	if !gated {
		t.Fatal("open_forward_tap is not in requiredCap: reading a forward's payload would be ungated")
	}
	if want != protocol.Capability_ForwardTap {
		t.Fatalf("gated on %v, want forward_tap", want)
	}
	if want&(protocol.Capability_ForwardLocal|protocol.Capability_ForwardRemote) != 0 {
		t.Fatal("holding a forward must not satisfy a tap on it")
	}
}
