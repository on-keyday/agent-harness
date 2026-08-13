package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// editBuffer is the text model behind the file-editor popup: logical lines,
// a cursor, and a window that renders ONLY the rows on screen.
//
// It exists because bubbles/textarea renders the whole document every frame —
// its View loops over every line, wraps and styles it into one string, and
// hands that to a viewport which then shows ~30 rows of it
// (bubbles@v1.0.0/textarea/textarea.go:1093-1200). That is O(document) per
// keystroke: measured 38ms for a 93KB Japanese file versus 3ms for 1KB, and
// shrinking the visible box did not help (24.9ms at 14 rows, 26.6ms at 40).
// Editing a real memo on Windows was visibly laggy.
//
// The rule that keeps this one O(visible): never compute an absolute display
// coordinate. The window is (top, topSeg) — a logical line plus which of its
// wrapped segments sits on the first row — and rendering walks FORWARD from
// there until height rows are filled. Scrolling nudges that pair by a row at
// a time; a cursor jump further than one screen recenters instead of walking.
// No code path outside the window touches a line.
//
// Deliberately not implemented: word wrap (this wraps by display cell, which
// is how Japanese wraps anyway and which keeps the cursor↔display mapping
// simple), selection, undo. This edits memos, not source.
type editBuffer struct {
	lines [][]rune // logical lines, without their newlines

	row, col int // cursor: line index, rune index within that line
	desired  int // sticky display column for up/down

	top, topSeg int // window origin: line index, wrapped-segment index
	width       int // display cells available for text
	height      int // display rows available

	focused bool
}

func newEditBuffer() *editBuffer {
	return &editBuffer{lines: [][]rune{{}}, width: 1, height: 1}
}

// SetSize sets the text area in display cells / rows.
func (b *editBuffer) SetSize(w, h int) {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	b.width, b.height = w, h
	b.scrollToCursor()
}

func (b *editBuffer) Focus()        { b.focused = true }
func (b *editBuffer) Blur()         { b.focused = false }
func (b *editBuffer) Focused() bool { return b.focused }

// SetValue replaces the document and puts the cursor at the end, matching
// what an editor does when it opens a file you are about to append to.
func (b *editBuffer) SetValue(s string) {
	parts := strings.Split(s, "\n")
	b.lines = make([][]rune, len(parts))
	for i, p := range parts {
		b.lines[i] = []rune(p)
	}
	b.row = len(b.lines) - 1
	b.col = len(b.lines[b.row])
	b.desired = b.displayCol()
	b.top, b.topSeg = 0, 0
	b.scrollToCursor()
}

// Value renders the document back to a string.
func (b *editBuffer) Value() string {
	var sb strings.Builder
	for i, ln := range b.lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(string(ln))
	}
	return sb.String()
}

// ---- editing -------------------------------------------------------------

// InsertRunes inserts rs at the cursor. A bracketed paste arrives as one
// multi-rune event, so this takes a slice rather than a single rune; any
// newlines inside it split lines the same way typing Enter would.
//
// Input is sanitized first — see sanitizeInput. Skipping that shipped a bug
// where a control character from the terminal was written into the file and
// the next open refused it as "not editable text".
func (b *editBuffer) InsertRunes(rs []rune) {
	for _, r := range sanitizeInput(rs) {
		if r == '\n' {
			b.InsertNewline()
			continue
		}
		ln := b.lines[b.row]
		out := make([]rune, 0, len(ln)+1)
		out = append(out, ln[:b.col]...)
		out = append(out, r)
		out = append(out, ln[b.col:]...)
		b.lines[b.row] = out
		b.col++
	}
	b.desired = b.displayCol()
	b.scrollToCursor()
}

// InsertNewline splits the current line at the cursor.
func (b *editBuffer) InsertNewline() {
	ln := b.lines[b.row]
	head := append([]rune{}, ln[:b.col]...)
	tail := append([]rune{}, ln[b.col:]...)
	b.lines = append(b.lines, nil)
	copy(b.lines[b.row+2:], b.lines[b.row+1:])
	b.lines[b.row] = head
	b.lines[b.row+1] = tail
	b.row++
	b.col = 0
	b.desired = 0
	b.scrollToCursor()
}

// Backspace deletes the rune before the cursor, joining lines at column 0.
func (b *editBuffer) Backspace() {
	switch {
	case b.col > 0:
		ln := b.lines[b.row]
		b.lines[b.row] = append(ln[:b.col-1], ln[b.col:]...)
		b.col--
	case b.row > 0:
		prev := b.lines[b.row-1]
		joinAt := len(prev)
		b.lines[b.row-1] = append(prev, b.lines[b.row]...)
		b.lines = append(b.lines[:b.row], b.lines[b.row+1:]...)
		b.row--
		b.col = joinAt
	default:
		return // start of buffer
	}
	b.desired = b.displayCol()
	b.scrollToCursor()
}

// DeleteForward deletes the rune under the cursor, joining the next line when
// the cursor sits at end of line.
func (b *editBuffer) DeleteForward() {
	ln := b.lines[b.row]
	switch {
	case b.col < len(ln):
		b.lines[b.row] = append(ln[:b.col], ln[b.col+1:]...)
	case b.row < len(b.lines)-1:
		b.lines[b.row] = append(ln, b.lines[b.row+1]...)
		b.lines = append(b.lines[:b.row+1], b.lines[b.row+2:]...)
	default:
		return // end of buffer
	}
	b.desired = b.displayCol()
	b.scrollToCursor()
}

// ---- cursor --------------------------------------------------------------

func (b *editBuffer) CursorLeft() {
	if b.col > 0 {
		b.col--
	} else if b.row > 0 {
		b.row--
		b.col = len(b.lines[b.row])
	}
	b.desired = b.displayCol()
	b.scrollToCursor()
}

func (b *editBuffer) CursorRight() {
	if b.col < len(b.lines[b.row]) {
		b.col++
	} else if b.row < len(b.lines)-1 {
		b.row++
		b.col = 0
	}
	b.desired = b.displayCol()
	b.scrollToCursor()
}

// CursorUp / CursorDown move by LOGICAL line, keeping the sticky display
// column. Moving by wrapped segment would be closer to a full editor, but it
// makes every vertical keypress depend on the wrap of the line it leaves —
// for memo-length lines this is what people expect and it stays O(1).
func (b *editBuffer) CursorUp() {
	if b.row == 0 {
		b.col = 0
	} else {
		b.row--
		b.col = b.colForDisplay(b.row, b.desired)
	}
	b.scrollToCursor()
}

func (b *editBuffer) CursorDown() {
	if b.row >= len(b.lines)-1 {
		b.col = len(b.lines[b.row])
	} else {
		b.row++
		b.col = b.colForDisplay(b.row, b.desired)
	}
	b.scrollToCursor()
}

func (b *editBuffer) CursorHome() {
	b.col = 0
	b.desired = 0
	b.scrollToCursor()
}

func (b *editBuffer) CursorEnd() {
	b.col = len(b.lines[b.row])
	b.desired = b.displayCol()
	b.scrollToCursor()
}

func (b *editBuffer) CursorTop() {
	b.row, b.col, b.desired = 0, 0, 0
	b.scrollToCursor()
}

func (b *editBuffer) CursorBottom() {
	b.row = len(b.lines) - 1
	b.col = len(b.lines[b.row])
	b.desired = b.displayCol()
	b.scrollToCursor()
}

// displayCol is the cursor's column in display cells within its line.
func (b *editBuffer) displayCol() int {
	w := 0
	for _, r := range b.lines[b.row][:b.col] {
		w = advance(w, r)
	}
	return w
}

// colForDisplay maps a display column back to a rune index on line row,
// clamping to the end of a shorter line.
func (b *editBuffer) colForDisplay(row, want int) int {
	w := 0
	for i, r := range b.lines[row] {
		if w >= want {
			return i
		}
		w = advance(w, r)
	}
	return len(b.lines[row])
}

// ---- wrapping and window -------------------------------------------------

// tabStop is the column interval a tab advances to. Eight, because that is
// what terminals, cat, and git diff assume — a file edited here should look
// the same everywhere else it is opened.
const tabStop = 8

// advance returns the display column after drawing r at column col. Every
// width calculation in this file goes through it, because a tab's width is
// not a property of the rune: it depends on where the rune lands. Summing
// runewidth alone (which reports 0 for a tab) puts the cursor in the wrong
// cell on any line that contains one — a Makefile, for instance.
func advance(col int, r rune) int {
	if r == '\t' {
		return col + tabStop - col%tabStop
	}
	return col + runewidth.RuneWidth(r)
}

// expandTabs renders a run of text starting at display column start, turning
// tabs into the spaces they occupy. The terminal never receives a raw tab, so
// its own tab stops can never disagree with the model's.
func expandTabs(rs []rune, start int) string {
	var sb strings.Builder
	col := start
	for _, r := range rs {
		if r == '\t' {
			next := advance(col, r)
			sb.WriteString(strings.Repeat(" ", next-col))
			col = next
			continue
		}
		sb.WriteRune(r)
		col = advance(col, r)
	}
	return sb.String()
}

// sanitizeInput filters runes arriving from the terminal before they can
// reach the document. It mirrors what bubbles/textarea did through
// runeutil.Sanitizer (textarea.go:366) and which was lost when this buffer
// replaced that widget:
//
//   - control characters are dropped — a NUL reaching the file is what made
//     a saved memo fail to reopen ("not editable text"), and a Windows
//     console with an IME is exactly where stray control bytes come from
//   - utf8.RuneError is dropped rather than written as U+FFFD
//   - CRLF and a lone CR both collapse to a single newline, so pasting from
//     a Windows file does not double every line
//   - a tab is KEPT (rendering expands it; the file gets the real byte)
func sanitizeInput(rs []rune) []rune {
	out := make([]rune, 0, len(rs))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == utf8.RuneError:
			// drop
		case r == '\r':
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++ // CRLF is one break
			}
			out = append(out, '\n')
		case r == '\n':
			out = append(out, '\n')
		case r == '\t':
			// Kept, not expanded: a Makefile is only valid with real tabs, and
			// silently swapping in spaces is the same class of damage as
			// letting a control byte through.
			out = append(out, '\t')
		case unicode.IsControl(r):
			// drop
		default:
			out = append(out, r)
		}
	}
	return out
}

// wrapSegments returns the rune index at which each display segment of line
// starts, wrapping at width display cells. The first element is always 0. A
// wide rune that would straddle the edge moves whole to the next segment —
// splitting one is not representable.
func wrapSegments(line []rune, width int) []int {
	if width < 1 {
		width = 1
	}
	starts := []int{0}
	w := 0
	for i, r := range line {
		next := advance(w, r)
		// A rune that does not fit moves whole to the next row — but only if
		// this row already holds something. Without that guard a tab wider
		// than the whole row would break before itself forever.
		if next > width && w > 0 {
			starts = append(starts, i)
			w = advance(0, r)
			continue
		}
		w = next
	}
	return starts
}

// segCount is how many display rows a line occupies.
func (b *editBuffer) segCount(row int) int {
	return len(wrapSegments(b.lines[row], b.width))
}

// cursorSeg is which wrapped segment of its own line the cursor sits on.
func (b *editBuffer) cursorSeg() int {
	starts := wrapSegments(b.lines[b.row], b.width)
	seg := 0
	for i, s := range starts {
		if b.col >= s {
			seg = i
		}
	}
	return seg
}

// scrollToCursor nudges the window so the cursor is visible.
//
// The walk from the window origin to the cursor is bounded: if the cursor is
// more than one screen away (a jump, or a fresh SetValue on a long file) the
// window is recentred outright rather than walked to, so no path here is
// proportional to the document.
func (b *editBuffer) scrollToCursor() {
	if b.top > b.row {
		b.top, b.topSeg = b.row, b.cursorSeg()
		return
	}
	// Count display rows from the window origin down to the cursor, giving up
	// (and recentring) as soon as it is clear the cursor is off-screen below.
	rows := -b.topSeg
	for r := b.top; r < b.row; r++ {
		rows += b.segCount(r)
		if rows > b.height {
			b.parkCursorOnLastRow()
			return
		}
	}
	rows += b.cursorSeg()
	if rows < 0 {
		b.top, b.topSeg = b.row, b.cursorSeg()
		return
	}
	for rows >= b.height {
		b.scrollDownOne()
		rows--
	}
}

// parkCursorOnLastRow moves the window so the cursor lands on the BOTTOM row,
// which is what arriving from below means: the rows above it are the ones the
// operator wants to see.
//
// Putting it on the top row instead is the obvious-looking version and it is
// wrong — opening a file leaves the cursor on the trailing empty line, so the
// window would start there and every row would be blank. That is exactly how
// this shipped broken the first time.
//
// Walking back is bounded by the height, never by the document.
func (b *editBuffer) parkCursorOnLastRow() {
	b.top, b.topSeg = b.row, b.cursorSeg()
	for i := 0; i < b.height-1; i++ {
		if !b.scrollUpOne() {
			return
		}
	}
}

// scrollUpOne moves the window origin back one display row, reporting whether
// it could.
func (b *editBuffer) scrollUpOne() bool {
	if b.topSeg > 0 {
		b.topSeg--
		return true
	}
	if b.top == 0 {
		return false
	}
	b.top--
	b.topSeg = b.segCount(b.top) - 1
	return true
}

// scrollDownOne advances the window origin by one display row.
func (b *editBuffer) scrollDownOne() {
	if b.topSeg+1 < b.segCount(b.top) {
		b.topSeg++
		return
	}
	if b.top < len(b.lines)-1 {
		b.top++
		b.topSeg = 0
	}
}

// ---- rendering -----------------------------------------------------------

// Render returns exactly height rows of display text, starting at the window
// origin. Only the lines that appear are wrapped, which is the whole point:
// cost depends on the size of the window, not the size of the document.
func (b *editBuffer) Render() []string {
	rows := make([]string, 0, b.height)
	curRow, curCol := -1, 0
	line, seg := b.top, b.topSeg
	for len(rows) < b.height {
		if line >= len(b.lines) {
			rows = append(rows, "")
			continue
		}
		starts := wrapSegments(b.lines[line], b.width)
		if seg >= len(starts) {
			line++
			seg = 0
			continue
		}
		from := starts[seg]
		to := len(b.lines[line])
		if seg+1 < len(starts) {
			to = starts[seg+1]
		}
		if line == b.row && b.col >= from && (b.col < to || (seg == len(starts)-1 && b.col == to)) {
			curRow = len(rows)
			curCol = 0
			for _, r := range b.lines[line][from:b.col] {
				curCol = advance(curCol, r)
			}
		}
		// Each wrapped segment starts at display column 0, so tab expansion
		// is relative to the row, matching how wrapSegments measured it.
		rows = append(rows, expandTabs(b.lines[line][from:to], 0))
		seg++
	}
	if b.focused && curRow >= 0 {
		rows[curRow] = renderCursorAt(rows[curRow], curCol)
	}
	return rows
}

// renderCursorAt marks the cell at display column col, padding the row when
// the cursor sits past its end (end of line). row has already had its tabs
// expanded, so plain rune widths are the right measure here.
func renderCursorAt(row string, col int) string {
	runes := []rune(row)
	w := 0
	idx := len(runes)
	for i, r := range runes {
		if w == col {
			idx = i
			break
		}
		w += runewidth.RuneWidth(r)
	}
	if idx >= len(runes) {
		return row + strings.Repeat(" ", max(0, col-w)) + CursorStyle.Render(" ")
	}
	return string(runes[:idx]) + CursorStyle.Render(string(runes[idx])) + string(runes[idx+1:])
}
