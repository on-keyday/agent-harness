package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
	"github.com/on-keyday/objtrsf/exec/frame"
	"github.com/on-keyday/objtrsf/objproto"
)

// liveMux is a live interactive mux and the fake runner stream behind it.
func liveMux(t *testing.T) (*SessionMux, *fakeBidiStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Interactive, runner, NewRingBuffer(4096), SessionHooks{})
	t.Cleanup(mux.Stop)
	return mux, runner
}

func stdinReply() []byte {
	return encodeFrame(frame.FrameType_Stdin, []byte(vtgrid.ProgramStatusQueryReply))
}

// claude sends nothing until the terminal answers its probe, and the
// operator's terminals do not know 7501: the server answers (spec D18).
func TestSessionMux_AnswersLiveProbe(t *testing.T) {
	mux, runner := liveMux(t)
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("\x1b]7501;?\x1b\\")))
	waitFor(t, func() bool { return bytes.Contains(runner.Written(), stdinReply()) })
	if n := bytes.Count(runner.Written(), stdinReply()); n != 1 {
		t.Fatalf("replies = %d, want exactly 1", n)
	}
	_ = mux
}

// A probe recorded in a held capture was asked of a server that is gone, by a
// program that settled its answer long ago; answering it now would type the
// reply into whatever holds the PTY.
func TestSessionMux_DoesNotAnswerLoadedProbe(t *testing.T) {
	mux, runner := liveMux(t)
	mux.loadScreen([]byte("\x1b]7501;?\x07"))
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("\x1b]0;later\x07")))
	waitFor(t, func() bool { return mux.Title() == "later" })
	if bytes.Contains(runner.Written(), []byte("7501")) {
		t.Fatalf("a loaded probe was answered: %q", runner.Written())
	}
}

func TestList_ProgramStatusFromLiveMux(t *testing.T) {
	h := newTestHandler(t)
	pHex := h.Tasks.Create("/repo", "parent", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}}, "")
	cHex := h.Tasks.Create("/repo", "child", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		hexToTaskID(t, pHex), "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}, "")
	mux, runner := liveMux(t)
	msg := base64.StdEncoding.EncodeToString([]byte("approve Write: a.go"))
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("\x1b]7501;state=blocked:kind=permission:msg="+msg+"\x07")))
	waitFor(t, func() bool { s, _, _ := mux.Program(); return s != protocol.ProgramState_None })
	h.Sessions.Add(pHex, mux)

	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9741-1"), nextSendStreamID: 10}
	row, ok := listRowAs(t, h, op, pHex)
	if !ok || row.ProgramState != protocol.ProgramState_Blocked || row.ProgramKind != protocol.ProgramKind_Permission ||
		string(row.ProgramMsg) != "approve Write: a.go" {
		t.Fatalf("operator row program = %v/%v %q", row.ProgramState, row.ProgramKind, row.ProgramMsg)
	}

	child := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9742-1"), nextSendStreamID: 10}
	h.principals = map[string]protocol.TaskID{child.ConnectionID().String(): hexToTaskID(t, cHex)}
	hop, ok := listRowAs(t, h, child, pHex)
	if !ok || hop.ProgramState != protocol.ProgramState_Blocked || hop.ProgramKind != protocol.ProgramKind_Permission || len(hop.ProgramMsg) != 0 {
		t.Fatalf("parent-hop row program = %v/%v %q; want state/kind kept, msg emptied", hop.ProgramState, hop.ProgramKind, hop.ProgramMsg)
	}
}

func TestTaskEvent_CarriesProgramStatus(t *testing.T) {
	s := makeEventServer(t)
	tHex := s.tasks.Create("/r", "a", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}}, "")
	mux, runner := liveMux(t)
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("\x1b]7501;state=working\x07")))
	waitFor(t, func() bool { st, _, _ := mux.Program(); return st == protocol.ProgramState_Working })
	s.taskHandler.Sessions.Add(tHex, mux)
	op := subscribeTasksStatus(t, s, "ws:127.0.0.1:9743-1")
	s.tasks.OnHold(tHex)
	evs := op.take()
	if len(evs) != 1 || evs[0].ProgramState != protocol.ProgramState_Working {
		t.Fatalf("events = %+v, want one carrying program_state working", evs)
	}
}
