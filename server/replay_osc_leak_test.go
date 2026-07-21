package server

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// stripStdoutFrames concatenates the payloads of the Stdout/Stderr frames in a
// wire replay blob, yielding the raw VT byte stream a client's emulator sees.
func stripStdoutFrames(t *testing.T, data []byte) []byte {
	t.Helper()
	var out []byte
	for off := 0; off < len(data); {
		if len(data)-off < frameHeaderSize {
			t.Fatalf("truncated frame header at %d", off)
		}
		n := int(binary.BigEndian.Uint32(data[off+1 : off+5]))
		start := off + frameHeaderSize
		if start+n > len(data) {
			t.Fatalf("frame payload overruns blob at %d", off)
		}
		out = append(out, data[start:start+n]...)
		off = start + n
	}
	return out
}

// TestCapReplayTail_NoOSCLeak reproduces the grid-pane "window title text bleeds
// into the display" bug. A window-title OSC (ESC]0;…BEL) spans a frame boundary;
// the leading frame (carrying ESC]0; and the title head) is older scrollback that
// capReplayTail drops to honour the pane's replay limit. If the crop starts at a
// frame boundary that lies INSIDE the OSC, the client's emulator — fresh in
// ground state — renders the title's TAIL as ordinary printable text.
func TestCapReplayTail_NoOSCLeak(t *testing.T) {
	const titleTail = "LEAKMARKER_TAIL"
	// Frame 1: old scrollback filler + the HEAD of a window-title OSC.
	f1 := makeWireFrame(byte(frame.FrameType_Stdout),
		append(bytes.Repeat([]byte{'x'}, 300), []byte("\x1b]0;WINDOW_TITLE_")...))
	// Frame 2: the TAIL of the same OSC (terminated by BEL) + real visible output.
	f2 := makeWireFrame(byte(frame.FrameType_Stdout),
		[]byte(titleTail+"\x07visible_content"))

	data := append(append([]byte{}, f1...), f2...)

	// A limit small enough to evict frame 1 but keep frame 2: the naive crop
	// starts mid-OSC.
	cropped := capReplayTail(data, 100)

	emu := vt.NewEmulator(80, 24)
	if _, err := emu.Write(stripStdoutFrames(t, cropped)); err != nil {
		t.Fatalf("emulator write: %v", err)
	}
	screen := emu.String()
	if strings.Contains(screen, titleTail) {
		t.Fatalf("window-title tail %q bled into the rendered screen (mid-OSC crop):\n%s", titleTail, screen)
	}
	if !strings.Contains(screen, "visible_content") {
		t.Fatalf("real output after the title must still render, screen:\n%s", screen)
	}
}

// When a ground-state frame boundary within the limit DOES exist, capReplayTail
// must still crop there (not conservatively return everything). Here an OSC is
// fully contained in the leading frame, so the boundary before the visible frame
// is ground state and safe to cut at.
func TestCapReplayTail_DropsAtSafeBoundary(t *testing.T) {
	// Leading frame: old scrollback + a COMPLETE window-title OSC (ST-terminated).
	f1 := makeWireFrame(byte(frame.FrameType_Stdout),
		append(bytes.Repeat([]byte{'x'}, 300), []byte("\x1b]2;done\x1b\\")...))
	// Trailing frame: current visible output, starts at a ground boundary.
	f2 := makeWireFrame(byte(frame.FrameType_Stdout), []byte("HELLO_WORLD"))
	data := append(append([]byte{}, f1...), f2...)

	got := capReplayTail(data, 100)
	if !bytes.Equal(got, f2) {
		t.Fatalf("expected the crop to drop the leading frame and keep only f2 (%d bytes), got %d", len(f2), len(got))
	}

	emu := vt.NewEmulator(80, 24)
	if _, err := emu.Write(stripStdoutFrames(t, got)); err != nil {
		t.Fatalf("emulator write: %v", err)
	}
	if !strings.Contains(emu.String(), "HELLO_WORLD") {
		t.Fatalf("visible content must render:\n%s", emu.String())
	}
}

func TestVTGroundScan(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  bool // ground after consuming in
	}{
		{"plain text", "hello world", true},
		{"complete OSC BEL", "\x1b]0;title\x07", true},
		{"complete OSC ST", "\x1b]0;title\x1b\\", true},
		{"mid OSC", "\x1b]0;titl", false},
		{"mid OSC after text", "abc\x1b]2;win", false},
		{"complete CSI", "\x1b[31m", true},
		{"mid CSI", "\x1b[3", false},
		{"lone ESC", "\x1b", false},
		{"CSI then text", "\x1b[HHELLO", true},
		{"CAN aborts", "\x1b]0;partial\x18", true},
		{"DCS mid", "\x1bPq#0", false},
		{"DCS complete ST", "\x1bPq#0\x1b\\", true},
		{"charset designate", "\x1b(B", true},
		{"mid charset", "\x1b(", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var sc vtGroundScan
			sc.scan([]byte(c.in))
			if got := sc.ground(); got != c.want {
				t.Fatalf("ground()=%v want %v for %q", got, c.want, c.in)
			}
		})
	}
}

// State must persist across scan() calls so a sequence split across frames is
// still recognised at the boundary (the whole point for capReplayTail).
func TestVTGroundScan_SplitAcrossChunks(t *testing.T) {
	var sc vtGroundScan
	sc.scan([]byte("\x1b]0;win"))
	if sc.ground() {
		t.Fatal("mid-OSC after first chunk must not be ground")
	}
	sc.scan([]byte("dow title\x07"))
	if !sc.ground() {
		t.Fatal("OSC terminated in second chunk must return to ground")
	}
}
