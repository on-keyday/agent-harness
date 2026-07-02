package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCmdHistoryNavigationRestoresDraft(t *testing.T) {
	a := New(Config{})
	a.addCmdHistory("help")
	a.addCmdHistory("caps")

	a.cmdline.SetValue("draft")

	if !a.navigateCmdHistory(true) {
		t.Fatal("expected Up to navigate history")
	}
	if got := a.cmdline.Value(); got != "caps" {
		t.Fatalf("after first Up got %q, want caps", got)
	}

	if !a.navigateCmdHistory(true) {
		t.Fatal("expected second Up to navigate history")
	}
	if got := a.cmdline.Value(); got != "help" {
		t.Fatalf("after second Up got %q, want help", got)
	}

	if !a.navigateCmdHistory(false) {
		t.Fatal("expected Down to navigate history")
	}
	if got := a.cmdline.Value(); got != "caps" {
		t.Fatalf("after first Down got %q, want caps", got)
	}

	if !a.navigateCmdHistory(false) {
		t.Fatal("expected second Down to restore draft")
	}
	if got := a.cmdline.Value(); got != "draft" {
		t.Fatalf("after second Down got %q, want draft", got)
	}
}

func TestCmdHistorySkipsBlankAndConsecutiveDuplicate(t *testing.T) {
	a := New(Config{})

	a.addCmdHistory("help")
	a.addCmdHistory("help")
	a.addCmdHistory("   ")
	a.addCmdHistory("caps")

	want := []string{"help", "caps"}
	if len(a.cmdHistory) != len(want) {
		t.Fatalf("history=%v, want %v", a.cmdHistory, want)
	}
	for i := range want {
		if a.cmdHistory[i] != want[i] {
			t.Fatalf("history=%v, want %v", a.cmdHistory, want)
		}
	}
}

func TestCmdlineEnterAddsInvalidCommandToHistory(t *testing.T) {
	a := New(Config{})
	a.focus = focusCmdline
	a.cmdline.Focus()
	a.cmdline.SetValue("not-a-command")

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)

	if len(a.cmdHistory) != 1 || a.cmdHistory[0] != "not-a-command" {
		t.Fatalf("history=%v, want [not-a-command]", a.cmdHistory)
	}
	if got := a.cmdline.Value(); got != "" {
		t.Fatalf("cmdline value after enter = %q, want empty", got)
	}
}
