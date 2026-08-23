package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The grid is fed from the same frames the ring gets, and it exists before
// anyone has attached. That is the point rather than an implementation detail:
// its whole value is having seen output the ring will have evicted by the time
// a client arrives, and one created at attach time could only be seeded from
// the ring, which is the ring with extra steps.
func TestSessionMuxScreenIsFedFromRunnerPump(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	runner.QueueRead(makeWireFrame(1, []byte("\x1b[2J\x1b[1;1Hhello")))
	waitFor(t, func() bool { return strings.Contains(string(mux.screenRepaint()), "hello") })
}

// A ring too small to hold the frame that drew the screen still leaves the grid
// holding it. This is the case the whole design exists for, so it is asserted
// rather than assumed: the ring forgets, the grid does not.
func TestSessionMuxScreenSurvivesRingEviction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(16), SessionHooks{})

	drew := makeWireFrame(1, []byte("\x1b[2J\x1b[1;1Hgone-from-ring"))
	runner.QueueRead(drew)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(drew) })

	// Push it out of the ring entirely.
	bulk := makeWireFrame(1, []byte("\x1b[20;1Hlater"))
	runner.QueueRead(bulk)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(bulk) })

	if got := string(mux.replaySnapshot()); strings.Contains(got, "gone-from-ring") {
		t.Fatalf("precondition broken: the ring still holds the frame (%q)", got)
	}
	if got := string(mux.screenRepaint()); !strings.Contains(got, "gone-from-ring") {
		t.Errorf("the screen lost what the ring evicted; repaint = %q", got)
	}
}

// The window title is the ring-eviction case in its most ordinary form: an app
// titles itself once at startup and then runs for an hour, so by the time
// anybody attaches the OSC that set it is long gone from the ring and the title
// exists only in the grid. The measured symptom was `session snapshot`
// reporting an empty title for a session that plainly had one.
//
// Asserted through screenRepaint rather than through the grid's Title()
// accessor, because holding the title and HANDING IT OVER are separate
// properties and only the second one is what a snapshot or a reattach reads.
func TestSessionMuxScreenRepaintCarriesTitleTheRingEvicted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(16), SessionHooks{})

	titled := makeWireFrame(1, []byte("\x1b]0;a session title\x07"))
	runner.QueueRead(titled)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(titled) })

	// Push the OSC out of the ring entirely, the way an hour of output would.
	bulk := makeWireFrame(1, []byte("\x1b[20;1Hlater"))
	runner.QueueRead(bulk)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(bulk) })

	if got := string(mux.replaySnapshot()); strings.Contains(got, "a session title") {
		t.Fatalf("precondition broken: the ring still holds the title OSC (%q)", got)
	}
	if got := string(mux.screenRepaint()); !strings.Contains(got, "\x1b]0;a session title\x07") {
		t.Errorf("the repaint dropped the title the ring evicted; repaint = %q", got)
	}
}

// Both resize entry points reach the grid, because both funnel through
// applyWinSizeFrame. The observer path is the one easy to miss: exec_resize
// lets an observer stand in for the size whenever the control seat is empty,
// which is the unattended-worker case that capability exists for.
func TestSessionMuxScreenResizesFromBothEntryPoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	if cols, rows := mux.screenSize(); cols != 80 || rows != 24 {
		t.Fatalf("a session with no size yet renders at %dx%d, want the 80x24 default", cols, rows)
	}

	// Control path.
	if err := mux.applyWinSizeFrame(makeWinSizeFrame(30, 100)); err != nil {
		t.Fatalf("control resize: %v", err)
	}
	if cols, rows := mux.screenSize(); cols != 100 || rows != 30 {
		t.Errorf("after the control resize the screen is %dx%d, want 100x30", cols, rows)
	}

	// Observer path, control seat empty.
	if err := mux.applyObserverWinSize(makeWinSizeFrame(40, 150)); err != nil {
		t.Fatalf("observer resize: %v", err)
	}
	if cols, rows := mux.screenSize(); cols != 150 || rows != 40 {
		t.Errorf("after the observer resize the screen is %dx%d, want 150x40", cols, rows)
	}
}

// An observer's resize is ignored while a control client holds the seat, and
// the grid must agree with that rather than tracking a size the PTY never got.
func TestSessionMuxScreenIgnoresObserverResizeWhileControlHoldsTheSeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	if err := mux.applyWinSizeFrame(makeWinSizeFrame(30, 100)); err != nil {
		t.Fatalf("control resize: %v", err)
	}
	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := mux.applyObserverWinSize(makeWinSizeFrame(40, 150)); err != nil {
		t.Fatalf("observer resize: %v", err)
	}
	if cols, rows := mux.screenSize(); cols != 100 || rows != 30 {
		t.Errorf("the screen followed an observer resize the PTY never got: %dx%d, want 100x30", cols, rows)
	}
}
