package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// restoreCaller wires a principal task holding caps and returns its conn plus
// its own TaskID, so a test can put a target inside the caller's scope.
func restoreCaller(t *testing.T, h *TaskHandler, caps protocol.Capability, port string) (*fakeConn, protocol.TaskID) {
	t.Helper()
	idHex := h.Tasks.Create("repo", "p", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, caps, defaultScope(), "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:" + port + "-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	tid := hexToTaskID(t, idHex)
	h.principals[conn.ConnectionID().String()] = tid
	return conn, tid
}

func restoreRequest(t *testing.T, listOnly uint8, ids ...protocol.TaskID) []byte {
	t.Helper()
	rr := protocol.RestoreTasksRequest{ListOnly: listOnly}
	if len(ids) > 0 && !rr.SetTaskIds(ids) {
		t.Fatal("SetTaskIds refused")
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RestoreTasks, RequestId: 11}
	req.SetRestoreTasks(rr)
	return encodeTaskControlRequest(t, req)
}

// Scope answers WHICH tasks and never WHETHER, so the scope filter alone let a
// caller whose prune had been revoked put records back -- which is the control
// the revocation was for. Observed live: prune answered permission denied and
// restore, at the same moment on the same caps, answered "2 restored".
func TestRestoreDeniedWithoutPruneCap(t *testing.T) {
	h := newTestHandler(t)
	called := false
	h.RestoreFn = func([]string) ([]string, []string, []string) {
		called = true
		return nil, nil, nil
	}
	// Everything EXCEPT prune, so the denial cannot be read as "holds nothing".
	conn, _ := restoreCaller(t, h, protocol.Capability_All&^protocol.Capability_Prune, "9701")

	h.Handle(conn, restoreRequest(t, 0, protocol.TaskID{Id: [16]uint8{1}}))

	if called {
		t.Fatal("RestoreFn ran without the prune capability")
	}
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("resp.Kind = %v, want PermissionDenied", resp.Kind)
	}
	pd := resp.PermissionDenied()
	if pd == nil {
		t.Fatal("PermissionDenied() returned nil")
	}
	if pd.RequiredCap != protocol.Capability_Prune {
		t.Fatalf("RequiredCap = %v, want Prune", pd.RequiredCap)
	}
	if pd.RequestedKind != protocol.TaskControlKind_RestoreTasks {
		t.Fatalf("RequestedKind = %v, want RestoreTasks", pd.RequestedKind)
	}
	if resp.RequestId != 11 {
		t.Fatalf("RequestId = %d, want 11", resp.RequestId)
	}
}

// The list half must stay open: the ids of forgotten tasks live only in the
// WAL, so gating it too would leave the verb usable only by someone who had
// written the id down before the accident. Both halves arrive as the same kind,
// which is why the gate is inline rather than in requiredCap -- putting it
// there would take this away.
func TestRestoreListStaysOpenWithoutPruneCap(t *testing.T) {
	h := newTestHandler(t)
	h.RestorableFn = func() ([]Restorable, protocol.RestoreWALStatus) {
		return nil, protocol.RestoreWALStatus_Ok
	}
	conn, _ := restoreCaller(t, h, protocol.Capability_None, "9702")

	h.Handle(conn, restoreRequest(t, 1))

	resp := lastTaskControlResponse(t, conn)
	if resp.Kind == protocol.TaskControlKind_PermissionDenied {
		t.Fatal("listing was denied; it is the only way to learn the ids")
	}
	if resp.Kind != protocol.TaskControlKind_RestoreTasks {
		t.Fatalf("resp.Kind = %v, want RestoreTasks", resp.Kind)
	}
}

func TestRestoreProceedsWithPruneCap(t *testing.T) {
	h := newTestHandler(t)
	var got []string
	h.RestoreFn = func(ids []string) ([]string, []string, []string) {
		got = ids
		return ids, nil, nil
	}
	conn, caller := restoreCaller(t, h, protocol.Capability_Prune, "9703")
	// In the caller's own subtree: the scope filter drops a stranger's id
	// before RestoreFn sees it, which would hide whether the CAP gate passed.
	targetHex := h.Tasks.Create("repo", "child", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, caller, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_None, defaultScope(), "")

	h.Handle(conn, restoreRequest(t, 0, hexToTaskID(t, targetHex)))

	if len(got) != 1 {
		t.Fatalf("RestoreFn saw %v, want the one id", got)
	}
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_RestoreTasks {
		t.Fatalf("resp.Kind = %v, want RestoreTasks", resp.Kind)
	}
	if r := resp.RestoreTasks(); r == nil || r.Restored != 1 {
		t.Fatalf("restored count wrong: %+v", r)
	}
}
