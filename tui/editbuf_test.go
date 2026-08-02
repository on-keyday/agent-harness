package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func newBuf(text string, w, h int) *editBuffer {
	b := newEditBuffer()
	b.SetSize(w, h)
	b.SetValue(text)
	return b
}

func TestEditBufValueRoundTrip(t *testing.T) {
	for _, s := range []string{"", "alpha", "alpha\nbeta\n", "alpha\n\nbeta", "日本語\nの行\n"} {
		b := newBuf(s, 20, 5)
		if got := b.Value(); got != s {
			t.Errorf("Value()=%q, want %q", got, s)
		}
	}
}

func TestEditBufInsertRunes(t *testing.T) {
	// "ac\n" is two lines — ["ac", ""] — and SetValue leaves the cursor on the
	// last (empty) one, so reaching "between a and c" means going to the top
	// first. CursorHome alone would only move within the empty line.
	b := newBuf("ac\n", 20, 5)
	b.CursorTop()
	b.CursorRight() // between a and c
	b.InsertRunes([]rune("b"))
	if got, want := b.Value(), "abc\n"; got != want {
		t.Errorf("Value()=%q, want %q", got, want)
	}
	// A paste arrives as one multi-rune event and must not be split.
	b.InsertRunes([]rune("XY"))
	if got, want := b.Value(), "abXYc\n"; got != want {
		t.Errorf("after paste Value()=%q, want %q", got, want)
	}
}

func TestEditBufInsertWideRunes(t *testing.T) {
	b := newBuf("", 20, 5)
	b.InsertRunes([]rune("日本語"))
	if got, want := b.Value(), "日本語"; got != want {
		t.Errorf("Value()=%q, want %q", got, want)
	}
	b.CursorLeft()
	b.InsertRunes([]rune("X"))
	if got, want := b.Value(), "日本X語"; got != want {
		t.Errorf("Value()=%q, want %q — cursor must move by rune, not by byte or cell", got, want)
	}
}

func TestEditBufNewlineSplitsLine(t *testing.T) {
	b := newBuf("abcd", 20, 5)
	b.CursorHome()
	b.CursorRight()
	b.CursorRight()
	b.InsertNewline()
	if got, want := b.Value(), "ab\ncd"; got != want {
		t.Errorf("Value()=%q, want %q", got, want)
	}
	if b.row != 1 || b.col != 0 {
		t.Errorf("cursor=(%d,%d), want (1,0)", b.row, b.col)
	}
}

func TestEditBufBackspace(t *testing.T) {
	b := newBuf("ab\ncd", 20, 5)
	b.CursorEnd() // end of "cd" — SetValue leaves the cursor on the last line
	b.Backspace()
	if got, want := b.Value(), "ab\nc"; got != want {
		t.Errorf("Value()=%q, want %q", got, want)
	}
	b.Backspace()
	if got, want := b.Value(), "ab\n"; got != want {
		t.Errorf("Value()=%q, want %q", got, want)
	}
	// At column 0 backspace joins with the previous line.
	b.Backspace()
	if got, want := b.Value(), "ab"; got != want {
		t.Errorf("join: Value()=%q, want %q", got, want)
	}
	if b.row != 0 || b.col != 2 {
		t.Errorf("after join cursor=(%d,%d), want (0,2)", b.row, b.col)
	}
}

func TestEditBufBackspaceAtStartIsNoop(t *testing.T) {
	b := newBuf("abc", 20, 5)
	b.CursorHome()
	b.Backspace()
	if got, want := b.Value(), "abc"; got != want {
		t.Errorf("Value()=%q, want it unchanged", got)
	}
}

func TestEditBufDeleteForward(t *testing.T) {
	b := newBuf("ab\ncd", 20, 5)
	b.CursorTop()
	b.CursorEnd()
	b.DeleteForward() // joins the lines; cursor stays after "ab"
	if got, want := b.Value(), "abcd"; got != want {
		t.Errorf("Value()=%q, want %q", got, want)
	}
	b.DeleteForward() // now deletes the rune under the cursor
	if got, want := b.Value(), "abd"; got != want {
		t.Errorf("Value()=%q, want %q", got, want)
	}
	b.CursorBottom()
	b.DeleteForward() // at end of buffer: nothing to delete
	if got, want := b.Value(), "abd"; got != want {
		t.Errorf("Value()=%q, want it unchanged", got)
	}
}

func TestEditBufCursorAcrossLines(t *testing.T) {
	b := newBuf("ab\ncd", 20, 5)
	b.CursorTop()
	b.CursorEnd()
	b.CursorRight() // wraps to the start of the next line
	if b.row != 1 || b.col != 0 {
		t.Errorf("cursor=(%d,%d), want (1,0)", b.row, b.col)
	}
	b.CursorLeft() // and back
	if b.row != 0 || b.col != 2 {
		t.Errorf("cursor=(%d,%d), want (0,2)", b.row, b.col)
	}
}

func TestEditBufUpDownKeepsDesiredColumn(t *testing.T) {
	b := newBuf("abcdef\nxy\nabcdef", 20, 5)
	b.CursorTop()
	for i := 0; i < 5; i++ {
		b.CursorRight()
	}
	b.CursorDown() // onto the short line: clamps
	if b.row != 1 || b.col != 2 {
		t.Errorf("cursor=(%d,%d), want (1,2) clamped to the short line", b.row, b.col)
	}
	b.CursorDown() // onto a long line again: the original column comes back
	if b.row != 2 || b.col != 5 {
		t.Errorf("cursor=(%d,%d), want (2,5) — desired column must be sticky", b.row, b.col)
	}
}

// Wrapping is by display cell, so a wide rune counts as two. wrapSegments
// returns the rune index each display segment STARTS at, always beginning
// with 0.
func TestEditBufWrapSegmentsWideRunes(t *testing.T) {
	// width 6 => exactly three wide runes (6 cells) per segment
	got := wrapSegments([]rune("日本語日本語"), 6)
	if len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Errorf("segment starts=%v, want [0 3]", got)
	}
}

func TestEditBufWrapSegmentsNeverSplitsARune(t *testing.T) {
	// width 5 with wide runes: two fit (4 cells), the third would need cells
	// 5-6 and cannot straddle the edge, so it moves whole to the next row.
	got := wrapSegments([]rune("日本語"), 5)
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Errorf("segment starts=%v, want [0 2]", got)
	}
}

// Opening a file leaves the cursor at the end, on the trailing empty line.
// The window must show the last screenful — not start AT that empty line,
// which renders an entirely blank editor.
func TestEditBufRenderShowsContentAfterSetValue(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	b := newBuf(sb.String(), 40, 8)
	b.Focus()
	rows := b.Render()
	nonEmpty := 0
	for _, r := range rows {
		if strings.TrimSpace(stripANSI(r)) != "" {
			nonEmpty++
		}
	}
	if nonEmpty < len(rows)-1 {
		t.Errorf("only %d of %d rows have content: %q", nonEmpty, len(rows), rows)
	}
	// And what it shows is the END of the file, where the cursor is.
	if !strings.Contains(strings.Join(rows, "\n"), "line 499") {
		t.Errorf("window does not show the last line: %q", rows)
	}
}

// stripANSI drops SGR sequences so a test can look at the text a row carries.
func stripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case !inEsc:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func TestEditBufRenderShowsWindowOnly(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("line\n")
	}
	b := newBuf(sb.String(), 20, 5)
	rows := b.Render()
	if len(rows) != 5 {
		t.Fatalf("Render() returned %d rows, want exactly the height 5", len(rows))
	}
}

func TestEditBufRenderFollowsCursorDown(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("l")
		sb.WriteString(strings.Repeat("x", i%3))
		sb.WriteString("\n")
	}
	b := newBuf(sb.String(), 20, 5)
	b.CursorTop()
	for i := 0; i < 20; i++ {
		b.CursorDown()
	}
	// The cursor's line must be inside the rendered window.
	if b.top > b.row || b.row >= b.top+5 {
		t.Errorf("cursor row %d outside window [%d,%d)", b.row, b.top, b.top+5)
	}
}

func TestEditBufRenderWrapsLongLine(t *testing.T) {
	b := newBuf("abcdefghij", 4, 5)
	rows := b.Render()
	// 10 chars at width 4 => 3 display rows of content.
	if !strings.Contains(rows[0], "abcd") || !strings.Contains(rows[1], "efgh") || !strings.Contains(rows[2], "ij") {
		t.Errorf("rows=%q, want the line wrapped every 4 cells", rows[:3])
	}
}

// The point of this buffer: a keystroke must cost the same on a big document
// as on a small one. bubbles/textarea failed exactly here — 38ms per keystroke
// on a 93KB Japanese memo versus 3ms on 1KB, because its View re-renders the
// whole document every frame. This asserts the property directly rather than
// trusting the design to have preserved it.
func TestEditBufKeystrokeCostIsIndependentOfDocumentSize(t *testing.T) {
	build := func(nbytes int) *editBuffer {
		var sb strings.Builder
		for sb.Len() < nbytes {
			sb.WriteString("これは日本語のメモ行です。全角文字が多く含まれています。\n")
		}
		b := newEditBuffer()
		b.SetSize(110, 30)
		b.SetValue(sb.String())
		b.Focus()
		return b
	}
	measure := func(b *editBuffer) time.Duration {
		const iters = 200
		start := time.Now()
		for i := 0; i < iters; i++ {
			b.InsertRunes([]rune{'x'})
			_ = b.Render()
		}
		return time.Since(start) / iters
	}
	// Warm up so the first allocation burst is not attributed to the big doc.
	measure(build(1 << 10))

	small := measure(build(1 << 10))
	big := measure(build(93000))
	t.Logf("1KB: %v/keystroke, 93KB: %v/keystroke", small, big)

	// Generous bound: this is catching an O(document) regression (which was
	// ~13x), not policing normal variance on a loaded machine.
	if big > 5*small+200*time.Microsecond {
		t.Errorf("93KB costs %v per keystroke vs %v at 1KB — cost is scaling with the document again", big, small)
	}
}

func TestEditBufSetValueResetsState(t *testing.T) {
	b := newBuf("aaa\nbbb\nccc\n", 20, 2)
	b.CursorBottom()
	b.SetValue("x")
	if b.row != 0 || b.top != 0 {
		t.Errorf("after SetValue cursor=(%d) top=%d, want the window back at the start", b.row, b.top)
	}
	if got := b.Value(); got != "x" {
		t.Errorf("Value()=%q, want x", got)
	}
}
