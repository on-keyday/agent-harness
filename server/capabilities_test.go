package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestHasCap(t *testing.T) {
	have := protocol.Capability_Spawn | protocol.Capability_FileRead
	if !hasCap(have, protocol.Capability_Spawn) {
		t.Error("spawn should be present")
	}
	if hasCap(have, protocol.Capability_FileWrite) {
		t.Error("file_write should be absent")
	}
}

func TestIntersectCaps(t *testing.T) {
	parent := protocol.Capability_Spawn | protocol.Capability_FileRead
	req := protocol.Capability_All // inherit-all
	if got := intersectCaps(parent, req); got != parent {
		t.Fatalf("intersect with all = %#x, want parent %#x", got, parent)
	}
	// request more than parent holds → cannot widen.
	if got := intersectCaps(parent, protocol.Capability_FileWrite); got != protocol.Capability_None {
		t.Fatalf("intersect beyond parent = %#x, want none", got)
	}
}
