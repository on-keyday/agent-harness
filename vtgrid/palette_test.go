package vtgrid

// Indexed colours are the one place where switching a renderer can change
// OUTPUT that a caller already depends on: `session snapshot --color` reports
// `#rrggbb`, and a hex string only exists after some palette resolved the
// index. x/vt resolves through its own table; this package carries its own.
// If the two tables disagree, the same screen renders different colours after
// the switch, silently.
//
// So all 256 are compared, not spot-checked.

import (
	"fmt"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

func TestIndexedPaletteMatchesOracle(t *testing.T) {
	const n = 256
	emu := vt.NewEmulator(n, 1)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(io.Discard, emu) }()
	defer func() { _ = emu.Close(); <-done }()

	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "\x1b[38;5;%dmX", i)
	}
	_, _ = emu.Write([]byte(b.String()))

	bad := 0
	for i := 0; i < n; i++ {
		want := oracleHex(emu.CellAt(i, 0).Style.Fg)
		got := Indexed(uint8(i)).Hex()
		if got != want {
			if bad < 8 {
				t.Errorf("index %d: %s, oracle says %s", i, got, want)
			}
			bad++
		}
	}
	if bad > 8 {
		t.Errorf("... and %d more", bad-8)
	}
}

// oracleHex is how cli/snapshot_native.go renders an x/vt cell colour, kept
// here so the comparison is against the real projection rather than an
// idealised one.
func oracleHex(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// TestTruecolorAndDefaultRoundTrip covers the two kinds the palette does not
// touch: a 24-bit colour must come back exactly, and the terminal default must
// stay distinguishable from black rather than collapsing to #000000.
func TestTruecolorAndDefaultRoundTrip(t *testing.T) {
	if got := RGB(0xff, 0x87, 0xaf).Hex(); got != "#ff87af" {
		t.Errorf("truecolor Hex() = %q, want #ff87af", got)
	}
	if got := (Color{}).Hex(); got != "" {
		t.Errorf("default Hex() = %q, want the empty string", got)
	}
	if _, _, _, ok := (Color{}).RGB(); ok {
		t.Error("default colour reported components; a default is not a colour")
	}
	if got := Indexed(0).Hex(); got == "" {
		t.Error("palette index 0 rendered as the default; black is a colour")
	}
}
