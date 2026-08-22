// Package vtgrid is a headless terminal screen model: PTY bytes in, a
// character grid out.
//
// It exists because the harness renders session screens in several places —
// the CLI's snapshot, the TUI's pane view, and potentially the server, which
// today keeps only a byte ring plus two partial escape-sequence scanners
// (server/mode_tracker.go, server/replay_ground_scan.go) that approximate the
// terminal state a real model would hold.
//
// It is deliberately NOT a general-purpose emulator. The scope was chosen by
// measuring what real sessions actually emit: across the captured corpora in
// testdata/vtcorpus there are 37 distinct CSI final bytes, ten OSC commands
// and seven ESC finals (run TestVTCorpusCoverage). Everything outside that set
// is recognised well enough to be skipped without corrupting the grid, and
// nothing more — which is not a shortcut but the load-bearing behaviour: three
// agent corpora (codex, agy, opencode) plus htop were added after the fact,
// carrying the Kitty keyboard protocol, cursor-shape queries, VPA and OSC
// 1337/99 — almost none of which this implements — and every one rendered at
// 100% parity on first contact.
//
// In particular there is no scrollback here — the harness's 1 MiB replay ring
// already is the scrollback, and holding a second copy as cells is what makes
// a general emulator expensive.
//
// What it does not model, and why:
//
//   - Combining marks. A Cell holds one rune, not a grapheme cluster, so a
//     zero-width mark is dropped rather than attached. Storing clusters costs a
//     string header per cell.
//   - Reflow on resize. Rewrapping needs to know which line breaks were soft;
//     this model does not record that, so a resize keeps the top-left anchor
//     and is approximate.
//   - Sixel, kitty graphics, mouse reporting, and most DCS. Nothing in the
//     corpora emits them; they are consumed as opaque strings.
package vtgrid

import (
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// runeWidth measures a glyph's column span with an EXPLICIT condition rather
// than go-runewidth's package-level helpers.
//
// Those consult a DefaultCondition that is initialised from the environment:
// under an East Asian locale it reports the "ambiguous" width class as 2, and
// U+2500 BOX DRAWINGS LIGHT HORIZONTAL is in that class. A rule drawn across a
// 150-column screen then renders as 75 glyphs on a ja_JP machine and 150 on a
// C-locale one, from the same bytes. A screen model whose output depends on the
// reader's locale is not a screen model, so the condition is pinned here.
var widthCond = &runewidth.Condition{EastAsianWidth: false, StrictEmojiNeutral: true}

func runeWidth(r rune) int { return widthCond.RuneWidth(r) }

// Terminal is a screen that bytes are written into. It is not safe for
// concurrent use; the caller owns serialisation, exactly as with an io.Writer.
type Terminal struct {
	pri, alt *screen
	scr      *screen // whichever of the two is current

	cols, rows int

	x, y       int
	pen        pen
	autowrap   bool
	originMode bool
	cursorVis  bool

	// pendingWrap defers the wrap that a write in the last column implies.
	// Writing at the right margin does NOT move the cursor to the next line
	// straight away — the terminal stays put and wraps only when the *next*
	// glyph arrives. Getting this wrong shifts everything after a full line by
	// one row, so it is the first thing to check when a render is off by a
	// line.
	pendingWrap bool

	saved     savedCursor
	savedAlt  savedCursor
	tabs      []bool
	title     string
	altActive bool

	// Parser state, persisted across Write calls so a sequence split across
	// two frames is still recognised at the boundary.
	st       parseState
	params   []byte
	strIntro byte
	strBuf   []byte
	utf8buf  [4]byte
	utf8n    int
}

type savedCursor struct {
	x, y       int
	pen        pen
	originMode bool
	valid      bool
}

type parseState int

const (
	stGround parseState = iota
	stEsc
	stEscInt
	stCSI
	stStr
	stStrEsc
)

const (
	maxParams = 128
	maxString = 4096 // an OSC title or hyperlink; anything longer is truncated
)

// New builds a Terminal of the given size. A size of zero in either dimension
// is raised to one, because a zero-sized grid has no valid cursor position and
// every subsequent operation would have to special-case it.
func New(cols, rows int) *Terminal {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	t := &Terminal{cols: cols, rows: rows, autowrap: true, cursorVis: true}
	t.pri = newScreen(cols, rows, Color{})
	t.alt = newScreen(cols, rows, Color{})
	t.scr = t.pri
	t.resetTabs()
	return t
}

func (t *Terminal) resetTabs() {
	t.tabs = make([]bool, t.cols)
	for x := 8; x < t.cols; x += 8 {
		t.tabs[x] = true
	}
}

// Size reports the current grid dimensions.
func (t *Terminal) Size() (cols, rows int) { return t.cols, t.rows }

// Cursor reports where the next glyph would land, and whether the cursor is
// shown. The position is always inside the grid.
func (t *Terminal) Cursor() (x, y int, visible bool) { return t.x, t.y, t.cursorVis }

// AltScreen reports whether a full-screen application currently holds the
// alternate buffer. A reader uses this to know that the visible content will
// vanish when the app exits, rather than being scrollback.
func (t *Terminal) AltScreen() bool { return t.altActive }

// Title is the last window title set via OSC 0 or OSC 2, or "" if none was
// seen. It is not on the grid, and for an agent pane it is often the most
// informative byte on the wire — a spinner glyph there is re-asserted on every
// tick, which no cell is.
func (t *Terminal) Title() string { return t.title }

// CellAt returns the cell at the given position, or the zero Cell when out of
// bounds. A cell whose Width is 0 is the continuation of the wide glyph to its
// left and carries no character of its own.
func (t *Terminal) CellAt(x, y int) Cell {
	if x < 0 || y < 0 || x >= t.cols || y >= t.rows {
		return Cell{}
	}
	return t.scr.lines[y][x]
}

// Lines renders the current buffer as one string per row, exactly rows long.
// Trailing blanks are kept: whether they matter is the caller's decision, and
// trimming here would make Lines()[y] stop lining up with column indices.
func (t *Terminal) Lines() []string {
	out := make([]string, t.rows)
	var b strings.Builder
	for y := 0; y < t.rows; y++ {
		b.Reset()
		b.Grow(t.cols)
		for x := 0; x < t.cols; x++ {
			c := t.scr.lines[y][x]
			if c.Width == 0 {
				continue // continuation of the wide glyph to the left
			}
			if c.Rune == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(c.Rune)
		}
		out[y] = b.String()
	}
	return out
}

// String is Lines joined by newlines.
func (t *Terminal) String() string { return strings.Join(t.Lines(), "\n") }

// Resize changes the grid size. See screen.resize for what happens to content.
func (t *Terminal) Resize(cols, rows int) {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	if cols == t.cols && rows == t.rows {
		return
	}
	t.pri.resize(cols, rows, t.pen.BG)
	t.alt.resize(cols, rows, t.pen.BG)
	t.cols, t.rows = cols, rows
	t.resetTabs()
	t.clampCursor()
	t.pendingWrap = false
}

func (t *Terminal) clampCursor() {
	if t.x >= t.cols {
		t.x = t.cols - 1
	}
	if t.y >= t.rows {
		t.y = t.rows - 1
	}
	if t.x < 0 {
		t.x = 0
	}
	if t.y < 0 {
		t.y = 0
	}
}

// Write feeds terminal output into the grid. It never fails and never blocks:
// unlike a full emulator there is no response channel, because a headless
// screen has nobody to answer a device query. Queries are parsed and dropped.
func (t *Terminal) Write(p []byte) (int, error) {
	for _, b := range p {
		t.feed(b)
	}
	return len(p), nil
}

func (t *Terminal) feed(b byte) {
	switch t.st {
	case stGround:
		t.ground(b)
	case stEsc:
		t.escape(b)
	case stEscInt:
		// Intermediate bytes of a two-or-more byte escape (charset designators
		// like ESC ( B). The final byte selects; we honour none of them, but
		// they must be consumed so the payload does not print.
		if b >= 0x20 && b <= 0x2f {
			return
		}
		t.st = stGround
	case stCSI:
		t.csi(b)
	case stStr:
		t.str(b)
	case stStrEsc:
		if b == '\\' {
			t.endString()
			t.st = stGround
		} else if b == 0x1b {
			// stay: ESC ESC inside a string
		} else {
			t.strAppend(0x1b)
			t.strAppend(b)
			t.st = stStr
		}
	}
}

func (t *Terminal) ground(b byte) {
	switch {
	case b == 0x1b:
		t.utf8n = 0
		t.st = stEsc
	case b < 0x20:
		t.utf8n = 0
		t.control(b)
	case b == 0x7f:
		// DEL is discarded by a terminal, not printed.
	default:
		if t.utf8n > 0 || b >= 0x80 {
			t.utf8buf[t.utf8n] = b
			t.utf8n++
			switch {
			case utf8.FullRune(t.utf8buf[:t.utf8n]):
				r, size := utf8.DecodeRune(t.utf8buf[:t.utf8n])
				t.utf8n = 0
				// A lone continuation byte decodes as RuneError over one byte.
				// That is what the head of a ring replay looks like when the
				// frames holding the start of a multi-byte glyph were evicted,
				// so it is a routine input, not a corrupt stream — drop it
				// rather than printing a replacement glyph nobody emitted. A
				// genuine U+FFFD is three bytes wide and still prints.
				if r == utf8.RuneError && size == 1 {
					return
				}
				t.print(r)
			case t.utf8n == len(t.utf8buf):
				t.utf8n = 0
			}
			return
		}
		t.print(rune(b))
	}
}

func (t *Terminal) control(b byte) {
	switch b {
	case 0x07: // BEL
	case 0x08: // BS
		if t.pendingWrap {
			t.pendingWrap = false
		} else if t.x > 0 {
			t.x--
		}
	case 0x09: // HT
		t.tab()
	case 0x0a, 0x0b, 0x0c: // LF, VT, FF
		t.lineFeed()
	case 0x0d: // CR
		t.x = 0
		t.pendingWrap = false
	}
}

// tab advances to the next tab stop, or to the last column when none is left.
//
// It cancels a deferred wrap only when the cursor ACTUALLY MOVES. That
// distinction is not pedantry: with every tab stop cleared (CSI 3 g) a tab
// issued while sitting in the last column can go nowhere, and treating the
// no-op as a movement swallows the pending wrap, so the next glyph overwrites
// the last one instead of starting a new row. A line of tab-separated fields
// then collapses onto a single cell. Found by differential test against x/vt,
// which resolves the wrap at the next print rather than at the tab.
func (t *Terminal) tab() {
	for x := t.x + 1; x < t.cols; x++ {
		if t.tabs[x] {
			t.x = x
			t.pendingWrap = false
			return
		}
	}
	if t.x != t.cols-1 {
		t.x = t.cols - 1
		t.pendingWrap = false
	}
}

func (t *Terminal) lineFeed() {
	t.pendingWrap = false
	if t.y == t.scr.bottom {
		t.scr.scrollUp(1, t.pen.BG)
		return
	}
	if t.y < t.rows-1 {
		t.y++
	}
}

func (t *Terminal) reverseIndex() {
	t.pendingWrap = false
	if t.y == t.scr.top {
		t.scr.scrollDown(1, t.pen.BG)
		return
	}
	if t.y > 0 {
		t.y--
	}
}

// print places one glyph, honouring deferred wrap and wide-glyph pairing.
func (t *Terminal) print(r rune) {
	w := runeWidth(r)
	if w == 0 {
		// Combining mark. A Cell holds one rune, so there is nowhere to attach
		// it; dropping it is a known, documented divergence rather than a
		// silent one.
		return
	}
	if t.pendingWrap {
		t.pendingWrap = false
		t.x = 0
		t.lineFeed()
	}
	if t.x+w > t.cols {
		// A wide glyph that does not fit the last column moves to the next row
		// whole; terminals do not split one across the margin.
		if !t.autowrap {
			return
		}
		t.x = 0
		t.lineFeed()
	}
	row := t.scr.lines[t.y]
	// Landing on the continuation half of an existing wide glyph orphans its
	// head; blank the head so a stale character does not survive underneath.
	if t.x > 0 && row[t.x].Width == 0 {
		row[t.x-1] = blank(t.pen.BG)
	}
	if t.x+1 < t.cols && row[t.x].Width == 2 {
		row[t.x+1] = blank(t.pen.BG)
	}
	row[t.x] = Cell{Rune: r, Width: int8(w), Attr: t.pen.Attr, FG: t.pen.FG, BG: t.pen.BG}
	if w == 2 && t.x+1 < t.cols {
		row[t.x+1] = Cell{Width: 0, Attr: t.pen.Attr, FG: t.pen.FG, BG: t.pen.BG}
	}
	t.x += w
	if t.x >= t.cols {
		t.x = t.cols - 1
		if t.autowrap {
			t.pendingWrap = true
		}
	}
}

func (t *Terminal) escape(b byte) {
	t.st = stGround
	switch b {
	case '[':
		t.params = t.params[:0]
		t.st = stCSI
	case ']', 'P', 'X', '^', '_':
		t.strIntro = b
		t.strBuf = t.strBuf[:0]
		t.st = stStr
	case 0x1b:
		t.st = stEsc // ESC ESC: wait for the real intro byte
	case '(', ')', '*', '+', '-', '.', '/', '#', ' ':
		t.st = stEscInt
	case 'M': // RI
		t.reverseIndex()
	case 'D': // IND
		t.lineFeed()
	case 'E': // NEL
		t.x = 0
		t.lineFeed()
	case 'H': // HTS
		if t.x >= 0 && t.x < t.cols {
			t.tabs[t.x] = true
		}
	case '7': // DECSC
		t.saveCursor()
	case '8': // DECRC
		t.restoreCursor()
	case 'c': // RIS
		t.reset()
	case '=', '>': // keypad modes — no grid effect
	}
}

func (t *Terminal) saveCursor() {
	s := savedCursor{x: t.x, y: t.y, pen: t.pen, originMode: t.originMode, valid: true}
	if t.altActive {
		t.savedAlt = s
	} else {
		t.saved = s
	}
}

func (t *Terminal) restoreCursor() {
	s := t.saved
	if t.altActive {
		s = t.savedAlt
	}
	if !s.valid {
		t.x, t.y = 0, 0
		return
	}
	t.x, t.y, t.pen, t.originMode = s.x, s.y, s.pen, s.originMode
	t.pendingWrap = false
	t.clampCursor()
}

func (t *Terminal) reset() {
	t.pen.reset()
	t.autowrap, t.originMode, t.cursorVis = true, false, true
	t.pendingWrap = false
	t.x, t.y = 0, 0
	t.altActive = false
	t.scr = t.pri
	t.pri.top, t.pri.bottom = 0, t.rows-1
	t.alt.top, t.alt.bottom = 0, t.rows-1
	for y := 0; y < t.rows; y++ {
		t.pri.clearRow(y, Color{})
		t.alt.clearRow(y, Color{})
	}
	t.resetTabs()
}

func (t *Terminal) csi(b byte) {
	switch {
	case b >= 0x40 && b <= 0x7e:
		t.dispatchCSI(b)
		t.st = stGround
	default:
		if len(t.params) < maxParams {
			t.params = append(t.params, b)
		}
	}
}

// str consumes an OSC/DCS/APC/PM/SOS payload.
//
// Only BEL and the seven-bit ST (ESC \) terminate it. The eight-bit ST, 0x9C,
// is deliberately NOT honoured: in a UTF-8 stream 0x80-0xBF are continuation
// bytes, and 0x9C is the second byte of U+2733 ✳ — the glyph Claude Code puts
// in its window title. Accepting it as a terminator ends the sequence one byte
// into the title and prints the rest of it onto the grid. That is precisely the
// defect cli/snapshot_native.go documents in x/vt's Title callback ("truncates
// a title at its first multi-byte character"); this implementation reproduced
// it before the case was removed, which is how the cause was identified.
func (t *Terminal) str(b byte) {
	switch b {
	case 0x07:
		t.endString()
		t.st = stGround
	case 0x1b:
		t.st = stStrEsc
	default:
		t.strAppend(b)
	}
}

func (t *Terminal) strAppend(b byte) {
	if len(t.strBuf) < maxString {
		t.strBuf = append(t.strBuf, b)
	}
}

// endString handles a completed OSC/DCS/APC/PM/SOS. Only OSC 0 and OSC 2 have
// a grid-visible effect; the rest are consumed so their payload does not print.
func (t *Terminal) endString() {
	if t.strIntro != ']' {
		return
	}
	s := string(t.strBuf)
	i := strings.IndexByte(s, ';')
	if i < 0 {
		return
	}
	switch s[:i] {
	case "0", "2":
		title := s[i+1:]
		if utf8.ValidString(title) {
			t.title = title
		}
	}
}
