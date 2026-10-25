package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// `L` on the tasks pane opens the forward modal in link mode, through the
// same dispatcher an operator's keystroke goes through.
func TestLinkKeyOpensForwardModalInLinkMode(t *testing.T) {
	a, want := regrantApp(t)
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	a = m.(*App)
	if !a.portForwardModal.IsOpen() {
		t.Fatal("L did not open the forward modal")
	}
	if a.portForwardModal.Mode() != ForwardLink {
		t.Fatalf("mode = %v, want ForwardLink", a.portForwardModal.Mode())
	}
	if a.portForwardModal.TaskID() != want {
		t.Fatalf("task = %q, want %q", a.portForwardModal.TaskID(), want)
	}
	if !strings.Contains(a.portForwardModal.input.Placeholder, "<client-addr>/<prefix> <runner-addr>") {
		t.Fatalf("placeholder = %q", a.portForwardModal.input.Placeholder)
	}
	if !strings.Contains(a.portForwardModal.View(), "--ip") {
		t.Fatalf("the modal does not say it is a link:\n%s", a.portForwardModal.View())
	}
}

func TestLinkKeyNoSelection(t *testing.T) {
	a := New(Config{})
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	a = m.(*App)
	if a.portForwardModal.IsOpen() {
		t.Fatal("the modal opened with no task selected")
	}
}

// A line the parser refuses is reported, with the expected form, before
// anything is opened or registered.
func TestDoStartIPLinkRefusesABadLine(t *testing.T) {
	msg := DoStartIPLink(nil, "abcdef", "10.200.0.1/30", 1, nil)()
	st, ok := msg.(PortForwardStatusMsg)
	if !ok || !strings.Contains(st.Line, "want <client-addr>/<prefix> <runner-addr>") {
		t.Fatalf("msg = %#v", msg)
	}
}

func TestForwardLinkFlag(t *testing.T) {
	if ForwardLink.flag() != "--ip" {
		t.Fatalf("flag = %q", ForwardLink.flag())
	}
}
