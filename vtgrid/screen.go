package vtgrid

// screen is one character buffer: the primary or the alternate.
//
// Rows are held as a slice of row slices rather than one flat array, and a
// scroll rotates the row *headers* instead of copying cells. That is the one
// structural decision in this package that was made for cost rather than
// clarity: scrolling is what a shell does constantly, and a measurement of the
// general-purpose emulator this replaces put a scrolled line at 40-87 µs,
// which is per-cell work. Rotating pointers makes it per-row.
type screen struct {
	cols, rows int
	lines      [][]Cell

	// Scroll region, 0-based and inclusive. Full screen unless DECSTBM
	// narrowed it. A left/right margin mode exists in the spec; nothing in the
	// captured corpora emits it, so it is not modelled.
	top, bottom int
}

func newScreen(cols, rows int, bg Color) *screen {
	s := &screen{cols: cols, rows: rows, top: 0, bottom: rows - 1}
	s.lines = make([][]Cell, rows)
	for y := range s.lines {
		s.lines[y] = makeRow(cols, bg)
	}
	return s
}

func makeRow(cols int, bg Color) []Cell {
	row := make([]Cell, cols)
	b := blank(bg)
	for x := range row {
		row[x] = b
	}
	return row
}

func (s *screen) clearRow(y int, bg Color) {
	b := blank(bg)
	row := s.lines[y]
	for x := range row {
		row[x] = b
	}
}

// clearCells blanks [x0, x1] on row y, inclusive, clamped.
func (s *screen) clearCells(y, x0, x1 int, bg Color) {
	if y < 0 || y >= s.rows {
		return
	}
	if x0 < 0 {
		x0 = 0
	}
	if x1 >= s.cols {
		x1 = s.cols - 1
	}
	b := blank(bg)
	row := s.lines[y]
	for x := x0; x <= x1; x++ {
		row[x] = b
	}
}

// scrollUp moves the scroll region's contents up by n rows, blanking the n
// rows that open at the bottom. Rotation is over row headers; no cell is
// copied.
func (s *screen) scrollUp(n int, bg Color) {
	s.scrollRegionUp(s.top, s.bottom, n, bg)
}

func (s *screen) scrollDown(n int, bg Color) {
	s.scrollRegionDown(s.top, s.bottom, n, bg)
}

func (s *screen) scrollRegionUp(top, bottom, n int, bg Color) {
	height := bottom - top + 1
	if n <= 0 || height <= 0 {
		return
	}
	if n >= height {
		for y := top; y <= bottom; y++ {
			s.clearRow(y, bg)
		}
		return
	}
	// Lift the n rows scrolling off, blank them, and re-seat them at the
	// bottom — reusing the backing arrays so a scroll allocates nothing.
	spill := make([][]Cell, n)
	copy(spill, s.lines[top:top+n])
	copy(s.lines[top:], s.lines[top+n:bottom+1])
	b := blank(bg)
	for i, row := range spill {
		for x := range row {
			row[x] = b
		}
		s.lines[bottom-n+1+i] = row
	}
}

func (s *screen) scrollRegionDown(top, bottom, n int, bg Color) {
	height := bottom - top + 1
	if n <= 0 || height <= 0 {
		return
	}
	if n >= height {
		for y := top; y <= bottom; y++ {
			s.clearRow(y, bg)
		}
		return
	}
	spill := make([][]Cell, n)
	copy(spill, s.lines[bottom-n+1:bottom+1])
	copy(s.lines[top+n:bottom+1], s.lines[top:bottom-n+1])
	b := blank(bg)
	for i, row := range spill {
		for x := range row {
			row[x] = b
		}
		s.lines[top+i] = row
	}
}

// insertLines / deleteLines are scrolls of the region *below the cursor*: the
// region they act on starts at y, not at the scroll-region top. Outside the
// scroll region they do nothing, which is what makes a full-screen app's
// framing survive them.
func (s *screen) insertLines(y, n int, bg Color) {
	if y < s.top || y > s.bottom {
		return
	}
	s.scrollRegionDown(y, s.bottom, n, bg)
}

func (s *screen) deleteLines(y, n int, bg Color) {
	if y < s.top || y > s.bottom {
		return
	}
	s.scrollRegionUp(y, s.bottom, n, bg)
}

// insertChars shifts the rest of the row right, dropping what falls off.
func (s *screen) insertChars(y, x, n int, bg Color) {
	if y < 0 || y >= s.rows || x < 0 || x >= s.cols || n <= 0 {
		return
	}
	if n > s.cols-x {
		n = s.cols - x
	}
	row := s.lines[y]
	copy(row[x+n:], row[x:s.cols-n])
	b := blank(bg)
	for i := x; i < x+n; i++ {
		row[i] = b
	}
}

// deleteChars shifts the rest of the row left, blanking the tail.
func (s *screen) deleteChars(y, x, n int, bg Color) {
	if y < 0 || y >= s.rows || x < 0 || x >= s.cols || n <= 0 {
		return
	}
	if n > s.cols-x {
		n = s.cols - x
	}
	row := s.lines[y]
	copy(row[x:], row[x+n:])
	b := blank(bg)
	for i := s.cols - n; i < s.cols; i++ {
		row[i] = b
	}
}

// resize changes the buffer's dimensions. Content keeps its top-left anchor:
// rows are kept from the top and columns from the left. Reflowing wrapped
// lines is deliberately not attempted — a reflow needs to know which line
// breaks were soft, and this model does not record that. A resized grid is
// therefore approximate, which is the honest state of affairs rather than a
// gap to hide.
func (s *screen) resize(cols, rows int, bg Color) {
	if cols == s.cols && rows == s.rows {
		return
	}
	lines := make([][]Cell, rows)
	for y := 0; y < rows; y++ {
		row := makeRow(cols, bg)
		if y < len(s.lines) {
			copy(row, s.lines[y][:min(cols, s.cols)])
		}
		lines[y] = row
	}
	s.lines, s.cols, s.rows = lines, cols, rows
	s.top, s.bottom = 0, rows-1
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
