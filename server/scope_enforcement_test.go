package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Every TaskControl kind that names a target task must refuse one outside the
// caller's scope, and must refuse it as "no such task" rather than
// permission_denied — a missing-capability answer about a task the caller
// cannot see is an existence oracle.
//
// The caller here holds every capability except info_global and has the
// default subtree scope, so a failure can only be the target gate. If any of
// these regress, a --caps cancel worker can kill the operator's sessions
// again.
func TestOutOfScopeTargetsAnswerNotFound(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9700-1")}
	cid := conn.ConnectionID().String()
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[cid] = hexToTaskID(t, c)
	utid := hexToTaskID(t, u)

	send := func(t *testing.T, req *protocol.TaskControlRequest) protocol.TaskControlResponse {
		t.Helper()
		h.Handle(conn, encodeTaskControlRequest(t, req))
		resp := lastTaskControlResponse(t, conn)
		if resp.Kind == protocol.TaskControlKind_PermissionDenied {
			t.Fatalf("answered permission_denied for an out-of-scope target; "+
				"want the kind's own not-found (req kind %v)", req.Kind)
		}
		return resp
	}

	t.Run("cancel", func(t *testing.T) {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Cancel, RequestId: 1}
		req.SetCancel(protocol.CancelTask{TaskId: utid})
		resp := send(t, req)
		if got := resp.Cancel(); got == nil || got.Status != protocol.CancelResult_NoSuchTask {
			t.Fatalf("status = %v, want no_such_task", got)
		}
		if ut, ok := h.Tasks.Get(u); ok && ut.Status == protocol.TaskStatus_Cancelled {
			t.Fatal("the out-of-scope task was cancelled anyway")
		}
	})

	t.Run("attach_session", func(t *testing.T) {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AttachSession, RequestId: 2}
		req.SetAttach(protocol.AttachSessionRequest{TaskId: utid})
		resp := send(t, req)
		if got := resp.Attach(); got == nil || got.Status != protocol.AttachSessionStatus_NotFound {
			t.Fatalf("status = %v, want not_found", got)
		}
	})

	t.Run("await_idle", func(t *testing.T) {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 3}
		req.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: utid})
		resp := send(t, req)
		if got := resp.AwaitIdle(); got == nil || got.Status != protocol.AwaitIdleStatus_NotFound {
			t.Fatalf("status = %v, want not_found", got)
		}
	})

	t.Run("open_file_transfer", func(t *testing.T) {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenFileTransfer, RequestId: 4}
		req.SetOpenFileTransfer(protocol.OpenFileTransferRequest{
			TaskId: utid, Direction: protocol.FileTransferDirection_Pull,
		})
		resp := send(t, req)
		if got := resp.OpenFileTransfer(); got == nil || got.Status != protocol.OpenFileTransferStatus_NoSuchTask {
			t.Fatalf("status = %v, want no_such_task", got)
		}
	})

	t.Run("list_files", func(t *testing.T) {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ListFiles, RequestId: 5}
		req.SetListFiles(protocol.ListFilesRequest{TaskId: utid})
		resp := send(t, req)
		if got := resp.ListFiles(); got == nil || got.Status != protocol.ListFilesStatus_NoSuchTask {
			t.Fatalf("status = %v, want no_such_task", got)
		}
	})

	t.Run("git_query", func(t *testing.T) {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_GitQuery, RequestId: 6}
		req.SetGitQuery(protocol.GitQueryRequest{TaskId: utid})
		resp := send(t, req)
		if got := resp.GitQuery(); got == nil || got.Status != protocol.GitQueryStatus_NoSuchTask {
			t.Fatalf("status = %v, want no_such_task", got)
		}
	})

	t.Run("open_port_forward", func(t *testing.T) {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenPortForward, RequestId: 7}
		req.SetOpenPortForward(protocol.OpenPortForwardRequest{TaskId: utid})
		resp := send(t, req)
		if got := resp.OpenPortForward(); got == nil || got.Status != protocol.OpenPortForwardStatus_NoSuchTask {
			t.Fatalf("status = %v, want no_such_task", got)
		}
	})

	t.Run("register_port_forward", func(t *testing.T) {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RegisterPortForward, RequestId: 8}
		req.SetRegisterPortForward(protocol.RegisterPortForwardRequest{
			TaskId: utid, Direction: protocol.PortForwardDirection_Local,
		})
		resp := send(t, req)
		if got := resp.RegisterPortForward(); got == nil || got.Status != protocol.OpenPortForwardStatus_NoSuchTask {
			t.Fatalf("status = %v, want no_such_task", got)
		}
	})
}

// The mirror of the above: a target INSIDE the scope must get past the gate.
// Without this, a gate that denied everything would pass the test above.
func TestInScopeTargetPassesTheGate(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9701-1")}
	cid := conn.ConnectionID().String()
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[cid] = hexToTaskID(t, c)

	// g is c's grandchild — inside the default subtree scope.
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Cancel, RequestId: 9}
	req.SetCancel(protocol.CancelTask{TaskId: hexToTaskID(t, g)})
	h.Handle(conn, encodeTaskControlRequest(t, req))

	resp := lastTaskControlResponse(t, conn)
	if got := resp.Cancel(); got == nil || got.Status != protocol.CancelResult_Ok {
		t.Fatalf("status = %v, want ok for a descendant", got)
	}
	if gt, ok := h.Tasks.Get(g); !ok || gt.Status != protocol.TaskStatus_Cancelled {
		t.Fatal("the in-scope descendant was not cancelled")
	}
}
