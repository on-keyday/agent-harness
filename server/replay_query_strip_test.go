package server

import (
	"encoding/binary"
	"testing"

	"github.com/on-keyday/objtrsf/exec/frame"
)

// A replayed ring must not ask the operator's terminal anything.
//
// The bug this pins: a nested TUI (herdr/htop class) probes the palette at
// startup with OSC 4/10/11 `?` queries. Those bytes land in the ring like any
// other output, and every reattach ships them verbatim to the attaching
// client's terminal, which dutifully answers all 258 of them — long after the
// asker exited. The answers arrive as INPUT to a pty where a shell is now
// sitting at a prompt, and readline eats the `ESC]` introducer and
// self-inserts the rest, so the operator gets screenfuls of
// `4;0;rgb:0c0c/0c0c/0c0c` per attach. Measured on a live dummy instance:
// `session snapshot --raw` contained ESC]10;? ESC]11;? ESC]4;0;? … verbatim.
//
// Only the REPLAY is filtered. A live app must still be able to query the
// terminal and get a real answer — that round-trip is deliberate, and the
// reply only makes sense while the asker is still there to read it.
func TestStripReplyQueries(t *testing.T) {
	esc := "\x1b"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"osc4 palette query dropped", esc + "]4;0;?" + esc + "\\", ""},
		{"osc10 fg query dropped", esc + "]10;?" + esc + "\\", ""},
		{"osc11 bg query dropped", esc + "]11;?" + esc + "\\", ""},
		{"osc4 query with BEL terminator dropped", esc + "]4;1;?\x07", ""},
		{"osc52 clipboard READ dropped", esc + "]52;c;?" + esc + "\\", ""},
		{
			"osc52 clipboard WRITE kept (not a query)",
			esc + "]52;c;aGVsbG8=" + esc + "\\",
			esc + "]52;c;aGVsbG8=" + esc + "\\",
		},
		{
			"window title kept",
			esc + "]0;my title\x07",
			esc + "]0;my title\x07",
		},
		{
			"osc4 palette SET kept (sets a colour, asks nothing)",
			esc + "]4;1;rgb:c5c5/0f0f/1f1f" + esc + "\\",
			esc + "]4;1;rgb:c5c5/0f0f/1f1f" + esc + "\\",
		},
		{"DSR cursor position request dropped", esc + "[6n", ""},
		{"DSR status request dropped", esc + "[5n", ""},
		{"DECDSR private status request dropped", esc + "[?6n", ""},
		{"primary DA dropped", esc + "[c", ""},
		{"secondary DA dropped", esc + "[>c", ""},
		{"tertiary DA dropped", esc + "[=c", ""},
		{"DECRQM dropped", esc + "[?2026$p", ""},
		{"XTVERSION dropped", esc + "[>0q", ""},
		{"XTGETTCAP dropped", esc + "P+q544e" + esc + "\\", ""},
		{"DECRQSS dropped", esc + "P$qm" + esc + "\\", ""},
		{
			"DECSCUSR kept — CSI ... q without '>' sets the cursor shape",
			esc + "[5 q",
			esc + "[5 q",
		},
		{
			"SGR kept",
			esc + "[1;31m",
			esc + "[1;31m",
		},
		{
			"private mode set kept — the preamble depends on these surviving",
			esc + "[?1002h" + esc + "[?1006h",
			esc + "[?1002h" + esc + "[?1006h",
		},
		{
			"plain text untouched",
			"hello world\r\n",
			"hello world\r\n",
		},
		{
			"queries removed from around real output",
			"before" + esc + "]11;?" + esc + "\\" + esc + "[6n" + "after",
			"beforeafter",
		},
		{
			"the observed faketui burst keeps only the mode sets and the text",
			esc + "[?1002h" + esc + "[?1006h" + esc + "[?2004h" +
				esc + "]10;?" + esc + "\\" + esc + "]11;?" + esc + "\\" +
				esc + "]4;0;?" + esc + "\\" + esc + "]4;1;?" + esc + "\\" +
				"FAKETUI_STARTED\r\n",
			esc + "[?1002h" + esc + "[?1006h" + esc + "[?2004h" +
				"FAKETUI_STARTED\r\n",
		},
		{
			"a sequence cut off by the ring edge is kept as-is",
			"tail" + esc + "]4;7;rgb:cccc",
			"tail" + esc + "]4;7;rgb:cccc",
		},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(stripReplyQueries([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("stripReplyQueries(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// The stripper runs over every replay, so it must not rewrite a stream that
// has nothing to strip — both for cost and because any difference here is a
// display change nobody asked for.
func TestStripReplyQueriesLeavesCleanStreamIdentical(t *testing.T) {
	in := []byte("\x1b[?1049h\x1b[2J\x1b[H\x1b[1;32mprompt\x1b[0m$ \x1b]0;title\x07ls -la\r\n")
	got := stripReplyQueries(in)
	if string(got) != string(in) {
		t.Errorf("clean stream was rewritten:\n got %q\nwant %q", got, in)
	}
}

// parseFrames splits wire frames and returns (types, payloads). It fails the
// test on any header that does not match the bytes that follow — which is the
// property the frame-level stripper must not break: rewriting a payload
// without its length header desyncs the client's parser for the rest of the
// stream, and the symptom is a hung client, not stray text.
func parseFrames(t *testing.T, data []byte) ([]frame.FrameType, [][]byte) {
	t.Helper()
	var types []frame.FrameType
	var payloads [][]byte
	off := 0
	for off < len(data) {
		if len(data)-off < frameHeaderSize {
			t.Fatalf("truncated frame header at %d (len %d)", off, len(data))
		}
		n := int(binary.BigEndian.Uint32(data[off+1 : off+5]))
		if off+frameHeaderSize+n > len(data) {
			t.Fatalf("frame at %d claims %d payload bytes, only %d remain",
				off, n, len(data)-off-frameHeaderSize)
		}
		types = append(types, frame.FrameType(data[off]))
		payloads = append(payloads, data[off+frameHeaderSize:off+frameHeaderSize+n])
		off += frameHeaderSize + n
	}
	return types, payloads
}

func concatPayloads(p [][]byte) string {
	var s []byte
	for _, b := range p {
		s = append(s, b...)
	}
	return string(s)
}

func TestStripReplyQueriesFrames_RewritesHeaderLengths(t *testing.T) {
	esc := "\x1b"
	in := encodeStdoutFrame([]byte("before" + esc + "]11;?" + esc + "\\" + "after"))
	out := stripReplyQueriesFrames(in)
	types, payloads := parseFrames(t, out)
	if len(types) != 1 || types[0] != frame.FrameType_Stdout {
		t.Fatalf("frames = %v, want one Stdout", types)
	}
	if got := string(payloads[0]); got != "beforeafter" {
		t.Errorf("payload = %q, want %q", got, "beforeafter")
	}
}

// The burst that produced the reported symptom does not arrive as one write.
// A query straddling two frames must still be recognised, or the palette dump
// survives in pieces.
func TestStripReplyQueriesFrames_QuerySplitAcrossFrames(t *testing.T) {
	esc := "\x1b"
	in := append(encodeStdoutFrame([]byte("head"+esc+"]4;0")), encodeStdoutFrame([]byte(";?"+esc+"\\"+"tail"))...)
	out := stripReplyQueriesFrames(in)
	_, payloads := parseFrames(t, out)
	if got := concatPayloads(payloads); got != "headtail" {
		t.Errorf("payload = %q, want %q — a query split across frames survived", got, "headtail")
	}
}

func TestStripReplyQueriesFrames_AllQueryFrameIsDropped(t *testing.T) {
	esc := "\x1b"
	in := append(encodeStdoutFrame([]byte("keep me")), encodeStdoutFrame([]byte(esc+"]4;0;?"+esc+"\\"))...)
	out := stripReplyQueriesFrames(in)
	types, payloads := parseFrames(t, out)
	if len(types) != 1 {
		t.Fatalf("frames = %d, want 1 (the all-query frame should vanish)", len(types))
	}
	if got := string(payloads[0]); got != "keep me" {
		t.Errorf("payload = %q, want %q", got, "keep me")
	}
}

// A Control frame is out-of-band with respect to the terminal byte stream, so
// it must pass through untouched AND must not break a sequence that spans it.
func TestStripReplyQueriesFrames_ControlFramePassesThrough(t *testing.T) {
	esc := "\x1b"
	ctrl := encodeFrame(frame.FrameType_Control, []byte{0xde, 0xad, 0xbe, 0xef})
	in := encodeStdoutFrame([]byte("a" + esc + "]11"))
	in = append(in, ctrl...)
	in = append(in, encodeStdoutFrame([]byte(";?"+esc+"\\"+"b"))...)
	out := stripReplyQueriesFrames(in)
	types, payloads := parseFrames(t, out)
	var sawControl bool
	for i, ty := range types {
		if ty == frame.FrameType_Control {
			sawControl = true
			if string(payloads[i]) != string([]byte{0xde, 0xad, 0xbe, 0xef}) {
				t.Errorf("control payload rewritten: %x", payloads[i])
			}
		}
	}
	if !sawControl {
		t.Error("control frame was dropped")
	}
	var text []byte
	for i, ty := range types {
		if ty == frame.FrameType_Stdout {
			text = append(text, payloads[i]...)
		}
	}
	if string(text) != "ab" {
		t.Errorf("stdout payload = %q, want %q — the control frame broke the span", text, "ab")
	}
}

func TestStripReplyQueriesFrames_CleanStreamIdentical(t *testing.T) {
	in := append(encodeStdoutFrame([]byte("\x1b[1;32m$ \x1b[0mls\r\n")),
		encodeStdoutFrame([]byte("\x1b]0;title\x07file.txt\r\n"))...)
	out := stripReplyQueriesFrames(in)
	if string(out) != string(in) {
		t.Errorf("clean stream rewritten:\n got %q\nwant %q", out, in)
	}
}

// The ring can end mid-sequence. Those bytes are real output whose tail has not
// arrived; losing them would corrupt the screen, so they must survive.
func TestStripReplyQueriesFrames_PartialSequenceAtEndSurvives(t *testing.T) {
	in := encodeStdoutFrame([]byte("text\x1b]4;7;rgb:cc"))
	out := stripReplyQueriesFrames(in)
	_, payloads := parseFrames(t, out)
	if got := concatPayloads(payloads); got != "text\x1b]4;7;rgb:cc" {
		t.Errorf("payload = %q, want the partial sequence preserved", got)
	}
}
