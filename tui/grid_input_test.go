package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyToBytes(t *testing.T) {
	cases := []struct {
		name string
		m    tea.KeyMsg
		want string
	}{
		{"runes", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")}, "hi"},
		{"space", tea.KeyMsg{Type: tea.KeySpace}, " "},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, "\r"},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}, "\t"},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}, "\x1b"},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, "\x7f"},
		{"ctrl-c", tea.KeyMsg{Type: tea.KeyCtrlC}, "\x03"},
		{"ctrl-u", tea.KeyMsg{Type: tea.KeyCtrlU}, "\x15"},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, "\x1b[A"},
		{"down", tea.KeyMsg{Type: tea.KeyDown}, "\x1b[B"},
		{"left", tea.KeyMsg{Type: tea.KeyLeft}, "\x1b[D"},
		{"alt-x", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x"), Alt: true}, "\x1bx"},
		{"ctrl-at-nul", tea.KeyMsg{Type: tea.KeyCtrlAt}, ""}, // bare Ctrl / Ctrl+@ -> dropped, not ^@
	}
	for _, c := range cases {
		if got := string(keyToBytes(c.m)); got != c.want {
			t.Errorf("%s: keyToBytes = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGridModel_InputModeToggle(t *testing.T) {
	m := NewGridModel()
	m.panes = mkPanes("AAAAAAAA", "BBBBBBBB")
	m.open = true
	m.SetSize(120, 40)

	// i enters input mode on the focused pane.
	m2, _ := m.Update(keyMsg("i"))
	if !m2.input {
		t.Fatal("i should enter input mode")
	}
	// In input mode, 'l' is forwarded to the pane, NOT acted on as navigation:
	// input mode stays on and focus does not change.
	m3, _ := m2.Update(keyMsg("l"))
	if !m3.input {
		t.Fatal("keys in input mode must not exit input mode")
	}
	if m3.focus != 0 {
		t.Fatalf("focus must not navigate in input mode (got %d)", m3.focus)
	}
	// Ctrl+] exits back to navigation.
	m4, _ := m3.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	if m4.input {
		t.Fatal("Ctrl+] should exit input mode")
	}
	// Now 'l' navigates again (0 -> 1).
	m5, _ := m4.Update(keyMsg("l"))
	if m5.focus != 1 {
		t.Fatalf("after exiting input mode, l should navigate (focus -> %d)", m5.focus)
	}
}

// A bare Ctrl press reaches keyToBytes in more than one shape. The Type==0
// guard catches KeyCtrlAt; terminals that report modifier presses can deliver
// it as KeyRunes carrying a NUL rune instead, which used to put a ^@ in the
// pane. Nothing worth forwarding encodes to NUL, so neither shape may produce
// bytes.
func TestKeyToBytesNeverForwardsNUL(t *testing.T) {
	cases := map[string]tea.KeyMsg{
		"KeyCtrlAt (Type 0)": {Type: tea.KeyCtrlAt},
		"NUL rune":           {Type: tea.KeyRunes, Runes: []rune{0}},
		"NUL rune with alt":  {Type: tea.KeyRunes, Runes: []rune{0}, Alt: true},
	}
	for name, msg := range cases {
		if got := keyToBytes(msg); len(got) != 0 {
			t.Errorf("%s: keyToBytes = %q, want nothing forwarded", name, got)
		}
	}
	// A NUL mixed into real input loses only the NUL.
	if got := keyToBytes(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a', 0, 'b'}}); string(got) != "ab" {
		t.Errorf("mixed runes = %q, want \"ab\"", got)
	}
	// Ordinary keys are untouched.
	if got := keyToBytes(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")}); string(got) != "hi" {
		t.Errorf("plain runes = %q, want \"hi\"", got)
	}
	if got := keyToBytes(tea.KeyMsg{Type: tea.KeyCtrlC}); string(got) != "\x03" {
		t.Errorf("ctrl+c = %q, want \\x03", got)
	}
}
