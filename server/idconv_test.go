package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoardRunnerIDFromProto(t *testing.T) {
	p := protocol.RunnerID{Id: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}}

	got := boardRunnerIDFromProto(p)
	if got.Id != p.Id {
		t.Fatalf("runner id round-trip mismatch: got %x want %x", got.Id, p.Id)
	}
}

// The zero identity must survive as the zero identity. This test used to assert
// the OPPOSITE: an absent value was rewritten to a placeholder IPv4, because the
// board's schema refused ip_addr_len == 0 and the encoder asserted on it. With
// identities opaque there is nothing to substitute, and substituting anything
// would make "no sender" indistinguishable from a real runner.
func TestBoardRunnerIDFromProto_ZeroStaysZero(t *testing.T) {
	got := boardRunnerIDFromProto(protocol.RunnerID{})
	if got.Id != ([16]byte{}) {
		t.Fatalf("zero identity did not stay zero: %x", got.Id)
	}
}

func TestBoardTaskIDFromProto(t *testing.T) {
	var p protocol.TaskID
	p.Id = [16]byte{1, 2, 3}
	got := boardTaskIDFromProto(p)
	if got.Id != p.Id {
		t.Fatalf("task id mismatch: %x != %x", got.Id, p.Id)
	}
}
