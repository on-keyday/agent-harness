package vtgrid

import (
	"strconv"
	"strings"
)

// ANSI re-emits the current grid as text carrying SGR escapes, so writing it to
// a terminal shows the screen the way it looked — colours and attributes
// included — rather than the flattened text Lines() gives.
//
// It is the inverse of Write for everything this model keeps, and
// TestANSIRoundTrip holds it to that over every captured corpus: rendering a
// grid and parsing the result back must produce the same grid.
//
// Three presentation decisions, none of them forced by the model:
//
//   - Rows are separated by CRLF, not LF. A row whose content fills the last
//     column leaves a deferred wrap pending; a bare LF would move down without
//     clearing it and the next row would start one column in. This is the kind
//     of thing that only shows up on full-width rows, which is to say on
//     exactly the screens worth looking at.
//   - Each style change emits a full `ESC [ 0 ; … m` rather than a delta. A
//     delta is smaller and every mistake in one is invisible until some later
//     cell inherits an attribute nobody set.
//   - Trailing cells are dropped only when they are blank AND carry no styling
//     at all. "Blank and therefore invisible" is a trap: a status bar is blank
//     cells with a background, an underline or a strike paints a blank cell,
//     and reverse video turns its foreground into the thing you see. Rather
//     than enumerate which attributes are visible on a space — a list that is
//     wrong the first time somebody adds one — anything not fully default
//     survives, which is also what makes the round trip exact rather than
//     merely convincing.
//
// There is no cursor positioning and no screen clear: the result is a block of
// text, not a program that repaints a terminal.
func (t *Terminal) ANSI() string {
	var b strings.Builder
	b.Grow(t.rows * t.cols)
	for y := 0; y < t.rows; y++ {
		if y > 0 {
			b.WriteString("\r\n")
		}
		t.appendANSIRow(&b, y)
	}
	return b.String()
}

func (t *Terminal) appendANSIRow(b *strings.Builder, y int) {
	row := t.scr.lines[y]
	end := lastVisibleCell(row)
	if end < 0 {
		return
	}
	var cur pen
	styled := false
	var link uint16
	for x := 0; x <= end; x++ {
		c := row[x]
		if c.Width == 0 {
			continue // continuation of the wide glyph to the left
		}
		want := pen{
			Attr: c.Attr, Under: c.Under,
			FG: c.FG, BG: c.BG, UnderFG: c.UnderFG,
			link: c.link,
		}
		if want.link != link {
			t.writeLinkSGR(b, want.link)
			link = want.link
		}
		styleOnly := want
		styleOnly.link = 0
		curStyle := cur
		curStyle.link = 0
		if styleOnly != curStyle {
			b.WriteString(sgr(styleOnly))
			cur = want
			styled = styleOnly != pen{}
		}
		b.WriteString(t.Text(c))
	}
	if link != 0 {
		t.writeLinkSGR(b, 0)
	}
	if styled {
		b.WriteString("\x1b[0m")
	}
}

// writeLinkSGR opens or closes a hyperlink. OSC 8 is not an SGR at all — it is
// a string sequence whose scope runs until the next one — so it is emitted
// beside the style rather than folded into it, and a row that ends inside a
// link closes it so the next row does not inherit one.
func (t *Terminal) writeLinkSGR(b *strings.Builder, id uint16) {
	b.WriteString("\x1b]8;")
	if id != 0 && int(id) < len(t.links) {
		l := t.links[id]
		b.WriteString(l.Params)
		b.WriteByte(';')
		b.WriteString(l.URL)
	} else {
		b.WriteByte(';')
	}
	b.WriteString("\x1b\\")
}

// lastVisibleCell returns the index of the rightmost cell worth emitting, or -1
// for a row that is entirely blank and unstyled.
//
// The test is "is this cell distinguishable from a fresh one", not "would a
// reader notice it": see the note on trimming in ANSI.
func lastVisibleCell(row []Cell) int {
	for x := len(row) - 1; x >= 0; x-- {
		if !isDefaultBlank(row[x]) {
			return x
		}
	}
	return -1
}

// isDefaultBlank reports whether a cell is indistinguishable from one that was
// never written to.
//
// It compares against a blank cell WHOLE rather than listing the fields that
// matter. The list form was here first and it was wrong twice: once when the
// rule was "invisible" instead of "unstyled", and again when Under/UnderFG/link
// were added and it kept checking only Attr/FG/BG, silently dropping an
// underlined trailing space. A field added to Cell must not need this function
// edited to stay correct.
func isDefaultBlank(c Cell) bool {
	if c.Rune != 0 && c.Rune != ' ' {
		return false
	}
	c.Rune = ' '
	return c == blank(Color{})
}

// sgr renders a complete pen state, always starting from a reset so the result
// does not depend on what preceded it.
func sgr(p pen) string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	for _, m := range [...]struct {
		a    Attr
		code string
	}{
		{AttrBold, ";1"}, {AttrFaint, ";2"}, {AttrItalic, ";3"},
		{AttrBlink, ";5"}, {AttrRapidBlink, ";6"}, {AttrReverse, ";7"},
		{AttrConceal, ";8"}, {AttrStrike, ";9"},
	} {
		if p.Attr&m.a != 0 {
			b.WriteString(m.code)
		}
	}
	// Underline carries a style, so it is re-emitted in the sub-parameter form
	// that can express one. A plain `4` would flatten curly to single.
	switch p.Under {
	case UnderlineNone:
	case UnderlineSingle:
		b.WriteString(";4")
	default:
		b.WriteString(";4:")
		b.WriteString(strconv.Itoa(int(p.Under)))
	}
	appendColorSGR(&b, p.UnderFG, 0, 58, 0)
	appendColorSGR(&b, p.FG, 30, 38, 90)
	appendColorSGR(&b, p.BG, 40, 48, 100)
	b.WriteByte('m')
	return b.String()
}

// appendColorSGR writes one colour using the compact form when the palette
// index has one: 0-7 and 8-15 have dedicated codes, everything else goes
// through the 38/48 extended form. base is the 8-colour origin, ext the
// extended selector, bright the origin for the high eight.
func appendColorSGR(b *strings.Builder, c Color, base, ext, bright int) {
	switch c.Kind {
	case ColorDefault:
		return
	case ColorBasic:
		if base == 0 {
			// No basic spelling exists for this slot (underline colour), so the
			// extended form is the only way to say it.
			b.WriteByte(';')
			b.WriteString(strconv.Itoa(ext))
			b.WriteString(";5;")
			b.WriteString(strconv.Itoa(int(c.N)))
			return
		}
		if n := int(c.N); n < 8 {
			b.WriteByte(';')
			b.WriteString(strconv.Itoa(base + n))
		} else {
			b.WriteByte(';')
			b.WriteString(strconv.Itoa(bright + n - 8))
		}
	case ColorIndexed:
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(ext))
		b.WriteString(";5;")
		b.WriteString(strconv.Itoa(int(c.N)))
	case ColorRGB:
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(ext))
		b.WriteString(";2;")
		b.WriteString(strconv.Itoa(int(c.R)))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(int(c.G)))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(int(c.B)))
	}
}
