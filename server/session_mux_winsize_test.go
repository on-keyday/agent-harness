package server

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/exec/frame"
)

// makeWinSizeFrame builds a wire-encoded TerminalWindowSize control frame,
// mirroring exec.CommandExecutionStream.SetTerminalWindowSize.
func makeWinSizeFrame(rows, cols uint16) []byte {
	ctrl := frame.Control{Type: frame.ControlType_TerminalWindowSize}
	ctrl.SetTerminalWindowSize(frame.TerminalWindowSize{Rows: rows, Columns: cols})
	enc := ctrl.MustAppend(nil)
	hdr := frame.FrameHeader{Type: frame.FrameType_Control, Len: uint32(len(enc))}
	return append(hdr.MustAppend(nil), enc...)
}

// A read-only viewer never sends its own size (viewerInputDrain discards input),
// so the mux must replay the controlling client's last TerminalWindowSize ahead
// of the ring — otherwise a snapshot renderer cannot size its grid to match the
// size the absolute-positioned output was painted at.
func TestSessionMux_AttachViewer_ReplaysWindowSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	// Controlling client attaches and sends its terminal size; tuiPump forwards
	// it to the runner and records it as lastWinSize.
	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	winFrame := makeWinSizeFrame(43, 167)
	tui.QueueRead(winFrame)
	waitFor(t, func() bool {
		mux.mu.Lock()
		defer mux.mu.Unlock()
		return bytes.Equal(mux.lastWinSize, winFrame)
	})

	// A new viewer must receive the size frame as the first bytes of its replay.
	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	got := viewer.WaitWritten(t, len(winFrame))
	if !bytes.Equal(got[:len(winFrame)], winFrame) {
		t.Fatalf("viewer replay must START with the window-size frame\n got %q\nwant %q",
			got[:len(winFrame)], winFrame)
	}
}

// A viewer attaching before any size was seen must not get a spurious leading
// frame (lastWinSize empty → nothing prepended).
func TestSessionMux_AttachViewer_NoSizeWhenNoneSeen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	pre := makeWireFrame(byte(frame.FrameType_Stdout), []byte("hello"))
	runner.QueueRead(pre)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(pre) })

	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	// Replay should be exactly the ring content, no leading size frame.
	if got := viewer.WaitWritten(t, len(pre)); !bytes.Equal(got, pre) {
		t.Fatalf("replay got %q want %q (no size frame expected)", got, pre)
	}
}

// A cowriter forwards its input frames to the runner but its resize (size
// authority belongs to the control client) is dropped.
func TestSessionMux_CoWriter_ForwardsInputDropsResize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	cw := newFakeStream(t)
	if err := mux.AttachCoWriter(ctx, cw); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}

	stdin1 := makeWireFrame(byte(frame.FrameType_Stdin), []byte("alpha"))
	resize := makeWinSizeFrame(99, 99)
	stdin2 := makeWireFrame(byte(frame.FrameType_Stdin), []byte("omega"))
	cw.QueueRead(stdin1)
	cw.QueueRead(resize)
	cw.QueueRead(stdin2)

	// Once stdin2 (sent AFTER resize) reached the runner, the resize — had it
	// been forwarded — would already be present too. So absence is reliable.
	waitFor(t, func() bool { return bytes.Contains(runner.Written(), stdin2) })
	w := runner.Written()
	if !bytes.Contains(w, stdin1) {
		t.Fatal("cowriter stdin1 was not forwarded to the runner")
	}
	if bytes.Contains(w, resize) {
		t.Fatal("cowriter resize must be DROPPED, not forwarded (no size authority)")
	}
}

// A cowriter joins without taking the writer slot and without claiming size
// authority (its resize must not become m.lastWinSize).
func TestSessionMux_CoWriter_NoTakeoverNoSizeAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	cw := newFakeStream(t)
	if err := mux.AttachCoWriter(ctx, cw); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}
	if !mux.IsAttached() {
		t.Fatal("control tui must remain attached after a cowriter joins (no takeover)")
	}

	cw.QueueRead(makeWinSizeFrame(99, 99)) // cowriter resize — must be ignored
	barrier := makeWireFrame(byte(frame.FrameType_Stdin), []byte("x"))
	cw.QueueRead(barrier)
	waitFor(t, func() bool { return bytes.Contains(runner.Written(), barrier) })

	mux.mu.Lock()
	gotSize := len(mux.lastWinSize)
	mux.mu.Unlock()
	if gotSize != 0 {
		t.Fatalf("cowriter resize must NOT set lastWinSize (got %d bytes)", gotSize)
	}
}
