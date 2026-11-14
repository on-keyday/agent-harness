package server

import (
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func mustParseCID(t *testing.T, s string) objproto.ConnectionID {
	t.Helper()
	return objproto.MustParseConnectionID(s)
}

// TestRestartCancelsDetached verifies that the restart loop marks Detached
// tasks as Cancelled (simulating what Run does after WAL replay).
func TestRestartCancelsDetached(t *testing.T) {
	s := New(Config{})

	taskID := s.tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}}, "")
	s.tasks.Assign(taskID, testRunnerID("runner-1"), "/wt", false)
	if err := s.tasks.SetDetached(taskID); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}

	// Simulate the restart loop from Run.
	for _, task := range s.tasks.List(0) {
		if task.Status == protocol.TaskStatus_Detached {
			s.tasks.Cancel(task.ID, protocol.EndReason_Cancelled, protocol.TaskID{})
		}
	}

	got, ok := s.tasks.Get(taskID)
	if !ok {
		t.Fatal("task disappeared")
	}
	if got.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("want Cancelled, got %v", got.Status)
	}
}

// boardRunnerID builds an protocol.RunnerID from a connection ID string so
// tests can call board.Registry().Ticket without going through the protocol
// wire. It derives the identity the same way makeProtoRunnerID does, so a test
// that registers by connection-id string and validates by identity agrees with
// itself — the server itself derives nothing from an address any more.
func boardRunnerID(t *testing.T, connIDStr string) protocol.RunnerID {
	t.Helper()
	return makeProtoRunnerID(t, connIDStr)
}

// boardTaskID converts a hex task ID string to protocol.TaskID.
func boardTaskID(taskIDHex string) protocol.TaskID {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)
	return tid
}
