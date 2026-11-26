package server

import (
	"reflect"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// exec of a server-defined action, and exec_run's object gates
// (2026-11-26-grant-conditions-and-bound-enrollment-design.md, C7, C8, C10, C12, C17).

const execBodyStreamID = 7

type execActEnv struct {
	*condGateEnv
	execs int
}

// newExecActEnv is condGateEnv with an action store, and streams on both the
// client and the runner connection so an exec can actually open.
func newExecActEnv(t *testing.T) *execActEnv {
	t.Helper()
	e := newCondGateEnv(t, nil)
	e.h.Actions, _ = LoadActionStore("")
	e.conn.nextStreamID, e.conn.autoSendStreams = 50, true
	e.runner.nextStreamID, e.runner.autoSendStreams = 100, true
	return &execActEnv{condGateEnv: e}
}

// grantOnly sets the caller's exec_run override to the target T conditioned
// on ids; no ids = the caller's exec_run is unconditioned.
func (e *execActEnv) grantOnly(t *testing.T, ids ...[16]byte) {
	t.Helper()
	s := authority.Scope{Base: protocol.ScopeBase_Subtree}
	if len(ids) > 0 {
		s.Overrides = []authority.ScopeOverride{{Caps: protocol.Capability_ExecRun, Base: protocol.ScopeBase_Subtree, Cond: actionCond(ids...)}}
	}
	e.h.Tasks.mu.Lock()
	e.h.Tasks.tasks[e.caller].Scope = s
	e.h.Tasks.mu.Unlock()
}

func (e *execActEnv) define(t *testing.T, name string, specs []string, words ...string) [16]byte {
	t.Helper()
	rec, err := e.h.Actions.Create(actDef(t, name, specs, words...), "operator", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rec.Id.Id
}

func invoke(id [16]byte, values ...string) []byte {
	inv := protocol.ActionInvocation{Id: protocol.ActionID{Id: id}}
	var vs []protocol.ActionValue
	for _, v := range values {
		var av protocol.ActionValue
		av.SetBytes([]byte(v))
		vs = append(vs, av)
	}
	inv.SetValues(vs)
	var b protocol.ExecRunBody
	b.SetActionPresent(true)
	b.SetAction(inv)
	return b.MustAppend(nil)
}

func rawArgv(argv ...string) []byte {
	b := protocol.ExecRunBody{Argv: buildTestArgv(argv)}
	return b.MustAppend(nil)
}

func buildTestArgv(argv []string) protocol.ExecArgv {
	var a protocol.ExecArgv
	for _, s := range argv {
		var one protocol.ExecArg
		one.SetArg([]byte(s))
		a.Argv = append(a.Argv, one)
	}
	a.ArgvLen = uint16(len(a.Argv))
	return a
}

// exec runs handleOpenExecRun as connection conn (the caller's, by default)
// against T with body on its stream.
func (e *execActEnv) exec(t *testing.T, conn *fakeConn, req protocol.ExecRunRequest, body []byte) protocol.ExecRunResponse {
	t.Helper()
	if conn == nil {
		conn = e.conn
	}
	// fakeConn hands out one bidirectional stream per arming.
	e.execs++
	conn.nextStreamID = trsf.StreamID(50 + 2*e.execs)
	e.runner.nextStreamID = trsf.StreamID(100 + 2*e.execs)
	req.TaskId, req.PayloadStreamId = e.tid, execBodyStreamID
	c := gcConn{fakeConn: conn, streams: map[trsf.StreamID]trsf.ReceiveStream{execBodyStreamID: &fakeRecv{data: body}}}
	return e.h.handleOpenExecRun(c, &req)
}

// sentToRunner is the last exec the runner was told about: its envelope, and
// the body written on the runner's last send stream.
func (e *execActEnv) sentToRunner(t *testing.T) (protocol.RunnerExecRunRequest, protocol.RunnerExecRunBody) {
	t.Helper()
	msgs := e.runner.Sent()
	if len(msgs) == 0 {
		t.Fatal("nothing sent to the runner")
	}
	var rr protocol.RunnerRequest
	if err := rr.DecodeExact(msgs[len(msgs)-1][1:]); err != nil || rr.OpenExecRun() == nil {
		t.Fatalf("runner request: %v %+v", err, rr)
	}
	var body protocol.RunnerExecRunBody
	ss := e.runner.sendStreams
	if err := body.DecodeExact(ss[len(ss)-1].bytes); err != nil {
		t.Fatalf("runner body: %v", err)
	}
	return *rr.OpenExecRun(), body
}

func argvOf(b protocol.RunnerExecRunBody) []string { return execArgvStrings(b.Argv) }

func wantStatus(t *testing.T, what string, r protocol.ExecRunResponse, want protocol.ExecRunStatus) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %v, want %v", what, r.Status, want)
	}
}

func TestActionRunResolvesOnTheServer(t *testing.T) {
	e := newExecActEnv(t)
	a := e.define(t, "hello", []string{"who=token"}, "echo", "hello", "{who}")
	e.grantOnly(t, a)
	wantStatus(t, "action run", e.exec(t, nil, protocol.ExecRunRequest{}, invoke(a, "world")), protocol.ExecRunStatus_Ok)
	_, body := e.sentToRunner(t)
	if got := argvOf(body); !reflect.DeepEqual(got, []string{"echo", "hello", "world"}) {
		t.Fatalf("runner argv %q", got)
	}
	rows := e.h.visibleExecRuns(e.cid, protocol.TaskID{})
	if len(rows) != 1 || !rows[0].ActionPresent() || *rows[0].ActionId() != (protocol.ActionID{Id: a}) ||
		*rows[0].ActionRevision() != 1 || string(*rows[0].ActionName()) != "hello" {
		t.Fatalf("exec row: %+v", rows)
	}
}

// C12/C17: whatever the request asked, an action runs with no shell, no PTY
// and no stdin, and each value is one argv element.
func TestActionRunForcesFlagsAndOneElementPerValue(t *testing.T) {
	e := newExecActEnv(t)
	a := e.define(t, "say", []string{"what=choice:a b,$(id);x"}, "echo", "{what}")
	e.grantOnly(t, a)
	var req protocol.ExecRunRequest
	req.SetShellLine(true)
	req.SetPty(true)
	req.SetStdinEnabled(true)
	req.SetSshdParent(true)
	for _, v := range []string{"a b", "$(id);x"} {
		wantStatus(t, v, e.exec(t, nil, req, invoke(a, v)), protocol.ExecRunStatus_Ok)
		env, body := e.sentToRunner(t)
		if env.ShellLine() || env.Pty() || env.StdinEnabled() || env.SshdParent() {
			t.Fatalf("%q: runner flags shell=%v pty=%v stdin=%v sshd=%v, want all off", v, env.ShellLine(), env.Pty(), env.StdinEnabled(), env.SshdParent())
		}
		if got := argvOf(body); !reflect.DeepEqual(got, []string{"echo", v}) || len(body.Term) != 0 {
			t.Fatalf("%q: argv %q term %q", v, got, body.Term)
		}
	}
}

func TestRawArgvRefusedOnActionSetConditionedExecRun(t *testing.T) {
	e := newExecActEnv(t)
	e.grantOnly(t, e.define(t, "a", nil, "true"))
	wantStatus(t, "raw argv", e.exec(t, nil, protocol.ExecRunRequest{}, rawArgv("echo", "raw")), protocol.ExecRunStatus_OutsideCondition)
	if len(e.runner.Sent()) != 0 {
		t.Fatal("a refused exec reached the runner")
	}
}

func TestActionOutsideTheConditionRefused(t *testing.T) {
	e := newExecActEnv(t)
	a := e.define(t, "a", nil, "true")
	b := e.define(t, "b", nil, "false")
	e.grantOnly(t, a)
	wantStatus(t, "action b", e.exec(t, nil, protocol.ExecRunRequest{}, invoke(b)), protocol.ExecRunStatus_OutsideCondition)
}

// Through the dispatch: a target outside the caller's exec_run reach answers
// not_found, and the body stream is discarded unread.
func TestActionRunOnTargetOutOfScopeIsNotFound(t *testing.T) {
	e := newExecActEnv(t)
	a := e.define(t, "a", nil, "true")
	e.grantOnly(t, a)
	other := e.h.Tasks.Create("/r", "o", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	recv := &fakeRecv{data: invoke(a)}
	c := gcConn{fakeConn: e.conn, streams: map[trsf.StreamID]trsf.ReceiveStream{execBodyStreamID: recv}}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenExecRun, RequestId: 9}}
	req.SetOpenExecRun(protocol.ExecRunRequest{TaskId: hexToTaskID(t, other), PayloadStreamId: execBodyStreamID})
	e.h.Handle(c, encodeTaskControlRequest(t, req))
	deadline := time.Now().Add(2 * time.Second)
	for len(e.conn.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	resp := lastTaskControlResponse(t, e.conn)
	if r := resp.OpenExecRun(); r == nil || r.Status != protocol.ExecRunStatus_NotFound {
		t.Fatalf("out-of-scope target: %+v", resp)
	}
	if !recv.cancelled {
		t.Fatal("the body stream of a refused exec was not discarded")
	}
}

func TestDeletedActionIsNoSuchAction(t *testing.T) {
	e := newExecActEnv(t)
	a := e.define(t, "a", nil, "true")
	e.grantOnly(t, a)
	wantStatus(t, "before delete", e.exec(t, nil, protocol.ExecRunRequest{}, invoke(a)), protocol.ExecRunStatus_Ok)
	if err := e.h.Actions.Remove(a); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "after delete", e.exec(t, nil, protocol.ExecRunRequest{}, invoke(a)), protocol.ExecRunStatus_NoSuchAction)
}

// C16: a re-created action with the old name has a new id, which the old
// grant does not name.
func TestRecreatedActionIsNotTheOldGrant(t *testing.T) {
	e := newExecActEnv(t)
	old := e.define(t, "deploy", nil, "true")
	e.grantOnly(t, old)
	e.h.Actions.Remove(old)
	fresh := e.define(t, "deploy", nil, "rm", "-rf", "tmp")
	if fresh == old {
		t.Fatal("a re-created action took the old id")
	}
	wantStatus(t, "new id", e.exec(t, nil, protocol.ExecRunRequest{}, invoke(fresh)), protocol.ExecRunStatus_OutsideCondition)
	wantStatus(t, "old id", e.exec(t, nil, protocol.ExecRunRequest{}, invoke(old)), protocol.ExecRunStatus_NoSuchAction)
}

func TestEditedActionRunsTheNewTemplate(t *testing.T) {
	e := newExecActEnv(t)
	a := e.define(t, "a", nil, "echo", "one")
	e.grantOnly(t, a)
	if _, err := e.h.Actions.Edit(a, 1, actDef(t, "a", nil, "echo", "two"), "operator", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "run", e.exec(t, nil, protocol.ExecRunRequest{}, invoke(a)), protocol.ExecRunStatus_Ok)
	if _, body := e.sentToRunner(t); !reflect.DeepEqual(argvOf(body), []string{"echo", "two"}) {
		t.Fatalf("argv %q, want the edited template", argvOf(body))
	}
	if rows := e.h.visibleExecRuns(e.cid, protocol.TaskID{}); *rows[0].ActionRevision() != 2 {
		t.Fatalf("row revision %d, want 2", *rows[0].ActionRevision())
	}
}

func TestUnconditionedHolderRunsActionsAndArgv(t *testing.T) {
	e := newExecActEnv(t)
	a := e.define(t, "a", nil, "true")
	e.grantOnly(t)
	wantStatus(t, "action", e.exec(t, nil, protocol.ExecRunRequest{}, invoke(a)), protocol.ExecRunStatus_Ok)
	wantStatus(t, "raw argv", e.exec(t, nil, protocol.ExecRunRequest{}, rawArgv("echo", "raw")), protocol.ExecRunStatus_Ok)
}

// Kill's object is the action the exec was started with: a holder of an
// ActionSet-conditioned exec_run stops its actions, not a raw-argv exec.
func TestKillActionExecAllowedRawExecRefused(t *testing.T) {
	e := newExecActEnv(t)
	a := e.define(t, "a", nil, "sleep", "60")
	e.grantOnly(t, a)
	started := e.exec(t, nil, protocol.ExecRunRequest{}, invoke(a))
	wantStatus(t, "start action", started, protocol.ExecRunStatus_Ok)

	op := &fakeConn{id: objproto.ConnectionID{}, nextStreamID: 60, autoSendStreams: true}
	raw := e.exec(t, op, protocol.ExecRunRequest{}, rawArgv("sleep", "60"))
	wantStatus(t, "operator raw start", raw, protocol.ExecRunStatus_Ok)

	if r := e.h.handleExecRunKill(e.cid, &protocol.ExecRunKillRequest{ExecId: raw.ExecId}); r.Status != protocol.ExecRunStatus_NotFound {
		t.Fatalf("kill of a raw-argv exec through a conditioned grant: %v, want not_found", r.Status)
	}
	if r := e.h.handleExecRunKill(e.cid, &protocol.ExecRunKillRequest{ExecId: started.ExecId}); r.Status != protocol.ExecRunStatus_Ok {
		t.Fatalf("kill of the action exec: %v, want ok", r.Status)
	}
}

// Pins what transcript.go:64-65 already gives through inScope: a conditioned
// exec_run does not widen a transcript listing to shared sessions.
func TestConditionedExecRunIncludesNoSharedTranscripts(t *testing.T) {
	f := newTranscriptServerFixture(t, true)
	global := authority.Scope{Base: protocol.ScopeBase_Global}
	conditioned := authority.Scope{Base: protocol.ScopeBase_Global, Overrides: []authority.ScopeOverride{
		{Caps: protocol.Capability_ExecRun, Base: protocol.ScopeBase_Global, Cond: actionCond([16]byte{1})}}}
	c := f.agentCaller(t, protocol.Capability_TranscriptRead|protocol.Capability_ExecRun, conditioned)
	u := f.agentCaller(t, protocol.Capability_TranscriptRead|protocol.Capability_ExecRun, global)
	for _, caller := range []*fakeConn{c, u} {
		if st := transcriptStatus(t, f.list(t, caller)); st != protocol.TranscriptStatus_Ok {
			t.Fatalf("status %v", st)
		}
	}
	sent := f.sentTranscriptRequests(t)
	if len(sent) != 2 || sent[0].IncludeShared() || !sent[1].IncludeShared() {
		t.Fatalf("include_shared: conditioned %v, unconditioned %v; want false, true", sent[0].IncludeShared(), sent[1].IncludeShared())
	}
}
