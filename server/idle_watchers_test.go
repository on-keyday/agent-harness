package server

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// A watcher that is armed and then stopped through its channel returns
// without calling fn: the kill path owns what (if anything) is sent.
func TestSessionMux_IdleWatcherStopChannelEndsWithoutFiring(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()

	stop := make(chan struct{})
	fired := make(chan bool, 1)
	// No output yet (lastOutput == 0), so the watcher is waiting.
	mux.ArmIdleWatcher(10*time.Millisecond, stop, func(stopped bool, _ int64) { fired <- stopped })
	close(stop)
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
	select {
	case <-fired:
		t.Fatal("fn ran after stop was closed")
	case <-time.After(3 * idleWatchTick):
	}
}

func TestIdleWatcherRegistry_IdsStartAtOneAndRemoveIsOnce(t *testing.T) {
	r := newIdleWatcherRegistry()
	a := r.add(&idleWatcher{taskIDHex: "aa"})
	b := r.add(&idleWatcher{taskIDHex: "bb"})
	if a != 1 || b != 2 {
		t.Fatalf("ids = %d,%d, want 1,2", a, b)
	}
	if _, ok := r.remove(a); !ok {
		t.Fatal("first remove reported absent")
	}
	if _, ok := r.remove(a); ok {
		t.Fatal("second remove reported present")
	}
	if got := r.list("bb"); len(got) != 1 || got[0].id != b {
		t.Fatalf("list(bb) = %+v", got)
	}
}

// The arm response carries the registry id, and a fire removes the entry.
func TestHandleAwaitIdle_BoardArmCarriesIdAndFireDeregisters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xDD
	reg.Add("dd000000000000000000000000000000", mux)
	board := agentboardForTest(t)
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg, Board: board}
	conn := &fakeConn{}

	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("output")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })

	req := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Board}
	req.SetTopic([]byte("chat.deadbeef"))
	resp := awaitIdleViaHandle(t, h, conn, req)
	if resp.Status != protocol.AwaitIdleStatus_Armed || resp.WatcherId == 0 {
		t.Fatalf("resp = %+v, want Armed with a non-zero watcher id", resp)
	}
	waitForWithin(t, 3*time.Second, func() bool { return len(h.idleWatchers().list("")) == 0 })
}

func agentboardForTest(t *testing.T) *agentboard.Board {
	t.Helper()
	b := agentboard.New(agentboard.Config{RingN: 8, MaxTopics: 16, MaxPayload: 4096})
	t.Cleanup(b.Close)
	return b
}

// armBoardWatcher registers a board-sink watcher directly, bypassing the
// session mux: these tests are about who may see and end one, not about
// firing.
func armBoardWatcher(h *TaskHandler, taskHex string, requester protocol.TaskID, cid string) uint64 {
	return h.idleWatchers().add(&idleWatcher{
		taskIDHex: taskHex, sink: protocol.AwaitIdleSink_Board, topic: "chat.x",
		requester: requester, clientCID: cid, stop: make(chan struct{}),
	})
}

func TestIdleWatchers_OperatorSeesAllAgentSeesOwn(t *testing.T) {
	h := newTestHandler(t)
	target := "ee000000000000000000000000000000"
	scope := Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{target}}
	aHex := h.Tasks.Create("repo", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, scope, "")
	bHex := h.Tasks.Create("repo", "b", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, scope, "")
	a, b := hexToTaskID(t, aHex), hexToTaskID(t, bHex)
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	aConn := "ws:127.0.0.1:9601-1"
	bConn := "ws:127.0.0.1:9601-2"
	opConn := "ws:127.0.0.1:9601-9"
	h.principals[aConn] = a
	h.principals[bConn] = b

	wa := armBoardWatcher(h, target, a, aConn)
	wb := armBoardWatcher(h, target, b, bConn)
	wo := armBoardWatcher(h, target, protocol.TaskID{}, "ws:127.0.0.1:9601-3")

	if got := h.visibleIdleWatchers(opConn, protocol.TaskID{}); len(got) != 3 {
		t.Fatalf("operator sees %d watchers, want 3", len(got))
	}
	got := h.visibleIdleWatchers(aConn, protocol.TaskID{})
	if len(got) != 1 || got[0].WatcherId != wa {
		t.Fatalf("agent A sees %+v, want only watcher %d", got, wa)
	}
	for _, id := range []uint64{wb, wo} {
		r := h.handleAwaitIdleKill(aConn, &protocol.AwaitIdleKillRequest{WatcherId: id})
		if r.Status != protocol.AwaitIdleKillStatus_NotFound {
			t.Fatalf("A killing %d: %v, want NotFound", id, r.Status)
		}
		if _, ok := h.idleWatchers().get(id); !ok {
			t.Fatalf("watcher %d gone after a refused kill", id)
		}
	}
	if r := h.handleAwaitIdleKill(aConn, &protocol.AwaitIdleKillRequest{WatcherId: wa}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("A killing its own: %v, want Ok", r.Status)
	}
	if r := h.handleAwaitIdleKill(opConn, &protocol.AwaitIdleKillRequest{WatcherId: wb}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("operator killing B's: %v, want Ok", r.Status)
	}
}

// An agent whose scope no longer covers the target does not see its own
// watcher on it: the rest of the repo already reports that task as absent.
func TestIdleWatchers_AgentLosesSightWhenScopeNarrows(t *testing.T) {
	h := newTestHandler(t)
	aHex := h.Tasks.Create("repo", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None,
		Scope{Base: protocol.ScopeBase_Subtree}, "")
	a := hexToTaskID(t, aHex)
	h.principals = map[string]protocol.TaskID{"ws:127.0.0.1:9601-1": a}
	armBoardWatcher(h, "ee000000000000000000000000000000", a, "ws:127.0.0.1:9601-1")
	if got := h.visibleIdleWatchers("ws:127.0.0.1:9601-1", protocol.TaskID{}); len(got) != 0 {
		t.Fatalf("agent sees %d watchers on an out-of-scope task, want 0", len(got))
	}
}

func TestDropIdleWatchersForConn_ReplyOnly(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: NewSessionRegistry()}
	cid := "ws:127.0.0.1:9601-1"
	reply := h.idleWatchers().add(&idleWatcher{taskIDHex: "aa", sink: protocol.AwaitIdleSink_Reply, clientCID: cid, stop: make(chan struct{})})
	board := armBoardWatcher(h, "aa", protocol.TaskID{}, cid)
	other := h.idleWatchers().add(&idleWatcher{taskIDHex: "aa", sink: protocol.AwaitIdleSink_Reply, clientCID: "ws:127.0.0.1:9601-2", stop: make(chan struct{})})

	h.DropIdleWatchersForConn(cid)

	if _, ok := h.idleWatchers().get(reply); ok {
		t.Fatal("reply watcher of the closed conn survived")
	}
	for _, id := range []uint64{board, other} {
		if _, ok := h.idleWatchers().get(id); !ok {
			t.Fatalf("watcher %d was dropped, want kept", id)
		}
	}
}

// A killed reply-sink watcher answers its blocked caller with cancelled.
func TestHandleAwaitIdle_ReplyKilledAnswersCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xBB
	reg.Add("bb000000000000000000000000000000", mux)
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg}
	conn := &fakeConn{}

	// No output: the watcher waits indefinitely until killed.
	tcr := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}
	tcr.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Reply})
	h.Handle(conn, tcr.MustAppend(nil))
	waitFor(t, func() bool { return len(h.idleWatchers().list("")) == 1 })
	id := h.idleWatchers().list("")[0].id

	if r := h.handleAwaitIdleKill("", &protocol.AwaitIdleKillRequest{WatcherId: id}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("kill: %v", r.Status)
	}
	waitFor(t, func() bool { return len(conn.Sent()) == 1 })
	resp := decodeAwaitIdleResponse(t, conn.Sent()[0])
	if resp.Status != protocol.AwaitIdleStatus_Cancelled || resp.WatcherId != id {
		t.Fatalf("resp = %+v, want Cancelled for watcher %d", resp, id)
	}
	// Output after the kill must not produce a second response.
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
	time.Sleep(3 * idleWatchTick)
	if n := len(conn.Sent()); n != 1 {
		t.Fatalf("%d responses, want exactly 1", n)
	}
}

// A killed board-sink watcher publishes nothing, even once the session idles.
func TestHandleAwaitIdle_BoardKilledPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xDD
	reg.Add("dd000000000000000000000000000000", mux)
	board := agentboardForTest(t)
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg, Board: board}
	conn := &fakeConn{}

	req := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Board}
	req.SetTopic([]byte("chat.deadbeef"))
	resp := awaitIdleViaHandle(t, h, conn, req)
	if r := h.handleAwaitIdleKill("", &protocol.AwaitIdleKillRequest{WatcherId: resp.WatcherId}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("kill: %v", r.Status)
	}
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })
	time.Sleep(50*time.Millisecond + 3*idleWatchTick)
	if msgs, _ := board.ListRetained("chat.deadbeef"); len(msgs) != 0 {
		t.Fatalf("%d messages published after kill, want 0", len(msgs))
	}
}

// Fire and kill racing at the threshold edge: exactly one of them wins.
func TestIdleWatcher_FireKillRaceHasOneWinner(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		runner := newFakeStream(t)
		reg := NewSessionRegistry()
		mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
		var tid protocol.TaskID
		tid.Id[0] = 0xBB
		reg.Add("bb000000000000000000000000000000", mux)
		h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg}
		conn := &fakeConn{}
		runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
		waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })

		tcr := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}
		tcr.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 1, Sink: protocol.AwaitIdleSink_Reply})
		h.Handle(conn, tcr.MustAppend(nil))
		// id is 1: a fresh handler per iteration.
		kr := h.handleAwaitIdleKill("", &protocol.AwaitIdleKillRequest{WatcherId: 1})
		waitFor(t, func() bool { return len(conn.Sent()) >= 1 })
		time.Sleep(2 * idleWatchTick)
		if n := len(conn.Sent()); n != 1 {
			t.Fatalf("iter %d: %d responses, want 1", i, n)
		}
		st := decodeAwaitIdleResponse(t, conn.Sent()[0]).Status
		killWon := kr.Status == protocol.AwaitIdleKillStatus_Ok
		if killWon != (st == protocol.AwaitIdleStatus_Cancelled) {
			t.Fatalf("iter %d: kill=%v but response=%v", i, kr.Status, st)
		}
		mux.Stop()
		cancel()
	}
}
