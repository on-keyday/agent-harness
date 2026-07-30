package tui

import (
	"errors"
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
// built from cli.PortForwardDirFlag / pfShortID / cli.PortForwardSpecString /
// cli.PortForwardOrigin, and SelectedID reading the id under the (default,
// top-row) cursor. "8080" and "6001" also appear inside the spec cell, so
// they don't actually pin the id column — the id and origin cells are pinned
// separately below via portForwardInfoRow, indexed by column, immune to that
// ambiguity (and to bubbles/table's column-width truncation, which a raw
// substring-of-View() check can't distinguish from "value never rendered").
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
	a.OriginKind = protocol.ClientKind_Tui
	a.SetOriginCid([]byte("ws:10.0.0.1:1-1"))
	b.ForwardId = 2
	b.Direction = protocol.PortForwardDirection_Remote
	b.SetBindAddr([]byte("127.0.0.1"))
	b.BindPort = 6001
	b.SetTargetHost([]byte("localhost"))
	b.TargetPort = 6000
	b.OriginKind = protocol.ClientKind_Cli
	b.SetOriginCid([]byte("ws:10.0.0.2:2-2"))
	m.ApplySnapshot([]protocol.PortForwardInfo{a, b})
	m.Open()

	view := m.View()
	for _, want := range []string{"-L", "-R", "8080", "6001", "(2)", "ws:10.0.0.1:1-1", "ws:10.0.0.2:2-2"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if id, ok := m.SelectedID(); !ok || id != 1 {
		t.Errorf("SelectedID() = (%d,%v), want (1,true)", id, ok)
	}

	// Pin the id and origin cells directly (column-indexed, not a substring
	// search over the rendered view).
	rowA := portForwardInfoRow(&a)
	if rowA[0] != "1" {
		t.Errorf("a: id cell = %q, want %q", rowA[0], "1")
	}
	if !strings.Contains(rowA[4], "tui") || !strings.Contains(rowA[4], "ws:10.0.0.1:1-1") {
		t.Errorf("a: origin cell = %q, want kind %q + cid %q", rowA[4], "tui", "ws:10.0.0.1:1-1")
	}
	rowB := portForwardInfoRow(&b)
	if rowB[0] != "2" {
		t.Errorf("b: id cell = %q, want %q", rowB[0], "2")
	}
	if !strings.Contains(rowB[4], "cli") || !strings.Contains(rowB[4], "ws:10.0.0.2:2-2") {
		t.Errorf("b: origin cell = %q, want kind %q + cid %q", rowB[4], "cli", "ws:10.0.0.2:2-2")
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

// TestForwardsModalKey_ConnectedOpensAndFetches guards the complement of
// TestForwardsModalKey_NotConnected: with a client bound, `f` must both open
// the modal AND dispatch the fetch (DoListForwards) — the predecessor to this
// test only asserted the two halves separately (open, via the old
// SetSessions-based flow; not-connected, in isolation), so a regression that
// opened the modal without ever fetching would have passed unnoticed.
func TestForwardsModalKey_ConnectedOpensAndFetches(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{} // non-nil is enough; the returned cmd is never executed
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	a = m.(*App)
	if !a.forwardsModal.IsOpen() {
		t.Fatal("`f` with a client should open the forwards modal")
	}
	if cmd == nil {
		t.Fatal("`f` with a client should dispatch a fetch (DoListForwards)")
	}
}

// forwardsModalTwoRows builds a modal open with two rows, for the key-routing
// tests below (confirm, navigation) that need a real selection.
func forwardsModalTwoRows() ForwardsModal {
	m := NewForwardsModal()
	m.SetSize(120, 40)
	var a, b protocol.PortForwardInfo
	a.ForwardId = 1
	a.SetBindAddr([]byte("127.0.0.1"))
	a.BindPort = 8080
	a.SetTargetHost([]byte("svc"))
	a.TargetPort = 80
	b.ForwardId = 2
	b.SetBindAddr([]byte("127.0.0.1"))
	b.BindPort = 9090
	b.SetTargetHost([]byte("svc2"))
	b.TargetPort = 90
	m.ApplySnapshot([]protocol.PortForwardInfo{a, b})
	m.Open()
	return m
}

// TestForwardsModalKey_NavigationDoesNotKill is the regression test for the
// finding this task exists to fix: `k`/`j` are bubbles/table's own
// LineUp/LineDown bindings (table.go DefaultKeyMap) and must reach the
// table untouched — neither may dispatch a kill or arm a confirmation. Before
// this fix, `k` was intercepted ahead of the table and killed the row under
// the cursor outright, with no confirmation, reachable by a navigation
// reflex.
func TestForwardsModalKey_NavigationDoesNotKill(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{}
	a.forwardsModal = forwardsModalTwoRows()

	for _, key := range []string{"j", "k"} {
		m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		a = m.(*App)
		if cmd != nil {
			t.Fatalf("%q (table navigation) must not dispatch a kill", key)
		}
		if a.forwardsModal.IsConfirming() {
			t.Fatalf("%q (table navigation) must not arm a kill confirmation", key)
		}
		if !a.forwardsModal.IsOpen() {
			t.Fatalf("%q must not close the modal", key)
		}
	}
}

// TestForwardsModalKey_XArmsConfirmation guards that `x` does not kill
// immediately — it only arms the y/n prompt. No command may be dispatched at
// this point: the kill is not yet confirmed.
func TestForwardsModalKey_XArmsConfirmation(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{}
	a.forwardsModal = forwardsModalTwoRows()

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	a = m.(*App)
	if cmd != nil {
		t.Fatal("`x` alone must not dispatch a kill — it only arms the confirmation")
	}
	if !a.forwardsModal.IsConfirming() {
		t.Fatal("`x` should arm the kill confirmation")
	}
	if !a.forwardsModal.IsOpen() {
		t.Fatal("`x` must not close the modal")
	}
}

// TestForwardsModalKey_XThenNCancelsWithoutKilling is the human-mandated
// confirm-gate test: "x then n must not kill". Esc is checked too, since
// CancelKillConfirm's key set is {n, N, esc}.
func TestForwardsModalKey_XThenNCancelsWithoutKilling(t *testing.T) {
	for _, cancelKey := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'n'}},
		{Type: tea.KeyRunes, Runes: []rune{'N'}},
		{Type: tea.KeyEsc},
	} {
		a := New(Config{})
		a.client = &cli.Client{}
		a.forwardsModal = forwardsModalTwoRows()

		m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		a = m.(*App)
		if !a.forwardsModal.IsConfirming() {
			t.Fatalf("setup: `x` should have armed the confirmation (cancel key %v)", cancelKey)
		}

		m, cmd := a.Update(cancelKey)
		a = m.(*App)
		if cmd != nil {
			t.Fatalf("cancel key %v must not dispatch a kill", cancelKey)
		}
		if a.forwardsModal.IsConfirming() {
			t.Fatalf("cancel key %v should clear the pending confirmation", cancelKey)
		}
		if !a.forwardsModal.IsOpen() {
			t.Fatalf("cancel key %v must cancel the confirm, not close the whole modal", cancelKey)
		}
	}
}

// TestForwardsModalKey_XThenYDispatchesKill: the confirmed-kill path — once
// armed, `y` (or `Y`) dispatches the kill and clears the pending state.
func TestForwardsModalKey_XThenYDispatchesKill(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{}
	a.forwardsModal = forwardsModalTwoRows()

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	a = m.(*App)
	if !a.forwardsModal.IsConfirming() {
		t.Fatal("setup: `x` should have armed the confirmation")
	}

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	a = m.(*App)
	if cmd == nil {
		t.Fatal("`y` after `x` should dispatch the kill")
	}
	if a.forwardsModal.IsConfirming() {
		t.Fatal("`y` should clear the pending confirmation")
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

// TestForwardKillResultMsg_RefreshesOnlyWhenModalOpen guards the "x, y ...
// followed by a refresh" requirement: a re-fetch is dispatched only while the
// forwards modal is actually open (a cmdline `forward kill` with the modal
// closed should not pay for an unwanted extra round trip).
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

// TestForwardKillResultMsg_ShowsTaskAndSpec guards that kill feedback names
// what was killed, not just its server-assigned integer id — the old line
// was "forward cancelled: <task>  -L 8080:h:80"; a bare "forward 17 killed"
// gives no correlation between what the operator picked and what was
// confirmed. TaskID/Spec are optional (empty for a bare cmdline `forward
// kill <id>`, where there is nothing to echo beyond the id itself).
func TestForwardKillResultMsg_ShowsTaskAndSpec(t *testing.T) {
	a := New(Config{})
	m, _ := a.Update(ForwardKillResultMsg{ID: 7, TaskID: "abcdef012345", Spec: "-L 8080:h:80"})
	a = m.(*App)
	got := strings.Join(a.cmdresult.lines, "\n")
	for _, want := range []string{"7", "abcdef012345", "-L 8080:h:80"} {
		if !strings.Contains(got, want) {
			t.Errorf("kill result line missing %q:\n%s", want, got)
		}
	}
}

// TestForwardsSnapshotMsg_CmdresultOnlyWhenRequested guards the ToCmdresult
// discriminator: the text table (cli.PortForwardInfoLines — a banner + header
// + one line per forward) must land in cmdresult only for the `forward ls`
// cmdline dispatch, never for the `f` key or a kill-triggered modal refresh —
// cmdresult is a 200-line ring that evicts oldest-first, so an unconditional
// dump could evict an earlier notice the operator still needs. Errors must
// still surface on both paths.
func TestForwardsSnapshotMsg_CmdresultOnlyWhenRequested(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 1
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.SetTargetHost([]byte("svc"))

	a := New(Config{})
	m, _ := a.Update(ForwardsSnapshotMsg{Forwards: []protocol.PortForwardInfo{fi}, ToCmdresult: false})
	a = m.(*App)
	if strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "PORT FORWARDS") {
		t.Fatal("ToCmdresult=false (f key / modal refresh) must not dump the text table into cmdresult")
	}

	m, _ = a.Update(ForwardsSnapshotMsg{Forwards: []protocol.PortForwardInfo{fi}, ToCmdresult: true})
	a = m.(*App)
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "PORT FORWARDS") {
		t.Fatal("ToCmdresult=true (forward ls) should dump the text table into cmdresult")
	}
}

// TestForwardsSnapshotMsg_ErrorSurfacesRegardlessOfToCmdresult guards that
// the error path is NOT gated by ToCmdresult — only the success-path text
// dump is conditional.
func TestForwardsSnapshotMsg_ErrorSurfacesRegardlessOfToCmdresult(t *testing.T) {
	boom := errors.New("boom")
	for _, toCmdresult := range []bool{false, true} {
		a := New(Config{})
		m, _ := a.Update(ForwardsSnapshotMsg{Err: boom, ToCmdresult: toCmdresult})
		a = m.(*App)
		if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), boom.Error()) {
			t.Errorf("ToCmdresult=%v: error should still surface in cmdresult", toCmdresult)
		}
	}
}
