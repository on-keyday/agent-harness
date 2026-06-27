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
