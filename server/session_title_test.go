package server

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
	"github.com/on-keyday/objtrsf/objproto"
)

// titledMux is a live interactive mux whose program has titled itself.
func titledMux(t *testing.T, title string) *SessionMux {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Interactive, runner, NewRingBuffer(4096), SessionHooks{})
	t.Cleanup(mux.Stop)
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("\x1b]0;"+title+"\x07")))
	waitFor(t, func() bool { return mux.Title() != "" })
	return mux
}

func TestList_TitleFromLiveMux(t *testing.T) {
	h := newTestHandler(t)
	tHex := h.Tasks.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	h.Sessions.Add(tHex, titledMux(t, "✳ topic\u0085"))
	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9731-1"), nextSendStreamID: 10}
	row, ok := listRowAs(t, h, op, tHex)
	if !ok {
		t.Fatal("row missing")
	}
	if !row.LiveTerminal() || string(row.Title) != "✳ topic" {
		t.Fatalf("live_terminal=%v title=%q, want true and the sanitized title", row.LiveTerminal(), row.Title)
	}
}

func TestList_NoMuxNoTerminal(t *testing.T) {
	h := newTestHandler(t)
	tHex := h.Tasks.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9732-1"), nextSendStreamID: 10}
	row, _ := listRowAs(t, h, op, tHex)
	if row.LiveTerminal() || len(row.Title) != 0 {
		t.Fatalf("oneshot row live_terminal=%v title=%q, want false and empty", row.LiveTerminal(), row.Title)
	}
}

func TestTaskEvent_CarriesTitle(t *testing.T) {
	s := makeEventServer(t)
	tHex := s.tasks.Create("/r", "a", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	s.taskHandler.Sessions.Add(tHex, titledMux(t, "✳ topic"))
	op := subscribeTasksStatus(t, s, "ws:127.0.0.1:9733-1")
	s.tasks.OnHold(tHex)
	evs := op.take()
	if len(evs) != 1 || !evs[0].LiveTerminal() || string(evs[0].Title) != "✳ topic" {
		t.Fatalf("events = %+v, want one carrying live_terminal and the title", evs)
	}
}

// The title survives a deliberate restart through the held capture: the
// repaint carries it, and the re-adopted model parses it back.
func TestHeldCapture_TitleSurvives(t *testing.T) {
	src := titledMux(t, "✳ topic")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dst := NewSessionMux(ctx, "task", protocol.TaskKind_Interactive, newFakeStream(t), NewRingBuffer(4096), SessionHooks{})
	defer dst.Stop()
	if _, _, err := dst.loadHeldCapture(src.heldCapture()); err != nil {
		t.Fatal(err)
	}
	if got := dst.Title(); got != "✳ topic" {
		t.Fatalf("title after held capture = %q", got)
	}
}

// handleList enriches live fields AFTER redacting the parent-hop row; the
// title is live but is content, so the parent's must not come back.
func TestList_ParentHopRowHasNoTitle(t *testing.T) {
	h := newTestHandler(t)
	pHex := h.Tasks.Create("/repo", "parent", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	cHex := h.Tasks.Create("/repo", "child", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		hexToTaskID(t, pHex), "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	h.Sessions.Add(pHex, titledMux(t, "parent's private title"))
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9734-1"), nextSendStreamID: 10}
	h.principals = map[string]protocol.TaskID{conn.ConnectionID().String(): hexToTaskID(t, cHex)}
	row, ok := listRowAs(t, h, conn, pHex)
	if !ok {
		t.Fatal("child must see its parent's row")
	}
	if len(row.Title) != 0 {
		t.Fatalf("parent-hop row title = %q, want empty", row.Title)
	}
	if !row.LiveTerminal() {
		t.Error("parent-hop row lost live_terminal; liveness is what the hop is for")
	}
}
