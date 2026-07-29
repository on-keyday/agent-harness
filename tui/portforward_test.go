package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
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

// TestForwardsModalApplySnapshot guards the server-backed rendering: columns
// built from cli.PortForwardDirFlag / pfShortID / cli.PortForwardSpecString,
// and SelectedID reading the id under the (default, top-row) cursor.
func TestForwardsModalApplySnapshot(t *testing.T) {
	m := NewForwardsModal()
	m.SetSize(120, 40)
	var a, b protocol.PortForwardInfo
	a.ForwardId = 1
	a.Direction = protocol.PortForwardDirection_Local
	a.SetBindAddr([]byte("127.0.0.1"))
	a.BindPort = 8080
	a.SetTargetHost([]byte("svc"))
	a.TargetPort = 80
	b.ForwardId = 2
	b.Direction = protocol.PortForwardDirection_Remote
	b.SetBindAddr([]byte("127.0.0.1"))
	b.BindPort = 6001
	b.SetTargetHost([]byte("localhost"))
	b.TargetPort = 6000
	m.ApplySnapshot([]protocol.PortForwardInfo{a, b})
	m.Open()

	view := m.View()
	for _, want := range []string{"-L", "-R", "8080", "6001", "(2)"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if id, ok := m.SelectedID(); !ok || id != 1 {
		t.Errorf("SelectedID() = (%d,%v), want (1,true)", id, ok)
	}
}

func TestForwardsModalEmpty(t *testing.T) {
	m := NewForwardsModal()
	m.SetSize(80, 24)
	m.ApplySnapshot(nil)
	m.Open()
	if !strings.Contains(m.View(), "no active forwards") {
		t.Errorf("empty view should say 'no active forwards':\n%s", m.View())
	}
	if _, ok := m.SelectedID(); ok {
		t.Error("SelectedID() should report false on an empty list")
	}
}

// TestForwardsModalKey_NotConnected guards the `f` key's client guard: with no
// client bound (initial connect still pending), pressing `f` must not dispatch
// DoListForwards (that closure would nil-panic on execute — see
// app_noclient_test.go for the same pattern on cmdline actions) and must leave
// the modal closed with a visible notice.
func TestForwardsModalKey_NotConnected(t *testing.T) {
	a := New(Config{}) // client is nil (BindClient never called)
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	a = m.(*App)
	if a.forwardsModal.IsOpen() {
		t.Fatal("`f` with no client should not open the forwards modal")
	}
	if cmd != nil {
		t.Fatal("`f` with no client should not dispatch a command")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "not connected") {
		t.Errorf("expected a 'not connected' notice, got:\n%s", strings.Join(a.cmdresult.lines, "\n"))
	}
}

// TestForwardsModalKey_EscCloses guards that Esc still closes the modal once
// open (bypassing the network fetch by opening it directly).
func TestForwardsModalKey_EscCloses(t *testing.T) {
	a := New(Config{})
	a.forwardsModal.Open()
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	a = m.(*App)
	if a.forwardsModal.IsOpen() {
		t.Fatal("Esc should close the forwards modal")
	}
}

// TestPortForwardRegisteredMsg_BackfillsForwardID guards the async id handoff
// a local (-L) forward needs: PortForwardStartedMsg carries ForwardID=0 (not
// yet known), and PortForwardRegisteredMsg fills it in once cli.RunForward's
// registration completes.
func TestPortForwardRegisteredMsg_BackfillsForwardID(t *testing.T) {
	a := New(Config{})
	m, _ := a.Update(PortForwardStartedMsg{ID: 1, TaskID: "abcdef", Direction: ForwardLocal, Spec: "8080:h:80"})
	a = m.(*App)
	if a.activeForwards[1].ForwardID != 0 {
		t.Fatalf("before registration: ForwardID = %d, want 0", a.activeForwards[1].ForwardID)
	}
	m, _ = a.Update(PortForwardRegisteredMsg{ID: 1, ForwardID: 42})
	a = m.(*App)
	if a.activeForwards[1].ForwardID != 42 {
		t.Fatalf("after registration: ForwardID = %d, want 42", a.activeForwards[1].ForwardID)
	}
}

// TestKillLocalForward_UnregisteredForwardWarns guards the ForwardID==0 race
// window for a just-started local forward: killLocalForward must not dispatch
// (there is no id to kill yet) and must explain why.
func TestKillLocalForward_UnregisteredForwardWarns(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{} // non-nil is enough; the returned cmd is never executed
	sess := &PortForwardSession{ID: 1, TaskID: "abc", Direction: ForwardLocal, Spec: "8080:h:80", ForwardID: 0}
	if cmd := a.killLocalForward(sess); cmd != nil {
		t.Fatal("killLocalForward with ForwardID==0 should not dispatch")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "not fully registered") {
		t.Error("expected a 'not fully registered' notice")
	}
}

// TestKillLocalForward_RoutesThroughKillRPC guards that a registered forward
// dispatches through DoKillForward rather than calling sess.Cancel directly —
// the "exactly one way to stop a forward" requirement.
func TestKillLocalForward_RoutesThroughKillRPC(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{}
	cancelled := false
	sess := &PortForwardSession{
		ID: 1, TaskID: "abc", Direction: ForwardLocal, Spec: "8080:h:80", ForwardID: 42,
		Cancel: func() { cancelled = true },
	}
	if cmd := a.killLocalForward(sess); cmd == nil {
		t.Fatal("killLocalForward with a registered forward should dispatch a kill command")
	}
	if cancelled {
		t.Error("killLocalForward must not call sess.Cancel directly — the RPC is the only stop path")
	}
}

// TestForwardKillResultMsg_RefreshesOnlyWhenModalOpen guards the "k ... followed
// by a refresh" requirement: a re-fetch is dispatched only while the forwards
// modal is actually open (a cmdline `forward kill` with the modal closed
// should not pay for an unwanted extra round trip).
func TestForwardKillResultMsg_RefreshesOnlyWhenModalOpen(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{}

	m, cmd := a.Update(ForwardKillResultMsg{ID: 7})
	a = m.(*App)
	if cmd != nil {
		t.Fatal("kill result with modal closed should not dispatch a refresh")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "killed") {
		t.Error("expected a 'killed' confirmation line")
	}

	a.forwardsModal.Open()
	_, cmd = a.Update(ForwardKillResultMsg{ID: 7})
	if cmd == nil {
		t.Fatal("kill result with modal open should dispatch a refresh")
	}
}
