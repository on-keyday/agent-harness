package server

import (
	"hash/fnv"
	"net/netip"
	"testing"

	"github.com/on-keyday/objtrsf/objproto"
)

// tcid turns a fixture label into a distinct, REAL ConnectionID.
//
// It exists because the fixtures it replaces were keying the registry with
// "r1", "A", "runner-1" — strings no connection id could ever be. That was
// possible only while the key was text, and it meant those tests exercised a
// map rather than the thing the map stands for. Now that RunnerEntry.ID is
// objproto.ConnectionID they cannot even be written, which is the point.
//
// Deterministic in the label so a test that mentions the same one twice gets
// the same connection, and distinct across labels so two fixtures are two
// runners.
func tcid(label string) objproto.ConnectionID {
	h := fnv.New32a()
	_, _ = h.Write([]byte(label))
	// Port 1024..65535: 0 is not a port and the low range reads as reserved.
	port := uint16(1024 + h.Sum32()%(65535-1024))
	return objproto.ConnectionID{
		Transport: "ws",
		Addr:      netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port),
		ID:        uint16(h.Sum32() & 0xffff),
	}
}

// The helper has to keep its own promises, or a fixture collision would look
// like a registry bug.
func TestTcidIsStableAndDistinct(t *testing.T) {
	if tcid("r1") != tcid("r1") {
		t.Error("the same label produced two connections")
	}
	seen := map[objproto.ConnectionID]string{}
	for _, l := range []string{"r1", "r2", "A", "runner-1", "runner-2", "host-a", "host-b"} {
		if prev, dup := seen[tcid(l)]; dup {
			t.Errorf("%q and %q collide", l, prev)
		}
		seen[tcid(l)] = l
	}
	// And it must round-trip through the canonical text, since that is what
	// the wire and the WAL carry.
	got, err := objproto.ParseConnectionID(tcid("r1").String(), 0)
	if err != nil {
		t.Fatalf("a fixture id is not parseable: %v", err)
	}
	if got != tcid("r1") {
		t.Errorf("round trip changed it: %v vs %v", got, tcid("r1"))
	}
}
