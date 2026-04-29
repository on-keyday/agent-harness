package agentboard

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func mkProtoRid(b byte) protocol.RunnerID {
	return protocol.RunnerID{
		Transport:    []byte("ws"),
		IpAddr:       []byte{127, 0, 0, 1},
		Port:         9000,
		UniqueNumber: uint16(b),
	}
}

func mkProtoTid(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = b
	return t
}

// Local agentboard.RunnerID with same shape — what Hello will actually carry.
func mkBoardRid(b byte) RunnerID {
	return RunnerID{
		Transport:    []byte("ws"),
		IpAddr:       []byte{127, 0, 0, 1},
		Port:         9000,
		UniqueNumber: uint16(b),
	}
}

func mkBoardTid(b byte) TaskID {
	var t TaskID
	t.Id[0] = b
	return t
}

func TestRegistry_RegisterProtoValidateBoard(t *testing.T) {
	r := newRegistry()
	var ticket [16]byte
	ticket[0] = 0xAA
	r.Register(mkProtoRid(1), mkProtoTid(1), ticket)

	// Hello arrives with agentboard.RunnerID/TaskID (same logical id, different Go type)
	if status := r.Validate(mkBoardRid(1), mkBoardTid(1), ticket); status != HelloStatusOk {
		t.Errorf("matching ticket → status=%v, want ok", status)
	}
	var bad [16]byte
	if status := r.Validate(mkBoardRid(1), mkBoardTid(1), bad); status != HelloStatusBadTicket {
		t.Errorf("wrong ticket → status=%v, want bad_ticket", status)
	}
}

func TestRegistry_UnknownTask(t *testing.T) {
	r := newRegistry()
	var ticket [16]byte
	if status := r.Validate(mkBoardRid(1), mkBoardTid(2), ticket); status != HelloStatusUnknownTask {
		t.Errorf("unregistered → status=%v, want unknown_task", status)
	}
}

func TestRegistry_Revoke(t *testing.T) {
	r := newRegistry()
	var ticket [16]byte
	ticket[0] = 0x55
	r.Register(mkProtoRid(1), mkProtoTid(3), ticket)
	r.Revoke(mkProtoRid(1), mkProtoTid(3))
	if status := r.Validate(mkBoardRid(1), mkBoardTid(3), ticket); status != HelloStatusUnknownTask {
		t.Errorf("after revoke → status=%v, want unknown_task", status)
	}
}
