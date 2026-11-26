package server

import (
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// listOne sends a List naming one task (ListQuery.task_id) as conn and
// decodes the body.
func listOne(t *testing.T, h *TaskHandler, conn *fakeConn, q protocol.ListQuery) protocol.ListResultBody {
	t.Helper()
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_List}}
	req.SetList(q)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	if len(conn.sendStreams) != 1 {
		t.Fatalf("expected 1 send stream, got %d", len(conn.sendStreams))
	}
	body := decodeListBody(t, conn.sendStreams[0].bytes)
	conn.sendStreams = nil
	return body
}

// A List naming one task carries that task's row alone, by the visibility rule
// of a full list: the caller's own row whole, even ended and with
// finished_limit 0; its parent redacted; a task it may not see as no row, not
// an error. It carries no runners, even to an operator.
func TestListNamingOneTask(t *testing.T) {
	h := newTestHandler(t)
	runner := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-93")}
	h.Registry.Add(&RunnerEntry{ID: runner.id, Hostname: "host-a", MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, Conn: runner})

	bHex := h.Tasks.Create("/repo/b", "parent prompt", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}},
		"claude")
	cHex := h.Tasks.Create("/repo/c", "child prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		hexToTaskID(t, bHex), "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_Spawn, Scope: authority.Scope{}},
		"claude")
	sHex := h.Tasks.Create("/repo/s", "stranger prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}},
		"")
	h.Tasks.mu.Lock()
	h.Tasks.tasks[cHex].Status = protocol.TaskStatus_Succeeded
	h.Tasks.mu.Unlock()

	if h.principals == nil {
		h.principals = make(map[objproto.ConnectionID]protocol.TaskID)
	}
	caller := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9840-1"), autoSendStreams: true}
	h.principals[caller.ConnectionID()] = hexToTaskID(t, cHex)

	ids := func(body protocol.ListResultBody) []string {
		var out []string
		for _, ti := range body.Tasks {
			out = append(out, hex.EncodeToString(ti.Id.Id[:]))
		}
		return out
	}

	self := listOne(t, h, caller, protocol.ListQuery{FinishedLimit: 0, TaskId: hexToTaskID(t, cHex)})
	if got := ids(self); len(got) != 1 || got[0] != cHex {
		t.Fatalf("own row: got %v, want [%s]", got, cHex)
	}
	if string(self.Tasks[0].Prompt) != "child prompt" {
		t.Errorf("own row must not be redacted; prompt=%q", self.Tasks[0].Prompt)
	}
	if self.FinishedOmitted != 0 {
		t.Errorf("one-task list: finished_omitted = %d, want 0", self.FinishedOmitted)
	}

	parent := listOne(t, h, caller, protocol.ListQuery{TaskId: hexToTaskID(t, bHex)})
	if got := ids(parent); len(got) != 1 || got[0] != bHex {
		t.Fatalf("parent row: got %v, want [%s]", got, bHex)
	}
	if len(parent.Tasks[0].Prompt) != 0 {
		t.Errorf("parent row must come back redacted, as in a full list; prompt=%q", parent.Tasks[0].Prompt)
	}

	stranger := listOne(t, h, caller, protocol.ListQuery{TaskId: hexToTaskID(t, sHex)})
	if got := ids(stranger); len(got) != 0 {
		t.Fatalf("a task the caller may not see: got %v, want no row", got)
	}

	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9840-2"), autoSendStreams: true}
	opBody := listOne(t, h, op, protocol.ListQuery{TaskId: hexToTaskID(t, sHex)})
	if got := ids(opBody); len(got) != 1 || got[0] != sHex {
		t.Fatalf("operator naming S: got %v, want [%s]", got, sHex)
	}
	if opBody.RunnersLen != 0 {
		t.Errorf("one-task list carries no runners; got %d", opBody.RunnersLen)
	}
}
