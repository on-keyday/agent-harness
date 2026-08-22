package vtgrid

import "strings"

// Two things a cell can carry are variable-length: a hyperlink's URL and the
// combining marks that turn its base rune into a grapheme cluster. Storing
// either in the cell means a string header — 16 bytes plus a heap object — on
// every one of a screen's ~5,000 cells, whether or not it has one, and that
// alone would cost more than the whole grid.
//
// Both are rare and both repeat heavily: a hyperlinked pane reuses a handful of
// URLs, and the same accented character recurs. So the Terminal interns them
// and the cell carries a uint16 index, with 0 meaning none.
//
// The tables only grow. A screen is bounded and a session is not, so a
// long-running pane that cycles through thousands of distinct URLs would grow
// this without bound — hence the caps below, past which the cell simply
// carries no link rather than the table carrying every one it ever saw.

// Link is a hyperlink as OSC 8 delivers it: an id and other parameters, then
// the URI. Params is kept because `id=` is what lets a terminal treat two runs
// as one link, and dropping it would merge or split them wrongly.
type Link struct {
	URL    string
	Params string
}

const (
	maxLinks     = 4096
	maxClusters  = 4096
	maxCombining = 8 // combining marks kept per cell
)

// internLink returns the index for a link, adding it if new. Zero means "no
// link", so the table's first slot is never used.
func (t *Terminal) internLink(l Link) uint16 {
	if l.URL == "" {
		return 0
	}
	key := l.Params + "\x00" + l.URL
	if id, ok := t.linkIndex[key]; ok {
		return id
	}
	if len(t.links) >= maxLinks {
		return 0
	}
	if t.links == nil {
		t.links = []Link{{}} // index 0 is "none"
		t.linkIndex = map[string]uint16{}
	}
	t.links = append(t.links, l)
	id := uint16(len(t.links) - 1)
	t.linkIndex[key] = id
	return id
}

// Link returns the hyperlink a cell belongs to. ok is false for a cell with
// none, which is almost all of them.
func (t *Terminal) Link(c Cell) (l Link, ok bool) {
	if c.link == 0 || int(c.link) >= len(t.links) {
		return Link{}, false
	}
	return t.links[c.link], true
}

// internCombining returns the index for a run of combining marks.
func (t *Terminal) internCombining(marks []rune) uint16 {
	if len(marks) == 0 {
		return 0
	}
	key := string(marks)
	if id, ok := t.combIndex[key]; ok {
		return id
	}
	if len(t.combining) >= maxClusters {
		return 0
	}
	if t.combining == nil {
		t.combining = []string{""} // index 0 is "none"
		t.combIndex = map[string]uint16{}
	}
	t.combining = append(t.combining, key)
	id := uint16(len(t.combining) - 1)
	t.combIndex[key] = id
	return id
}

// Text returns what a cell displays: its base rune followed by any combining
// marks. Use it rather than Cell.Rune whenever the result is shown to someone
// — the marks are the difference between "が" and "か".
//
// A continuation cell (Width 0) returns "": it is the right half of the glyph
// to its left and has no text of its own.
func (t *Terminal) Text(c Cell) string {
	if c.Width == 0 {
		return ""
	}
	if c.Rune == 0 {
		return " "
	}
	if c.combining == 0 || int(c.combining) >= len(t.combining) {
		return string(c.Rune)
	}
	var b strings.Builder
	b.WriteRune(c.Rune)
	b.WriteString(t.combining[c.combining])
	return b.String()
}

// addCombining attaches a mark to the cell most recently printed. A combining
// mark has no width of its own; it belongs to the glyph before it, and a
// terminal that drops it silently changes the text.
func (t *Terminal) addCombining(r rune) {
	if t.lastX < 0 || t.lastY < 0 || t.lastX >= t.cols || t.lastY >= t.rows {
		return
	}
	cell := &t.scr.lines[t.lastY][t.lastX]
	if cell.Rune == 0 {
		return // nothing to attach to
	}
	prev := ""
	if cell.combining != 0 && int(cell.combining) < len(t.combining) {
		prev = t.combining[cell.combining]
	}
	if len([]rune(prev)) >= maxCombining {
		return
	}
	cell.combining = t.internCombining([]rune(prev + string(r)))
}
