package vtgrid

// Hyperlinks (OSC 8) and grapheme clusters, the two things a cell carries that
// are not fixed-size. Both live in a side table with the cell holding an index,
// so these also pin that the indexes resolve.

import "testing"

func TestHyperlink(t *testing.T) {
	term := New(40, 1)
	_, _ = term.Write([]byte(
		"\x1b]8;id=1;https://example.com\x1b\\LINK\x1b]8;;\x1b\\ plain"))

	l, ok := term.Link(term.CellAt(0, 0))
	if !ok || l.URL != "https://example.com" || l.Params != "id=1" {
		t.Errorf("linked cell = %+v (ok=%v), want the URL and its params", l, ok)
	}
	if _, ok := term.Link(term.CellAt(5, 0)); ok {
		t.Error("the cell after the closing OSC 8 still carries a link")
	}
}

// TestHyperlinkSurvivesAnSGRReset is the regression test for a real defect:
// pen.reset() cleared the link along with the colours. OSC 8 is not an SGR —
// its scope runs until the next OSC 8 — and an app routinely opens a link and
// THEN sets the link's own styling with `ESC[0;…m`, so a reset that closed the
// link dropped it from essentially every hyperlink in a real capture.
func TestHyperlinkSurvivesAnSGRReset(t *testing.T) {
	term := New(40, 1)
	_, _ = term.Write([]byte(
		"\x1b]8;;https://example.com\x1b\\\x1b[0;2;4mTEXT\x1b]8;;\x1b\\"))
	l, ok := term.Link(term.CellAt(0, 0))
	if !ok || l.URL != "https://example.com" {
		t.Fatalf("link after an SGR reset = %+v (ok=%v), want it intact", l, ok)
	}
	if c := term.CellAt(0, 0); c.Under != UnderlineSingle || c.Attr&AttrFaint == 0 {
		t.Errorf("the reset should still have applied the styling: %+v", c)
	}
}

func TestCombiningMarks(t *testing.T) {
	// か + COMBINING VOICED SOUND MARK is が, in two runes and one cell.
	term := New(10, 1)
	_, _ = term.Write([]byte("がX"))

	if got := term.Text(term.CellAt(0, 0)); got != "が" {
		t.Errorf("cell text = %q, want the base rune with its mark", got)
	}
	if got := term.CellAt(0, 0).Width; got != 2 {
		t.Errorf("width = %d, want 2 — the mark adds no column", got)
	}
	// The mark must not consume a column: X follows the wide glyph's pair.
	if got := term.Lines()[0]; got[:len("がX")] != "がX" {
		t.Errorf("line = %q, want the mark attached and X right after", got)
	}
}

func TestCombiningMarksSurviveANSI(t *testing.T) {
	term := New(10, 1)
	_, _ = term.Write([]byte("é̂!"))
	again := New(10, 1)
	_, _ = again.Write([]byte(term.ANSI()))
	if a, b := term.Text(term.CellAt(0, 0)), again.Text(again.CellAt(0, 0)); a != b {
		t.Errorf("round trip: %q -> %q", a, b)
	}
}
