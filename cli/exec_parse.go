package cli

import (
	"regexp"
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

// scanExec looks for the completion (END) sentinel in captured. Until a whole
// line that is exactly `<end><digits>` appears, it returns done=false — this is
// the synchronous completion signal, and because it is matched against the
// whole accumulated buffer, a sentinel split across two PTY frames never
// completes early.
//
// The echoed input line (which literally contains both marker substrings inside
// the `printf` arguments) is never mistaken for a boundary: the anchored `^…$`
// patterns require the marker to stand alone on its own line, and the END
// pattern additionally requires it to be followed by digits and the line end.
//
// When done, output is the verbatim byte region strictly between the START
// sentinel's output line and the END sentinel's line (ring-replay content that
// precedes the START line is thereby excluded).
func scanExec(captured []byte, s execSentinels) execScan {
	endRe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(s.end) + `([0-9]+)\r?$`)
	em := endRe.FindSubmatchIndex(captured)
	if em == nil {
		return execScan{done: false, exitCode: -1}
	}
	code, err := strconv.Atoi(string(captured[em[2]:em[3]]))
	if err != nil {
		code = -1
	}

	// Output begins right after the START sentinel's own line. sm[1] indexes the
	// '\n' terminating that line (the `\r?$` consumes a trailing CR), so the
	// output starts at sm[1]+1. Fall back to 0 if the START line is somehow
	// absent (both markers are injected together, so this is defensive only).
	startRe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(s.start) + `\r?$`)
	outStart := 0
	if sm := startRe.FindIndex(captured); sm != nil {
		outStart = sm[1] + 1
	}
	outEnd := em[0] // first byte of the END sentinel line; the '\n' before it bounds the last output line

	if outStart < 0 || outStart > outEnd || outEnd > len(captured) {
		return execScan{done: true, exitCode: code, output: nil}
	}
	return execScan{done: true, exitCode: code, output: captured[outStart:outEnd]}
}

// partialOutput returns the best-effort command output when the END sentinel
// never arrived (timeout / early stream close): the bytes after the START
// sentinel's line, or nil if even the START line is not yet visible. Used only
// on the timeout path — the happy path uses scanExec's precise slice.
func partialOutput(b []byte, s execSentinels) []byte {
	startRe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(s.start) + `\r?$`)
	if sm := startRe.FindIndex(b); sm != nil && sm[1]+1 <= len(b) {
		return b[sm[1]+1:]
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
