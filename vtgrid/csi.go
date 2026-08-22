package vtgrid

import "strconv"

// csiParams is one parsed CSI parameter list. Zero means "absent" throughout,
// because that is how the wire spells it: `ESC[H`, `ESC[;H` and `ESC[1;1H` are
// the same command. Each caller applies its own default, since the default is
// 1 for a movement and 0 for an erase mode.
type csiParams struct {
	prefix byte // '?', '>', '<', '=' — 0 when the sequence had none
	n      []int
}

func (p csiParams) at(i, def int) int {
	if i >= len(p.n) || p.n[i] == 0 {
		return def
	}
	return p.n[i]
}

// atRaw is at() without the zero-means-default rule, for the parameters where
// zero is a real value (erase modes, SGR codes).
func (p csiParams) atRaw(i int) int {
	if i >= len(p.n) {
		return 0
	}
	return p.n[i]
}

// parseCSIParams splits the bytes between the CSI intro and its final byte.
// Colons are folded to semicolons: they separate sub-parameters (the
// `38:2::r:g:b` colour form) and treating them as ordinary separators is what
// lets one SGR loop read both spellings.
func parseCSIParams(b []byte) csiParams {
	var p csiParams
	if len(b) > 0 && (b[0] == '?' || b[0] == '>' || b[0] == '<' || b[0] == '=') {
		p.prefix = b[0]
		b = b[1:]
	}
	if len(b) == 0 {
		return p
	}
	start := 0
	for i := 0; i <= len(b); i++ {
		if i == len(b) || b[i] == ';' || b[i] == ':' {
			if i == start {
				p.n = append(p.n, 0)
			} else {
				v, err := strconv.Atoi(string(b[start:i]))
				if err != nil {
					v = 0
				}
				p.n = append(p.n, v)
			}
			start = i + 1
		}
	}
	return p
}

func (t *Terminal) dispatchCSI(final byte) {
	p := parseCSIParams(t.params)

	// Private sequences are modes and queries only; none of them draw.
	if p.prefix != 0 {
		switch final {
		case 'h':
			t.setModes(p, true)
		case 'l':
			t.setModes(p, false)
		}
		return
	}

	switch final {
	case 'A': // CUU
		t.moveCursor(0, -p.at(0, 1))
	case 'B', 'e': // CUD, VPR
		t.moveCursor(0, p.at(0, 1))
	case 'C', 'a': // CUF, HPR
		t.moveCursor(p.at(0, 1), 0)
	case 'D': // CUB
		t.moveCursor(-p.at(0, 1), 0)
	case 'E': // CNL
		t.x = 0
		t.moveCursor(0, p.at(0, 1))
	case 'F': // CPL
		t.x = 0
		t.moveCursor(0, -p.at(0, 1))
	case 'G', '`': // CHA, HPA
		t.setCursor(p.at(0, 1)-1, t.y)
	case 'd': // VPA
		t.setCursorRow(p.at(0, 1) - 1)
	case 'H', 'f': // CUP, HVP
		t.setCursorRow(p.at(0, 1) - 1)
		t.setCursor(p.at(1, 1)-1, t.y)
	case 'I': // CHT
		for i := p.at(0, 1); i > 0; i-- {
			t.tab()
		}
	case 'Z': // CBT
		for i := p.at(0, 1); i > 0; i-- {
			t.backTab()
		}
	case 'J': // ED
		t.eraseDisplay(p.atRaw(0))
	case 'K': // EL
		t.eraseLine(p.atRaw(0))
	case 'L': // IL
		t.scr.insertLines(t.y, p.at(0, 1), t.pen.BG)
		t.x = 0
		t.pendingWrap = false
	case 'M': // DL
		t.scr.deleteLines(t.y, p.at(0, 1), t.pen.BG)
		t.x = 0
		t.pendingWrap = false
	case '@': // ICH
		t.scr.insertChars(t.y, t.x, p.at(0, 1), t.pen.BG)
		t.pendingWrap = false
	case 'P': // DCH
		t.scr.deleteChars(t.y, t.x, p.at(0, 1), t.pen.BG)
		t.pendingWrap = false
	case 'X': // ECH — erase in place, cursor does not move
		n := p.at(0, 1)
		t.scr.clearCells(t.y, t.x, t.x+n-1, t.pen.BG)
		t.pendingWrap = false
	case 'S': // SU
		t.scr.scrollUp(p.at(0, 1), t.pen.BG)
	case 'T': // SD
		t.scr.scrollDown(p.at(0, 1), t.pen.BG)
	case 'g': // TBC
		t.clearTabs(p.atRaw(0))
	case 'm': // SGR
		t.sgr(p)
	case 'r': // DECSTBM
		t.setScrollRegion(p)
	case 's': // ANSI.SYS save cursor
		t.saveCursor()
	case 'u': // ANSI.SYS restore cursor
		t.restoreCursor()
	case 'h':
		// Non-private modes: only IRM (4) and LNM (20) exist in practice and
		// neither appears in any captured corpus. Recognised, not acted on.
	case 'l':
	case 'c', 'n', 't', 'q', 'p', 'x':
		// Device attributes, status reports, window manipulation, cursor style,
		// soft reset queries. A headless grid has nobody to answer, and none of
		// them change what is on screen.
	}
}

// moveCursor is a relative move. It clamps rather than scrolling: CUU at the
// top of the screen does nothing, which is what separates it from RI.
func (t *Terminal) moveCursor(dx, dy int) {
	t.pendingWrap = false
	t.x += dx
	t.y += dy
	t.clampToRegion()
}

func (t *Terminal) setCursor(x, y int) {
	t.pendingWrap = false
	t.x, t.y = x, y
	t.clampToRegion()
}

// setCursorRow honours origin mode, where row 1 means the top of the scroll
// region rather than the top of the screen.
func (t *Terminal) setCursorRow(row int) {
	t.pendingWrap = false
	if t.originMode {
		row += t.scr.top
	}
	t.y = row
	t.clampToRegion()
}

// clampToRegion keeps the cursor on the grid, and inside the scroll region
// when origin mode confines it there.
func (t *Terminal) clampToRegion() {
	if t.x < 0 {
		t.x = 0
	}
	if t.x >= t.cols {
		t.x = t.cols - 1
	}
	lo, hi := 0, t.rows-1
	if t.originMode {
		lo, hi = t.scr.top, t.scr.bottom
	}
	if t.y < lo {
		t.y = lo
	}
	if t.y > hi {
		t.y = hi
	}
}

func (t *Terminal) backTab() {
	t.pendingWrap = false
	for x := t.x - 1; x > 0; x-- {
		if t.tabs[x] {
			t.x = x
			return
		}
	}
	t.x = 0
}

func (t *Terminal) clearTabs(mode int) {
	switch mode {
	case 0:
		if t.x >= 0 && t.x < t.cols {
			t.tabs[t.x] = false
		}
	case 3:
		for i := range t.tabs {
			t.tabs[i] = false
		}
	}
}

func (t *Terminal) eraseLine(mode int) {
	t.pendingWrap = false
	switch mode {
	case 0:
		t.scr.clearCells(t.y, t.x, t.cols-1, t.pen.BG)
	case 1:
		t.scr.clearCells(t.y, 0, t.x, t.pen.BG)
	case 2:
		t.scr.clearCells(t.y, 0, t.cols-1, t.pen.BG)
	}
}

func (t *Terminal) eraseDisplay(mode int) {
	t.pendingWrap = false
	switch mode {
	case 0:
		t.scr.clearCells(t.y, t.x, t.cols-1, t.pen.BG)
		for y := t.y + 1; y < t.rows; y++ {
			t.scr.clearRow(y, t.pen.BG)
		}
	case 1:
		for y := 0; y < t.y; y++ {
			t.scr.clearRow(y, t.pen.BG)
		}
		t.scr.clearCells(t.y, 0, t.x, t.pen.BG)
	case 2, 3:
		// 3 additionally clears scrollback, which this model does not keep.
		for y := 0; y < t.rows; y++ {
			t.scr.clearRow(y, t.pen.BG)
		}
	}
}

// setScrollRegion implements DECSTBM. An empty or inverted region is refused
// outright (the terminal keeps its previous margins), and a successful set
// homes the cursor — apps rely on that to start drawing without a separate CUP.
func (t *Terminal) setScrollRegion(p csiParams) {
	top := p.at(0, 1) - 1
	bottom := p.at(1, t.rows) - 1
	if top < 0 {
		top = 0
	}
	if bottom >= t.rows {
		bottom = t.rows - 1
	}
	if top >= bottom {
		return
	}
	t.scr.top, t.scr.bottom = top, bottom
	t.x = 0
	t.y = 0
	if t.originMode {
		t.y = top
	}
	t.pendingWrap = false
}

func (t *Terminal) setModes(p csiParams, set bool) {
	if p.prefix != '?' {
		return
	}
	for _, m := range p.n {
		switch m {
		case 6: // DECOM — origin mode also homes the cursor
			t.originMode = set
			t.x = 0
			t.y = 0
			if set {
				t.y = t.scr.top
			}
		case 7: // DECAWM
			t.autowrap = set
			t.pendingWrap = false
		case 25: // DECTCEM
			t.cursorVis = set
		case 47, 1047:
			t.switchScreen(set, false)
		case 1048:
			if set {
				t.saveCursor()
			} else {
				t.restoreCursor()
			}
		case 1049:
			t.switchScreen(set, true)
		}
	}
}

// switchScreen moves between the primary and alternate buffers.
//
// The saveCursor variant (1049) is the one applications actually use, and the
// asymmetry matters: entering saves the cursor and CLEARS the alternate buffer
// so the app starts on a blank screen, while leaving restores the cursor and
// leaves the primary buffer exactly as the app found it. Clearing on the way
// out instead would erase the scrollback the user came back for.
func (t *Terminal) switchScreen(toAlt, saveRestore bool) {
	if toAlt == t.altActive {
		return
	}
	if toAlt {
		if saveRestore {
			t.saveCursor()
		}
		t.altActive = true
		t.scr = t.alt
		t.scr.top, t.scr.bottom = 0, t.rows-1
		for y := 0; y < t.rows; y++ {
			t.scr.clearRow(y, t.pen.BG)
		}
		if !saveRestore {
			t.x, t.y = 0, 0
		}
		t.pendingWrap = false
		return
	}
	t.altActive = false
	t.scr = t.pri
	if saveRestore {
		t.restoreCursor()
	}
	t.pendingWrap = false
	t.clampCursor()
}

// sgr applies Select Graphic Rendition. Codes not listed are ignored rather
// than reset — an unknown attribute must not silently drop the colour that was
// set beside it in the same sequence.
func (t *Terminal) sgr(p csiParams) {
	if len(p.n) == 0 {
		t.pen.reset()
		return
	}
	for i := 0; i < len(p.n); i++ {
		switch c := p.n[i]; {
		case c == 0:
			t.pen.reset()
		case c == 1:
			t.pen.Attr |= AttrBold
		case c == 2:
			t.pen.Attr |= AttrFaint
		case c == 3:
			t.pen.Attr |= AttrItalic
		case c == 4:
			t.pen.Attr |= AttrUnderline
		case c == 5 || c == 6:
			t.pen.Attr |= AttrBlink
		case c == 7:
			t.pen.Attr |= AttrReverse
		case c == 8:
			t.pen.Attr |= AttrConceal
		case c == 9:
			t.pen.Attr |= AttrStrike
		case c == 21 || c == 22:
			t.pen.Attr &^= AttrBold | AttrFaint
		case c == 23:
			t.pen.Attr &^= AttrItalic
		case c == 24:
			t.pen.Attr &^= AttrUnderline
		case c == 25:
			t.pen.Attr &^= AttrBlink
		case c == 27:
			t.pen.Attr &^= AttrReverse
		case c == 28:
			t.pen.Attr &^= AttrConceal
		case c == 29:
			t.pen.Attr &^= AttrStrike
		case c >= 30 && c <= 37:
			t.pen.FG = Indexed(uint8(c - 30))
		case c == 38:
			i = t.extendedColor(p, i, &t.pen.FG)
		case c == 39:
			t.pen.FG = Color{}
		case c >= 40 && c <= 47:
			t.pen.BG = Indexed(uint8(c - 40))
		case c == 48:
			i = t.extendedColor(p, i, &t.pen.BG)
		case c == 49:
			t.pen.BG = Color{}
		case c >= 90 && c <= 97:
			t.pen.FG = Indexed(uint8(c - 90 + 8))
		case c >= 100 && c <= 107:
			t.pen.BG = Indexed(uint8(c - 100 + 8))
		}
	}
}

// extendedColor consumes the parameters after a 38 or 48 and returns the index
// of the last one it used.
//
// It tolerates the colon spelling's extra colour-space slot (`38:2::r:g:b`),
// which arrives here as an empty parameter after the 2, by skipping a single
// zero before reading the components. That heuristic is wrong for a literal
// `38;2;0;g;b` — a truecolor with a zero red channel — so the skip only
// happens when there are enough parameters left to afford it.
func (t *Terminal) extendedColor(p csiParams, i int, dst *Color) int {
	if i+1 >= len(p.n) {
		return i
	}
	switch p.n[i+1] {
	case 5:
		if i+2 < len(p.n) {
			*dst = Indexed(uint8(p.n[i+2]))
			return i + 2
		}
		return i + 1
	case 2:
		j := i + 2
		if j < len(p.n) && p.n[j] == 0 && len(p.n)-j >= 4 {
			j++ // colon form's empty colour-space id
		}
		if j+2 < len(p.n) {
			*dst = RGB(uint8(p.n[j]), uint8(p.n[j+1]), uint8(p.n[j+2]))
			return j + 2
		}
		return len(p.n) - 1
	}
	return i + 1
}
