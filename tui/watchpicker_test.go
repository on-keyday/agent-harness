package tui

import (
	"encoding/hex"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func choiceKeys(cs []watchChoice) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.key
	}
	return out
}

// A live session can be watched every way; a task without one (a oneshot, a
// finished task) has only its progress line.
func TestWatchChoices(t *testing.T) {
	if got := strings.Join(choiceKeys(watchChoices(true)), ","); got != "quiet,lifecycle,program,any,progress" {
		t.Fatalf("live choices = %s", got)
	}
	if got := strings.Join(choiceKeys(watchChoices(false)), ","); got != "progress" {
		t.Fatalf("non-live choices = %s", got)
	}
}

func TestWatchLive(t *testing.T) {
	for _, c := range []struct {
		kind   protocol.TaskKind
		status protocol.TaskStatus
		want   bool
	}{
		{protocol.TaskKind_Interactive, protocol.TaskStatus_Running, true},
		{protocol.TaskKind_Interactive, protocol.TaskStatus_Detached, true},
		{protocol.TaskKind_Interactive, protocol.TaskStatus_Succeeded, false},
		{protocol.TaskKind_Oneshot, protocol.TaskStatus_Running, false},
	} {
		if got := watchLive(&protocol.TaskInfo{Kind: c.kind, Status: c.status}); got != c.want {
			t.Errorf("watchLive(%v,%v) = %v, want %v", c.kind, c.status, got, c.want)
		}
	}
}

// The cursor starts on the last choice so `w Enter` repeats it; a digit picks
// directly; Esc closes without picking.
func TestWatchPicker_Keys(t *testing.T) {
	var p WatchPicker
	p.Open("t1", protocol.AwaitSink_Reply, watchChoices(true), "program")
	if c, ok := p.Key(tea.KeyMsg{Type: tea.KeyEnter}); !ok || c.key != "program" {
		t.Fatalf("Enter picked %+v, %v; want the last choice, program", c, ok)
	}
	p.Open("t1", protocol.AwaitSink_Reply, watchChoices(true), "")
	p.Key(tea.KeyMsg{Type: tea.KeyDown})
	if c, ok := p.Key(tea.KeyMsg{Type: tea.KeyEnter}); !ok || c.key != "lifecycle" {
		t.Fatalf("Down Enter picked %+v; want lifecycle", c)
	}
	p.Open("t1", protocol.AwaitSink_Reply, watchChoices(true), "")
	if c, ok := p.Key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}}); !ok || c.key != "progress" {
		t.Fatalf("5 picked %+v; want progress", c)
	}
	p.Open("t1", protocol.AwaitSink_Reply, watchChoices(false), "lifecycle")
	if c, ok := p.Key(tea.KeyMsg{Type: tea.KeyEnter}); !ok || c.key != "progress" {
		t.Fatalf("a last choice the task cannot offer must fall back: picked %+v", c)
	}
	p.Open("t1", protocol.AwaitSink_Reply, watchChoices(true), "")
	if _, ok := p.Key(tea.KeyMsg{Type: tea.KeyEsc}); ok || p.IsOpen() {
		t.Fatal("Esc must close without picking")
	}
}

// `w` / `W` open the one picker (reply / notify sink); `m` / `M` are gone —
// the progress watcher is a choice in it.
func TestAwaitKeysOpenWatchPicker(t *testing.T) {
	a := New(Config{})
	var tid protocol.TaskID
	tid.Id[0] = 0xab
	a.tasks.SetRows([]protocol.TaskInfo{{Id: tid, Kind: protocol.TaskKind_Interactive, Status: protocol.TaskStatus_Detached}}, nil)
	id := hex.EncodeToString(tid.Id[:])
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'W'}})
	a = m.(*App)
	if !a.watchPicker.IsOpen() || a.watchPicker.taskID != id || a.watchPicker.sink != protocol.AwaitSink_Notify {
		t.Fatalf("W: picker open=%v task=%q sink=%v", a.watchPicker.IsOpen(), a.watchPicker.taskID, a.watchPicker.sink)
	}
	if !strings.Contains(a.watchPicker.View(), "progress") {
		t.Fatalf("picker view lacks the progress choice:\n%s", a.watchPicker.View())
	}
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	a = m.(*App)
	if a.watchPicker.IsOpen() || a.lastWatch != "lifecycle" {
		t.Fatalf("after 2: open=%v last=%q", a.watchPicker.IsOpen(), a.lastWatch)
	}
	for _, b := range mainKeyBindings {
		for _, k := range b.Keys {
			if k == "m" || k == "M" {
				if b.Scope == scopeTasks {
					t.Fatalf("m/M still bound on the tasks pane: %+v", b.Short)
				}
			}
		}
	}
}
