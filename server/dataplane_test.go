package server

import (
	"net/netip"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func TestMintGrantFileTransferCarriesDirection(t *testing.T) {
	tid := protocol.TaskID{Id: [16]uint8{4}}
	g := mintGrant(protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, tid, 5*time.Minute)
	if g.Kind != protocol.TaskControlKind_OpenFileTransfer {
		t.Fatalf("kind: %v", g.Kind)
	}
	if d := g.Direction(); d == nil || *d != protocol.FileTransferDirection_Pull {
		t.Fatalf("direction: %v", d)
	}
	if g.TaskId.Id != tid.Id {
		t.Fatalf("task id lost")
	}
	if g.ExpiresUnixMs <= uint64(time.Now().UnixMilli()) {
		t.Fatalf("expiry is not in the future")
	}
}

// list_files takes no direction, and the union must not carry one for it.
func TestMintGrantListFilesHasNoDirection(t *testing.T) {
	g := mintGrant(protocol.TaskControlKind_ListFiles, protocol.FileTransferDirection_Pull, protocol.TaskID{}, time.Minute)
	if g.Kind != protocol.TaskControlKind_ListFiles {
		t.Fatalf("kind: %v", g.Kind)
	}
	if d := g.Direction(); d != nil {
		t.Fatalf("list_files grant carries a direction: %v", *d)
	}
}

func TestMintGrantIdsAreRandom(t *testing.T) {
	a := mintGrant(protocol.TaskControlKind_ListFiles, 0, protocol.TaskID{}, time.Minute)
	b := mintGrant(protocol.TaskControlKind_ListFiles, 0, protocol.TaskID{}, time.Minute)
	if a.GrantId == b.GrantId {
		t.Fatalf("two mints produced the same grant id")
	}
	if a.GrantId == ([16]uint8{}) {
		t.Fatalf("grant id is zero")
	}
}

func TestRandomSlotIDAvoidsTheIdsItIsGiven(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := randomSlotID(7, 9)
		if got == 0 {
			t.Fatalf("drew an unusable slot id")
		}
		if got == 7 || got == 9 {
			t.Fatalf("drew a slot id it was told to avoid: %d", got)
		}
	}
}

func cid(transport string, port uint16, id uint16) objproto.ConnectionID {
	return objproto.NewConnectionID(transport,
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port), id)
}

// Both transports route now: a mixed pair is joined by negotiating the smaller
// packet size, not refused. What still cannot route is a connection with no
// transport at all, which is a zero ConnectionID rather than a real peer.
func TestDataPlaneRouteAcceptsMixedTransports(t *testing.T) {
	if !dataPlaneRoute(cid("udp", 1, 10), cid("udp", 2, 11)) {
		t.Fatalf("udp/udp should route")
	}
	if !dataPlaneRoute(cid("ws", 1, 10), cid("ws", 2, 11)) {
		t.Fatalf("ws/ws should route")
	}
	if !dataPlaneRoute(cid("ws", 1, 10), cid("udp", 2, 11)) {
		t.Fatalf("a ws client with a udp runner should route once the MTU is negotiated")
	}
	if dataPlaneRoute(cid("", 1, 10), cid("udp", 2, 11)) {
		t.Fatalf("an empty transport must never route")
	}
	if dataPlaneRoute(cid("udp", 1, 10), cid("", 2, 11)) {
		t.Fatalf("an empty transport must never route")
	}
}

// The smaller of the two sizes, and only when they differ: a same-transport
// pair keeps whatever each end would have derived on its own.
func TestNegotiatedMTU(t *testing.T) {
	if got := negotiatedMTU("udp", "udp"); got != 0 {
		t.Fatalf("same transport should negotiate nothing, got %d", got)
	}
	if got := negotiatedMTU("ws", "ws"); got != 0 {
		t.Fatalf("same transport should negotiate nothing, got %d", got)
	}
	wsInitial, _ := peer.MTUForTransport("ws")
	udpInitial, _ := peer.MTUForTransport("udp")
	if wsInitial <= udpInitial {
		t.Fatalf("this test assumes ws is the larger of the two: ws=%d udp=%d", wsInitial, udpInitial)
	}
	if got := negotiatedMTU("ws", "udp"); int(got) != udpInitial {
		t.Fatalf("ws/udp should take the udp size %d, got %d", udpInitial, got)
	}
	if got := negotiatedMTU("udp", "ws"); int(got) != udpInitial {
		t.Fatalf("udp/ws should take the udp size %d, got %d", udpInitial, got)
	}
}

// The record goes when the runner says the transfer ended -- that is the
// primary path, and it is what keeps an entry's life equal to the transfer's
// rather than to a timer.
func TestFinishDataPlaneDropsTheRecord(t *testing.T) {
	s := &Server{}
	now := uint64(time.Now().UnixMilli())
	s.rememberGrant("t1", issuedGrant{grantID: [16]byte{1}, issuedUnixMs: now})
	s.rememberGrant("t1", issuedGrant{grantID: [16]byte{2}, issuedUnixMs: now})

	s.finishDataPlane([16]byte{1})
	s.grantsMu.Lock()
	left := s.issuedGrants["t1"]
	s.grantsMu.Unlock()
	if len(left) != 1 || left[0].grantID != [16]byte{2} {
		t.Fatalf("finish removed the wrong record: %+v", left)
	}

	s.finishDataPlane([16]byte{2})
	s.grantsMu.Lock()
	_, taskKept := s.issuedGrants["t1"]
	s.grantsMu.Unlock()
	if taskKept {
		t.Fatalf("the task should hold no entry once its last grant finished")
	}

	// A finish for something already gone is a no-op, not a panic: the message
	// can arrive twice, or after a narrowing caps change already withdrew it.
	s.finishDataPlane([16]byte{9})
}

// The age floor is for a runner that died mid transfer and sent nothing. It
// must NOT be the grant's expiry: a transfer may legitimately outlive that, and
// dropping its record would take the server's ability to revoke it -- the one
// thing a narrowing caps change promises.
func TestRememberGrantFloorIsAgeNotGrantExpiry(t *testing.T) {
	s := &Server{}
	now := time.Now()
	old := uint64(now.Add(-2 * dataPlaneRecordMaxAge).UnixMilli())
	recent := uint64(now.Add(-dataPlaneGrantTTL * 2).UnixMilli()) // past the grant TTL, well inside the floor

	s.rememberGrant("ancient", issuedGrant{grantID: [16]byte{1}, issuedUnixMs: old})
	s.rememberGrant("long-transfer", issuedGrant{grantID: [16]byte{2}, issuedUnixMs: recent})
	s.rememberGrant("fresh", issuedGrant{grantID: [16]byte{3}, issuedUnixMs: uint64(now.UnixMilli())})

	s.grantsMu.Lock()
	_, ancientKept := s.issuedGrants["ancient"]
	_, longKept := s.issuedGrants["long-transfer"]
	s.grantsMu.Unlock()

	if ancientKept {
		t.Fatalf("a record older than the floor should be gone")
	}
	if !longKept {
		t.Fatalf("a transfer outliving the grant TTL must keep its record, or it stops being revocable")
	}
}
