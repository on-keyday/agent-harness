package server

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The whole point of splitting the two: they differ in what the operator can DO
// through them, so one total would answer the wrong question.
func TestObserverCountsSplitsViewersFromCowriters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	for i := 0; i < 2; i++ {
		if err := mux.AttachViewer(ctx, newFakeStream(t), 0); err != nil {
			t.Fatalf("AttachViewer: %v", err)
		}
	}
	if err := mux.AttachCoWriter(ctx, newFakeStream(t), 0); err != nil {
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

	if err := mux.AttachViewer(ctx, newFakeStream(t), 0); err != nil {
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

	if err := mux.AttachCoWriter(ctx, newFakeStream(t), 0); err != nil {
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
	if err := mux.AttachViewer(ctx, v, 0); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	waitFor(t, func() bool { return count() >= 1 })

	if err := mux.AttachCoWriter(ctx, newFakeStream(t), 0); err != nil {
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

	if err := mux.AttachViewer(ctx, newFakeStream(t), 0); err != nil {
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
