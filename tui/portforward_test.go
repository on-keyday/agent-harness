package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPortForwardModal_OpenClose(t *testing.T) {
	var m PortForwardModal
	if m.IsOpen() {
		t.Fatal("new modal should be closed")
	}
	m.Open("abc123")
	if !m.IsOpen() || m.TaskID() != "abc123" {
		t.Fatalf("after Open: open=%v task=%q", m.IsOpen(), m.TaskID())
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("after Close: should be closed")
	}
}

func TestSelectForwards_ByTaskAndDirection(t *testing.T) {
	m := map[int]*PortForwardSession{
		1: {ID: 1, TaskID: "a", Direction: ForwardLocal, Spec: "8080:h:80"},
		2: {ID: 2, TaskID: "a", Direction: ForwardLocal, Spec: "9090:h:90"},
		3: {ID: 3, TaskID: "a", Direction: ForwardRemote, Spec: "1:h:2"},
		4: {ID: 4, TaskID: "b", Direction: ForwardLocal, Spec: "7:h:7"},
	}
	local := selectForwards(m, "a", ForwardLocal)
	if len(local) != 2 || local[0].ID != 1 || local[1].ID != 2 {
		t.Fatalf("local for a: %+v", local)
	}
	if got := selectForwards(m, "a", ForwardRemote); len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("remote for a: %+v", got)
	}
	if got := selectForwards(m, "z", ForwardLocal); len(got) != 0 {
		t.Fatalf("unknown task: %+v", got)
	}
}

func TestForwardPicker_Pick(t *testing.T) {
	var p ForwardPicker
	p.Open(ForwardLocal, []*PortForwardSession{{ID: 10, Spec: "a"}, {ID: 20, Spec: "b"}})
	if !p.IsOpen() {
		t.Fatal("picker should be open")
	}
	if got := p.Pick("1"); got == nil || got.ID != 10 {
		t.Fatalf("Pick(1) = %+v", got)
	}
	if got := p.Pick("2"); got == nil || got.ID != 20 {
		t.Fatalf("Pick(2) = %+v", got)
	}
	if p.Pick("3") != nil {
		t.Fatal("Pick(3) out of range should be nil")
	}
	if p.Pick("x") != nil {
		t.Fatal("Pick(non-digit) should be nil")
	}
	p.Close()
	if p.IsOpen() {
		t.Fatal("picker should be closed")
	}
}

func TestPortForwardModal_RemoteMode(t *testing.T) {
	var m PortForwardModal
	m.OpenMode("t", ForwardRemote)
	if m.Mode() != ForwardRemote {
		t.Fatalf("mode = %v, want remote", m.Mode())
	}
}

// TestForwardLifecycle_StoppedRemovesEntry guards the bug where a finished/failed
// forward stayed in activeForwards and kept showing in the stop picker.
func TestForwardLifecycle_StoppedRemovesEntry(t *testing.T) {
	a := New(Config{})
	m, _ := a.Update(PortForwardStartedMsg{ID: 1, TaskID: "abcdef", Direction: ForwardRemote, Spec: "8080:h:80"})
	a = m.(*App)
	if len(a.activeForwards) != 1 {
		t.Fatalf("after start: want 1 active, got %d", len(a.activeForwards))
	}
	m, _ = a.Update(PortForwardStoppedMsg{ID: 1, TaskID: "abcdef"})
	a = m.(*App)
	if len(a.activeForwards) != 0 {
		t.Fatalf("after stop: want 0 active (entry should be removed), got %d", len(a.activeForwards))
	}
	if got := selectForwards(a.activeForwards, "abcdef", ForwardRemote); len(got) != 0 {
		t.Fatalf("stop picker should be empty, got %d", len(got))
	}
}

func TestSortedForwards_Order(t *testing.T) {
	// ForwardLocal=0 < ForwardRemote=1, so within a task -L sorts before -R.
	m := map[int]*PortForwardSession{
		3: {ID: 3, TaskID: "b", Direction: ForwardLocal, Spec: "7:h:7"},
		1: {ID: 1, TaskID: "a", Direction: ForwardRemote, Spec: "1:h:2"},
		2: {ID: 2, TaskID: "a", Direction: ForwardLocal, Spec: "8080:h:80"},
		4: {ID: 4, TaskID: "a", Direction: ForwardLocal, Spec: "9090:h:90"},
	}
	got := sortedForwards(m)
	want := []int{2, 4, 1, 3} // a/-L/2, a/-L/4, a/-R/1, b/-L/3
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("pos %d: got ID %d, want %d", i, got[i].ID, id)
		}
	}
}

func TestForwardRow_Cells(t *testing.T) {
	s := &PortForwardSession{ID: 1, TaskID: "abcdef012345aa", Direction: ForwardLocal, Spec: "8080:h:80"}
	row := forwardRow(s)
	if row[0] != "abcdef012345" { // pfShortID truncates to 12
		t.Fatalf("task cell = %q, want abcdef012345", row[0])
	}
	if row[1] != "-L" {
		t.Fatalf("dir cell = %q, want -L", row[1])
	}
	if row[2] != "8080:h:80" {
		t.Fatalf("spec cell = %q, want 8080:h:80", row[2])
	}
	r := forwardRow(&PortForwardSession{TaskID: "x", Direction: ForwardRemote, Spec: "1:h:2"})
	if r[1] != "-R" {
		t.Fatalf("remote dir cell = %q, want -R", r[1])
	}
}

func TestForwardsModal_OpenClose(t *testing.T) {
	m := NewForwardsModal()
	if m.IsOpen() {
		t.Fatal("new modal should be closed")
	}
	m.Open()
	if !m.IsOpen() {
		t.Fatal("after Open: should be open")
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("after Close: should be closed")
	}
}

func TestForwardsModal_SetSessions_CountAndEmpty(t *testing.T) {
	m := NewForwardsModal()
	m.SetSessions(nil)
	if len(m.sessions) != 0 {
		t.Fatalf("empty: sessions = %d, want 0", len(m.sessions))
	}
	m.SetSessions([]*PortForwardSession{
		{ID: 1, TaskID: "abcdef012345aa", Direction: ForwardLocal, Spec: "8080:h:80"},
		{ID: 2, TaskID: "abcdef012345aa", Direction: ForwardRemote, Spec: "9000:h:9000"},
	})
	if len(m.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(m.sessions))
	}
}

func TestForwardsModal_KeyOpensAndEscCloses(t *testing.T) {
	a := New(Config{})
	// Seed one active forward (default focus is focusTasks, logs not editing,
	// so the `f` guard passes).
	m, _ := a.Update(PortForwardStartedMsg{ID: 1, TaskID: "abcdef012345", Direction: ForwardLocal, Spec: "8080:h:80"})
	a = m.(*App)

	// Press `f` → modal opens, seeded with the active forwards snapshot.
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	a = m.(*App)
	if !a.forwardsModal.IsOpen() {
		t.Fatal("`f` should open the forwards modal")
	}
	if len(a.forwardsModal.sessions) != 1 {
		t.Fatalf("modal sessions = %d, want 1", len(a.forwardsModal.sessions))
	}

	// A forward that stops while the modal is open updates the snapshot live.
	m, _ = a.Update(PortForwardStoppedMsg{ID: 1, TaskID: "abcdef012345"})
	a = m.(*App)
	if len(a.forwardsModal.sessions) != 0 {
		t.Fatalf("after stop while open: sessions = %d, want 0", len(a.forwardsModal.sessions))
	}

	// Esc closes.
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	a = m.(*App)
	if a.forwardsModal.IsOpen() {
		t.Fatal("Esc should close the forwards modal")
	}
}
