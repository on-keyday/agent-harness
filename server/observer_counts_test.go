package server

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/exec/frame"
)

// The whole point of splitting the two: they differ in what the operator can DO
// through them, so one total would answer the wrong question.
func TestObserverCountsSplitsViewersFromCowriters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	for i := 0; i < 2; i++ {
		if err := mux.AttachViewer(ctx, newFakeStream(t), 0, false); err != nil {
			t.Fatalf("AttachViewer: %v", err)
		}
	}
	if err := mux.AttachCoWriter(ctx, newFakeStream(t), 0, false); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}

	v, cw := mux.ObserverCounts()
	if v != 2 || cw != 1 {
		t.Errorf("ObserverCounts() = (%d viewers, %d cowriters), want (2, 1)", v, cw)
	}
	// ViewerCount is viewers ONLY — it delegates, so it can never disagree.
	if got := mux.ViewerCount(); got != 2 {
		t.Errorf("ViewerCount() = %d, want 2 (cowriters must not be counted)", got)
	}
}

// The control attach is not an observer. Conflating them would make a plain
// reattach look like someone was spectating.
func TestControlAttachIsNotCountedAsObserver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	if err := mux.Attach(ctx, newFakeStream(t)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	v, cw := mux.ObserverCounts()
	if v != 0 || cw != 0 {
		t.Errorf("a control attach counted as an observer: (%d, %d), want (0, 0)", v, cw)
	}
	if !mux.IsAttached() {
		t.Error("IsAttached() false after a control attach")
	}
}

// This is the case that motivated the feature: watching through the WebUI
// preview is a viewer attach, which does NOT move the task out of Detached. A
// reader who takes Detached to mean "nobody is here" is wrong, and the counts
// are the only thing that says so.
func TestViewerDoesNotOccupyControlSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	if err := mux.AttachViewer(ctx, newFakeStream(t), 0, false); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	if mux.IsAttached() {
		t.Fatal("a viewer took the control slot")
	}
	if v, _ := mux.ObserverCounts(); v != 1 {
		t.Errorf("viewers = %d, want 1 — the session is being watched with no control attached", v)
	}
}

// A dropped observer must leave the count, or the display accumulates ghosts
// that never go away for the life of the session.
func TestObserverCountsDropWithTheStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	if err := mux.AttachCoWriter(ctx, newFakeStream(t), 0, false); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}
	if _, cw := mux.ObserverCounts(); cw != 1 {
		t.Fatalf("cowriters = %d, want 1", cw)
	}
	mux.Stop()
	waitFor(t, func() bool {
		v, cw := mux.ObserverCounts()
		return v == 0 && cw == 0
	})
}

// An observer attaching or leaving moves NO task status by design, so the hook
// is the only signal an event-driven client gets. Verified live before it
// existed: `ls` reported a viewer while the TUI showed none, and the TUI
// corrected itself only when an unrelated task event happened to arrive.
func TestObserverHookFiresOnAttachAndDetach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)

	var mu sync.Mutex
	var fired []string
	mux := NewSessionMux(ctx, "task-obs", runner, NewRingBuffer(256), SessionHooks{
		OnObservers: func(id string) {
			mu.Lock()
			fired = append(fired, id)
			mu.Unlock()
		},
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(fired)
	}

	v := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v, 0, false); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	waitFor(t, func() bool { return count() >= 1 })

	if err := mux.AttachCoWriter(ctx, newFakeStream(t), 0, false); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}
	waitFor(t, func() bool { return count() >= 2 })

	// Leaving fires too, or a count that went up never comes down for a
	// client that refreshes only on events.
	mux.dropViewer(mux.anyViewerForTest())
	waitFor(t, func() bool { return count() >= 3 })

	mu.Lock()
	defer mu.Unlock()
	for _, id := range fired {
		if id != "task-obs" {
			t.Errorf("hook fired with task id %q, want task-obs", id)
		}
	}
}

// The hook must not be dispatched while the mux lock is held: dropViewerLocked
// runs under m.mu, and a hook that publishes (the server's does) would invert
// the lock order against runnerPump's fan-out. Firing into a callback that
// takes the mux lock deadlocks if this regresses.
func TestObserverHookIsNotCalledUnderTheMuxLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)

	done := make(chan struct{}, 4)
	var mux *SessionMux
	mux = NewSessionMux(ctx, "task-lock", runner, NewRingBuffer(256), SessionHooks{
		OnObservers: func(string) {
			// Re-entering the mux is exactly what a publishing hook does
			// (the server reads ObserverCounts to fill the event).
			mux.ObserverCounts()
			done <- struct{}{}
		},
	})

	if err := mux.AttachViewer(ctx, newFakeStream(t), 0, false); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the attach hook never completed — it is being called under the mux lock")
	}

	// dropViewer is the under-lock path: it takes m.mu and calls
	// dropViewerLocked. Stop() is deliberately NOT used here — it clears the
	// viewer map directly and fires no hook, because a stopping session is
	// followed by a task status event that carries the (now zero) counts.
	mux.dropViewer(mux.anyViewerForTest())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the drop hook never completed — dropViewerLocked is calling it under the lock")
	}
}

// --- exec_resize: orthogonal to the mode, deferential to the control seat ---

// Without exec_resize an observer's size frames are discarded, exactly as every
// observer's were before the capability existed — the default is unchanged.
func TestObserverResizeIgnoredWithoutTheCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "t", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()

	s := newFakeStream(t)
	if err := mux.AttachCoWriter(ctx, s, 0, false); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}
	s.QueueRead(makeWinSizeFrame(40, 150))
	// A plain keystroke after it, which a cowriter IS allowed to send: if the
	// runner sees that and not the resize, the filter is per-frame and correct.
	s.QueueRead(makeWireFrame(byte(frame.FrameType_Stdin), []byte("x")))
	waitFor(t, func() bool { return len(runner.Written()) > 0 })
	if got := runner.Written(); bytes.Contains(got, makeWinSizeFrame(40, 150)) {
		t.Error("a cowriter without exec_resize had its resize forwarded")
	}
}

// With exec_resize and an EMPTY control seat, the resize lands: it reaches the
// runner and is remembered as the size a later observer replays.
func TestObserverResizeAppliedWhenNoControlAttached(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "t", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()

	s := newFakeStream(t)
	if err := mux.AttachViewer(ctx, s, 0, true); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	want := makeWinSizeFrame(40, 150)
	s.QueueRead(want)
	waitFor(t, func() bool { return bytes.Contains(runner.Written(), want) })

	// A viewer with exec_resize but no cowrite must still not be able to TYPE —
	// the two axes are independent, and this is the one that could be conflated.
	s.QueueRead(makeWireFrame(byte(frame.FrameType_Stdin), []byte("rm -rf /")))
	time.Sleep(150 * time.Millisecond)
	if bytes.Contains(runner.Written(), []byte("rm -rf /")) {
		t.Error("exec_resize let a VIEWER type into the session")
	}
}

// The seat rule: while a control client holds it, the size is the control
// client's and an observer resize is ignored however it is granted.
func TestObserverResizeIgnoredWhileControlAttached(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "t", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()

	if err := mux.Attach(ctx, newFakeStream(t)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	s := newFakeStream(t)
	if err := mux.AttachCoWriter(ctx, s, 0, true); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}
	blocked := makeWinSizeFrame(40, 150)
	s.QueueRead(blocked)
	// Followed by a keystroke the cowriter IS allowed to send, so waiting on it
	// proves the resize was seen and dropped rather than merely not yet read.
	s.QueueRead(makeWireFrame(byte(frame.FrameType_Stdin), []byte("ok")))
	waitFor(t, func() bool { return bytes.Contains(runner.Written(), []byte("ok")) })
	if bytes.Contains(runner.Written(), blocked) {
		t.Error("an observer resized the session out from under the attached control client")
	}
}
