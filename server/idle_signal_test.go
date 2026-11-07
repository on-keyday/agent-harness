package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

func ms(n int64) int64 { return n * int64(time.Millisecond) }

func TestLegSatisfied(t *testing.T) {
	th := 2500 * time.Millisecond
	st := func(now, start, input, output int64) idleStamps {
		return idleStamps{Now: ms(now), MuxStart: ms(start), LastInput: ms(input), LastOutput: ms(output), Threshold: th}
	}
	cases := []struct {
		name               string
		nonWorking         bool
		stateAt, workingAt int64
		s                  idleStamps
		want               bool
	}{
		{"working never fires", false, ms(200), ms(150), st(9000, 100, 0, 150), false},
		{"no input, reported since start: at once", true, ms(200), 0, st(210, 100, 0, 150), true},
		{"no input, reported before start (resume)", true, ms(50), 0, st(210, 100, 0, 150), false},
		{"no input, reported before start, no output", true, ms(50), 0, st(9000, 100, 0, 0), false},
		{"late previous-turn idle after input", true, ms(1200), ms(500), st(1300, 100, 1000, 1250), false},
		{"working then idle after input", true, ms(1300), ms(1100), st(1310, 100, 1000, 1250), true},
		{"input started nothing: quiet for threshold", true, ms(900), ms(500), st(3600, 100, 1000, 1050), true},
		{"input started nothing: not quiet long enough", true, ms(900), ms(500), st(3500, 100, 1000, 1050), false},
	}
	for _, c := range cases {
		if got := legSatisfied(c.nonWorking, c.stateAt, c.workingAt, c.s); got != c.want {
			t.Errorf("%s: legSatisfied = %v, want %v", c.name, got, c.want)
		}
	}
}

// signalFixture is a live interactive session registered under a real task,
// so the lifecycle leg has a store entry to read.
type signalFixture struct {
	h      *TaskHandler
	mux    *SessionMux
	runner *fakeBidiStream
	idHex  string
	tid    protocol.TaskID
	board  *agentboard.Board
}

func newSignalFixture(t *testing.T) *signalFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	board := agentboard.New(agentboard.Config{RingN: 8, MaxTopics: 16, MaxPayload: 4096})
	t.Cleanup(board.Close)
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: NewSessionRegistry(), Board: board}
	idHex := h.Tasks.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}, "")
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, idHex, protocol.TaskKind_Interactive, runner, NewRingBuffer(4096), SessionHooks{})
	t.Cleanup(mux.Stop)
	h.Sessions.Add(idHex, mux)
	return &signalFixture{h: h, mux: mux, runner: runner, idHex: idHex, tid: hexToTaskID(t, idHex), board: board}
}

func (f *signalFixture) output(t *testing.T, s string) {
	t.Helper()
	before := f.mux.LastOutputUnixNano()
	f.runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte(s)))
	waitFor(t, func() bool { return f.mux.LastOutputUnixNano() > before })
}

// input forwards one Stdin frame through a cowriter, as `session send` does.
func (f *signalFixture) input(t *testing.T) {
	t.Helper()
	cw := newFakeStream(t)
	if err := f.mux.AttachCoWriter(context.Background(), cw, 0, false); err != nil {
		t.Fatalf("attach cowriter: %v", err)
	}
	before := f.mux.LastInputUnixNano()
	cw.QueueRead(encodeFrame(frame.FrameType_Stdin, []byte("go\r")))
	waitFor(t, func() bool { return f.mux.LastInputUnixNano() > before })
	time.Sleep(2 * time.Millisecond)
}

func (f *signalFixture) lifecycle(st protocol.LifecycleState) {
	f.h.Tasks.SetLifecycle(f.idHex, st, time.Now().UnixNano(), time.Now().UnixNano())
}

func (f *signalFixture) arm(t *testing.T, conn *fakeConn, sig protocol.AwaitIdleSignal, sink protocol.AwaitSink, topic string) {
	t.Helper()
	tcr := protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}}
	req := protocol.AwaitIdleRequest{TaskId: f.tid, ThresholdMs: 2500, Sink: sink, Signal: sig}
	req.SetTopic([]byte(topic))
	tcr.SetAwaitIdle(req)
	f.h.Handle(conn, tcr.MustAppend(nil))
}

func TestAwaitIdleSignal_ProgramFiresOnDone(t *testing.T) {
	f := newSignalFixture(t)
	f.output(t, "\x1b]7501;state=idle\x07")
	f.input(t)
	conn := &fakeConn{}
	f.arm(t, conn, protocol.AwaitIdleSignal_Program, protocol.AwaitSink_Reply, "")
	time.Sleep(700 * time.Millisecond)
	if n := len(conn.Sent()); n != 0 {
		t.Fatalf("fired on the previous turn's idle (%d messages)", n)
	}
	f.output(t, "\x1b]7501;state=working\x07")
	f.output(t, "\x1b]7501;state=done\x07")
	doneAt := time.Now()
	waitForWithin(t, 2*time.Second, func() bool { return len(conn.Sent()) == 1 })
	if d := time.Since(doneAt); d > 1500*time.Millisecond {
		t.Fatalf("fired %v after done; the program leg should beat 2500ms of quiet", d)
	}
	r := decodeAwaitIdleResponse(t, conn.Sent()[0])
	if r.Status != protocol.AwaitIdleStatus_Fired || r.FiredBy != protocol.AwaitIdleSignal_Program || r.ProgramState != protocol.ProgramState_Done {
		t.Fatalf("response = %+v, want fired by program, done", r)
	}
}

func TestAwaitIdleSignal_LifecycleFires(t *testing.T) {
	f := newSignalFixture(t)
	f.lifecycle(protocol.LifecycleState_Idle)
	f.output(t, "prompt")
	f.input(t)
	conn := &fakeConn{}
	f.arm(t, conn, protocol.AwaitIdleSignal_Lifecycle, protocol.AwaitSink_Reply, "")
	f.output(t, "spinner")
	f.lifecycle(protocol.LifecycleState_Working)
	time.Sleep(600 * time.Millisecond)
	if n := len(conn.Sent()); n != 0 {
		t.Fatalf("fired while working (%d messages)", n)
	}
	f.lifecycle(protocol.LifecycleState_Idle)
	waitForWithin(t, 2*time.Second, func() bool { return len(conn.Sent()) == 1 })
	r := decodeAwaitIdleResponse(t, conn.Sent()[0])
	if r.FiredBy != protocol.AwaitIdleSignal_Lifecycle || r.Lifecycle != protocol.LifecycleState_Idle {
		t.Fatalf("response = %+v, want fired by lifecycle, idle", r)
	}
}

// Review Focus 2: lifecycle stuck at working (Esc while thinking) — any still
// fires through the quiet leg.
func TestAwaitIdleSignal_AnyFallsBackToQuiet(t *testing.T) {
	f := newSignalFixture(t)
	f.input(t)
	f.lifecycle(protocol.LifecycleState_Working)
	f.output(t, "thinking")
	conn := &fakeConn{}
	tcr := protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}}
	tcr.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: f.tid, ThresholdMs: 100, Signal: protocol.AwaitIdleSignal_Any})
	f.h.Handle(conn, tcr.MustAppend(nil))
	waitForWithin(t, 2*time.Second, func() bool { return len(conn.Sent()) == 1 })
	r := decodeAwaitIdleResponse(t, conn.Sent()[0])
	if r.FiredBy != protocol.AwaitIdleSignal_Quiet || r.Lifecycle != protocol.LifecycleState_Working {
		t.Fatalf("response = %+v, want fired by quiet with lifecycle working", r)
	}
}

// Review Focus 1: a source that never reports arms (no refusal) and still ends
// with the session; kill disarms another one.
func TestAwaitIdleSignal_NeverReportingSourceEndsOnStop(t *testing.T) {
	f := newSignalFixture(t)
	f.output(t, "$ ")
	killed := &fakeConn{}
	f.arm(t, killed, protocol.AwaitIdleSignal_Lifecycle, protocol.AwaitSink_Reply, "")
	stopped := &fakeConn{}
	f.arm(t, stopped, protocol.AwaitIdleSignal_Lifecycle, protocol.AwaitSink_Reply, "")
	time.Sleep(700 * time.Millisecond)
	if len(killed.Sent())+len(stopped.Sent()) != 0 {
		t.Fatal("a lifecycle watcher fired on a task that never reported lifecycle")
	}
	ws := f.h.idleWatchers().list(f.idHex)
	if len(ws) != 2 {
		t.Fatalf("armed watchers = %d, want 2", len(ws))
	}
	if r := f.h.handleAwaitIdleKill(killed.ConnectionID().String(), &protocol.AwaitIdleKillRequest{WatcherId: ws[0].id}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("kill = %v", r.Status)
	}
	waitFor(t, func() bool { return len(killed.Sent()) == 1 })
	if r := decodeAwaitIdleResponse(t, killed.Sent()[0]); r.Status != protocol.AwaitIdleStatus_Cancelled {
		t.Fatalf("killed watcher status = %v, want cancelled", r.Status)
	}
	f.mux.Stop()
	waitForWithin(t, 2*time.Second, func() bool { return len(stopped.Sent()) == 1 })
	if r := decodeAwaitIdleResponse(t, stopped.Sent()[0]); r.Status != protocol.AwaitIdleStatus_SessionStopped {
		t.Fatalf("status = %v, want session_stopped", r.Status)
	}
}

// Review Focus 5: when several legs hold at once, any reports the most specific.
func TestAwaitIdleSignal_AnyReportsMostSpecific(t *testing.T) {
	f := newSignalFixture(t)
	f.lifecycle(protocol.LifecycleState_Idle)
	f.output(t, "\x1b]7501;state=done\x07")
	conn := &fakeConn{}
	tcr := protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}}
	tcr.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: f.tid, ThresholdMs: 1, Signal: protocol.AwaitIdleSignal_Any})
	time.Sleep(5 * time.Millisecond)
	f.h.Handle(conn, tcr.MustAppend(nil))
	waitForWithin(t, 2*time.Second, func() bool { return len(conn.Sent()) == 1 })
	if r := decodeAwaitIdleResponse(t, conn.Sent()[0]); r.FiredBy != protocol.AwaitIdleSignal_Program {
		t.Fatalf("fired_by = %v, want program", r.FiredBy)
	}
}

func TestAwaitIdleSignal_BadSignal(t *testing.T) {
	f := newSignalFixture(t)
	conn := &fakeConn{}
	f.arm(t, conn, protocol.AwaitIdleSignal(9), protocol.AwaitSink_Reply, "")
	waitFor(t, func() bool { return len(conn.Sent()) == 1 })
	if r := decodeAwaitIdleResponse(t, conn.Sent()[0]); r.Status != protocol.AwaitIdleStatus_BadRequest {
		t.Fatalf("status = %v, want bad_request", r.Status)
	}
	if n := len(f.h.idleWatchers().list("")); n != 0 {
		t.Fatalf("a bad signal left %d watchers registered", n)
	}
}

func TestAwaitIdleSignal_BoardPayload(t *testing.T) {
	f := newSignalFixture(t)
	f.output(t, "\x1b]7501;state=done\x07")
	conn := &fakeConn{}
	f.arm(t, conn, protocol.AwaitIdleSignal_Any, protocol.AwaitSink_Board, "chat.deadbeef")
	waitForWithin(t, 2*time.Second, func() bool {
		msgs, _ := f.board.ListRetained("chat.deadbeef")
		return len(msgs) == 1
	})
	msgs, _ := f.board.ListRetained("chat.deadbeef")
	payload := string(msgs[0].Payload)
	for _, want := range []string{`"kind":"session_idle"`, `"status":"fired"`, `"last_output_at_unix_ms":`,
		`"signal":"any"`, `"fired_by":"program"`, `"lifecycle":""`, `"program_state":"done"`, `"program_kind":""`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("board payload = %q, missing %s", payload, want)
		}
	}
}

func TestAwaitIdleList_ShowsSignal(t *testing.T) {
	f := newSignalFixture(t)
	conn := &fakeConn{}
	f.arm(t, conn, protocol.AwaitIdleSignal_Lifecycle, protocol.AwaitSink_Board, "chat.deadbeef")
	waitFor(t, func() bool { return len(conn.Sent()) == 1 })
	rows := f.h.visibleIdleWatchers(conn.ConnectionID().String(), protocol.TaskID{})
	if len(rows) != 1 || rows[0].Signal != protocol.AwaitIdleSignal_Lifecycle {
		t.Fatalf("rows = %+v, want one lifecycle row", rows)
	}
}

func TestFireIdleNotifyText(t *testing.T) {
	for _, c := range []struct {
		fire IdleFire
		want string
	}{
		{IdleFire{FiredBy: protocol.AwaitIdleSignal_Program, ProgramState: protocol.ProgramState_Done}, "idle (program done)"},
		{IdleFire{FiredBy: protocol.AwaitIdleSignal_Program, ProgramState: protocol.ProgramState_Blocked, ProgramKind: protocol.ProgramKind_Permission}, "idle (program blocked:permission)"},
		{IdleFire{FiredBy: protocol.AwaitIdleSignal_Lifecycle, Lifecycle: protocol.LifecycleState_Waiting}, "idle (lifecycle waiting)"},
		{IdleFire{FiredBy: protocol.AwaitIdleSignal_Quiet, LastOutput: time.Now().UnixNano()}, "since last output"},
	} {
		if got := idleNotifyText("1a2b3c4d00000000", c.fire); !strings.Contains(got, c.want) || !strings.Contains(got, "1a2b3c4d") {
			t.Errorf("idleNotifyText(%+v) = %q, want it to contain %q", c.fire, got, c.want)
		}
	}
}
