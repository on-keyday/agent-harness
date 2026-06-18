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

func TestBoardTaskIDFromProto(t *testing.T) {
	var p protocol.TaskID
	p.Id = [16]byte{1, 2, 3}
	got := boardTaskIDFromProto(p)
	if got.Id != p.Id {
		t.Fatalf("task id mismatch: %x != %x", got.Id, p.Id)
	}
}
