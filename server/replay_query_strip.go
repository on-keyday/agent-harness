package server

import (
	"bytes"
	"encoding/binary"

	"github.com/on-keyday/objtrsf/exec/frame"
)

// stripReplyQueries removes reply-soliciting sequences from bytes that are
// about to be REPLAYED to a (re)attaching client.
//
// Why the replay and only the replay. A terminal query is a question whose
// answer arrives as INPUT, and the answer is only meaningful to the process
// that asked it. Replaying the ring re-asks every question the session ever
// asked, to a terminal that answers all of them, at a moment when the asker is
// usually long gone — so the answers land in whatever is reading the pty now.
// Observed: a palette-probing TUI (herdr/htop class) emits OSC 4 for all 256
// entries plus OSC 10/11 at startup; those sit in the ring, and each reattach
// makes the operator's terminal recite 258 replies at a shell prompt, where
// readline eats the ESC] introducer and self-inserts `4;0;rgb:0c0c/0c0c/0c0c`
// and friends across the screen.
//
// The LIVE path is deliberately left alone (server/session_mux.go runnerPump
// fans out unfiltered). A running app must be able to ask the real terminal and
// get a real answer; that round-trip is the design, and it is correct exactly
// while the asker is still there to read the reply.
//
// Parsing mirrors vtGroundScan's shape (same 7-bit intro bytes, same BEL / ST
// string terminators) rather than inventing a second dialect. It is a copy in
// structure only: that scanner reports whether a crop is safe and must stay
// allocation-free, while this one has to buffer each sequence to decide
// keep-or-drop.
func stripReplyQueries(in []byte) []byte {
	// Overwhelmingly common case: nothing to strip. Every query form below
	// begins ESC[ , ESC] or ESCP, so a stream with none of those is returned
	// as-is without copying — the replay of an ordinary shell session.
	if !bytes.ContainsAny(in, "\x1b") {
		return in
	}
	var s queryStripper
	out := s.strip(in)
	// A sequence the input cut in half never completes, so the terminal will
	// never answer it either. Pass it through instead of guessing: dropping it
	// would silently lose real output whose tail simply has not arrived.
	return append(out, s.pending()...)
}

// queryStripper is stripReplyQueries' state, kept across calls so a sequence
// SPLIT ACROSS FRAMES is still recognised at the boundary. The ring stores wire
// frames, and an app's output is chunked by write size, not by sequence — a
// 258-query palette burst lands across several frames with queries straddling
// them. A per-frame stateless pass would miss exactly those.
//
// Bytes of a sequence still being accumulated when a chunk ends are HELD (they
// are in pending(), not in that chunk's output) and emitted with the chunk that
// completes it, if it is kept. Byte order is preserved, which is all a terminal
// cares about; only the frame boundary they sit on moves.
type queryStripper struct {
	st  gsState
	seq []byte
}

// pending returns the bytes of an incomplete sequence held back so far.
func (s *queryStripper) pending() []byte { return s.seq }

// strip filters one chunk, carrying parser state in and out.
func (s *queryStripper) strip(in []byte) []byte {
	out := make([]byte, 0, len(in)+len(s.seq))
	seq := s.seq
	st := s.st
	// flush appends the accumulated sequence unless it asks the terminal
	// something.
	flush := func() {
		if !solicitsReply(seq) {
			out = append(out, seq...)
		}
		seq = seq[:0]
	}
	for _, c := range in {
		// CAN / SUB abort any sequence in progress. Whatever was accumulated is
		// no longer a sequence, so it is passed through rather than judged.
		if c == 0x18 || c == 0x1a {
			out = append(out, seq...)
			out = append(out, c)
			seq = seq[:0]
			st = gsGround
			continue
		}
		switch st {
		case gsGround:
			if c == 0x1b {
				st = gsEsc
				seq = append(seq, c)
			} else {
				out = append(out, c)
			}
		case gsEsc:
			seq = append(seq, c)
			switch {
			case c == '[':
				st = gsCSI
			case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
				st = gsStr
			case c == 0x1b:
				// ESC ESC: the first is not part of a sequence after all.
				out = append(out, seq[:len(seq)-1]...)
				seq = seq[:1]
				seq[0] = 0x1b
			case c >= 0x20 && c <= 0x2f:
				st = gsEscInt
			default:
				st = gsGround // two-byte escape, complete
				flush()
			}
		case gsEscInt:
			seq = append(seq, c)
			switch {
			case c >= 0x20 && c <= 0x2f:
				// more intermediates
			case c == 0x1b:
				st = gsEsc
			default:
				st = gsGround
				flush()
			}
		case gsCSI:
			seq = append(seq, c)
			switch {
			case c >= 0x40 && c <= 0x7e:
				st = gsGround
				flush()
			case c == 0x1b:
				// Resync: the run so far was never terminated, so keep it and
				// start over from this ESC.
				out = append(out, seq[:len(seq)-1]...)
				seq = seq[:1]
				seq[0] = 0x1b
				st = gsEsc
			}
		case gsStr:
			seq = append(seq, c)
			switch c {
			case 0x07, 0x9c: // BEL or 8-bit ST
				st = gsGround
				flush()
			case 0x1b:
				st = gsStrEsc
			}
		case gsStrEsc:
			seq = append(seq, c)
			switch c {
			case '\\': // ESC '\' = ST
				st = gsGround
				flush()
			case 0x1b:
				// stay, still awaiting '\'
			default:
				st = gsStr // the ESC was string data
			}
		}
	}
	s.seq = seq
	s.st = st
	return out
}

// stripReplyQueriesFrames is stripReplyQueries over WIRE FRAMES — what the ring
// actually stores, and what the replay hands to a client.
//
// Filtering the concatenated bytes would be wrong in a way that shows up as a
// hung client rather than as stray text: each frame carries a 4-byte payload
// length, so removing bytes from a payload without rewriting its header leaves
// the header claiming more than follows, and the client's frame parser reads
// the next header out of payload bytes and desyncs for the rest of the stream.
// So each Stdout/Stderr payload is filtered and re-encoded at its new length,
// and a payload that filters down to nothing drops its frame entirely.
//
// Non-output frames (Control — e.g. TerminalWindowSize) pass through untouched
// and do NOT interrupt the scanner: they are out-of-band with respect to the
// terminal byte stream, so a sequence spanning them is still one sequence.
func stripReplyQueriesFrames(data []byte) []byte {
	if !bytes.ContainsAny(data, "\x1b") {
		return data
	}
	out := make([]byte, 0, len(data))
	var s queryStripper
	off := 0
	for off < len(data) {
		if len(data)-off < frameHeaderSize {
			break // malformed tail: pass the remainder through
		}
		total := frameHeaderSize + int(binary.BigEndian.Uint32(data[off+1:off+5]))
		if total <= 0 || off+total > len(data) {
			break // malformed frame: don't rewrite what we cannot parse
		}
		fb := data[off : off+total]
		switch frame.FrameType(fb[0]) {
		case frame.FrameType_Stdout, frame.FrameType_Stderr:
			filtered := s.strip(fb[frameHeaderSize:])
			if len(filtered) > 0 {
				out = append(out, encodeFrame(frame.FrameType(fb[0]), filtered)...)
			}
		default:
			out = append(out, fb...)
		}
		off += total
	}
	// Whatever the walk could not parse goes out verbatim, and so does any
	// held-back partial sequence — as its own frame, since the frame it came
	// from has already been written at its filtered length.
	if pend := s.pending(); len(pend) > 0 {
		out = append(out, encodeStdoutFrame(pend)...)
	}
	out = append(out, data[off:]...)
	return out
}

// solicitsReply reports whether one COMPLETE escape sequence asks the terminal
// to send something back.
func solicitsReply(seq []byte) bool {
	if len(seq) < 2 || seq[0] != 0x1b {
		return false
	}
	switch seq[1] {
	case '[':
		return csiSolicitsReply(seq[2:])
	case ']':
		return oscSolicitsReply(seq[2:])
	case 'P':
		// DCS. XTGETTCAP is `+q<names>`, DECRQSS is `$q<setting>`; both are
		// requests. Every other DCS (sixel, DECRQSS *responses*) is not.
		body := seq[2:]
		return len(body) >= 2 && body[1] == 'q' && (body[0] == '+' || body[0] == '$')
	}
	return false
}

// csiSolicitsReply judges a CSI body (everything after "ESC["), final byte
// included.
func csiSolicitsReply(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	final := body[len(body)-1]
	params := body[:len(body)-1]
	switch final {
	case 'n':
		// DSR (CSI Ps n) and DECDSR (CSI ? Ps n). A bare `CSI n` is the same
		// request with a default parameter. Nothing sends a CSI-n downstream
		// for any other purpose — the 0/3 forms are the terminal's ANSWER, and
		// an answer never appears in output headed TO a terminal.
		return true
	case 'c':
		// DA1 (CSI c), DA2 (CSI > c), DA3 (CSI = c).
		return true
	case 'p':
		// DECRQM: CSI [?] Ps $ p. The '$' intermediate is what separates it
		// from DECSCL / DECSTR, which ask nothing.
		return bytes.HasSuffix(params, []byte("$"))
	case 'q':
		// XTVERSION is CSI > Ps q. Without the '>' this is DECSCUSR, which
		// SETS the cursor shape and must survive — dropping it would change
		// how the replayed screen looks.
		return len(params) > 0 && params[0] == '>'
	}
	return false
}

// oscSolicitsReply judges an OSC body (everything after "ESC]"), terminator
// included.
//
// The query form across every OSC that has one — palette (4), the dynamic
// colours (10-19), and the clipboard (52) — is a final parameter of exactly
// "?". Keying on that instead of on a list of numbers keeps a colour SET (which
// carries an rgb: value) and a title (which carries text), and needs no update
// when a terminal grows another queryable slot.
func oscSolicitsReply(body []byte) bool {
	// Trim the terminator: BEL, 8-bit ST, or ESC '\'.
	switch {
	case bytes.HasSuffix(body, []byte{0x07}), bytes.HasSuffix(body, []byte{0x9c}):
		body = body[:len(body)-1]
	case bytes.HasSuffix(body, []byte{0x1b, '\\'}):
		body = body[:len(body)-2]
	default:
		return false // not a complete OSC; caller passes it through
	}
	i := bytes.LastIndexByte(body, ';')
	if i < 0 {
		return false // no parameters at all, e.g. a malformed OSC
	}
	return string(body[i+1:]) == "?"
}
