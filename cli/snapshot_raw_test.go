//go:build !js

package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/exec/frame"
)

// wireFrame builds one exec/frame frame.
func wireFrame(t *testing.T, typ frame.FrameType, payload string) []byte {
	t.Helper()
	hdr := frame.FrameHeader{Type: typ, Len: uint32(len(payload))}
	return append(hdr.MustAppend(nil), payload...)
}

// collectFrames is CollectRaw's frame loop over a fixed byte slice: the same
// switch, without the attach and the settle window, so the classification can be
// tested without a server.
//
// It exists because the classification IS the feature. `session snapshot --raw`
// promises the bytes the PTY emitted, and a server that replays a mode preamble
// and a screen repaint alongside them can only keep that promise if the two are
// told apart by frame TYPE — which is the one thing a merged byte stream cannot
// express.
//
// The meter it returns is the SAME liveMeter the real loop drives, not a
// re-implementation of the counting: the reset rule is the part worth pinning,
// and a copy of it here could only ever agree with itself. Time is supplied
// through fixed instants, so the window arithmetic is exercised without a clock.
func collectFrames(t *testing.T, wire []byte, includeSynth bool) (out []byte, synth int, m liveMeter) {
	t.Helper()
	m = liveMeter{start: attachAt}
	r := bytes.NewReader(wire)
	for r.Len() > 0 {
		f := &frame.Frame{}
		if err := f.Read(r); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch f.Header.Type {
		case frame.FrameType_Stdout, frame.FrameType_Stderr:
			if d := f.Data(); d != nil {
				out = append(out, (*d)...)
				m.pty(len(*d))
			}
		case frame.FrameType_Synth:
			if d := f.Data(); d != nil {
				synth += len(*d)
				m.synth(synthAt)
				if includeSynth {
					out = append(out, (*d)...)
				}
			}
		}
	}
	return out, synth, m
}

// Two fixed instants 1500 ms apart — the CLI's default settle window — so a
// window length can be asserted exactly.
var (
	attachAt = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	synthAt  = attachAt.Add(200 * time.Millisecond)
	endAt    = attachAt.Add(1500 * time.Millisecond)
)

func TestCollectRawSeparatesSynthesisedBytesFromPTYBytes(t *testing.T) {
	var wire []byte
	wire = append(wire, wireFrame(t, frame.FrameType_Synth, "\x1b[?25l")...) // preamble
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "a")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Synth, "REPAINT")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "b")...)

	pty, synth, _ := collectFrames(t, wire, false)
	if want := "ab"; string(pty) != want {
		t.Errorf("PTY bytes = %q, want %q — a synthesised frame must not reach the raw output", pty, want)
	}
	if want := len("\x1b[?25l") + len("REPAINT"); synth != want {
		t.Errorf("withheld %d synthesised bytes, want %d — the count is what lets --raw "+
			"say it held something back instead of appearing to show everything", synth, want)
	}
}

// Keeping them is what the CLI does by DEFAULT (`--without-synth` is the opt-out
// that reaches the case above): they are what the server actually sent, and the
// repaint is what reconstructs a screen whose opening bytes the ring has since
// evicted. Interleaved rather than appended, because where a repaint sits
// relative to the replay is most of what is being read.
func TestCollectRawCanIncludeSynthesisedBytesInPlace(t *testing.T) {
	var wire []byte
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "a")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Synth, "REPAINT")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "b")...)

	got, synth, _ := collectFrames(t, wire, true)
	if want := "aREPAINTb"; string(got) != want {
		t.Errorf("output = %q, want %q — synthesised bytes must appear where they arrived, "+
			"not appended or reordered", got, want)
	}
	if synth != len("REPAINT") {
		t.Errorf("synth count = %d, want %d; the count is reported either way", synth, len("REPAINT"))
	}
}

// Stderr is PTY output too. It has always been part of what --raw returns, and
// the new classification must not quietly narrow that to stdout.
func TestCollectRawKeepsStderrAsPTYOutput(t *testing.T) {
	var wire []byte
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "out")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Stderr, "err")...)

	pty, synth, _ := collectFrames(t, wire, false)
	if want := "outerr"; string(pty) != want {
		t.Errorf("PTY bytes = %q, want %q", pty, want)
	}
	if synth != 0 {
		t.Errorf("withheld %d bytes with no synthesised frames present", synth)
	}
}

// The measurement this exists for: a view-attach opens with the server replaying
// its ring at wire speed, so output that arrived BEFORE the closing repaint says
// nothing about how fast the session is producing anything now. The window has
// to start at the last synthesised frame, and the counters with it.
func TestLiveWindowStartsAtTheLastSynthesisedFrame(t *testing.T) {
	var wire []byte
	wire = append(wire, wireFrame(t, frame.FrameType_Synth, "\x1b[?25l")...)       // preamble
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "old transcript")...) // replayed ring
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "more history")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Synth, "REPAINT")...) // end of replay
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "tick")...)   // live
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "tock")...)

	_, _, m := collectFrames(t, wire, true)
	got := m.result(endAt)

	if got.Frames != 2 {
		t.Errorf("frames = %d, want 2 — the two replayed frames must not be counted; "+
			"counting all four would report the LINK's speed, not the session's", got.Frames)
	}
	if want := len("tick") + len("tock"); got.Bytes != want {
		t.Errorf("bytes = %d, want %d", got.Bytes, want)
	}
	if !got.Anchored {
		t.Error("anchored = false after a synthesised frame was seen")
	}
	// The window runs from the repaint, not from the attach: 1500-200.
	if got.WindowMs != 1300 {
		t.Errorf("window_ms = %d, want 1300 — measured from the repaint, not from attach", got.WindowMs)
	}
}

// Without a synthesised frame there is no boundary to anchor to, and the window
// still holds replayed history. The counts are then not a rate, and Anchored is
// the only thing that says so — the numbers themselves look identical.
func TestLiveWindowIsUnanchoredWhenNoRepaintArrives(t *testing.T) {
	var wire []byte
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "history")...)

	_, _, m := collectFrames(t, wire, true)
	got := m.result(endAt)

	if got.Anchored {
		t.Error("anchored = true with no synthesised frame on the stream")
	}
	if got.Frames != 1 || got.Bytes != len("history") {
		t.Errorf("counts = %d frames / %d bytes, want 1 / %d; an unanchored window still "+
			"reports what it saw", got.Frames, got.Bytes, len("history"))
	}
	if got.WindowMs != 1500 {
		t.Errorf("window_ms = %d, want the whole capture (1500)", got.WindowMs)
	}
}

// A silent session is the case the field exists to keep honest: zero frames
// within a stated window is a measurement. Reporting the count without the
// window, or eliding the zero, would make "nothing arrived" and "nothing was
// collected" the same output.
func TestLiveWindowReportsZeroAgainstItsWindow(t *testing.T) {
	wire := wireFrame(t, frame.FrameType_Synth, "REPAINT")

	_, _, m := collectFrames(t, wire, true)
	got := m.result(endAt)

	if got.Frames != 0 || got.Bytes != 0 {
		t.Errorf("counts = %d/%d, want 0/0", got.Frames, got.Bytes)
	}
	if !got.Anchored || got.WindowMs != 1300 {
		t.Errorf("got anchored=%v window_ms=%d, want true/1300 — a zero needs its window "+
			"to be readable as a rate", got.Anchored, got.WindowMs)
	}
}
