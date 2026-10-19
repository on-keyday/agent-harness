package tui

import (
	"encoding/hex"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func auditApp(t *testing.T) (*App, string) {
	t.Helper()
	a := New(Config{})
	a.width, a.height = 160, 40
	var sp, obs, done protocol.TaskID
	sp.Id[0], obs.Id[0], done.Id[0] = 0xab, 0xcd, 0xef
	a.tasks.SetRows([]protocol.TaskInfo{
		{Id: sp, Status: protocol.TaskStatus_Running, Capabilities: protocol.Capability_Spawn | protocol.Capability_ExecCowrite},
		{Id: obs, Status: protocol.TaskStatus_Running, Capabilities: protocol.Capability_BoardObserve},
		{Id: done, Status: protocol.TaskStatus_Succeeded, Capabilities: protocol.Capability_Purge},
	}, nil)
	return a, hex.EncodeToString(sp.Id[:])
}

func press(t *testing.T, a *App, keys ...tea.KeyMsg) *App {
	t.Helper()
	for _, k := range keys {
		m, _ := a.Update(k)
		a = m.(*App)
	}
	return a
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var enter = tea.KeyMsg{Type: tea.KeyEnter}
var down = tea.KeyMsg{Type: tea.KeyDown}

func TestIOpensCapsAudit(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"))
	if !a.capsAuditModal.IsOpen() {
		t.Fatal("i did not open the caps audit modal")
	}
	if v := a.capsAuditModal.View(); !strings.Contains(v, "spawn") || strings.Contains(v, "purge") {
		t.Fatalf("modal view lacks spawn or shows the ended task's purge:\n%s", v)
	}
}

func TestCapsAuditModalExpandsBit(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"), enter) // cursor starts on the first row: spawn
	if !strings.Contains(a.capsAuditModal.View(), "ab000000") {
		t.Fatalf("enter on spawn did not list its holder:\n%s", a.capsAuditModal.View())
	}
}

func TestCapsAuditModalHolderOpensRegrant(t *testing.T) {
	a, want := auditApp(t)
	a = press(t, a, runes("i"), enter, down, enter)
	if a.capsAuditModal.IsOpen() {
		t.Fatal("the modal stayed open after choosing a holder")
	}
	if !a.authorityPicker.IsOpen() || a.authorityPicker.Mode() != PickerModeRegrant || a.authorityPicker.TargetID() != want {
		t.Fatalf("picker open=%v target=%q, want re-grant on %s", a.authorityPicker.IsOpen(), a.authorityPicker.TargetID(), want)
	}
}

func TestCapsAuditModalToggles(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"), runes("a"))
	if !strings.Contains(a.capsAuditModal.View(), "purge") {
		t.Fatal("a did not add the ended task's bit")
	}
	a = press(t, a, runes("e"))
	if !strings.Contains(a.capsAuditModal.View(), "notify") {
		t.Fatal("e did not add the unheld bits")
	}
}

func TestCapsAuditModalScopeNone(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"))
	// expand every row
	for i := 0; i < 6; i++ {
		a = press(t, a, enter, down)
	}
	if !strings.Contains(a.capsAuditModal.View(), "scope 対象外") {
		t.Fatalf("board_observe holder lacks the scope-none mark:\n%s", a.capsAuditModal.View())
	}
}
