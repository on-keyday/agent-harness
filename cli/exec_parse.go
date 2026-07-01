package cli

import (
	"bytes"
	"strconv"
	"unicode/utf8"
)

// execSentinels are the two marker strings that bracket one exec invocation's
// output on the session PTY. They are injected as `printf '<start>\n'; <cmd>;
// printf '<end>%s\n' "$?"` so that the START and END markers appear alone on
// their own output lines around the command's combined output, and the END
// marker is immediately followed by the command's exit code. The marker strings
// embed a random nonce so they cannot collide with a shell prompt or program
// output.
type execSentinels struct {
	start string // e.g. "__HEXEC_<nonce>_S__"
	end   string // e.g. "__HEXEC_<nonce>_E__"
}

// execScan is the outcome of scanning an accumulated cowrite byte capture.
type execScan struct {
	done     bool   // the END sentinel line was seen (command finished)
	exitCode int    // command exit code (valid when done; -1 otherwise)
	output   []byte // verbatim bytes of the command-output region (empty until done)
}

// scanExec looks for the completion (END) sentinel in captured. Until a
// standalone line `<end><digits>` appears, it returns done=false — this is the
// synchronous completion signal, and because it is matched against the whole
// accumulated buffer, a sentinel split across two PTY frames never completes
// early.
//
// A "standalone" marker is preceded by a line boundary (buffer start, '\r', OR
// '\n') and, for END, followed by digits then a boundary. Accepting a bare '\r'
// as a boundary is load-bearing: a real bash PTY echoes the injected Enter as a
// lone CR (no LF) right before the START sentinel's output line, so keying only
// on '\n' would miss it. The echoed input line (which contains both markers
// inside the printf arguments) is never matched: there the marker is preceded
// by a quote, not a boundary, and END is followed by "%s" not digits.
//
// When done, output is the verbatim byte region between the START sentinel's
// line and the END sentinel's line (ring-replay and prompt/echo bytes that
// precede the START line are thereby excluded).
func scanExec(captured []byte, s execSentinels) execScan {
	endStart, code, ok := findEndSentinel(captured, s.end)
	if !ok {
		return execScan{done: false, exitCode: -1}
	}
	outStart := 0
	if cs, ok := findStartContent(captured, s.start); ok {
		outStart = cs
	}
	if outStart < 0 || outStart > endStart {
		return execScan{done: true, exitCode: code, output: nil}
	}
	return execScan{done: true, exitCode: code, output: captured[outStart:endStart]}
}

// precededByBoundary reports whether position i begins a line: it is the buffer
// start, or the previous byte is '\r' or '\n'.
func precededByBoundary(b []byte, i int) bool {
	return i == 0 || b[i-1] == '\n' || b[i-1] == '\r'
}

// findStartContent returns the index where command output begins — just past
// the standalone START marker's own line terminator (a lone '\r', lone '\n', or
// '\r\n'). ok=false if no standalone occurrence exists yet.
func findStartContent(b []byte, marker string) (int, bool) {
	m := []byte(marker)
	for from := 0; ; {
		idx := bytes.Index(b[from:], m)
		if idx < 0 {
			return 0, false
		}
		pos := from + idx
		after := pos + len(m)
		if precededByBoundary(b, pos) && (after >= len(b) || b[after] == '\r' || b[after] == '\n') {
			j := after
			if j < len(b) && b[j] == '\r' {
				j++
			}
			if j < len(b) && b[j] == '\n' {
				j++
			}
			return j, true
		}
		from = pos + 1
	}
}

// findEndSentinel returns the byte index at which the standalone END sentinel
// line starts (its first marker byte), the parsed exit code, and ok. The marker
// must be preceded by a boundary, followed by one+ digits, then a boundary.
func findEndSentinel(b []byte, marker string) (lineStart, code int, ok bool) {
	m := []byte(marker)
	for from := 0; ; {
		idx := bytes.Index(b[from:], m)
		if idx < 0 {
			return 0, 0, false
		}
		pos := from + idx
		if precededByBoundary(b, pos) {
			k := pos + len(m)
			d := k
			for d < len(b) && b[d] >= '0' && b[d] <= '9' {
				d++
			}
			if d > k && (d >= len(b) || b[d] == '\r' || b[d] == '\n') {
				if n, err := strconv.Atoi(string(b[k:d])); err == nil {
					return pos, n, true
				}
			}
		}
		from = pos + 1
	}
}

// partialOutput returns the best-effort command output when the END sentinel
// never arrived (timeout / early stream close): the bytes after the START
// sentinel's line, or nil if even the START line is not yet visible. Used only
// on the timeout path — the happy path uses scanExec's precise slice.
func partialOutput(b []byte, s execSentinels) []byte {
	if cs, ok := findStartContent(b, s.start); ok {
		return b[cs:]
	}
	return nil
}

// interpretPlain renders raw PTY output bytes into the plain text a terminal
// would display, then returns it. It strips SGR/CSI/OSC escape sequences,
// applies carriage-return (`\r`) overwrites and erase-line control, keeps `\n`
// as a logical line break, and imposes NO terminal width — so long logical
// lines are never soft-wrapped (the whole point: greppable un-wrapped output).
// It is a line-oriented interpretation of the byte stream, not a fixed-width VT
// grid render (which is what causes snapshot's soft-wrap splitting).
func interpretPlain(b []byte) string {
	var out []byte
	var line []rune
	cursor := 0

	flush := func() {
		out = append(out, []byte(string(line))...)
		out = append(out, '\n')
		line = line[:0]
		cursor = 0
	}

	i, n := 0, len(b)
	for i < n {
		c := b[i]
		switch {
		case c == '\n':
			flush()
			i++
		case c == '\r':
			cursor = 0
			i++
		case c == 0x1b: // ESC — CSI / OSC / other escape
			i = consumeEscape(b, i, &line, &cursor)
		case c == '\t':
			putRune(&line, &cursor, '\t')
			i++
		case c < 0x20: // other C0 control (BEL, backspace, …) — drop
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				// invalid byte; skip it rather than emit a replacement char
				i++
				continue
			}
			putRune(&line, &cursor, r)
			i += size
		}
	}
	// Trailing partial line (input did not end in '\n') is emitted without a
	// terminating newline.
	out = append(out, []byte(string(line))...)
	return string(out)
}

// putRune writes r at the cursor column of line, overwriting an existing cell
// (as a terminal does after a `\r`) or padding with spaces and appending when
// the cursor is at/after the current line end.
func putRune(line *[]rune, cursor *int, r rune) {
	c := *cursor
	if c < len(*line) {
		(*line)[c] = r
	} else {
		for len(*line) < c {
			*line = append(*line, ' ')
		}
		*line = append(*line, r)
	}
	*cursor = c + 1
}

// consumeEscape processes one escape sequence beginning at b[i] (b[i]==0x1b),
// applying erase-line effects to line/cursor and dropping everything else
// (SGR, cursor movement, OSC strings). It returns the index just past the
// consumed sequence.
func consumeEscape(b []byte, i int, line *[]rune, cursor *int) int {
	if i+1 >= len(b) {
		return i + 1 // dangling ESC
	}
	switch b[i+1] {
	case '[': // CSI: params/intermediates until a final byte 0x40..0x7e
		j := i + 2
		for j < len(b) && !(b[j] >= 0x40 && b[j] <= 0x7e) {
			j++
		}
		if j >= len(b) {
			return len(b) // incomplete CSI: drop the remainder
		}
		applyCSI(b[j], string(b[i+2:j]), line, cursor)
		return j + 1
	case ']': // OSC: string until BEL or ST (ESC \)
		j := i + 2
		for j < len(b) {
			if b[j] == 0x07 {
				return j + 1
			}
			if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return len(b)
	default:
		return i + 2 // other ESC (e.g. charset designator): drop ESC + next byte
	}
}

// applyCSI handles the CSI sequences that affect visible line content —
// erase-in-line (K) and, approximately, erase-in-display (J). SGR ('m') and all
// cursor-movement finals are intentionally ignored (stripped).
func applyCSI(final byte, params string, line *[]rune, cursor *int) {
	switch final {
	case 'K':
		switch params {
		case "", "0": // cursor to end of line
			if *cursor <= len(*line) {
				*line = (*line)[:*cursor]
			}
		case "1": // start of line to cursor → blank with spaces
			for k := 0; k < *cursor && k < len(*line); k++ {
				(*line)[k] = ' '
			}
		case "2": // whole line
			*line = (*line)[:0]
		}
	case 'J':
		switch params {
		case "", "0":
			if *cursor <= len(*line) {
				*line = (*line)[:*cursor]
			}
		case "2":
			*line = (*line)[:0]
		}
	}
}
