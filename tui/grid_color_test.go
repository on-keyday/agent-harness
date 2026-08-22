package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/vtgrid"
)

// Colour hex rendering itself is pinned in vtgrid (TestIndexedPaletteMatchesOracle
// compares all 256 palette entries against the reference emulator). What
// belongs here is the pane's own decision: which style differences must break a
// coalesced run, and that a colour reaches lipgloss at all.
func TestCellStyleKey_DistinguishesStyles(t *testing.T) {
	red := vtgrid.Cell{Rune: 'a', Width: 1, FG: vtgrid.RGB(0xff, 0, 0)}
	grn := vtgrid.Cell{Rune: 'a', Width: 1, FG: vtgrid.RGB(0, 0xff, 0)}
	bold := vtgrid.Cell{Rune: 'a', Width: 1, Attr: vtgrid.AttrBold}
	curly := vtgrid.Cell{Rune: 'a', Width: 1, Under: vtgrid.UnderlineCurly}
	plain := vtgrid.Cell{Rune: 'a', Width: 1}

	if cellStyleKey(red) == cellStyleKey(grn) {
		t.Error("different fg colors must produce different keys")
	}
	if cellStyleKey(red) == cellStyleKey(bold) {
		t.Error("color vs bold must differ")
	}
	if cellStyleKey(bold) == cellStyleKey(curly) {
		t.Error("bold vs underlined must differ")
	}
	if cellStyleKey(plain) != cellStyleKey(vtgrid.Cell{}) {
		t.Error("an unstyled cell must key the same as the zero cell")
	}
	if got := cellLipgloss(red).GetForeground(); got == nil {
		t.Error("cellLipgloss for a red cell must set a foreground")
	}
	if !cellLipgloss(curly).GetUnderline() {
		t.Error("a curly underline must still reach lipgloss as an underline")
	}
}

// The pane resolves a palette index to the same hex the snapshot path reports,
// so a session's colours look the same in a grid pane and in `session snapshot
// --color`.
func TestCellStyleKeyUsesTheResolvedPalette(t *testing.T) {
	basic := vtgrid.Cell{Rune: 'a', Width: 1, FG: vtgrid.Basic(1)}
	indexed := vtgrid.Cell{Rune: 'a', Width: 1, FG: vtgrid.Indexed(1)}
	if cellStyleKey(basic) != cellStyleKey(indexed) {
		t.Errorf("the two spellings of palette entry 1 render alike, so they must "+
			"coalesce: %q vs %q", cellStyleKey(basic), cellStyleKey(indexed))
	}
}
