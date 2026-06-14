package server

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestSessionMux_AttachAfterAltScreenExitSkipsEpisode is the regression for the
// view/reattach corruption: a full-screen app (htop) ran and exited, but its
// alt-screen episode still sits in the ring as absolute-cursor frame fragments.
// Replaying it verbatim paints garbage onto the primary screen. After the app
// has left the alt screen, reattach must replay only from the ESC[?1049l exit
// onward, dropping the dead episode.
func TestSessionMux_AttachAfterAltScreenExitSkipsEpisode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	// Ring large enough that nothing is evicted: we are testing the trim, not
	// eviction.
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<16), SessionHooks{})

	enter := makeWireFrame(1, []byte("\x1b[?1049hHTOP-EPISODE-CONTENT"))
	mid := makeWireFrame(1, []byte("MORE-HTOP-FRAME-FRAGMENTS"))
	exit := makeWireFrame(1, []byte("\x1b[?1049l[prompt]$ "))

	total := 0
	for _, f := range [][]byte{enter, mid, exit} {
		runner.QueueRead(f)
		total += len(f)
	}
	waitFor(t, func() bool { return mux.RingBufferLen() == total })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Session is on the primary screen (last 1049 was reset). modeTracker
	// excludes alt-screen from the preamble, so replay is just the exit frame.
	got := tui.WaitWritten(t, len(exit))
	if bytes.Contains(got, []byte("HTOP-EPISODE-CONTENT")) || bytes.Contains(got, []byte("MORE-HTOP-FRAME-FRAGMENTS")) {
		t.Fatalf("replay leaked the dead alt-screen episode:\n got=%q", got)
	}
	if !bytes.Equal(got, exit) {
		t.Fatalf("replay\n got=%q\nwant=%q (exit frame only)", got, exit)
	}
}

// TestSessionMux_AttachWhileAltScreenLiveReplaysFull guards the other side: a
// full-screen app is still LIVE (in the alt screen) at attach time. Here we must
// NOT trim — the app repaints over any partial frame, and trimming would hide
// its in-progress output. Full ring replay.
func TestSessionMux_AttachWhileAltScreenLiveReplaysFull(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<16), SessionHooks{})

	pre := makeWireFrame(1, []byte("pre-htop scrollback "))
	enter := makeWireFrame(1, []byte("\x1b[?1049hLIVE-HTOP-FRAME"))
	total := len(pre) + len(enter)
	runner.QueueRead(pre)
	runner.QueueRead(enter)
	waitFor(t, func() bool { return mux.RingBufferLen() == total })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Live alt-screen: the preamble re-enters the alt screen (ESC[?1049h) as its
	// own stdout frame, then the full ring snapshot replays — no trimming while
	// the app is live.
	want := append([]byte{}, makeWireFrame(1, []byte("\x1b[?1049h"))...)
	want = append(want, pre...)
	want = append(want, enter...)
	got := tui.WaitWritten(t, len(want))
	if !bytes.Equal(got, want) {
		t.Fatalf("live alt-screen replay\n got=%q\nwant=%q (ESC[?1049h + full ring)", got, want)
	}
}

// TestSessionMux_AttachLiveAltScreenReentersEvenIfEnterEvicted is the live-app
// counterpart to the trim test. A full-screen app is still in the alt screen,
// but its establishing ESC[?1049h has been evicted from the ring window. Replay
// must re-enter the alt screen via the preamble, so the surviving frame
// fragment lands on the alt buffer instead of corrupting the primary screen.
func TestSessionMux_AttachLiveAltScreenReentersEvenIfEnterEvicted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	enter := makeWireFrame(1, []byte("\x1b[?1049hENTER"))        // establishing alt-enter
	bulk := makeWireFrame(1, []byte("\x1b[10;5HFRAGMENTxxxxxx")) // mid-frame fragment, no 1049
	// Ring holds exactly one of these frames → the alt-enter is evicted.
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(len(bulk)), SessionHooks{})

	runner.QueueRead(enter)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(enter) })
	runner.QueueRead(bulk)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(bulk) })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	got := tui.WaitWritten(t, len(bulk))
	if !bytes.Contains(got, []byte("\x1b[?1049h")) {
		t.Fatalf("live alt-screen reattach must re-enter the alt screen even when\nthe enter sequence was evicted; got=%q", got)
	}
	// And the surviving fragment is still replayed (after the re-entry).
	if !bytes.Contains(got, []byte("FRAGMENTxxxxxx")) {
		t.Fatalf("surviving fragment missing from replay; got=%q", got)
	}
}

func TestRingBuffer_SnapshotFrom(t *testing.T) {
	r := NewRingBuffer(1 << 16)
	r.Append([]byte("AAA")) // index 0
	r.Append([]byte("BBB")) // index 1
	r.Append([]byte("CCC")) // index 2

	if got := r.SnapshotFrom(0); !bytes.Equal(got, []byte("AAABBBCCC")) {
		t.Fatalf("SnapshotFrom(0) = %q, want full", got)
	}
	if got := r.SnapshotFrom(2); !bytes.Equal(got, []byte("CCC")) {
		t.Fatalf("SnapshotFrom(2) = %q, want CCC", got)
	}
	if got := r.SnapshotFrom(3); got != nil {
		t.Fatalf("SnapshotFrom(3) = %q, want nil (beyond newest)", got)
	}
	if r.AppendCount() != 3 {
		t.Fatalf("AppendCount = %d, want 3", r.AppendCount())
	}

	// Force eviction: a tiny ring keeps only the last frame. A mark pointing at
	// an evicted frame degrades to "from the oldest surviving frame".
	small := NewRingBuffer(4)
	small.Append([]byte("AAA")) // index 0, evicted next
	small.Append([]byte("BBB")) // index 1, survives
	if got := small.SnapshotFrom(0); !bytes.Equal(got, []byte("BBB")) {
		t.Fatalf("SnapshotFrom(0) after eviction = %q, want BBB", got)
	}
}
