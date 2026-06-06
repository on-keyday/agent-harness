package tui

import "testing"

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
