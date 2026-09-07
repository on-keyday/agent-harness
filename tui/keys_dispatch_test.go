package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestMainKeyRowsDispatch is the other direction of TestMainKeysAreAllDocumented:
// a row that names a mainKeys letter must also run something, or the help
// would advertise a key the dispatcher never sees. Quit, help and the logs
// filter are dispatched outside the table and are the listed exceptions.
func TestMainKeyRowsDispatch(t *testing.T) {
	structural := map[string]bool{mainKeys.Quit: true, mainKeys.Help: true, mainKeys.LogFilter: true}
	letters := map[string]bool{}
	v := reflect.ValueOf(mainKeys)
	for i := 0; i < v.NumField(); i++ {
		letters[v.Field(i).String()] = true
	}
	for _, b := range mainKeyBindings {
		for _, k := range b.Keys {
			if letters[k] && !structural[k] && b.Do == nil {
				t.Errorf("row for %q has no Do: documented, but nothing dispatches it", k)
			}
		}
	}
}

func runeKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// A key fires only in a pane its Scope names. Elsewhere it is either explained
// (OutsideHint) or left to the pane, and the command line always types it.
func TestDispatchMainKeyHonoursScope(t *testing.T) {
	a := New(Config{Server: "ws://test:8080"})
	a.width, a.height = 120, 40
	a.layout()

	a.focus = focusRunners
	if _, ok := a.dispatchMainKey(runeKey('G'), false); !ok {
		t.Fatal("G in the runners pane should be consumed, with a hint")
	}
	if got := strings.Join(a.cmdresult.lines, "\n"); !strings.Contains(got, "git: focus the tasks pane first") {
		t.Fatalf("expected the OutsideHint, got %q", got)
	}
	if _, ok := a.dispatchMainKey(runeKey('c'), false); ok {
		t.Fatal("c (tasks-only, no hint) in the runners pane must fall through to the pane")
	}

	a.focus = focusCmdline
	for _, r := range []rune{'G', 's', 'c'} {
		if _, ok := a.dispatchMainKey(runeKey(r), false); ok {
			t.Errorf("%q must reach the command line as text", r)
		}
	}

	a.focus = focusLogs
	if _, ok := a.dispatchMainKey(runeKey('s'), true); ok {
		t.Fatal("nothing fires while the logs filter is being typed")
	}
}

// The overlay table's order is its stacking order. These two pairs are the
// ones where a surface is opened FROM the one it must precede.
func TestOverlayOrderTopmostFirst(t *testing.T) {
	idx := func(fn func(*App, tea.KeyMsg) tea.Cmd) int {
		want := reflect.ValueOf(fn).Pointer()
		for i, o := range appOverlays {
			if reflect.ValueOf(o.key).Pointer() == want {
				return i
			}
		}
		t.Fatal("handler not in appOverlays")
		return -1
	}
	if idx((*App).inForwardTap) > idx((*App).inForwardsModal) {
		t.Error("the forward tap is drawn over the forwards modal and must come first")
	}
	if idx((*App).inFileEditor) > idx((*App).inFilePicker) {
		t.Error("the file editor is drawn over the file picker and must come first")
	}
}
