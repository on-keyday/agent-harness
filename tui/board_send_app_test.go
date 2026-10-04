package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Through the App's real key routing (updateKey -> inBoardModal): `a` on the
// chain list opens the editor, typed keys land in it, and the Enter that sends
// is consumed by the editor -- it must not also open the conversation.
func TestApp_ChainListReplyKeysStayInTheEditor(t *testing.T) {
	a := &App{boardModal: NewBoardModal()}
	a.boardModal.Open()
	a.boardModal.SetSize(160, 45)
	a.boardModal.ApplyChains(chainFixture(t))
	a.boardModal.mode = boardChainList

	a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !a.boardModal.Composing() {
		t.Fatal("`a` on the chain list did not open the editor")
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x1")})
	_, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not produce the send")
	}
	if a.boardModal.Composing() {
		t.Error("still composing after Enter")
	}
	if got := a.boardModal.Mode(); got != boardChainList {
		t.Errorf("mode after the sending Enter = %v, want the chain list", got)
	}
}
