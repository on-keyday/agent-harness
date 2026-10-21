package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// The request carries no identity at all, so the only way a test can show the
// server is not trusting the client is to give the server facts the old env
// path could not have known: the registry's hostname for the assigned runner
// and the task's recorded repo.
func TestHandleNotify_IdentityFromPrincipalNotClient(t *testing.T) {
	h := newTestHandler(t)
	rid := protocol.RunnerID{Id: [16]byte{0x5A}}
	h.Registry.Add(&RunnerEntry{
		ID:       objproto.MustParseConnectionID("ws:127.0.0.1:9701-1"),
		Identity: rid,
		Hostname: "runner-host",
	})
	tidHex := h.Tasks.Create("/repo/a", "p", protocol.TaskKind_Interactive,
		protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_Notify, authority.Scope{}, "")
	h.Tasks.Assign(tidHex, rid, "/wt", true)
	tid := hexToTaskID(t, tidHex)

	b := agentboardForTest(t)
	st := b.Attach(rid, tid, "forged-host", "claude")
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return st }

	var got []protocol.NotifyEvent
	h.OnNotify = func(ev protocol.NotifyEvent) { got = append(got, ev) }

	agent := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9702-1")}
	h.principals = map[string]protocol.TaskID{agent.ConnectionID().String(): tid}
	sendNotify(t, h, agent)

	if len(got) != 1 {
		t.Fatalf("OnNotify calls = %d, want 1", len(got))
	}
	ev := got[0]
	if ev.Origin != protocol.NotifyOrigin_Worker {
		t.Fatalf("origin = %v, want worker", ev.Origin)
	}
	w := ev.Worker()
	if w == nil {
		t.Fatal("worker = nil, want the principal's identity")
	}
	if string(w.TaskId) != tidHex {
		t.Errorf("task_id = %q, want %q", w.TaskId, tidHex)
	}
	if string(w.RunnerId) != rid.Hex() {
		t.Errorf("runner_id = %q, want %q", w.RunnerId, rid.Hex())
	}
	if string(w.Hostname) != "runner-host" {
		t.Errorf("hostname = %q, want the registry's, not the agent hello's", w.Hostname)
	}
	if string(w.Repo) != "/repo/a" {
		t.Errorf("repo = %q, want the task's recorded repo", w.Repo)
	}

	// An operator connection has no principal: external, no worker block.
	got = nil
	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9703-1")}
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return nil }
	sendNotify(t, h, op)
	if len(got) != 1 || got[0].Origin != protocol.NotifyOrigin_External || got[0].Worker() != nil {
		t.Fatalf("operator notify = %+v, want one external event with no worker", got)
	}
}

func sendNotify(t *testing.T, h *TaskHandler, conn *fakeConn) {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Notify, RequestId: 1}
	nr := protocol.NotifyRequest{Level: protocol.NotifyLevel_Info}
	nr.SetTitle([]byte("t"))
	nr.SetText([]byte("x"))
	req.SetNotify(nr)
	h.Handle(conn, encodeTaskControlRequest(t, req))
}
