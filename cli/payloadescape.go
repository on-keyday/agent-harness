package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"unicode/utf8"
)

// BodyMode says what happens to payload bytes on the way out. It exists so
// the decision is made in one place and travels as one value: a pair of bools
// at each call site can be swapped without the compiler noticing, and a swap
// here corrupts extracted bytes while leaving nothing on screen to notice.
//
// It is exported because a caller that is not writing to a file — the TUI,
// which renders into a viewport — cannot be judged by BodyModeFor and has to
// say what it wants. For that caller the answer is always BodyEscaped: a
// bordered panel is repainted over by a stray ESC, which is the same reason
// tui/rawforward.go sanitizes what it draws.
type BodyMode int

const (
	// BodyExact is the zero value ON PURPOSE. A mode nobody set yields the
	// published bytes, so a forgotten assignment fails toward a visible
	// problem (an ESC reaching a terminal) rather than an invisible one
	// (a file whose bytes are not the message).
	BodyExact BodyMode = iota
	// BodyEscaped renders every terminal-steering byte as a visible escape.
	BodyEscaped
)

// EscapeForTerminal returns b with every terminal-steering CODE POINT
// rendered as a visible \xNN escape: C0 except \n and \t, U+007F, and C1
// (U+0080-U+009F — U+009B is CSI, and a terminal honouring 8-bit controls
// acts on it the way it acts on ESC [). Everything from U+00A0 up passes
// through unchanged, so ordinary multibyte text — Japanese, emoji, Cyrillic,
// accented Latin — comes back byte-identical: their UTF-8 continuation bytes
// sit in 0x80-0xBF, and scanning BYTES would land the C1 arm inside them.
// The scan is therefore rune-wise, the same choice sanitizeOutput in
// tui/rawforward.go makes with strings.Map; the two differ only in the
// replacement (see below) and in the invalid-byte case, which sanitizeOutput
// never sees because strings.Map already turned it into U+FFFD.
//
// A byte that does not begin a valid UTF-8 sequence (utf8.DecodeRune reports
// RuneError with size 1) is escaped as itself, so an invalid byte reaches the
// terminal visibly, never raw.
//
// The escape spelling is \xNN rather than sanitizeOutput's "." for the reason
// the spec gives: a bordered panel must preserve the column count, so it maps
// one rune to one rune; a transcript has no border to protect and gains from
// keeping the byte identifiable.
func EscapeForTerminal(b []byte) string {
	var sb bytes.Buffer
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			// Not a valid UTF-8 sequence start: the byte itself is what the
			// reader needs to see. \n and \t stay verbatim even here.
			c := b[i]
			if c == '\n' || c == '\t' {
				sb.WriteByte(c)
			} else {
				fmt.Fprintf(&sb, "\\x%02x", c)
			}
			i++
			continue
		}
		switch {
		case r == '\n' || r == '\t' || r >= 0xA0:
			sb.WriteRune(r)
		case r < 0x20 || r >= 0x7f: // C0 (minus \n/\t), U+007F, and the C1 range
			fmt.Fprintf(&sb, "\\x%02x", r)
		default:
			sb.WriteRune(r)
		}
		i += size
	}
	return sb.String()
}

// IsTTY reports whether f is a character device. Mirrors isTTY in
// cmd/harness-cli/git.go:95, which exists for the same reason: the only
// condition under which presentation escapes belong in the output — a
// redirected or piped destination has to stay byte-clean.
func IsTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// BodyModeFor is the ONLY place the destination is judged.
//
// raw always wins: --raw exists so an operator reading interactively can copy
// exact bytes without redirecting. A non-character device yields BodyExact
// regardless — that is obligation 2, the data path; escaping it would corrupt
// every `board read > out` and `| jq` extraction, invisibly.
func BodyModeFor(out *os.File, raw bool) BodyMode {
	if raw || out == nil || !IsTTY(out) {
		return BodyExact
	}
	return BodyEscaped
}

// writeBoardBody prints one message body to w. It is the single
// body-printing function both `board read` and the thread verbs call, and it
// keeps cmd_board.go's existing JSON-indent behaviour: a body that parses as
// JSON is indented, anything else is written as stored.
//
// BodyExact writes the published bytes (indented for JSON — that transform
// was already the contract — but no escaping). BodyEscaped runs the result
// through EscapeForTerminal. The JSON case needs the escape pass in both
// modes' terms: encoding/json escapes C0 inside strings but leaves C1 raw, so
// an indented body would still hand a terminal a live U+009B.
func writeBoardBody(w io.Writer, payload []byte, mode BodyMode) {
	if json.Valid(payload) {
		var buf bytes.Buffer
		_ = json.Indent(&buf, payload, "", "  ")
		payload = buf.Bytes()
	}
	if mode == BodyEscaped {
		fmt.Fprintln(w, EscapeForTerminal(payload))
		return
	}
	w.Write(payload) //nolint:errcheck
	fmt.Fprintln(w)
}
