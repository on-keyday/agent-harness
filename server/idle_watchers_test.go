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
