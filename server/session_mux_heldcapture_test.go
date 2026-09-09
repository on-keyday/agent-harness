package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/vtgrid"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// A capture round-trips: what one mux hands over is what the next one comes back
// holding. Both halves are asserted, because a session is its screen AND the
// geometry that screen was painted at.
//
// The reason this is a test and not a comment: the first version of re-adoption
// injected the captured repaint into the RING, and the ring is not what an
// attach paints from last. attachObserver ends every replay with a repaint built
// from the screen MODEL, the model never saw the injected frame (recordFrame
// feeds it only for Stdout/Stderr), so a blank repaint went out after the
// restored content and erased it. Measured live: 829 replayed bytes, 460 of them
// the screen and 369 the blank repaint that wiped it, and a re-adopted session
// showing nothing but its window title. Asserting through screenRepaint is
// asserting on the thing that actually reaches a client.
func TestHeldCaptureRoundTripsScreenAndSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	before := newFakeStream(t)
	old := NewSessionMux(ctx, "task", before, NewRingBuffer(1<<20), SessionHooks{})
	if err := old.applyWinSizeFrame(makeWinSizeFrame(30, 100)); err != nil {
		t.Fatalf("resize: %v", err)
	}
	before.QueueRead(makeWireFrame(1, []byte("\x1b[2J\x1b[1;1Hheld-content-here")))
	waitFor(t, func() bool { return strings.Contains(string(old.screenRepaint()), "held-content-here") })

	capture := old.heldCapture()
	if len(capture) == 0 {
		t.Fatal("heldCapture produced nothing")
	}

	// The next server: a fresh mux on a fresh stream, seeded from the capture
	// alone. Nothing else carries session state across a restart.
	after := newFakeStream(t)
	fresh := NewSessionMux(ctx, "task", after, NewRingBuffer(1<<20), SessionHooks{})
	gotSize, gotScreen, err := fresh.loadHeldCapture(capture)
	if err != nil {
		t.Fatalf("loadHeldCapture: %v", err)
	}
	if !gotSize || !gotScreen {
		t.Fatalf("capture reported size=%v screen=%v, want both", gotSize, gotScreen)
	}

	if got := string(fresh.screenRepaint()); !strings.Contains(got, "held-content-here") {
		t.Errorf("the restored screen is blank; repaint = %q", got)
	}
	if cols, rows := fresh.screenSize(); cols != 100 || rows != 30 {
		t.Errorf("restored grid is %dx%d, want 100x30 — a repaint lands on the wrong geometry", cols, rows)
	}
	// Without this an attach replays no size at all and the client renders at
	// its own fallback, which is the "reported no terminal size" a re-adopted
	// session used to answer with.
	if len(fresh.lastWinSizeBytes()) == 0 {
		t.Error("the mux has no size to replay to an attaching client")
	}
	// Applying the size frame is ALSO what makes the runner stream findable for
	// the peer: the runner's side of a rebind is a lookup that waits, and a
	// trsf stream nothing has crossed does not exist yet. Over UDP that was the
	// difference between a rebind and a permanent "stream lookup failed".
	if w := after.WaitWritten(t, 1); !bytes.Contains(w, makeWinSizeFrame(30, 100)) {
		t.Errorf("the size frame never reached the runner stream; written = %q", w)
	}
}

// The capture is not history and must not be replayed as though the runner had
// sent it. A client that asks for the PTY-only view (`snapshot --raw
// --without-synth`) is asking what the process produced, and bytes a previous
// server's grid emitted are not that.
func TestHeldCaptureDoesNotEnterTheRing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})
	capture := append(makeWinSizeFrame(24, 80), makeWireFrame(4, []byte("\x1b[1;1Hrestored"))...)
	if _, _, err := mux.loadHeldCapture(capture); err != nil {
		t.Fatalf("loadHeldCapture: %v", err)
	}

	if mux.RingBufferLen() != 0 {
		t.Errorf("the ring holds %d bytes of a capture this server never received",
			mux.RingBufferLen())
	}
	// And it is on the screen, so this is about WHERE it went, not whether it
	// arrived.
	if got := string(mux.screenRepaint()); !strings.Contains(got, "restored") {
		t.Errorf("capture reached neither the ring nor the screen; repaint = %q", got)
	}
	// A restored screen is not the agent writing: the busy/idle badge reads this
	// timestamp, and drainHeldSessions waits on its quiescence before the NEXT
	// hold captures anything.
	if lo := mux.LastOutputUnixNano(); lo != 0 {
		t.Errorf("the capture stamped output activity (%d); no agent wrote anything", lo)
	}
}

// renderReplay feeds a replay burst through a VT emulator the way a client
// does, and returns what would be ON the screen.
//
// RENDERED, not searched. A byte-level assertion is the trap this bug sat in:
// the broken version delivered the restored content and then appended a repaint
// built from an empty model, so the marker was present in the bytes and absent
// from the screen. Only an emulator can tell those apart, and the emulator is
// what the operator is looking at.
func renderReplay(t *testing.T, replay []byte, cols, rows int) string {
	t.Helper()
	term := vtgrid.New(cols, rows)
	for off := 0; off+frameHeaderSize <= len(replay); {
		n := int(binary.BigEndian.Uint32(replay[off+1 : off+5]))
		end := off + frameHeaderSize + n
		if end > len(replay) {
			break
		}
		switch frame.FrameType(replay[off]) {
		case frame.FrameType_Stdout, frame.FrameType_Stderr, frame.FrameType_Synth:
			_, _ = term.Write(replay[off+frameHeaderSize : end])
		}
		off = end
	}
	return string(term.Repaint())
}

// An attaching client SEES the restored screen. This is the level the reported
// symptom lived at — a re-adopted session that came back black — so the guard
// is at that level too.
func TestAttachAfterCaptureShowsTheRestoredScreen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})
	capture := append(makeWinSizeFrame(24, 80), makeWireFrame(4, []byte("\x1b[1;1Hvisible-after-attach"))...)
	if _, _, err := mux.loadHeldCapture(capture); err != nil {
		t.Fatalf("loadHeldCapture: %v", err)
	}

	// A control attach and an observer attach assemble their replays
	// separately, and the closing repaint is in BOTH — so a regression in
	// either one is a black screen for whoever uses that path (`r` in the TUI;
	// `session snapshot` and every grid pane).
	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer, 0, false); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}

	for _, tc := range []struct {
		name   string
		stream *fakeBidiStream
	}{{"control attach", tui}, {"viewer attach", viewer}} {
		deadline := time.Now().Add(time.Second)
		for {
			if strings.Contains(renderReplay(t, tc.stream.Written(), 80, 24), "visible-after-attach") {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("%s: the restored screen is not on the client's screen; replay rendered as %q",
					tc.name, renderReplay(t, tc.stream.Written(), 80, 24))
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
