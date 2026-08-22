//go:build !js

package cli

import (
	"bytes"
	"testing"

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
func collectFrames(t *testing.T, wire []byte) (pty []byte, synth int) {
	t.Helper()
	r := bytes.NewReader(wire)
	for r.Len() > 0 {
		f := &frame.Frame{}
		if err := f.Read(r); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch f.Header.Type {
		case frame.FrameType_Stdout, frame.FrameType_Stderr:
			if d := f.Data(); d != nil {
				pty = append(pty, (*d)...)
			}
		case frame.FrameType_Synth:
			if d := f.Data(); d != nil {
				synth += len(*d)
			}
		}
	}
	return pty, synth
}

func TestCollectRawSeparatesSynthesisedBytesFromPTYBytes(t *testing.T) {
	var wire []byte
	wire = append(wire, wireFrame(t, frame.FrameType_Synth, "\x1b[?25l")...) // preamble
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "a")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Synth, "REPAINT")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "b")...)

	pty, synth := collectFrames(t, wire)
	if want := "ab"; string(pty) != want {
		t.Errorf("PTY bytes = %q, want %q — a synthesised frame must not reach the raw output", pty, want)
	}
	if want := len("\x1b[?25l") + len("REPAINT"); synth != want {
		t.Errorf("withheld %d synthesised bytes, want %d — the count is what lets --raw "+
			"say it held something back instead of appearing to show everything", synth, want)
	}
}

// Stderr is PTY output too. It has always been part of what --raw returns, and
// the new classification must not quietly narrow that to stdout.
func TestCollectRawKeepsStderrAsPTYOutput(t *testing.T) {
	var wire []byte
	wire = append(wire, wireFrame(t, frame.FrameType_Stdout, "out")...)
	wire = append(wire, wireFrame(t, frame.FrameType_Stderr, "err")...)

	pty, synth := collectFrames(t, wire)
	if want := "outerr"; string(pty) != want {
		t.Errorf("PTY bytes = %q, want %q", pty, want)
	}
	if synth != 0 {
		t.Errorf("withheld %d bytes with no synthesised frames present", synth)
	}
}
