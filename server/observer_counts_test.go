package server

import (
	"context"
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
