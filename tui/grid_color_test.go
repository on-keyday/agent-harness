package tui

import (
	"image/color"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestPaneColorHex(t *testing.T) {
	if got := paneColorHex(nil); got != "" {
		t.Fatalf("nil color -> %q, want empty", got)
	}
	if got := paneColorHex(color.RGBA{R: 0xff, A: 0xff}); got != "#ff0000" {
		t.Fatalf("red -> %q, want #ff0000", got)
	}
	if got := paneColorHex(color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff}); got != "#123456" {
		t.Fatalf("-> %q, want #123456", got)
	}
}

func TestCellStyleKey_DistinguishesStyles(t *testing.T) {
	red := &uv.Cell{Content: "a", Style: uv.Style{Fg: color.RGBA{R: 0xff, A: 0xff}}}
	grn := &uv.Cell{Content: "a", Style: uv.Style{Fg: color.RGBA{G: 0xff, A: 0xff}}}
	bold := &uv.Cell{Content: "a", Style: uv.Style{Attrs: uv.AttrBold}}
	plain := &uv.Cell{Content: "a"}

	if cellStyleKey(red) == cellStyleKey(grn) {
		t.Fatal("different fg colors must produce different keys")
	}
	if cellStyleKey(red) == cellStyleKey(bold) {
		t.Fatal("color vs bold must differ")
	}
	if cellStyleKey(plain) != cellStyleKey(nil) {
		t.Fatal("an unstyled cell must key the same as the default")
	}
	// cellLipgloss for a colored cell sets a foreground.
	if got := cellLipgloss(red).GetForeground(); got == nil {
		t.Fatal("cellLipgloss for a red cell must set a foreground")
	}
}
