package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func sessionMode(t *testing.T, h *TaskHandler, conn *fakeConn, id string, background bool, cond *protocol.BackgroundConditions) protocol.SessionModeResponse {
	t.Helper()
	sm := protocol.SessionModeRequest{TaskId: hexToTaskID(t, id)}
	sm.SetBackground(background)
	if cond != nil {
		sm.SetConditionsPresent(true)
		sm.SetConditions(*cond)
	}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_SessionMode, RequestId: 8}}
	req.SetSessionMode(sm)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	r := resp.SessionMode()
	if r == nil {
		t.Fatalf("no session_mode response: %+v", resp)
	}
	return *r
}

func modeOpConn() *fakeConn {
	return &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9160-1")}
}

func TestSwitchToForegroundDisarmsWatcherAndDeadline(t *testing.T) {
	f := bgFixture(t, RunConditions{Deadline: time.Hour, TurnEnd: protocol.TurnEndSignal_Lifecycle})
	f.h.armRunWatcher(f.idHex, f.mux)
	if r := sessionMode(t, f.h, modeOpConn(), f.idHex, false, nil); r.Status != protocol.SessionModeStatus_Ok {
		t.Fatalf("status %v %q", r.Status, r.ErrorMsg)
	}
	f.lifecycle(protocol.LifecycleState_Working)
	time.Sleep(2 * time.Millisecond)
	f.lifecycle(protocol.LifecycleState_Idle)
	time.Sleep(1500 * time.Millisecond)
	e, _ := f.h.Tasks.Get(f.idHex)
	if e.Status == protocol.TaskStatus_Cancelled || e.DeadlineAt != 0 || e.RunMode != protocol.RunMode_Foreground {
		t.Fatalf("status %v reason %v deadline %d mode %v", e.Status, e.EndReason, e.DeadlineAt, e.RunMode)
	}
}

func TestSwitchToBackgroundNeedsDeadline(t *testing.T) {
	f := newSignalFixture(t)
	f.h.Tasks.Assign(f.idHex, testRunnerID("r1"), "", false)
	if r := sessionMode(t, f.h, modeOpConn(), f.idHex, true, nil); r.Status != protocol.SessionModeStatus_DeadlineRequired {
		t.Fatalf("no conditions: %v", r.Status)
	}
	if r := sessionMode(t, f.h, modeOpConn(), f.idHex, true, &protocol.BackgroundConditions{}); r.Status != protocol.SessionModeStatus_DeadlineRequired {
		t.Fatalf("zero deadline: %v", r.Status)
	}
}

func TestSwitchToBackgroundIgnoresTurnEndedBeforeSwitch(t *testing.T) {
	f := newSignalFixture(t)
	f.h.Tasks.Assign(f.idHex, testRunnerID("r1"), "", false)
	f.lifecycle(protocol.LifecycleState_Working)
	time.Sleep(2 * time.Millisecond)
	f.lifecycle(protocol.LifecycleState_Idle)
	time.Sleep(2 * time.Millisecond)
	cond := &protocol.BackgroundConditions{DeadlineMs: 3600000, TurnEnd: protocol.TurnEndSignal_Lifecycle}
	if r := sessionMode(t, f.h, modeOpConn(), f.idHex, true, cond); r.Status != protocol.SessionModeStatus_Ok {
		t.Fatalf("status %v %q", r.Status, r.ErrorMsg)
	}
	time.Sleep(1200 * time.Millisecond)
	if e, _ := f.h.Tasks.Get(f.idHex); e.Status == protocol.TaskStatus_Cancelled {
		t.Fatalf("a turn that ended before the switch ended the run (%v)", e.EndReason)
	}
	f.lifecycle(protocol.LifecycleState_Working)
	time.Sleep(2 * time.Millisecond)
	f.lifecycle(protocol.LifecycleState_Idle)
	if e := waitEnded(t, f.h, f.idHex, 3*time.Second); e.EndReason != protocol.EndReason_TurnDoneLifecycle {
		t.Fatalf("end reason %v", e.EndReason)
	}
}

func TestSwitchQueuedRefused(t *testing.T) {
	h := newTestHandler(t)
	id := createBG(t, h.Tasks, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	if r := sessionMode(t, h, modeOpConn(), id, false, nil); r.Status != protocol.SessionModeStatus_Queued {
		t.Fatalf("status %v", r.Status)
	}
}

// agentConn makes a connection whose principal is a fresh agent task holding
// cancel, scoped to its own subtree.
func modeAgentConn(t *testing.T, h *TaskHandler, port string) (*fakeConn, string) {
	t.Helper()
	id := h.Tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_Cancel, Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}}, "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:" + port + "-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, id)
	return conn, id
}

func TestSwitchOutOfScopeIsNotFound(t *testing.T) {
	h := newTestHandler(t)
	conn, _ := modeAgentConn(t, h, "9161")
	victim := createBG(t, h.Tasks, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	h.Tasks.Assign(victim, testRunnerID("r1"), "", false)
	if r := sessionMode(t, h, conn, victim, false, nil); r.Status != protocol.SessionModeStatus_NotFound {
		t.Fatalf("status %v", r.Status)
	}
	if e, _ := h.Tasks.Get(victim); e.RunMode != protocol.RunMode_Background {
		t.Fatal("an out-of-scope caller switched the task")
	}
}

// Spec A-D19: a task may not switch itself to foreground — it would remove its
// own deadline.
func TestSwitchSelfToForegroundRefused(t *testing.T) {
	h := newTestHandler(t)
	conn, self := modeAgentConn(t, h, "9162")
	h.Tasks.Assign(self, testRunnerID("r1"), "", false)
	if _, err := h.Tasks.SwitchRunMode(self, protocol.RunMode_Background, RunConditions{Deadline: time.Hour}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if r := sessionMode(t, h, conn, self, false, nil); r.Status != protocol.SessionModeStatus_SelfSwitchRefused {
		t.Fatalf("status %v", r.Status)
	}
	if e, _ := h.Tasks.Get(self); e.RunMode != protocol.RunMode_Background || e.DeadlineAt == 0 {
		t.Fatal("the task escaped its deadline")
	}
}

func TestSwitchStreamRefusesQuietTurnEnd(t *testing.T) {
	h := newTestHandler(t)
	id := h.Tasks.Create("/r", "", protocol.TaskKind_Stream, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "claude")
	h.Tasks.Assign(id, testRunnerID("r1"), "", false)
	cond := &protocol.BackgroundConditions{DeadlineMs: 1000, TurnEnd: protocol.TurnEndSignal_Quiet}
	if r := sessionMode(t, h, modeOpConn(), id, true, cond); r.Status != protocol.SessionModeStatus_TurnEndUnsupported {
		t.Fatalf("status %v", r.Status)
	}
}
