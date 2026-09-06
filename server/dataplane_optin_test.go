package server

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Splice is the default and it is decided here. Measured on scripts/netem-lab,
// forwarding loses on every path -- 1.23x at 4ms end-to-end RTT, 2.59x at 20ms,
// 3.06x at 200ms, 8.05x once 1% loss is added -- because the relay leaves one
// congestion loop spanning the whole path where the splice runs two, each over
// half of it.
//
// The refusal must come FIRST, so a request that named splice pays nothing at
// all: no grant minted, no authorize round trip, no proxy entry.
func TestSpliceRouteTouchesNothing(t *testing.T) {
	// A handler with nothing wired: had the splice check not come first, this
	// would reach SetupDataPlane or a runner and would not return.
	h := &TaskHandler{}
	out, err := h.openDataPlane(nil, nil,
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Push,
		protocol.TaskID{}, protocol.FileTransferRoute_Splice)
	if err != nil {
		t.Fatalf("splice must never fail: %v", err)
	}
	if out != nil {
		t.Fatalf("splice set something up: %+v", out)
	}
}

// The zero value is splice, so a caller that says nothing -- an old peer, a
// widget passing the default -- takes the path that was always there.
func TestZeroValuedRequestsSplice(t *testing.T) {
	var oft protocol.OpenFileTransferRequest
	if oft.Route != protocol.FileTransferRoute_Splice {
		t.Fatalf("a zero-valued OpenFileTransferRequest names %v", oft.Route)
	}
	var lf protocol.ListFilesRequest
	if lf.Route != protocol.FileTransferRoute_Splice {
		t.Fatalf("a zero-valued ListFilesRequest names %v", lf.Route)
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
	if back.Route != protocol.FileTransferRoute_Splice {
		t.Fatalf("the route came back as %v from an unset request", back.Route)
	}
}

// The invariant the earlier shape got wrong: "not wanted" and "not possible"
// were one false, so a missing endpoint came out as a splice nobody asked for.
// A caller that named forwarded or direct asked for the server NOT to read
// these bytes; answering with the splice would hand over exactly what was
// withheld, and would do it silently.
func TestANamedRouteThatCannotBeTakenIsRefusedNotSpliced(t *testing.T) {
	h := &TaskHandler{} // no SetupDataPlane hook
	for _, r := range []protocol.FileTransferRoute{
		protocol.FileTransferRoute_Forwarded,
		protocol.FileTransferRoute_Direct,
	} {
		out, err := h.openDataPlane(nil, nil,
			protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Push,
			protocol.TaskID{}, r)
		if err == nil {
			t.Fatalf("route %v was not refused; out=%+v", r, out)
		}
		if out != nil {
			t.Fatalf("route %v refused but still handed back %+v", r, out)
		}
		// The caller has to be able to say WHICH route failed, or it cannot
		// decide what to retry on.
		if !strings.Contains(err.Error(), r.String()) {
			t.Errorf("refusal does not name the route: %v", err)
		}
	}
}
