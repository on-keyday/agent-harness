package server

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

func TestSetLifecycle_StampsWorkingAt(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}, "")
	e, _ := s.SetLifecycle(id, protocol.LifecycleState_Working, 1, 100)
	if e.LifecycleWorkingAt != 100 {
		t.Fatalf("after working: LifecycleWorkingAt = %d, want 100", e.LifecycleWorkingAt)
	}
	e, _ = s.SetLifecycle(id, protocol.LifecycleState_Idle, 2, 200)
	if e.LifecycleWorkingAt != 100 || e.LifecycleAt != 200 {
		t.Fatalf("after idle: workingAt=%d at=%d, want 100 and 200", e.LifecycleWorkingAt, e.LifecycleAt)
	}
	if _, st := s.SetLifecycle(id, protocol.LifecycleState_Working, 1, 300); st != protocol.LifecycleSetStatus_Stale {
		t.Fatalf("an older reporter stamp: %v, want stale", st)
	}
	if e, _ := s.Get(id); e.LifecycleWorkingAt != 100 {
		t.Fatalf("a stale working moved LifecycleWorkingAt to %d", e.LifecycleWorkingAt)
	}
}

func newStampMux(t *testing.T) (*SessionMux, *fakeBidiStream, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Interactive, runner, NewRingBuffer(4096), SessionHooks{})
	t.Cleanup(mux.Stop)
	return mux, runner, ctx
}

func TestSessionMux_MuxStartStamped(t *testing.T) {
	before := time.Now().UnixNano()
	mux, _, _ := newStampMux(t)
	if got := mux.MuxStartUnixNano(); got < before || got > time.Now().UnixNano() {
		t.Fatalf("MuxStartUnixNano = %d, want within the constructor call", got)
	}
}

func TestSessionMux_LastInputStdinOnly(t *testing.T) {
	mux, runner, ctx := newStampMux(t)

	control := newFakeStream(t)
	if err := mux.Attach(ctx, control); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// Non-input frames first: none of them may move the stamp.
	control.QueueRead(makeWinSizeFrame(40, 150))
	control.QueueRead(encodeFrame(frame.FrameType_Control, []byte{0x7f}))
	marker := encodeFrame(frame.FrameType_Control, []byte{0x7e})
	control.QueueRead(marker)
	waitFor(t, func() bool { return bytes.Contains(runner.Written(), marker) })
	if got := mux.LastInputUnixNano(); got != 0 {
		t.Fatalf("resize/control frames stamped lastInput = %d", got)
	}

	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer, 0, false); err != nil {
		t.Fatalf("attach viewer: %v", err)
	}
	viewer.QueueRead(encodeFrame(frame.FrameType_Stdin, []byte("v")))
	time.Sleep(100 * time.Millisecond)
	if got := mux.LastInputUnixNano(); got != 0 {
		t.Fatalf("a viewer's discarded Stdin stamped lastInput = %d", got)
	}

	control.QueueRead(encodeFrame(frame.FrameType_Stdin, []byte("a")))
	waitFor(t, func() bool { return mux.LastInputUnixNano() != 0 })
	first := mux.LastInputUnixNano()

	cw := newFakeStream(t)
	if err := mux.AttachCoWriter(ctx, cw, 0, false); err != nil {
		t.Fatalf("attach cowriter: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	cw.QueueRead(encodeFrame(frame.FrameType_Stdin, []byte("b")))
	waitFor(t, func() bool { return mux.LastInputUnixNano() > first })
}

func TestSessionMux_ProbeReplyIsNotInput(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("\x1b]7501;?\x1b\\")))
	waitFor(t, func() bool { return bytes.Contains(runner.Written(), []byte("7501;?:states")) })
	if got := mux.LastInputUnixNano(); got != 0 {
		t.Fatalf("the server's own probe reply stamped lastInput = %d", got)
	}
}

func TestSessionMux_ProgramStamps(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("\x1b]7501;state=working\x07")))
	waitFor(t, func() bool { at, _ := mux.ProgramStamps(); return at != 0 })
	at1, w1 := mux.ProgramStamps()
	if w1 != at1 {
		t.Fatalf("a change to working: at=%d workingAt=%d, want equal", at1, w1)
	}
	time.Sleep(2 * time.Millisecond)
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("\x1b]7501;state=done\x07")))
	waitFor(t, func() bool { at, _ := mux.ProgramStamps(); return at > at1 })
	at2, w2 := mux.ProgramStamps()
	if w2 != w1 {
		t.Fatalf("done moved programWorkingAt %d -> %d", w1, w2)
	}
	time.Sleep(2 * time.Millisecond)
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("\x1b]7501;state=done\x07x")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() > at2 })
	if at3, _ := mux.ProgramStamps(); at3 != at2 {
		t.Fatalf("a repeated done moved programAt %d -> %d", at2, at3)
	}
}

func TestSessionMux_LoadScreenStampsProgramAt(t *testing.T) {
	mux, _, _ := newStampMux(t)
	before := time.Now().UnixNano()
	mux.loadScreen([]byte("\x1b]7501;state=done\x07"))
	at, workingAt := mux.ProgramStamps()
	if at < before || at > time.Now().UnixNano() {
		t.Fatalf("restored programAt = %d, want the restore time", at)
	}
	if workingAt != 0 {
		t.Fatalf("restored done set programWorkingAt = %d", workingAt)
	}
}
