package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
)

func seqMsgs(seqs ...uint64) []cli.BoardMessage {
	out := make([]cli.BoardMessage, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, cli.BoardMessage{Seq: s, Payload: []byte(fmt.Sprintf("m%d", s))})
	}
	return out
}

func TestApp_BoardOlderKey(t *testing.T) {
	a := &App{boardModal: NewBoardModal()}
	a.boardModal.Open()
	a.boardModal.SetSize(160, 45)
	a.boardModal.ApplyMessages("grp", seqMsgs(100, 101), nil, true)
	if _, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(modalKeys.BoardOlder)}); cmd == nil {
		t.Fatal("O on a topic's messages produced no request for older ones")
	}
	if !strings.Contains(a.boardModal.View(), modalKeys.BoardOlder+": older") {
		t.Error("the footer does not offer the key")
	}
}

func TestBoardModal_PrependOlder(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(160, 45)
	m.ApplyMessages("grp", seqMsgs(100, 101), nil, true)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.SelectedMsgSeq() != 101 {
		t.Fatalf("setup: selected %d, want 101", m.SelectedMsgSeq())
	}
	if m.OldestSeq() != 100 {
		t.Fatalf("OldestSeq = %d, want 100", m.OldestSeq())
	}
	m.PrependOlder("grp", seqMsgs(98, 99))
	if len(m.msgs) != 4 || m.msgs[0].Seq != 98 || m.OldestSeq() != 98 {
		t.Fatalf("after prepend: %d msgs, first %d", len(m.msgs), m.msgs[0].Seq)
	}
	if m.SelectedMsgSeq() != 101 {
		t.Errorf("the selection moved: now %d, want 101", m.SelectedMsgSeq())
	}
	m.PrependOlder("other", seqMsgs(1))
	if len(m.msgs) != 4 {
		t.Error("a page for another topic was applied")
	}
	m.PrependOlder("grp", nil)
	if !strings.Contains(m.View(), "no older messages") {
		t.Error("an empty page does not say there is nothing older")
	}
}

// The message list is windowed around the cursor: a topic paged back past the
// ring holds more rows than the modal has.
func TestBoardMessagesView_FitsTheModal(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(100, 30)
	seqs := make([]uint64, 130)
	for i := range seqs {
		seqs[i] = uint64(1000 + i)
	}
	m.ApplyMessages("grp", seqMsgs(seqs...), nil, true)
	for i := 0; i < 70; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	v := m.View()
	if h := lipgloss.Height(v); h > 30 {
		t.Errorf("view is %d rows in a 30-row modal", h)
	}
	if !strings.Contains(v, fmt.Sprintf("seq=%-5d", m.SelectedMsgSeq())) {
		t.Errorf("the selected message (seq %d) is not on screen", m.SelectedMsgSeq())
	}
}

// At 80 columns the footer wraps rather than cutting off its tail, where the
// older-page key sits; the list shrinks by what the footer takes.
func TestBoardMessagesFooter_WrapsAt80(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(80, 26)
	seqs := make([]uint64, 100)
	for i := range seqs {
		seqs[i] = uint64(1000 + i)
	}
	m.ApplyMessages("grp", seqMsgs(seqs...), nil, true)
	v := m.View()
	if !strings.Contains(strings.ReplaceAll(v, "\n", " "), modalKeys.BoardOlder+": older") {
		t.Errorf("the older-page key is cut off the footer at 80 columns:\n%s", v)
	}
	if h := lipgloss.Height(v); h > 26 {
		t.Errorf("view is %d rows in a 26-row modal", h)
	}
	// The real terminal clips at 80; a wider line is what cut the key off.
	for _, line := range strings.Split(v, "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("a %d-column line in an 80-column modal: %q", w, line)
		}
	}
}
