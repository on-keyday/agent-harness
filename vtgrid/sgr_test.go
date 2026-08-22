package vtgrid

// SGR sub-parameters, which arrive after a COLON and belong to the code before
// them. Folding colons and semicolons together reads each sub-parameter as a
// code of its own and invents attributes: `4:3` (a curly underline) became
// underline+italic, `4:1` became underline+bold, and `58;5;1` (an underline
// colour) became blink+bold.
//
// None of this appears in any captured corpus — zero colon sub-parameters,
// zero SGR 58 — so the parity suite had nothing to say about it. It was found
// by asking what the TUI reads from a cell, since the TUI carries underline as
// a typed field and would have been the surface where the damage showed.

import "testing"

func penAt(t *testing.T, in string) Cell {
	t.Helper()
	term := New(8, 1)
	_, _ = term.Write([]byte(in))
	return term.CellAt(0, 0)
}

func TestSGRSubParameters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		attr  Attr
		under Underline
	}{
		{"plain underline", "\x1b[4mX", 0, UnderlineSingle},
		{"semicolon really is two codes", "\x1b[4;3mX", AttrItalic, UnderlineSingle},
		{"styled underline is ONE attribute", "\x1b[4:3mX", 0, UnderlineCurly},
		{"single styled underline", "\x1b[4:1mX", 0, UnderlineSingle},
		{"double via 4:2", "\x1b[4:2mX", 0, UnderlineDouble},
		{"dotted and dashed", "\x1b[4:4mX", 0, UnderlineDotted},
		{"4:0 turns underline off", "\x1b[4m\x1b[4:0mX", 0, UnderlineNone},
		{"24 turns underline off", "\x1b[4:3m\x1b[24mX", 0, UnderlineNone},
		{"bold survives beside a styled underline", "\x1b[1;4:3mX", AttrBold, UnderlineCurly},
		{"blink and rapid blink are different", "\x1b[5;6mX", AttrBlink | AttrRapidBlink, UnderlineNone},
		{"25 clears both blinks", "\x1b[5;6m\x1b[25mX", 0, UnderlineNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := penAt(t, tc.in)
			if c.Attr != tc.attr {
				t.Errorf("Attr = %08b, want %08b", c.Attr, tc.attr)
			}
			if c.Under != tc.under {
				t.Errorf("Underline = %d, want %d", c.Under, tc.under)
			}
		})
	}
}

// TestSGRUnderlineColourIsConsumed pins that 58 swallows its own arguments.
// It is not modelled — the cell has no underline colour — but a code that
// takes arguments and does not consume them leaves them to be read as
// attributes, which is the actual defect.
func TestSGRUnderlineColourIsConsumed(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Color
	}{
		{"\x1b[58;5;1mX", Indexed(1)},
		{"\x1b[58;2;10;20;30mX", RGB(10, 20, 30)},
		{"\x1b[58:2::10:20:30mX", RGB(10, 20, 30)},
		{"\x1b[59mX", Color{}},
		{"\x1b[58;5;1m\x1b[24mX", Color{}}, // 24 resets the colour with the underline
	} {
		c := penAt(t, tc.in)
		if c.Attr != 0 {
			t.Errorf("%q left Attr = %08b, want 0", tc.in, c.Attr)
		}
		if c.UnderFG != tc.want {
			t.Errorf("%q underline colour = %+v, want %+v", tc.in, c.UnderFG, tc.want)
		}
	}
	// …and it must not disturb a colour set beside it.
	if got := penAt(t, "\x1b[31;58;5;2mX").FG; got != Basic(1) {
		t.Errorf("foreground = %+v, want Basic(1)", got)
	}
}

// TestSGRColonColourForm covers the colour spelling the colon form allows,
// including its empty colour-space slot. The flat parser guessed at that slot
// with a heuristic that a literal zero red channel would have defeated; with
// the separators recorded there is nothing left to guess.
func TestSGRColonColourForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want Color
	}{
		{"semicolon truecolor", "\x1b[38;2;255;135;175mX", RGB(255, 135, 175)},
		{"colon truecolor", "\x1b[38:2:255:135:175mX", RGB(255, 135, 175)},
		{"colon truecolor with colour-space slot", "\x1b[38:2::255:135:175mX", RGB(255, 135, 175)},
		{"zero red is a channel, not a slot", "\x1b[38;2;0;135;175mX", RGB(0, 135, 175)},
		{"colon indexed", "\x1b[38:5:196mX", Indexed(196)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := penAt(t, tc.in).FG; got != tc.want {
				t.Errorf("foreground = %+v, want %+v", got, tc.want)
			}
		})
	}
}
