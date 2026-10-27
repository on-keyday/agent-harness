package server

import (
	"os"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func lifecycleSetAs(t *testing.T, h *TaskHandler, conn *fakeConn, st protocol.LifecycleState, reportedAt uint64) *protocol.LifecycleSetResponse {
	t.Helper()
	before := len(conn.Sent())
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_LifecycleSet, RequestId: 6}
	req.SetLifecycleSet(protocol.LifecycleSetRequest{Lifecycle: st, ReportedAt: reportedAt})
	h.Handle(conn, encodeTaskControlRequest(t, req))
	waitFor(t, func() bool { return len(conn.Sent()) == before+1 })
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_LifecycleSet || resp.LifecycleSet() == nil {
		t.Fatalf("response kind = %v, want lifecycle_set", resp.Kind)
	}
	return resp.LifecycleSet()
}

func TestLifecycleSet_WritesCallersOwnTask(t *testing.T) {
	f := newProgressFixture(t)
	resp := lifecycleSetAs(t, f.h, f.conn, protocol.LifecycleState_Waiting, 100)
	if resp.Status != protocol.LifecycleSetStatus_Ok || resp.LifecycleAt == 0 {
		t.Fatalf("resp = %+v, want ok with a stamp", resp)
	}
	if e, _ := f.h.Tasks.Get(f.tHex); e.Lifecycle != protocol.LifecycleState_Waiting {
		t.Fatalf("stored lifecycle = %v, want waiting", e.Lifecycle)
	}
	row, ok := listRowAs(t, f.h, f.conn, f.tHex)
	if !ok || row.Lifecycle != protocol.LifecycleState_Waiting || row.LifecycleAt != resp.LifecycleAt || row.LifecycleAgeMs >= 5000 {
		t.Fatalf("list row lifecycle=%v at=%d age=%d", row.Lifecycle, row.LifecycleAt, row.LifecycleAgeMs)
	}
}

func TestLifecycleSet_OperatorRefused(t *testing.T) {
	h := newTestHandler(t)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9731-1"), nextSendStreamID: 10}
	if resp := lifecycleSetAs(t, h, conn, protocol.LifecycleState_Working, 1); resp.Status != protocol.LifecycleSetStatus_NoPrincipal {
		t.Fatalf("status = %v, want no_principal", resp.Status)
	}
}

func TestLifecycleSet_UnsetRefused(t *testing.T) {
	f := newProgressFixture(t)
	for _, st := range []protocol.LifecycleState{protocol.LifecycleState_Unset, protocol.LifecycleState(9)} {
		if resp := lifecycleSetAs(t, f.h, f.conn, st, 1); resp.Status != protocol.LifecycleSetStatus_BadRequest {
			t.Fatalf("state %v: status = %v, want bad_request", st, resp.Status)
		}
	}
	if e, _ := f.h.Tasks.Get(f.tHex); e.LifecycleAt != 0 {
		t.Fatalf("a refused write stamped the task (%d)", e.LifecycleAt)
	}
}

// Async hooks can land out of order; the reporter's stamp decides (spec D10).
func TestLifecycleSet_OlderReportIsStale(t *testing.T) {
	f := newProgressFixture(t)
	var calls int
	f.h.OnTaskLifecycle = func(string) { calls++ }
	if r := lifecycleSetAs(t, f.h, f.conn, protocol.LifecycleState_Idle, 200); r.Status != protocol.LifecycleSetStatus_Ok {
		t.Fatalf("first: %v", r.Status)
	}
	if r := lifecycleSetAs(t, f.h, f.conn, protocol.LifecycleState_Working, 150); r.Status != protocol.LifecycleSetStatus_Stale {
		t.Fatalf("older report: %v, want stale", r.Status)
	}
	if e, _ := f.h.Tasks.Get(f.tHex); e.Lifecycle != protocol.LifecycleState_Idle {
		t.Fatalf("lifecycle = %v after a stale report, want idle", e.Lifecycle)
	}
	if calls != 1 {
		t.Fatalf("OnTaskLifecycle called %d times, want 1 (stale publishes nothing)", calls)
	}
}

// A resume may bind to another runner whose clock is behind (spec D23).
func TestLifecycleSet_ResumeResetsReporterStamp(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}, "")
	if _, st := s.SetLifecycle(id, protocol.LifecycleState_Idle, 200, 1000); st != protocol.LifecycleSetStatus_Ok {
		t.Fatalf("first: %v", st)
	}
	s.Finish(id, 0, nil)
	if _, err := s.Resume(id, "p2", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
		GrantUpdate{To: authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}},
		protocol.TaskKind_Interactive, "", protocol.TaskID{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, st := s.SetLifecycle(id, protocol.LifecycleState_Working, 50, 2000); st != protocol.LifecycleSetStatus_Ok {
		t.Fatalf("after resume: %v, want ok", st)
	}
}

func TestLifecycleSet_SurvivesParentHopRedaction(t *testing.T) {
	h := newTestHandler(t)
	pHex := h.Tasks.Create("/repo", "parent", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}}, "")
	cHex := h.Tasks.Create("/repo", "child", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		hexToTaskID(t, pHex), "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}, "")
	h.Tasks.SetProgress(pHex, "private", 1)
	h.Tasks.SetLifecycle(pHex, protocol.LifecycleState_Waiting, 1, 5)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9732-1"), nextSendStreamID: 10}
	h.principals = map[string]protocol.TaskID{conn.ConnectionID().String(): hexToTaskID(t, cHex)}
	row, ok := listRowAs(t, h, conn, pHex)
	if !ok {
		t.Fatal("child must see its parent's (redacted) row")
	}
	if len(row.Progress) != 0 || row.Lifecycle != protocol.LifecycleState_Waiting || row.LifecycleAt != 5 {
		t.Fatalf("parent-hop row progress=%q lifecycle=%v at=%d; want progress emptied, lifecycle kept", row.Progress, row.Lifecycle, row.LifecycleAt)
	}
}

func TestLifecycleSnapshot_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := NewTaskStore()
	id := old.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}, "")
	old.SetLifecycle(id, protocol.LifecycleState_Waiting, 77, 12345)
	if err := writeProgressSnapshot(dir, old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(progressSnapshotPath(dir)); err != nil {
		t.Fatalf("no snapshot written for a task with lifecycle and no progress: %v", err)
	}
	next := NewTaskStore()
	next.tasks[id] = &TaskEntry{ID: id}
	if n, err := loadProgressSnapshot(dir, next); err != nil || n != 1 {
		t.Fatalf("loadProgressSnapshot = %d, %v; want 1", n, err)
	}
	e, _ := next.Get(id)
	if e.Lifecycle != protocol.LifecycleState_Waiting || e.LifecycleAt != 12345 || e.LifecycleReportedAt != 77 {
		t.Fatalf("restored = %v@%d (reported %d), want waiting@12345 (77)", e.Lifecycle, e.LifecycleAt, e.LifecycleReportedAt)
	}
}

func TestTaskEvent_CarriesLifecycle(t *testing.T) {
	s := makeEventServer(t)
	aHex := s.tasks.Create("/r", "a", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_Spawn, Scope: authority.Scope{}}, "")
	op := subscribeTasksStatus(t, s, "ws:127.0.0.1:9933-1")
	s.tasks.SetLifecycle(aHex, protocol.LifecycleState_Idle, 3, 9)
	s.tasks.OnHold(aHex)
	evs := op.take()
	if len(evs) < 1 || evs[len(evs)-1].Lifecycle != protocol.LifecycleState_Idle || evs[len(evs)-1].LifecycleAt != 9 {
		t.Fatalf("events = %+v, want the last carrying lifecycle idle@9", evs)
	}
}
