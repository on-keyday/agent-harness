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
