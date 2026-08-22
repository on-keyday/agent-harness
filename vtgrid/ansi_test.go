package vtgrid

// ANSI() claims to re-emit the grid. The way to hold it to that is to parse
// what it produced and compare grids, over real captures rather than examples
// chosen to pass — a renderer and a parser written by the same hand agree on
// each other's mistakes only if the mistakes are symmetric, and a screen full
// of an agent's actual output is where they stop being.

import (
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
					if a == b {
						continue
					}
					t.Errorf("cell (%d,%d) does not survive the round trip\n"+
						"  before: %+v\n  after : %+v\n  row: %s",
						x, y, a, b, clip(src.Lines()[y]))
					return
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
	if got := again.CellAt(8, 0).BG; got.Kind != ColorIndexed || got.N != 4 {
		t.Errorf("cell 8 background = %+v, want palette index 4", got)
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
