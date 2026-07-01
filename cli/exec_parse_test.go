package cli

import (
	"strings"
	"testing"
)

func sent(nonce string) execSentinels {
	return execSentinels{
		start: "__HEXEC_" + nonce + "_S__",
		end:   "__HEXEC_" + nonce + "_E__",
	}
}

func TestScanExec_NotDoneUntilEndSentinel(t *testing.T) {
	s := sent("T")
	// Buffer ends mid-end-sentinel: the loop must keep reading, not match a
	// partial. This is the frame-spanning guarantee (scan the accumulated
	// buffer; a sentinel split across two PTY frames must not complete early).
	buf := []byte("__HEXEC_T_S__\r\nhi\r\n__HEXEC_T_E")
	if r := scanExec(buf, s); r.done {
		t.Fatalf("expected not done while end sentinel is incomplete")
	}
}

func TestScanExec_Basic_CRLF(t *testing.T) {
	s := sent("T")
	// A realistic cowrite capture: ring replay, then the echoed input line
	// (which literally contains both sentinel substrings and must NOT be
	// mistaken for a boundary), then the two output sentinel lines around the
	// real command output.
	buf := []byte(
		"junk from the replayed ring\r\n" +
			`printf '__HEXEC_T_S__\n'; echo hi; printf '__HEXEC_T_E__%s\n' "$?"` + "\r\n" +
			"__HEXEC_T_S__\r\n" +
			"hi\r\n" +
			"__HEXEC_T_E__0\r\n")
	r := scanExec(buf, s)
	if !r.done {
		t.Fatalf("expected done")
	}
	if r.exitCode != 0 {
		t.Fatalf("exit=%d want 0", r.exitCode)
	}
	if string(r.output) != "hi\r\n" {
		t.Fatalf("output=%q want %q (echo line must be excluded)", r.output, "hi\r\n")
	}
}

func TestScanExec_LF_only(t *testing.T) {
	s := sent("T")
	buf := []byte("__HEXEC_T_S__\nhi\n__HEXEC_T_E__0\n")
	r := scanExec(buf, s)
	if !r.done || r.exitCode != 0 || string(r.output) != "hi\n" {
		t.Fatalf("done=%v exit=%d out=%q", r.done, r.exitCode, r.output)
	}
}

func TestScanExec_MultiDigitExit(t *testing.T) {
	s := sent("T")
	buf := []byte("__HEXEC_T_S__\nboom\n__HEXEC_T_E__137\n")
	r := scanExec(buf, s)
	if !r.done || r.exitCode != 137 {
		t.Fatalf("done=%v exit=%d want done exit=137", r.done, r.exitCode)
	}
}

func TestScanExec_EmptyOutput(t *testing.T) {
	s := sent("T")
	buf := []byte("__HEXEC_T_S__\n__HEXEC_T_E__0\n")
	r := scanExec(buf, s)
	if !r.done || r.exitCode != 0 || len(r.output) != 0 {
		t.Fatalf("done=%v exit=%d out=%q want done exit=0 empty", r.done, r.exitCode, r.output)
	}
}

func TestInterpretPlain(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "hello\nworld\n", "hello\nworld\n"},
		{"no_trailing_nl", "hello\nworld", "hello\nworld"},
		{"sgr_strip", "\x1b[32mOK\x1b[0m\n", "OK\n"},
		{"cr_overwrite_same_len", "50%\r100%\n", "100%\n"},
		{"cr_overwrite_shorter", "aaaa\rbb\n", "bbaa\n"},
		{"erase_to_end_0K", "abcdef\rXY\x1b[K\n", "XY\n"},
		{"erase_whole_2K", "abc\x1b[2K\rZ\n", "Z\n"},
		{"no_rewrap_long_line", strings.Repeat("x", 300) + "\n", strings.Repeat("x", 300) + "\n"},
		{"crlf_pairs", "a\r\nb\r\n", "a\nb\n"},
		{"cursor_forward_stripped", "a\x1b[3Cb\n", "ab\n"},
		{"osc_title_stripped", "\x1b]0;window title\x07hi\n", "hi\n"},
	}
	for _, c := range cases {
		if got := interpretPlain([]byte(c.in)); got != c.want {
			t.Errorf("%s: interpretPlain(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
