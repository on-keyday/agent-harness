package tui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/mattn/go-runewidth"
)

// minColWidth keeps a squeezed column wide enough that a truncated value is
// still recognisable; narrower than this it is only noise.
const minColWidth = 3

// tableCellPadding is what bubbles' table adds to EVERY cell — one cell of
// padding on each side. It is not part of table.Column.Width, so a table
// renders at sum(Width) + 2*len(cols).
//
// The trap this file exists for: table.SetWidth does NOT resize columns. It
// records a width for the viewport and leaves the columns exactly as declared,
// so a table whose columns sum wider than its panel draws past the panel
// border, the terminal wraps the row, and every line below it is displaced —
// the whole frame looks shredded. Panels must therefore compute their own
// column widths from the space they were given, on every resize.
const tableCellPadding = 2

// fitColumns returns base rewritten to render in w cells: surplus goes to the
// flex column, and a shortfall comes out of every column in proportion,
// floored at minColWidth.
//
// base is never mutated and is always the input, so a resize is computed from
// the natural widths rather than from the previous result — otherwise repeated
// SetSize calls would ratchet the columns down and never recover when the
// terminal grows again.
func fitColumns(base []table.Column, w, flex int) []table.Column {
	if len(base) == 0 {
		return nil
	}
	out := make([]table.Column, len(base))
	copy(out, base)
	if flex < 0 || flex >= len(out) {
		flex = len(out) - 1
	}

	avail := w - tableCellPadding*len(out)

	// Per-column floor: a column keeps room for its own header where it can,
	// so a squeezed table still reads "Status  Repo" rather than "Sta…  R…".
	// Capped by the natural width so a column declared narrower than its title
	// (Act, 3 cells of header over 8 of data) is not inflated by the floor.
	floors := make([]int, len(out))
	for i, c := range out {
		f := runewidth.StringWidth(c.Title)
		if f > c.Width {
			f = c.Width
		}
		if f < minColWidth {
			f = minColWidth
		}
		floors[i] = f
	}
	// Fitting the panel outranks readable headers. When the header floors
	// alone do not fit — the tasks table's seven columns in a 38-cell half at
	// 80 columns — they are dropped to the bare minimum and the headers
	// truncate. Keeping them would put the table back outside its panel,
	// which is the bug this file fixes.
	floorSum := 0
	for _, f := range floors {
		floorSum += f
	}
	if floorSum > avail {
		for i := range floors {
			floors[i] = minColWidth
		}
	}

	natural := 0
	for _, c := range out {
		natural += c.Width
	}

	switch {
	case natural == avail:
	case natural < avail:
		out[flex].Width += avail - natural
	default:
		// Proportional shrink. Track what was actually handed out instead of
		// trusting the arithmetic: integer division loses cells, and the
		// per-column floor ADDS them back — a column whose share rounds below
		// minColWidth is raised to it, which can push the total past avail.
		// Both directions have to be reconciled or the table renders a few
		// cells wider than the panel, which is the whole bug.
		used := 0
		for i := range out {
			scaled := out[i].Width * avail / natural
			if scaled < floors[i] {
				scaled = floors[i]
			}
			out[i].Width = scaled
			used += scaled
		}
		if used < avail {
			out[flex].Width += avail - used
			used = avail
		}
		for used > avail {
			// Claw back from the widest column that still has room. Anything
			// at the floor is left alone; if they all are, the table cannot
			// fit and the clamp above already accepted that.
			wi, slack := -1, 0
			for i := range out {
				if s := out[i].Width - floors[i]; s > slack {
					wi, slack = i, s
				}
			}
			if wi < 0 {
				// Every column is at its floor: the table cannot fit in w.
				// Stop here rather than emit zero/negative widths (bubbles
				// renders those as ragged rows); App.View already refuses
				// terminals below 80 columns, so this is the degenerate case
				// of a very narrow modal, not a normal layout.
				break
			}
			out[wi].Width--
			used--
		}
	}
	return out
}

// flexColumn resolves a flex column by TITLE rather than by index, so
// inserting a column into a table's list cannot silently point the stretch at
// the wrong one. An unknown title falls back to the last column.
func flexColumn(cols []table.Column, title string) int {
	for i, c := range cols {
		if c.Title == title {
			return i
		}
	}
	return len(cols) - 1
}

// tableRenderWidth is what a column set actually occupies on screen. Tests use
// it; production code goes through fitColumns.
func tableRenderWidth(cols []table.Column) int {
	total := tableCellPadding * len(cols)
	for _, c := range cols {
		total += c.Width
	}
	return total
}
