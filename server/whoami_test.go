package server

import (
	"encoding/hex"
	"testing"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// whoamiResp drives a whoami request through the handler and returns the
// decoded WhoAmIResponse. Fails if the response is not a Whoami variant.
func whoamiResp(t *testing.T, h *TaskHandler, conn *fakeConn, reqID uint32) protocol.WhoAmIResponse {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Whoami, RequestId: reqID}
	req.SetWhoami(protocol.WhoAmIRequest{Reserved: 0})
	h.Handle(conn, encodeTaskControlRequest(t, req))

	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_Whoami {
		t.Fatalf("resp.Kind = %v, want Whoami", resp.Kind)
	}
	if resp.RequestId != reqID {
		t.Fatalf("RequestId = %d, want %d", resp.RequestId, reqID)
	}
	w := resp.Whoami()
	if w == nil {
		t.Fatal("Whoami() returned nil")
	}
	return *w
}

// An operator connection (no recorded principal) must report a zero principal
// task id and the full capability set — matching callerCaps's operator root.
func TestWhoamiOperator(t *testing.T) {
	h := newTestHandler(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9650-1")}

	w := whoamiResp(t, h, opConn, 7)

	if w.PrincipalTaskId.Id != ([16]byte{}) {
		t.Errorf("operator PrincipalTaskId = %x, want all-zero", w.PrincipalTaskId.Id)
	}
	if w.CreatorTaskId.Id != ([16]byte{}) {
		t.Errorf("operator CreatorTaskId = %x, want all-zero", w.CreatorTaskId.Id)
	}
	if w.Capabilities != protocol.Capability_All {
		t.Errorf("operator Capabilities = %v, want all", w.Capabilities)
	}
}

// A confined agent must report its own task id, its creator, and exactly the
// capability set bound to its task (never escalated to operator/all).
func TestWhoamiConfinedAgent(t *testing.T) {
	h := newTestHandler(t)

	// A creator task (the spawner) so we can assert creator propagation.
	creatorHex := h.Tasks.Create("repo", "creator", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	creatorTID := hexToTaskID(t, creatorHex)

	wantCaps := protocol.Capability_Spawn | protocol.Capability_FileRead
	childHex := h.Tasks.Create("repo", "child", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, creatorTID, "",
		protocol.RunnerSelector{}, nil, wantCaps, "")
	childTID := hexToTaskID(t, childHex)

	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9651-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = childTID

	w := whoamiResp(t, h, conn, 9)

	if hex.EncodeToString(w.PrincipalTaskId.Id[:]) != childHex {
		t.Errorf("PrincipalTaskId = %x, want %s", w.PrincipalTaskId.Id, childHex)
	}
	if hex.EncodeToString(w.CreatorTaskId.Id[:]) != creatorHex {
		t.Errorf("CreatorTaskId = %x, want %s", w.CreatorTaskId.Id, creatorHex)
	}
	if w.Capabilities != wantCaps {
		t.Errorf("Capabilities = %v, want %v", w.Capabilities, wantCaps)
	}
}

// whoami carries no capability gate: an agent holding Capability_None must
// still get an answer (its own none-caps), not a PermissionDenied.
func TestWhoamiNoCapRequired(t *testing.T) {
	h, conn := makeAgentConn(t, protocol.Capability_None)

	w := whoamiResp(t, h, conn, 11)

	if w.Capabilities != protocol.Capability_None {
		t.Errorf("Capabilities = %v, want none", w.Capabilities)
	}
	if w.PrincipalTaskId.Id == ([16]byte{}) {
		t.Error("PrincipalTaskId is all-zero; a confined agent must report its task id")
	}
}
