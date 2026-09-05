package server

import (
	"net/netip"
	"testing"
	"time"

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

// Forwarding rewrites a packet's connection id and sends it back out; doing
// that across transports has never been exercised, so a mixed pair takes the
// splice path instead.
func TestDataPlaneRouteRequiresOneTransport(t *testing.T) {
	if !dataPlaneRoute(cid("udp", 1, 10), cid("udp", 2, 11)) {
		t.Fatalf("udp/udp should route end to end")
	}
	if !dataPlaneRoute(cid("ws", 1, 10), cid("ws", 2, 11)) {
		t.Fatalf("ws/ws should route end to end")
	}
	if dataPlaneRoute(cid("ws", 1, 10), cid("udp", 2, 11)) {
		t.Fatalf("ws client with a udp runner must fall back to the splice")
	}
	if dataPlaneRoute(cid("", 1, 10), cid("", 2, 11)) {
		t.Fatalf("an empty transport must never route")
	}
}
