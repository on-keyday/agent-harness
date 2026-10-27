package agentboard

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func mkProtoRid(b byte) protocol.RunnerID {
	return protocol.RunnerID{Id: [16]byte{b}}
}

func mkProtoTid(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = b
	return t
}

// Local protocol.RunnerID over the same 16 bytes — what Hello actually carries.
func mkBoardRid(b byte) protocol.RunnerID {
	return protocol.RunnerID{Id: [16]byte{b}}
}

func mkBoardTid(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = b
	return t
}

func TestRegistry_RegisterProtoValidateBoard(t *testing.T) {
	r := newRegistry()
	var ticket [16]byte
	ticket[0] = 0xAA
	r.Register(mkProtoRid(1), mkProtoTid(1), ticket)

	// Hello arrives with protocol.RunnerID/protocol.TaskID (same logical id, different Go type)
	if got, ok := r.Ticket(mkBoardRid(1), mkBoardTid(1)); !ok || got != ticket {
		t.Errorf("Ticket = %x, %v; want the registered one", got, ok)
	}
}

func TestRegistry_UnknownTask(t *testing.T) {
	r := newRegistry()
	if _, ok := r.Ticket(mkBoardRid(1), mkBoardTid(2)); ok {
		t.Error("unregistered task has a ticket")
	}
}

func TestRegistry_Revoke(t *testing.T) {
	r := newRegistry()
	var ticket [16]byte
	ticket[0] = 0x55
	r.Register(mkProtoRid(1), mkProtoTid(3), ticket)
	r.Revoke(mkProtoRid(1), mkProtoTid(3))
	if _, ok := r.Ticket(mkBoardRid(1), mkBoardTid(3)); ok {
		t.Error("ticket survived revoke")
	}
}
