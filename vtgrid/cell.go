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
	// ColorBasic is a palette entry 0-15 as spelled by SGR 30-37 / 90-97.
	// ColorIndexed is the SAME palette entry spelled `38;5;n`. They are kept
	// apart because terminals do not treat them the same: the "bold is bright"
	// heuristic that most apply to 30-37 they do not apply to 38;5;n, so
	// `ESC[1;31m` and `ESC[1;38;5;1m` can show different colours. Collapsing
	// the two loses that, and re-emitting the wrong one repaints the screen —
	// measured across the captured corpora, the extended form is the MAJORITY
	// (13,274 uses against 11,922), and herdr, ConPTY and PowerShell use
	// nothing else.
	ColorBasic
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

// Basic, Indexed and RGB build the three non-default colours. Basic and
// Indexed name the same palette entry and differ only in how the wire spelled
// it; see ColorBasic.
func Basic(n uint8) Color       { return Color{Kind: ColorBasic, N: n} }
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

// cubeLevels are the six component values of the 6x6x6 colour cube that
// occupies palette entries 16-231.
var cubeLevels = [6]uint8{0x00, 0x5f, 0x87, 0xaf, 0xd7, 0xff}

// systemColors are palette entries 0-15. Unlike the cube and the greys these
// are not computed from a rule — they are the VGA/xterm values, and a terminal
// with a theme will show its own instead. They are here so that a hex render of
// an indexed colour matches what x/vt produces for the same cell, which is what
// keeps `session snapshot --color` output stable across the switch.
var systemColors = [16][3]uint8{
	{0x00, 0x00, 0x00}, {0x80, 0x00, 0x00}, {0x00, 0x80, 0x00}, {0x80, 0x80, 0x00},
	{0x00, 0x00, 0x80}, {0x80, 0x00, 0x80}, {0x00, 0x80, 0x80}, {0xc0, 0xc0, 0xc0},
	{0x80, 0x80, 0x80}, {0xff, 0x00, 0x00}, {0x00, 0xff, 0x00}, {0xff, 0xff, 0x00},
	{0x00, 0x00, 0xff}, {0xff, 0x00, 0xff}, {0x00, 0xff, 0xff}, {0xff, 0xff, 0xff},
}

// RGB resolves a colour to components. ok is false for the terminal default,
// which is not a colour and must stay distinguishable from black.
//
// An indexed colour is resolved through the standard 256-entry palette. That is
// a DISPLAY decision being made on the caller's behalf: a terminal with a theme
// paints index 1 as whatever its theme says, and a caller that cares should read
// Kind and N instead. It is offered because the alternative — every consumer
// carrying its own copy of the same table — is worse.
func (c Color) RGB() (r, g, b uint8, ok bool) {
	switch c.Kind {
	case ColorRGB:
		return c.R, c.G, c.B, true
	case ColorBasic, ColorIndexed:
		switch n := int(c.N); {
		case n < 16:
			s := systemColors[n]
			return s[0], s[1], s[2], true
		case n < 232:
			n -= 16
			return cubeLevels[(n/36)%6], cubeLevels[(n/6)%6], cubeLevels[n%6], true
		default:
			g := uint8(8 + 10*(n-232))
			return g, g, g, true
		}
	}
	return 0, 0, 0, false
}

// Hex renders a colour as "#rrggbb", or "" for the terminal default.
func (c Color) Hex() string {
	r, g, b, ok := c.RGB()
	if !ok {
		return ""
	}
	const digits = "0123456789abcdef"
	out := []byte("#000000")
	for i, v := range [3]uint8{r, g, b} {
		out[1+i*2] = digits[v>>4]
		out[2+i*2] = digits[v&0xf]
	}
	return string(out)
}
