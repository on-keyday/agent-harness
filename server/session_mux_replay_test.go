package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

type decodedFrame struct {
	Type frame.FrameType
	Data []byte
}

// decodeFrames reads a replay block back as the frames it is, which is the only
// way to assert what the client can tell apart. Reading it as bytes would see
// exactly what a client that ignores the type sees, and that is the thing under
// test.
func decodeFrames(t *testing.T, b []byte) []decodedFrame {
	t.Helper()
	var out []decodedFrame
	r := bytes.NewReader(b)
	for r.Len() > 0 {
		f := &frame.Frame{}
		if err := f.Read(r); err != nil {
			t.Fatalf("decode frame %d: %v", len(out), err)
		}
		var data []byte
		if d := f.Data(); d != nil {
			data = *d
		}
		out = append(out, decodedFrame{Type: f.Header.Type, Data: data})
	}
	return out
}

// The replay's shape is load-bearing and each part has a different job: the
// modes the grid does not model, then the history, then the screen. The two
// server-synthesised parts carry a frame type saying so, because
// `session snapshot --raw` exists to show the bytes the PTY actually emitted
// and cannot do that if invented bytes wear the same label.
func TestAttachReplayEndsWithASynthFramedRepaint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Interactive, runner, NewRingBuffer(1<<20), SessionHooks{})

	drew := makeWireFrame(1, []byte("\x1b[?25l\x1b[2J\x1b[1;1Hhello"))
	runner.QueueRead(drew)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(drew) })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// preamble (synth) + ring (stdout) + repaint (synth)
	frames := decodeFrames(t, tui.WaitWritten(t, len(drew)+1))
	if len(frames) < 3 {
		t.Fatalf("replay has %d frames, want at least preamble + ring + repaint: %+v", len(frames), frames)
	}

	last := frames[len(frames)-1]
	if last.Type != frame.FrameType_Synth {
		t.Errorf("last replay frame is %v, want Synth (the repaint)", last.Type)
	}
	if !strings.Contains(string(last.Data), "hello") {
		t.Errorf("the repaint does not carry the screen: %q", last.Data)
	}

	for i, f := range frames {
		if f.Type == frame.FrameType_Stdout && bytes.Contains(f.Data, []byte("\x1b[?25")) &&
			!bytes.Contains(f.Data, []byte("hello")) {
			t.Errorf("frame %d is a mode preamble still riding as Stdout; it is synthesised "+
				"and must say so: %q", i, f.Data)
		}
	}
}

// An observer gets the same treatment, and its replayLimit does not apply to
// the repaint: the limit bounds HISTORY, and the screen is not history. A
// monitoring pane that asks for no history at all still gets a correct screen,
// which is the whole change in what that knob means.
func TestObserverReplayCapsHistoryButNotTheScreen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Interactive, runner, NewRingBuffer(1<<20), SessionHooks{})

	drew := makeWireFrame(1, []byte("\x1b[2J\x1b[1;1Hscreen-content"))
	runner.QueueRead(drew)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(drew) })

	obs := newFakeStream(t)
	if err := mux.AttachViewer(ctx, obs, 1, false); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}

	var frames []decodedFrame
	waitFor(t, func() bool {
		frames = decodeFrames(t, obs.Written())
		return len(frames) > 0 && frames[len(frames)-1].Type == frame.FrameType_Synth
	})
	last := frames[len(frames)-1]
	if !strings.Contains(string(last.Data), "screen-content") {
		t.Errorf("a capped observer lost the screen; repaint = %q", last.Data)
	}
}

// An event-stream session has no terminal, so an attach must carry NOTHING the
// server synthesised — only the NDJSON the runner sent. A repaint here was a
// terminal program with no trailing newline: it arrived glued to the stream's
// next line, the chat showed that line as "(not the protocol)", and the escapes
// in it (leave the alt screen, home the cursor) reached the operator's
// terminal. Checked for both attach paths: the seat-taking one and an observer.
func TestStreamSessionAttachCarriesNoSynthesisedFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Stream, runner, NewRingBuffer(1<<20), SessionHooks{})

	// NDJSON the adapter writes, plus a stray escape a `session send` could
	// have put on the stream: nothing may be modelled or repainted from it.
	line := makeWireFrame(1, []byte(`{"v":1,"kind":"event","event":{"kind":"text","text":"hi"}}`+"\n\x1b[?1049h"))
	runner.QueueRead(line)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(line) })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	for i, f := range decodeFrames(t, tui.WaitWritten(t, len(line))) {
		if f.Type == frame.FrameType_Synth {
			t.Errorf("attach frame %d is synthesised on a stream session: %q", i, f.Data)
		}
	}

	obs := newFakeStream(t)
	if err := mux.AttachViewer(ctx, obs, 0, false); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	for i, f := range decodeFrames(t, obs.WaitWritten(t, len(line))) {
		if f.Type == frame.FrameType_Synth {
			t.Errorf("observer frame %d is synthesised on a stream session: %q", i, f.Data)
		}
	}
	if hc := mux.heldCapture(); bytes.Contains(hc, []byte("hi")) {
		t.Errorf("a held stream session captured a screen: %q", hc)
	}
}

// A held capture a PREVIOUS server wrote for a stream session still carries a
// repaint (that server modelled every kind). Loading it into a stream mux must
// model nothing, or the next attach gets a preamble built from it — the same
// escapes, arriving at exactly the moment an upgrade restarts the server.
func TestStreamSessionIgnoresACapturedScreen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Stream, runner, NewRingBuffer(1<<20), SessionHooks{})
	if _, gotScreen, err := mux.loadHeldCapture(encodeSynthFrame([]byte("\x1b[?1049h\x1b[?25lold screen"))); err != nil || !gotScreen {
		t.Fatalf("loadHeldCapture: gotScreen=%v err=%v", gotScreen, err)
	}
	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	for i, f := range decodeFrames(t, tui.Written()) {
		if f.Type == frame.FrameType_Synth {
			t.Errorf("frame %d is synthesised from an old capture: %q", i, f.Data)
		}
	}
}
