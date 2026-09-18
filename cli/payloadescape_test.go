package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestEscapeForTerminalCoversTheByteSet pins the byte set: C0 except \n and
// \t, 0x7f, and C1 (0x80-0x9f — U+009B is CSI, and a terminal honouring 8-bit
// controls acts on it the way it acts on ESC [).
func TestEscapeForTerminalCoversTheByteSet(t *testing.T) {
	in := []byte("a\x1b[2Jb\rc\x07d\x7fe\u009bf")
	got := EscapeForTerminal(in)
	for _, bad := range []string{"\x1b", "\r", "\x07", "\x7f", "\u009b"} {
		if strings.Contains(got, bad) {
			t.Errorf("output still holds %q raw: %q", bad, got)
		}
	}
	for _, want := range []string{`\x1b`, `\x0d`, `\x07`, `\x7f`, `\x9b`} {
		if !strings.Contains(got, want) {
			t.Errorf("escape %q not visible in %q", want, got)
		}
	}
}

func TestEscapeForTerminalKeepsNewlineAndTab(t *testing.T) {
	in := []byte("keep\tthis\nand this")
	if got := EscapeForTerminal(in); got != string(in) {
		t.Errorf("got %q, want it unchanged", got)
	}
}

// U+00A0 and above pass through unchanged even though their UTF-8 encoding
// contains bytes a byte-wise scanner would flag — the code-point rule.
func TestEscapeForTerminalLeavesHighBytesAlone(t *testing.T) {
	in := []byte("\u6c34\u2020\uFFFD") // 水, †, replacement char: all ≥ U+00A0
	if got := EscapeForTerminal(in); !bytes.Equal([]byte(got), in) {
		t.Errorf("0xA0+ code points were altered: %q", got)
	}
}

// TestEscapeForTerminalLeavesMultibyteTextAlone pins the rule the helper
// exists to respect: the set is CODE POINTS, not bytes. UTF-8 continuation
// bytes occupy 0x80-0xBF, so a byte-wise scan lands its C1 arm inside ordinary
// text — Japanese, emoji, Cyrillic, accented Latin. Each of these must come
// back byte-identical.
func TestEscapeForTerminalLeavesMultibyteTextAlone(t *testing.T) {
	for _, in := range []string{
		"日本語のメッセージ",
		"done ✅ shipped 🚀",
		"привет",
		"café crème",
		"ok 日 ok",
	} {
		if got := EscapeForTerminal([]byte(in)); got != in {
			t.Errorf("multibyte text was shredded:\n in  %q\n out %q", in, got)
		}
	}
}

// U+009B as a code point (C2 9B in UTF-8) is escaped, and so is a lone 0x9b
// byte that never began a valid sequence — a terminal honouring 8-bit
// controls treats both as CSI.
func TestEscapeForTerminalEscapesC1CodePointAndLoneByte(t *testing.T) {
	if got := EscapeForTerminal([]byte("a\u009bb")); !strings.Contains(got, `\x9b`) {
		t.Errorf("U+009B code point not escaped: %q", got)
	}
	if got := EscapeForTerminal([]byte{'a', 0x9b, 'b'}); !strings.Contains(got, `\x9b`) {
		t.Errorf("lone 0x9b byte not escaped: %q", got)
	}
}

// 0xff is not a valid UTF-8 start byte; an invalid byte reaches the terminal
// as its visible escape, never raw.
func TestEscapeForTerminalEscapesInvalidBytes(t *testing.T) {
	if got := EscapeForTerminal([]byte{'a', 0xff, 'b'}); !strings.Contains(got, `\xff`) {
		t.Errorf("0xff not escaped: %q", got)
	}
}

// Obligation 2. The regression test for the defect the spec's first draft
// had: a redirected destination must get the published bytes, byte for byte,
// whatever they hold.
func TestBoardReadRedirectedIsByteExact(t *testing.T) {
	body := []byte("x\x1b[2J\r\u009b\xff\xfe binary")
	var buf bytes.Buffer // not a character device
	writeBoardBody(&buf, body, bodyExact)
	if !bytes.Equal(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), body) {
		t.Errorf("redirected output altered the bytes:\n got %q\nwant %q", buf.Bytes(), body)
	}
}

func TestBoardReadRawIsByteExactOnATerminal(t *testing.T) {
	body := []byte("x\x1b[2J\xff")
	var buf bytes.Buffer
	writeBoardBody(&buf, body, bodyExact)
	if !bytes.Equal(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), body) {
		t.Errorf("--raw altered the bytes: %q", buf.Bytes())
	}
}

func TestBoardReadTerminalIsEscaped(t *testing.T) {
	var buf bytes.Buffer
	writeBoardBody(&buf, []byte("x\x1b[2J"), bodyEscaped)
	if bytes.Contains(buf.Bytes(), []byte{0x1b}) {
		t.Errorf("a terminal got a raw ESC: %q", buf.Bytes())
	}
}

// An indented JSON body still reaches a terminal carrying a live C1 unless
// the same gate covers it: encoding/json escapes C0 and leaves C1 raw. The
// body here carries a RAW C1 byte (U+009B as UTF-8 C2 9B), which RFC 8259
// permits unescaped inside a string — exactly what json.Indent passes
// through untouched.
func TestJSONBodyC1IsEscapedForTerminal(t *testing.T) {
	var buf bytes.Buffer
	writeBoardBody(&buf, []byte("{\"k\":\"a\u009bb\"}"), bodyEscaped)
	if bytes.Contains(buf.Bytes(), []byte{0xc2, 0x9b}) {
		t.Errorf("C1 survived into terminal output: %q", buf.Bytes())
	}
	if !bytes.Contains(buf.Bytes(), []byte(`\x9b`)) {
		t.Errorf("C1 escape not visible: %q", buf.Bytes())
	}
}

// bodyModeFor is the ONLY place the destination is judged; these pin its
// table. A zero-byte body through the exact mode round-trips unchanged.
func TestBodyModeFor(t *testing.T) {
	regular, err := os.CreateTemp(t.TempDir(), "notachrdev")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if st, _ := regular.Stat(); st.Mode()&os.ModeCharDevice != 0 {
		t.Skip("temp file is unexpectedly a character device")
	}

	// A non-character device yields bodyExact regardless of raw: a pipe or
	// redirect gets the published bytes even if the operator typed --raw.
	if got := bodyModeFor(regular, false); got != bodyExact {
		t.Errorf("regular file, raw=false: mode = %v, want bodyExact", got)
	}
	if got := bodyModeFor(regular, true); got != bodyExact {
		t.Errorf("regular file, raw=true: mode = %v, want bodyExact", got)
	}

	// /dev/null is a character device; --raw yields bodyExact even for a
	// terminal, no-raw yields bodyEscaped.
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer null.Close()
	if st, _ := null.Stat(); st.Mode()&os.ModeCharDevice == 0 {
		t.Skipf("%s is unexpectedly not a character device", os.DevNull)
	}
	if got := bodyModeFor(null, true); got != bodyExact {
		t.Errorf("terminal, raw=true: mode = %v, want bodyExact", got)
	}
	if got := bodyModeFor(null, false); got != bodyEscaped {
		t.Errorf("terminal, raw=false: mode = %v, want bodyEscaped", got)
	}
}

// A nil *os.File (a non-file io.Writer type-asserted at the call site) fails
// toward bodyExact: escaping requires proof of a terminal, not the absence
// of one.
func TestBodyModeForNilFileIsExact(t *testing.T) {
	if got := bodyModeFor(nil, false); got != bodyExact {
		t.Errorf("nil file: mode = %v, want bodyExact", got)
	}
}
