package vtgrid

// Attr is the set of text attributes a cell carries.
//
// Only the attributes a *reader* of a screen acts on are modelled. Faint is
// the one that earns its place beyond decoration: an agent TUI draws ghost
// autocomplete and placeholder text faint, and without it a suggestion the
// user never typed is indistinguishable from input they did.
type Attr uint8

const (
	AttrBold Attr = 1 << iota
	AttrFaint
	AttrItalic
	AttrUnderline
	AttrBlink
	AttrReverse
	AttrConceal
	AttrStrike
)

// ColorKind says how to read a Color's remaining fields. Default is not black:
// it means the cell never set one, which a renderer must be able to tell apart
// from a cell that set the palette entry that happens to look like the
// background.
type ColorKind uint8

const (
	ColorDefault ColorKind = iota
	ColorIndexed
	ColorRGB
)

// Color is a cell colour in the form it arrived: a palette index stays an
// index. Resolving indices to RGB is a display decision and belongs to
// whoever is displaying, not to the grid.
type Color struct {
	Kind ColorKind
	N    uint8 // palette index, when Kind == ColorIndexed
	R    uint8
	G    uint8
	B    uint8
}

// Indexed and RGB build the two non-default colours.
func Indexed(n uint8) Color     { return Color{Kind: ColorIndexed, N: n} }
func RGB(r, g, b uint8) Color   { return Color{Kind: ColorRGB, R: r, G: g, B: b} }
func (c Color) IsDefault() bool { return c.Kind == ColorDefault }

// Cell is one grid position.
//
// Width is the column span of Rune: 1 normally, 2 for an East Asian wide
// glyph, and **0 for the continuation cell that follows a wide one**. A
// continuation carries no rune of its own; a reader walking a row must skip it
// rather than emit a second character, and a writer landing on one must clear
// the pair. Modelling it as a cell (rather than leaving the grid ragged) keeps
// column arithmetic exact, which is the whole reason a grid exists.
type Cell struct {
	Rune  rune
	Width int8
	Attr  Attr
	FG    Color
	BG    Color
}

// blank is the cell an erase leaves behind. Erasing keeps the *background*
// colour of the pen in real terminals; we follow that, so a cleared region
// under a coloured background still reads as coloured.
func blank(bg Color) Cell { return Cell{Rune: ' ', Width: 1, BG: bg} }

// IsBlank reports whether the cell holds nothing a reader would show.
func (c Cell) IsBlank() bool { return c.Rune == ' ' || c.Rune == 0 }

// pen is the current drawing state: what a newly written cell inherits.
type pen struct {
	Attr Attr
	FG   Color
	BG   Color
}

func (p *pen) reset() { *p = pen{} }
