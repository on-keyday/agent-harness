package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// bodyMode says what happens to payload bytes on the way out. It exists so
// the decision is made in one place and travels as one value: a pair of bools
// at each call site can be swapped without the compiler noticing, and a swap
// here corrupts extracted bytes while leaving nothing on screen to notice.
type bodyMode int

const (
	// bodyExact is the zero value ON PURPOSE. A mode nobody set yields the
	// published bytes, so a forgotten assignment fails toward a visible
	// problem (an ESC reaching a terminal) rather than an invisible one
	// (a file whose bytes are not the message).
	bodyExact bodyMode = iota
	// bodyEscaped renders every terminal-steering byte as a visible escape.
	bodyEscaped
)

// EscapeForTerminal returns b with every byte that can steer a terminal
// rendered as a visible \xNN escape: C0 except \n and \t, 0x7f, and C1
// (0x80-0x9f — U+009B is CSI, and a terminal honouring 8-bit controls acts on
// it the way it acts on ESC [). Everything at 0xA0 and above passes through
// unchanged. It is byte-based, like tui/rawforward.go's decision, because the
// input is untrusted bytes and not known-good text.
//
// The escape spelling is \xNN rather than sanitizeOutput's "." for the reason
// the spec gives: a bordered panel must preserve the column count, so it maps
// one rune to one rune; a transcript has no border to protect and gains from
// keeping the byte identifiable.
func EscapeForTerminal(b []byte) string {
	var sb bytes.Buffer
	sb.Grow(len(b))
	for _, c := range b {
		switch {
		case c == '\n' || c == '\t' || c >= 0xA0:
			sb.WriteByte(c)
		case c < 0x20 || c >= 0x7f: // 0x7f through 0x9f, plus the C0 controls
			fmt.Fprintf(&sb, "\\x%02x", c)
		default:
			sb.WriteByte(c)
		}
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

// bodyModeFor is the ONLY place the destination is judged.
//
// raw always wins: --raw exists so an operator reading interactively can copy
// exact bytes without redirecting. A non-character device yields bodyExact
// regardless — that is obligation 2, the data path; escaping it would corrupt
// every `board read > out` and `| jq` extraction, invisibly.
func bodyModeFor(out *os.File, raw bool) bodyMode {
	if raw || out == nil || !IsTTY(out) {
		return bodyExact
	}
	return bodyEscaped
}

// writeBoardBody prints one message body to w. It is the single
// body-printing function both `board read` and the thread verbs call, and it
// keeps cmd_board.go's existing JSON-indent behaviour: a body that parses as
// JSON is indented, anything else is written as stored.
//
// bodyExact writes the published bytes (indented for JSON — that transform
// was already the contract — but no escaping). bodyEscaped runs the result
// through EscapeForTerminal. The JSON case needs the escape pass in both
// modes' terms: encoding/json escapes C0 inside strings but leaves C1 raw, so
// an indented body would still hand a terminal a live U+009B.
func writeBoardBody(w io.Writer, payload []byte, mode bodyMode) {
	if json.Valid(payload) {
		var buf bytes.Buffer
		_ = json.Indent(&buf, payload, "", "  ")
		payload = buf.Bytes()
	}
	if mode == bodyEscaped {
		fmt.Fprintln(w, EscapeForTerminal(payload))
		return
	}
	w.Write(payload) //nolint:errcheck
	fmt.Fprintln(w)
}
