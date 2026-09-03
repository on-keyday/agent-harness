package tui

import (
	"encoding/hex"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func regrantApp(t *testing.T) (*App, string) {
	t.Helper()
	a := New(Config{})
	var tid protocol.TaskID
	tid.Id[0] = 0xab
	a.tasks.SetRows([]protocol.TaskInfo{{Id: tid, Status: protocol.TaskStatus_Running}}, nil)
	return a, hex.EncodeToString(tid.Id[:])
}

// TestReGrantKeyOpensPicker: `a` on the tasks pane is the task list's
// re-grant action — it opens the authority picker on the selected task.
func TestReGrantKeyOpensPicker(t *testing.T) {
	a, want := regrantApp(t)
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	if !a.authorityPicker.IsOpen() {
		t.Fatal("picker must open")
	}
	if a.authorityPicker.Mode() != PickerModeRegrant {
		t.Fatal("picker must open in regrant mode")
	}
	if got := a.authorityPicker.TargetID(); got != want {
		t.Fatalf("TargetID = %q, want %q", got, want)
	}
	if got := a.cmdline.Value(); got != "" {
		t.Fatalf("cmdline must stay empty, got %q", got)
	}
}

// TestReGrantKeyNoSelection: with nothing selected the key warns and the
// picker stays closed.
func TestReGrantKeyNoSelection(t *testing.T) {
	a := New(Config{})
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	if a.authorityPicker.IsOpen() {
		t.Fatal("picker must not open without a selection")
	}
}

// TestPickerEscCloses: Esc closes without dispatching anything.
func TestPickerEscCloses(t *testing.T) {
	a, _ := regrantApp(t)
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	a = m.(*App)
	if a.authorityPicker.IsOpen() {
		t.Fatal("Esc must close the picker")
	}
	if cmd != nil {
		t.Fatal("Esc must not dispatch a command")
	}
}

// TestPickerEnterDispatchesSetCaps: Enter in regrant mode dispatches the
// DoSetCaps closure when a client is bound. The closure is not executed.
func TestPickerEnterDispatchesSetCaps(t *testing.T) {
	a, _ := regrantApp(t)
	a.BindClient(&cli.Client{})
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if a.authorityPicker.IsOpen() {
		t.Fatal("Enter must close the picker")
	}
	if cmd == nil {
		t.Fatal("Enter with a bound client must dispatch DoSetCaps")
	}
}

// TestPickerEnterNilClient: Enter with a nil client warns instead of
// dispatching (the closure would nil-panic when executed).
func TestPickerEnterNilClient(t *testing.T) {
	a, _ := regrantApp(t)
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if cmd != nil {
		t.Fatal("nil client must not dispatch")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "not connected") {
		t.Fatal("expected a 'not connected' notice")
	}
}

// TestPickerBatchedRunes: fast key-repeat (or paste) delivers ONE KeyMsg
// carrying several runes; each rune must be applied, not dropped.
func TestPickerBatchedRunes(t *testing.T) {
	a, _ := regrantApp(t)
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jjj")})
	a = m.(*App)
	if a.authorityPicker.cursor != 3 {
		t.Fatalf("cursor = %d after a batched jjj, want 3", a.authorityPicker.cursor)
	}
}

// TestPickerAllNoneQuickKeys: A / N mirror the WebUI chip row's [all]/[none].
func TestPickerAllNoneQuickKeys(t *testing.T) {
	a, _ := regrantApp(t)
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	a = m.(*App)
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
	a = m.(*App)
	caps, _, _, _ := a.authorityPicker.Result()
	if caps != protocol.Capability_None {
		t.Fatalf("N must clear all caps, got %v", caps)
	}
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	a = m.(*App)
	caps, _, _, _ = a.authorityPicker.Result()
	if caps != protocol.Capability_All {
		t.Fatalf("A must set every granular bit (== all), got %v", caps)
	}
}

// TestCapsNoArgOpensPicker / TestScopeNoArgOpensPicker: the no-arg cmdline
// forms open the picker in session mode instead of printing.
func TestCapsNoArgOpensPicker(t *testing.T) {
	a := New(Config{})
	a.runAction(verb.SetDefaultsAction{})
	if !a.authorityPicker.IsOpen() || a.authorityPicker.Mode() != PickerModeSession {
		t.Fatal("caps (no arg) must open the picker in session mode")
	}
}

func TestScopeNoArgOpensPicker(t *testing.T) {
	a := New(Config{})
	a.runAction(verb.SetDefaultsAction{})
	if !a.authorityPicker.IsOpen() || a.authorityPicker.Mode() != PickerModeSession {
		t.Fatal("scope (no arg) must open the picker in session mode")
	}
}

// TestPickerSessionApplyWritesDefaults: applying in session mode writes
// sessionCaps and sessionScope, no client needed.
func TestPickerSessionApplyWritesDefaults(t *testing.T) {
	a := New(Config{})
	before := a.sessionCaps
	a.runAction(verb.SetDefaultsAction{})
	// Toggle the first cap row, then cycle base to none. Which DIRECTION it
	// toggles follows the session default (none since default-deny), so the
	// assertion below is on the flip, not on the resulting level.
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeySpace})
	a = m.(*App)
	firstBit := a.authorityPicker.rows[0].bit
	for !strings.Contains(a.authorityPicker.rows[a.authorityPicker.cursor].label, "base:") {
		m, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		a = m.(*App)
	}
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeySpace}) // subtree -> none
	a = m.(*App)
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if cmd != nil {
		t.Fatal("session apply must not dispatch a client command")
	}
	if a.authorityPicker.IsOpen() {
		t.Fatal("Enter must close the picker")
	}
	if a.sessionScope.Base != protocol.ScopeBase_None {
		t.Fatalf("sessionScope.Base = %v, want None", a.sessionScope.Base)
	}
	if (a.sessionCaps^before)&firstBit != firstBit {
		t.Fatalf("sessionCaps did not flip the toggled bit %v (before=%v after=%v)", firstBit, before, a.sessionCaps)
	}
}
