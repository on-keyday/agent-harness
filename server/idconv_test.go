package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoardRunnerIDFromProto(t *testing.T) {
	var p protocol.RunnerID
	p.SetTransport([]byte("ws"))
	p.SetIpAddr([]byte{127, 0, 0, 1})
	p.Port = 8539
	p.UniqueNumber = 42

	got := boardRunnerIDFromProto(p)
	if string(got.Transport) != "ws" || len(got.IpAddr) != 4 || got.Port != 8539 || got.UniqueNumber != 42 {
		t.Fatalf("runner id round-trip mismatch: %+v", got)
	}
}

func TestBoardRunnerIDFromProto_ZeroLenIpAddrGuarded(t *testing.T) {
	// Empty IpAddr must be substituted with the IPv4 placeholder {0,0,0,0} to
	// satisfy the protocol encoder's hard IpAddrLen ∈ {4,16} assertion.
	var p protocol.RunnerID
	p.SetTransport([]byte("ws"))
	// deliberately leave IpAddr empty (zero-length)
	p.Port = 1234

	got := boardRunnerIDFromProto(p)
	if len(got.IpAddr) != 4 {
		t.Fatalf("expected guarded IpAddr length 4, got %d (%v)", len(got.IpAddr), got.IpAddr)
	}
	for _, b := range got.IpAddr {
		if b != 0 {
			t.Fatalf("expected all-zero placeholder IP, got %v", got.IpAddr)
		}
	}
}

func TestBoardRunnerIDFromProto_GarbageLenIpAddrGuarded(t *testing.T) {
	// A 7-byte IpAddr (neither 4 nor 16) must also be replaced by the placeholder.
	var p protocol.RunnerID
	p.SetIpAddr([]byte{1, 2, 3, 4, 5, 6, 7})

	got := boardRunnerIDFromProto(p)
	if len(got.IpAddr) != 4 {
		t.Fatalf("expected guarded IpAddr length 4, got %d (%v)", len(got.IpAddr), got.IpAddr)
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
