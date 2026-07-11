package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"strings"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// --- SessionMux.lastOutput stamping ---

func TestSessionMux_LastOutputStampsOnOutputFramesOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()

	if got := mux.LastOutputUnixNano(); got != 0 {
		t.Fatalf("lastOutput before any frame = %d, want 0", got)
	}

	// A Control frame must NOT stamp (it is plumbing, not PTY output).
	ctrl := makeWireFrame(byte(frame.FrameType_Control), []byte("resize"))
	runner.QueueRead(ctrl)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(ctrl) })
	if got := mux.LastOutputUnixNano(); got != 0 {
		t.Fatalf("lastOutput after control frame = %d, want 0", got)
	}

	// A Stdout frame stamps.
	out := makeWireFrame(byte(frame.FrameType_Stdout), []byte("hello"))
	runner.QueueRead(out)
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })
}

// --- ArmIdleWatcher semantics ---

func TestSessionMux_IdleWatcherFiresAfterQuiescence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()

	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("boot")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })

	var fired atomic.Int32
	var gotStopped atomic.Bool
	mux.ArmIdleWatcher(50*time.Millisecond, func(stopped bool, lo int64) {
		gotStopped.Store(stopped)
		if lo == 0 {
			t.Error("fired with lastOutput=0")
		}
		fired.Add(1)
	})
	// Fire latency is bounded by threshold+idleWatchTick.
	waitForWithin(t, 2*time.Second, func() bool { return fired.Load() == 1 })
	if gotStopped.Load() {
		t.Fatal("fired with stopped=true, want idle edge")
	}
	// One-shot: no second fire.
	time.Sleep(2 * idleWatchTick)
	if n := fired.Load(); n != 1 {
		t.Fatalf("fired %d times, want exactly 1", n)
	}
}

func TestSessionMux_IdleWatcherFiresImmediatelyWhenAlreadyIdle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()

	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })
	time.Sleep(60 * time.Millisecond) // put the session past the threshold

	fired := make(chan bool, 1)
	start := time.Now()
	mux.ArmIdleWatcher(50*time.Millisecond, func(stopped bool, _ int64) {
		fired <- stopped
	})
	select {
	case stopped := <-fired:
		if stopped {
			t.Fatal("stopped=true, want idle fire")
		}
		// The pre-tick check fires without waiting a full idleWatchTick.
		if e := time.Since(start); e > idleWatchTick/2 {
			t.Fatalf("already-idle arm took %v, want immediate", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never fired")
	}
}

func TestSessionMux_IdleWatcherSessionStopped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	// No output ever (lastOutput==0): the watcher must wait, not fire.
	fired := make(chan bool, 1)
	mux.ArmIdleWatcher(10*time.Millisecond, func(stopped bool, _ int64) {
		fired <- stopped
	})
	select {
	case <-fired:
		t.Fatal("fired before any output and before Stop")
	case <-time.After(2 * idleWatchTick):
	}

	mux.Stop()
	select {
	case stopped := <-fired:
		if !stopped {
			t.Fatal("stopped=false, want session_stopped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never fired after Stop")
	}
}

// waitForWithin is waitFor with a caller-chosen deadline (watcher fire
// latency legitimately exceeds waitFor's fixed 1s: threshold+idleWatchTick).
func waitForWithin(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", d)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- handleAwaitIdle wire behaviour ---

// awaitIdleViaHandle drives the full Handle path (cap gate included: the
// fakeConn has no principal so callerCaps resolves to operator/all) and
// returns the decoded response once exactly one message has been sent.
func awaitIdleViaHandle(t *testing.T, h *TaskHandler, conn *fakeConn, req protocol.AwaitIdleRequest) *protocol.AwaitIdleResponse {
	t.Helper()
	tcr := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}
	tcr.SetAwaitIdle(req)
	payload := tcr.MustAppend(nil)
	h.Handle(conn, payload)
	waitFor(t, func() bool { return len(conn.Sent()) == 1 })
	return decodeAwaitIdleResponse(t, conn.Sent()[0])
}

func decodeAwaitIdleResponse(t *testing.T, raw []byte) *protocol.AwaitIdleResponse {
	t.Helper()
	if len(raw) == 0 || raw[0] != byte(appwire.AppKind_TaskControl) {
		t.Fatalf("unexpected wire kind in %x", raw)
	}
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(raw[1:]); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_AwaitIdle {
		t.Fatalf("response kind = %v, want AwaitIdle", resp.Kind)
	}
	ai := resp.AwaitIdle()
	if ai == nil {
		t.Fatal("AwaitIdle variant is nil")
	}
	if resp.RequestId != 7 {
		t.Fatalf("request id = %d, want 7", resp.RequestId)
	}
	return ai
}

func TestHandleAwaitIdle_NotFound(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: NewSessionRegistry()}
	conn := &fakeConn{}
	var tid protocol.TaskID
	tid.Id[0] = 0xAA
	resp := awaitIdleViaHandle(t, h, conn, protocol.AwaitIdleRequest{TaskId: tid})
	if resp.Status != protocol.AwaitIdleStatus_NotFound {
		t.Fatalf("status = %v, want NotFound", resp.Status)
	}
}

func TestHandleAwaitIdle_BadRequest_BoardWithoutTopic(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: NewSessionRegistry()}
	conn := &fakeConn{}
	var tid protocol.TaskID
	req := protocol.AwaitIdleRequest{TaskId: tid, Sink: protocol.AwaitIdleSink_Board}
	resp := awaitIdleViaHandle(t, h, conn, req)
	if resp.Status != protocol.AwaitIdleStatus_BadRequest {
		t.Fatalf("status = %v, want BadRequest", resp.Status)
	}
}

func TestHandleAwaitIdle_NotifySinkArmsThenFires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xCC
	reg.Add("cc000000000000000000000000000000", mux)

	var notified atomic.Int32
	var notifiedText atomic.Value
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Sessions: reg,
		OnNotify: func(ev protocol.NotifyEvent) {
			notifiedText.Store(string(ev.Text))
			notified.Add(1)
		},
	}
	conn := &fakeConn{}

	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("output")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })

	req := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Notify}
	resp := awaitIdleViaHandle(t, h, conn, req)
	if resp.Status != protocol.AwaitIdleStatus_Armed {
		t.Fatalf("status = %v, want Armed (immediate reply)", resp.Status)
	}
	waitForWithin(t, 3*time.Second, func() bool { return notified.Load() == 1 })
	text, _ := notifiedText.Load().(string)
	if !strings.Contains(text, "idle") || !strings.Contains(text, "cc000000") {
		t.Fatalf("notify text = %q, want session short-id + idle wording", text)
	}
}

func TestHandleAwaitIdle_BoardSinkPublishesOnFire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xDD
	reg.Add("dd000000000000000000000000000000", mux)

	board := agentboard.New(agentboard.Config{RingN: 8, MaxTopics: 16, MaxPayload: 4096})
	defer board.Close()
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg, Board: board}
	conn := &fakeConn{}

	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("output")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })

	req := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Board}
	req.SetTopic([]byte("chat.deadbeef"))
	resp := awaitIdleViaHandle(t, h, conn, req)
	if resp.Status != protocol.AwaitIdleStatus_Armed {
		t.Fatalf("status = %v, want Armed (immediate reply)", resp.Status)
	}
	waitForWithin(t, 3*time.Second, func() bool {
		msgs, _ := board.ListRetained("chat.deadbeef")
		return len(msgs) == 1
	})
	msgs, _ := board.ListRetained("chat.deadbeef")
	payload := string(msgs[0].Payload)
	for _, want := range []string{`"kind":"session_idle"`, `"task":"dd000000000000000000000000000000"`, `"status":"fired"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("board payload = %q, missing %s", payload, want)
		}
	}
}

func TestHandleAwaitIdle_ReplyLongPollFires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xBB
	idHex := "bb000000000000000000000000000000"
	reg.Add(idHex, mux)

	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg}
	conn := &fakeConn{}

	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("turn output")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })

	tcr := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}
	tcr.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Reply})
	h.Handle(conn, tcr.MustAppend(nil))

	// Long-poll: the response arrives only after the quiescence edge.
	waitForWithin(t, 3*time.Second, func() bool { return len(conn.Sent()) == 1 })
	resp := decodeAwaitIdleResponse(t, conn.Sent()[0])
	if resp.Status != protocol.AwaitIdleStatus_Fired {
		t.Fatalf("status = %v, want Fired", resp.Status)
	}
	if resp.LastOutputAt == 0 {
		t.Fatal("LastOutputAt = 0, want stamped")
	}
}
