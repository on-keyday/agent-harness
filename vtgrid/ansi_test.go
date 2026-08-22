package vtgrid

// ANSI() claims to re-emit the grid. The way to hold it to that is to parse
// what it produced and compare grids, over real captures rather than examples
// chosen to pass — a renderer and a parser written by the same hand agree on
// each other's mistakes only if the mistakes are symmetric, and a screen full
// of an agent's actual output is where they stop being.

import (
	"fmt"
	"strings"
	"testing"
)

// gridOf flattens every cell that matters for a comparison: the rune, its
// width, and the full style. Comparing Lines() alone would pass a renderer that
// dropped every colour.
func gridOf(t *Terminal) []string {
	cols, rows := t.Size()
	out := make([]string, 0, rows)
	var b strings.Builder
	for y := 0; y < rows; y++ {
		b.Reset()
		for x := 0; x < cols; x++ {
			c := t.CellAt(x, y)
			b.WriteString(string(c.Rune))
			b.WriteByte('|')
			b.WriteString(cellStyleKey(c))
			b.WriteByte(';')
		}
		out = append(out, b.String())
	}
	return out
}

func cellStyleKey(c Cell) string {
	var b strings.Builder
	b.WriteString(sgr(pen{Attr: c.Attr, FG: c.FG, BG: c.BG}))
	b.WriteByte('w')
	b.WriteByte(byte('0' + c.Width))
	return b.String()
}

func TestANSIRoundTrip(t *testing.T) {
	for _, c := range vtCorpora {
		t.Run(c.Name, func(t *testing.T) {
			src := New(c.Cols, c.Rows)
			_, _ = src.Write(loadVTCorpus(t, c.Name))

			again := New(c.Cols, c.Rows)
			_, _ = again.Write([]byte(src.ANSI()))

			cols, rows := src.Size()
			for y := 0; y < rows; y++ {
				for x := 0; x < cols; x++ {
					a, b := src.CellAt(x, y), again.CellAt(x, y)
					// link and combining are indexes into each terminal's own
					// side table, so they are meaningful only relative to it.
					// Compare what they RESOLVE to; comparing the numbers would
					// fail whenever two terminals interned in a different order,
					// which is a difference in bookkeeping, not in the screen.
					if describeCell(src, a) != describeCell(again, b) {
						t.Errorf("cell (%d,%d) does not survive the round trip\n"+
							"  before: %s\n  after : %s\n  row: %s",
							x, y, describeCell(src, a), describeCell(again, b), clip(src.Lines()[y]))
						return
					}
				}
			}
		})
	}
}

// TestANSIKeepsABackgroundOnlyRun pins the trimming rule, which is the one
// place a "blank" cell is not disposable: a status bar is blank cells with a
// background, and dropping trailing blanks by looking at the rune alone erases
// it.
func TestANSIKeepsABackgroundOnlyRun(t *testing.T) {
	term := New(20, 1)
	_, _ = term.Write([]byte("hi\x1b[44m          \x1b[0m"))

	out := term.ANSI()
	if !strings.Contains(out, "\x1b[0;44m") {
		t.Errorf("ANSI() = %q, want it to carry the background", out)
	}
	again := New(20, 1)
	_, _ = again.Write([]byte(out))
	if got := again.CellAt(8, 0).BG; got != Basic(4) {
		t.Errorf("cell 8 background = %+v, want Basic(4) — `ESC[44m` is the "+
			"basic spelling and must come back as one", got)
	}
}

// TestANSIKeepsTheColourSpelling is why ColorBasic exists. SGR 31 and
// 38;5;1 name the same palette entry and are not interchangeable on a
// terminal: the "bold is bright" heuristic most apply to the first they do not
// apply to the second, so re-emitting one as the other repaints the screen.
//
// The captured corpora settle which way the pressure runs — the extended form
// is the majority (13,274 uses against 11,922) and herdr, ConPTY and PowerShell
// emit nothing else — so a renderer that normalised to the compact form was
// rewriting most of the colour it was handed.
func TestANSIKeepsTheColourSpelling(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		cell           Color
	}{
		{"basic stays basic", "\x1b[31mX", ";31", Basic(1)},
		{"bright stays bright", "\x1b[91mX", ";91", Basic(9)},
		{"extended stays extended", "\x1b[38;5;1mX", ";38;5;1", Indexed(1)},
		{"extended high", "\x1b[38;5;220mX", ";38;5;220", Indexed(220)},
		{"truecolor", "\x1b[38;2;255;135;175mX", ";38;2;255;135;175", RGB(255, 135, 175)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := New(10, 1)
			_, _ = term.Write([]byte(tc.in))
			if got := term.CellAt(0, 0).FG; got != tc.cell {
				t.Errorf("parsed as %+v, want %+v", got, tc.cell)
			}
			if out := term.ANSI(); !strings.Contains(out, tc.want) {
				t.Errorf("ANSI() = %q, want it to carry %q", out, tc.want)
			}
		})
	}

	// The two spellings resolve to the same colour, which is what keeps
	// --color/--json output unchanged by the distinction.
	if Basic(1).Hex() != Indexed(1).Hex() {
		t.Errorf("Basic(1).Hex() = %s, Indexed(1).Hex() = %s — the two spellings "+
			"name one palette entry and must render alike",
			Basic(1).Hex(), Indexed(1).Hex())
	}
}

// TestANSIRowsDoNotDriftOnAFullWidthRow is why rows are separated by CRLF and
// not LF. A row that fills the last column leaves a deferred wrap pending; a
// bare LF moves down without clearing it, and everything below starts one
// column to the right.
func TestANSIRowsDoNotDriftOnAFullWidthRow(t *testing.T) {
	term := New(8, 3)
	_, _ = term.Write([]byte("ABCDEFGH\r\nxy\r\nz"))

	again := New(8, 3)
	_, _ = again.Write([]byte(term.ANSI()))
	if got := again.Lines()[1]; !strings.HasPrefix(got, "xy") {
		t.Errorf("row 1 = %q, want it to start at column 0 with \"xy\"", got)
	}
}

// describeCell renders everything a cell means, with the side-table indexes
// resolved so two terminals can be compared.
func describeCell(t *Terminal, c Cell) string {
	link, _ := t.Link(c)
	return fmt.Sprintf("text=%q w=%d attr=%08b under=%d fg=%+v bg=%+v ulfg=%+v link=%q params=%q",
		t.Text(c), c.Width, c.Attr, c.Under, c.FG, c.BG, c.UnderFG, link.URL, link.Params)
}
