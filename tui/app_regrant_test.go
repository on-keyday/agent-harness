package tui

import (
	"encoding/hex"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestReGrantKeyPrefillsCmdline: `a` on the tasks pane is the task list's
// re-grant action — focus jumps to the command line prefilled with
// `caps set <id> ` so Enter routes through parseSetCaps → DoSetCaps.
func TestReGrantKeyPrefillsCmdline(t *testing.T) {
	a := New(Config{})
	var tid protocol.TaskID
	tid.Id[0] = 0xab
	a.tasks.SetRows([]protocol.TaskInfo{{Id: tid, Status: protocol.TaskStatus_Running}}, nil)

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	if a.focus != focusCmdline {
		t.Fatalf("focus = %v, want focusCmdline", a.focus)
	}
	want := "caps set " + hex.EncodeToString(tid.Id[:]) + " "
	if got := a.cmdline.Value(); got != want {
		t.Fatalf("cmdline = %q, want %q", got, want)
	}
}

// TestReGrantKeyNoSelection: with nothing selected the key warns instead of
// leaving a half-built `caps set  ` on the command line.
func TestReGrantKeyNoSelection(t *testing.T) {
	a := New(Config{})
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	if a.focus == focusCmdline {
		t.Fatal("no selection must not move focus to the command line")
	}
	if got := a.cmdline.Value(); got != "" {
		t.Fatalf("cmdline = %q, want empty", got)
	}
}
