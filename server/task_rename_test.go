package server

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func sendRename(t *testing.T, h *TaskHandler, conn *fakeConn, target, name string) protocol.TaskRenameStatus {
	t.Helper()
	tc := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_TaskRename, RequestId: 1}}
	req := protocol.TaskRenameRequest{TaskId: hexToTaskID(t, target)}
	req.SetName([]byte(name))
	tc.SetTaskRename(req)
	h.Handle(conn, encodeTaskControlRequest(t, tc))
	resp := lastTaskControlResponse(t, conn)
	got := resp.TaskRename()
	if got == nil {
		t.Fatalf("no task_rename response (kind %v)", resp.Kind)
	}
	return got.Status
}

func nameOf(t *testing.T, h *TaskHandler, id string) string {
	t.Helper()
	e, _ := h.Tasks.Get(id)
	return e.Name
}

func TestRenameByOperatorAnyTask(t *testing.T) {
	h, _, _, _, u := scopeFixture(t)
	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9910-1")}
	if st := sendRename(t, h, op, u, "nightly"); st != protocol.TaskRenameStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if got := nameOf(t, h, u); got != "nightly" {
		t.Fatalf("name = %q", got)
	}
}

// R3: a task renames a strict descendant inside its resume scope.
func TestRenameByTaskOnDescendant(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	conn := taskCaller(t, h, c, protocol.Capability_None)
	if st := sendRename(t, h, conn, g, "worker-1"); st != protocol.TaskRenameStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if got := nameOf(t, h, g); got != "worker-1" {
		t.Fatalf("name = %q", got)
	}
}

// R3: never itself (a self-rename would let a task pose as another on the
// board), never outside its subtree — both not_found, name untouched.
func TestRenameByTaskRefusesSelfAndNonDescendant(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	setScope(t, h, c, authority.Scope{Base: protocol.ScopeBase_Global})
	conn := taskCaller(t, h, c, protocol.Capability_None)
	for name, target := range map[string]string{"self": c, "unrelated": u} {
		if st := sendRename(t, h, conn, target, "ci"); st != protocol.TaskRenameStatus_NotFound {
			t.Fatalf("%s: status = %v, want not_found", name, st)
		}
		if got := nameOf(t, h, target); got != "" {
			t.Fatalf("%s: name changed to %q", name, got)
		}
	}
}

// Review Focus 4: the issue path's name rules; an empty name clears.
func TestRenameNameRules(t *testing.T) {
	h, _, _, _, u := scopeFixture(t)
	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9911-1")}
	for _, bad := range []string{strings.Repeat("a", 65), "a\nb", "\x1b[31m"} {
		if st := sendRename(t, h, op, u, bad); st != protocol.TaskRenameStatus_BadName {
			t.Fatalf("%q: status = %v, want bad_name", bad, st)
		}
	}
	if st := sendRename(t, h, op, u, strings.Repeat("a", 64)); st != protocol.TaskRenameStatus_Ok {
		t.Fatalf("64 bytes: status = %v", st)
	}
	if st := sendRename(t, h, op, u, ""); st != protocol.TaskRenameStatus_Ok || nameOf(t, h, u) != "" {
		t.Fatalf("clear: status = %v name %q", st, nameOf(t, h, u))
	}
}

// Review Focus 5: a rename survives a restart.
func TestRenameSurvivesReplay(t *testing.T) {
	s, path := storeWithWAL(t)
	id := s.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	if !s.SetName(id, "renamed") {
		t.Fatal("SetName: no such task")
	}
	if got, _ := replayInto(t, path).Get(id); got.Name != "renamed" {
		t.Fatalf("replayed name = %q", got.Name)
	}
}
