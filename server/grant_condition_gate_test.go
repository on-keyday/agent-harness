package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// A conditioned bit reaches nothing through the object-blind gates; only
// authorizeObject, which evaluates the condition, passes it (grant conditions
// C7, C14).

const fwdBoth = protocol.Capability_ForwardLocal | protocol.Capability_ForwardRemote

type condGateEnv struct {
	h      *TaskHandler
	conn   *fakeConn
	runner *fakeConn
	cid    objproto.ConnectionID
	caller string
	target string
	tid    protocol.TaskID
}

// newCondGateEnv makes a caller task C holding forward_local, forward_remote
// and exec_run, scoped to subtree, with the given overrides; and a running
// child T on an online runner.
func newCondGateEnv(t *testing.T, overrides []authority.ScopeOverride) *condGateEnv {
	t.Helper()
	h := newTestHandler(t)
	caps := fwdBoth | protocol.Capability_ExecRun
	c := h.Tasks.Create("/r", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: caps, Scope: authority.Scope{Base: protocol.ScopeBase_Subtree, Overrides: overrides}}, "")
	tg := h.Tasks.Create("/r", "t", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, hexToTaskID(t, c), "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	h.Tasks.mu.Lock()
	h.Tasks.tasks[tg].Status = protocol.TaskStatus_Running
	h.Tasks.tasks[tg].AssignedTo = testRunnerID("runner-1")
	h.Tasks.mu.Unlock()
	runner := &fakeConn{}
	h.Registry.Add(&RunnerEntry{ID: tcid("runner-1"), Identity: testRunnerID("runner-1"), Conn: runner})
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9710-1")}
	h.principals = map[objproto.ConnectionID]protocol.TaskID{conn.ConnectionID(): hexToTaskID(t, c)}
	return &condGateEnv{h: h, conn: conn, runner: runner, cid: conn.ConnectionID(), caller: c, target: tg, tid: hexToTaskID(t, tg)}
}

func portRange(t *testing.T, lo, hi uint16) *authority.Condition {
	return portsCond(t, authority.PortSpan{Lo: lo, Hi: hi})
}

func conditionedForwards(t *testing.T) []authority.ScopeOverride {
	return []authority.ScopeOverride{{Caps: fwdBoth, Base: protocol.ScopeBase_Subtree, Cond: portRange(t, 8000, 8100)}}
}

func TestConditionedBitReachesNothingThroughEveryGate(t *testing.T) {
	e := newCondGateEnv(t, []authority.ScopeOverride{
		{Caps: protocol.Capability_ExecRun, Base: protocol.ScopeBase_Subtree, Cond: actionsCond(t, 1)},
		{Caps: protocol.Capability_ForwardLocal, Base: protocol.ScopeBase_Subtree, Cond: portRange(t, 8000, 8100)},
	})
	for _, bit := range []protocol.Capability{protocol.Capability_ExecRun, protocol.Capability_ForwardLocal} {
		if e.h.inScope(e.cid, bit, e.target) {
			t.Errorf("%s: inScope reached the target", bit)
		}
		if e.h.authorize(e.cid, bit, e.target) {
			t.Errorf("%s: authorize reached the target", bit)
		}
		if all, allowed := e.h.scopeSet(e.cid, bit); all || len(allowed) != 0 {
			t.Errorf("%s: scopeSet = all %v, %d ids; want nothing", bit, all, len(allowed))
		}
	}
	// The unconditioned bit in the same scope still reaches it: the gate
	// above is the condition, not a broken fixture.
	if !e.h.authorize(e.cid, protocol.Capability_ForwardRemote, e.target) {
		t.Fatal("unconditioned forward_remote does not reach the child; the fixture is wrong")
	}
}

func TestAuthorizeObjectPassesAnAllowedPort(t *testing.T) {
	e := newCondGateEnv(t, conditionedForwards(t))
	if !e.h.authorizeObject(e.cid, protocol.Capability_ForwardLocal, e.target, authority.Object{Kind: protocol.ConditionKind_PortRange, Port: 8100}) {
		t.Fatal("an allowed port was refused")
	}
}

func TestAuthorizeObjectRefusesAnotherPort(t *testing.T) {
	e := newCondGateEnv(t, conditionedForwards(t))
	if e.h.authorizeObject(e.cid, protocol.Capability_ForwardLocal, e.target, authority.Object{Kind: protocol.ConditionKind_PortRange, Port: 8101}) {
		t.Fatal("a port outside the range passed")
	}
	// Out of scope stays out of scope, whatever the object.
	other := e.h.Tasks.Create("/r", "u", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	if e.h.authorizeObject(e.cid, protocol.Capability_ForwardLocal, other, authority.Object{Kind: protocol.ConditionKind_PortRange, Port: 8050}) {
		t.Fatal("a target outside the scope passed")
	}
}

func (e *condGateEnv) register(t *testing.T, dir protocol.PortForwardDirection, target, bind uint16) protocol.RegisterPortForwardResponse {
	t.Helper()
	ctrl := newRecordingBidiStream(777)
	t.Cleanup(func() { ctrl.CloseBoth() }) //nolint:errcheck
	e.conn.nextBidi = ctrl
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_RegisterPortForward, RequestId: 1}}
	rf := protocol.RegisterPortForwardRequest{TaskId: e.tid, Direction: dir, TargetPort: target, BindPort: bind}
	rf.SetTargetHost([]byte("127.0.0.1"))
	rf.SetBindAddr([]byte("127.0.0.1"))
	req.SetRegisterPortForward(rf)
	e.h.Handle(e.conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, e.conn)
	r := resp.RegisterPortForward()
	if r == nil {
		t.Fatal("no register response")
	}
	return *r
}

func (e *condGateEnv) open(t *testing.T, fid uint64, port uint16) protocol.OpenPortForwardStatus {
	t.Helper()
	e.conn.nextStreamID = 501
	e.runner.nextStreamID = 900
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenPortForward, RequestId: 2}}
	of := protocol.OpenPortForwardRequest{TaskId: e.tid, RemotePort: port, ForwardId: fid}
	of.SetRemoteHost([]byte("127.0.0.1"))
	req.SetOpenPortForward(of)
	e.h.Handle(e.conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, e.conn)
	r := resp.OpenPortForward()
	if r == nil {
		t.Fatal("no open response")
	}
	return r.Status
}

func (e *condGateEnv) kill(t *testing.T, dir protocol.PortForwardDirection, target, bind uint16) protocol.KillPortForwardStatus {
	t.Helper()
	id := e.h.pforwards().add(&portForward{direction: dir, taskIDHex: e.target, targetPort: target, bindPort: bind})
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_KillPortForward, RequestId: 3}}
	req.SetKillPortForward(protocol.KillPortForwardRequest{ForwardId: id})
	e.h.Handle(e.conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, e.conn)
	r := resp.KillPortForward()
	if r == nil {
		t.Fatalf("no kill response (kind %v)", resp.Kind)
	}
	return r.Status
}

func TestPortRangeHoldsOnEachForwardOpen(t *testing.T) {
	e := newCondGateEnv(t, conditionedForwards(t))
	reg := e.register(t, protocol.PortForwardDirection_Local, 8050, 18080)
	if reg.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("register with an allowed target_port: %v", reg.Status)
	}
	// The registration named an allowed port; a connection naming another is
	// refused on its own open.
	if st := e.open(t, reg.ForwardId, 9000); st != protocol.OpenPortForwardStatus_OutsideCondition {
		t.Fatalf("open on 9000 through an 8050 registration: %v, want outside_condition", st)
	}
	if st := e.open(t, reg.ForwardId, 8050); st != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("open on 8050: %v, want ok", st)
	}
	if st := e.register(t, protocol.PortForwardDirection_Local, 9000, 18081).Status; st != protocol.OpenPortForwardStatus_OutsideCondition {
		t.Fatalf("register -L to 9000: %v, want outside_condition", st)
	}
	// -R is bounded on bind_port (C14), not target_port.
	if st := e.register(t, protocol.PortForwardDirection_Remote, 8050, 9000).Status; st != protocol.OpenPortForwardStatus_OutsideCondition {
		t.Fatalf("register -R binding 9000: %v, want outside_condition", st)
	}
}

func TestKillPortForwardUsesTheRegistrationPort(t *testing.T) {
	e := newCondGateEnv(t, conditionedForwards(t))
	if st := e.kill(t, protocol.PortForwardDirection_Local, 9000, 8050); st != protocol.KillPortForwardStatus_OutsideCondition {
		t.Fatalf("kill -L to 9000: %v, want outside_condition", st)
	}
	if st := e.kill(t, protocol.PortForwardDirection_Local, 8050, 9000); st != protocol.KillPortForwardStatus_Ok {
		t.Fatalf("kill -L to 8050: %v, want ok", st)
	}
	if st := e.kill(t, protocol.PortForwardDirection_Remote, 8050, 9000); st != protocol.KillPortForwardStatus_OutsideCondition {
		t.Fatalf("kill -R binding 9000: %v, want outside_condition", st)
	}
}

func TestUnconditionedForwardsUnchanged(t *testing.T) {
	e := newCondGateEnv(t, nil)
	reg := e.register(t, protocol.PortForwardDirection_Local, 9000, 18080)
	if reg.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("register: %v", reg.Status)
	}
	if st := e.open(t, reg.ForwardId, 9001); st != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("open: %v", st)
	}
	if st := e.kill(t, protocol.PortForwardDirection_Local, 9000, 18080); st != protocol.KillPortForwardStatus_Ok {
		t.Fatalf("kill: %v", st)
	}
}
