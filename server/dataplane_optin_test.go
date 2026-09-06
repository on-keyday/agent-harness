package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The default is the splice, and this is where that is decided. Measured on
// scripts/netem-lab, forwarding loses on every path -- 1.23x at 4ms end-to-end
// RTT, 2.59x at 20ms, 3.06x at 200ms, 8.05x once 1% loss is added -- because
// the relay leaves one congestion loop spanning the whole path where the splice
// runs two, each over half of it.
//
// tryDataPlane must refuse BEFORE it looks at anything else, so a request that
// did not ask pays nothing at all: no grant minted, no authorize round trip to
// the runner, no proxy entry. It takes exactly the path it took before this
// design existed.
func TestDataPlaneRefusesWhenNotRequested(t *testing.T) {
	// A handler with nothing wired: if the refusal did not come first, this
	// would have to reach SetupDataPlane or a runner and would not return.
	h := &TaskHandler{}
	_, slot, _, mtu, ok := h.tryDataPlane(nil, nil,
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Push,
		protocol.TaskID{}, false)
	if ok {
		t.Fatal("routed a request that did not ask for it")
	}
	if slot != 0 || mtu != 0 {
		t.Fatalf("a refusal must hand back nothing: slot=%d mtu=%d", slot, mtu)
	}
}

// The bit is spelled data_plane, not no_data_plane, so that the default lives
// on the wire rather than in every call site remembering to pass a flag. A
// zero-valued request -- an old client, a caller that never heard of the route,
// a widget passing the default -- must splice.
func TestZeroValuedRequestsSplice(t *testing.T) {
	var oft protocol.OpenFileTransferRequest
	if oft.DataPlane() {
		t.Fatal("a zero-valued OpenFileTransferRequest asks for the route")
	}
	var lf protocol.ListFilesRequest
	if lf.DataPlane() {
		t.Fatal("a zero-valued ListFilesRequest asks for the route")
	}

	// And it survives a round trip, which is what an old peer actually sends.
	b, err := oft.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back protocol.OpenFileTransferRequest
	if _, err := back.Decode(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.DataPlane() {
		t.Fatal("the route bit came back set from an unset request")
	}
}
