package tui

import (
	"bytes"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

func TestRawConnectModal_ShowHideAndSpec(t *testing.T) {
	m := NewRawConnectModal()
	if m.IsOpen() {
		t.Fatal("fresh modal must be closed")
	}
	m.Show("4a1f0000000000000000000000000000")
	if !m.IsOpen() || m.TaskID() == "" {
		t.Fatal("Show must record the task and open")
	}
	m.SetSpec("127.0.0.1:6379")
	host, port, err := m.Target()
	if err != nil || host != "127.0.0.1" || port != 6379 {
		t.Fatalf("Target() = %s:%d err=%v", host, port, err)
	}
	m.SetSpec("garbage")
	if _, _, err := m.Target(); err == nil {
		t.Fatal("Target() must reject a spec that is not host:port")
	}
	m.Hide()
	if m.IsOpen() {
		t.Fatal("Hide must close the view")
	}
}

// Panes are addressed by the generation their messages carry; a reply for one
// pane must never be applied to another.
func TestRawModalRoutesByGeneration(t *testing.T) {
	m := NewRawConnectModal()
	m.Show("task-1")
	m.AddPane("task-1", "127.0.0.1", 8080, 7)
	m.AddPane("task-1", "127.0.0.1", 9090, 8)

	p := m.PaneForGen(7)
	if p == nil || p.port != 8080 {
		t.Fatalf("PaneForGen(7) = %+v, want the 8080 pane", p)
	}
	p.AppendOutput([]byte("hello"))
	if other := m.PaneForGen(8); other == nil || len(other.out) != 0 {
		t.Errorf("output leaked into the 9090 pane: %+v", other)
	}
	if m.PaneForGen(99) != nil {
		t.Errorf("unknown generation resolved to a pane")
	}
}

// esc hides the modal and leaves every connection running; ctrl+x closes
// exactly the active pane. The old modal closed the connection on esc, which also
// deregistered the forward server-side.
func TestRawModalHideKeepsPanesCloseDropsOne(t *testing.T) {
	m := NewRawConnectModal()
	m.Show("task-1")
	m.AddPane("task-1", "a", 1, 1)
	m.AddPane("task-1", "b", 2, 2)

	m.Hide()
	if m.IsOpen() {
		t.Errorf("Hide left the modal open")
	}
	if got := m.PaneCount(); got != 2 {
		t.Errorf("Hide dropped panes: %d left, want 2", got)
	}

	m.Show("task-1")
	m.MovePane(-99) // clamp back to [+ new]
	m.MovePane(+1)  // onto pane 1
	m.CloseActivePane()
	if got := m.PaneCount(); got != 1 {
		t.Fatalf("CloseActivePane left %d panes, want 1", got)
	}
	if p := m.PaneForGen(1); p != nil {
		t.Errorf("closed the wrong pane: gen 1 still present")
	}
}

// [+ new] is index 0 and stays; connecting appends and selects.
func TestRawModalNewSlotIsSticky(t *testing.T) {
	m := NewRawConnectModal()
	m.Show("task-1")
	if !m.OnNewSlot() {
		t.Fatalf("a fresh modal must start on the [+ new] slot")
	}
	m.AddPane("task-1", "a", 1, 1)
	if m.OnNewSlot() {
		t.Errorf("AddPane must select the new pane")
	}
	m.MovePane(-1)
	if !m.OnNewSlot() {
		t.Errorf("moving left from the first pane must reach [+ new]")
	}
	m.MovePane(-1)
	if !m.OnNewSlot() {
		t.Errorf("movement must clamp at [+ new], not wrap")
	}
}

// Quitting must close every pane: a RawConn whose process exits leaves a
// registration in `forward ls` that nothing can reach.
func TestRawModalCloseAllOnQuit(t *testing.T) {
	m := NewRawConnectModal()
	m.Show("task-1")
	m.AddPane("task-1", "a", 1, 1)
	m.AddPane("task-1", "b", 2, 2)
	m.CloseAllPanes()
	if m.PaneCount() != 0 {
		t.Errorf("CloseAllPanes left %d panes", m.PaneCount())
	}
	if m.ActivePane() != nil {
		t.Errorf("active pane survived CloseAllPanes")
	}
}

// TestRawPane_RingCapKeepsNewest checks the bound AND which end survives: a
// trim that kept the oldest bytes would satisfy a length-only assertion while
// showing the operator stale output.
//
// The filler deliberately carries an 11-byte marker distinct from the rest of
// the front and from the tail. An earlier version of this test built the front
// from a single repeated byte and asserted !HasPrefix(out, head[:16]), which is
// unfalsifiable: every 16-byte window of such filler is identical, so the front
// of a CORRECTLY trimmed buffer still matches. Assert the marker's literal
// absence instead.
func TestRawPane_RingCapKeepsNewest(t *testing.T) {
	var p rawPane
	headMarker := []byte("HEAD-MARKER") // same length as tail below, by design
	head := append(append([]byte(nil), headMarker...), bytes.Repeat([]byte("H"), rawTUIRingBytes-len(headMarker))...)
	p.AppendOutput(head)
	tail := []byte("TAIL-MARKER")
	p.AppendOutput(tail)
	if len(p.out) > rawTUIRingBytes {
		t.Fatalf("output ring = %d bytes, want <= %d", len(p.out), rawTUIRingBytes)
	}
	if !bytes.HasSuffix(p.out, tail) {
		t.Fatalf("newest bytes must survive the trim; output ends with %q", p.out[max(0, len(p.out)-16):])
	}
	if bytes.Contains(p.out, headMarker) {
		t.Fatal("oldest bytes must be trimmed from the front")
	}
}

// TestRawForwardMsgs_UnknownGenerationIgnored is the regression for the
// esc-then-reopen workflow, now expressed per pane: a message whose generation
// belongs to no pane (its pane was closed) must not splice bytes, or a close
// notice, into whichever pane happens to be selected.
func TestRawForwardMsgs_UnknownGenerationIgnored(t *testing.T) {
	a := New(Config{}) // tui/portforward_test.go's app-construction convention
	a.rawModal.Show("bbbb0000000000000000000000000000")
	a.rawModal.AddPane("bbbb0000000000000000000000000000", "127.0.0.1", 6379, 2)
	a.rawModal.SetConn(2, nil, nil, "connected (fwd 9)")

	a.Update(RawForwardDataMsg{Gen: 1, Data: []byte("stale")})
	if got := a.rawModal.ActivePane().out; len(got) != 0 {
		t.Fatalf("stale data applied: %q", got)
	}
	a.Update(RawForwardClosedMsg{Gen: 1, Reason: "stale close"})
	if !a.rawModal.ActivePane().live {
		t.Fatal("a close for an unknown generation must not mark a live pane closed")
	}

	a.Update(RawForwardDataMsg{Gen: 2, Data: []byte("fresh")})
	if got := string(a.rawModal.ActivePane().out); got != "fresh" {
		t.Fatalf("own-generation data not applied: %q", got)
	}
	a.Update(RawForwardClosedMsg{Gen: 2, Reason: "done"})
	if a.rawModal.ActivePane().live {
		t.Fatal("own-generation close must mark the pane closed")
	}
}

// TestRawForwardConnect_AttemptsAreIndependent replaces the old
// one-attempt-at-a-time guard. That guard existed because two attempts shared a
// single App-wide generation, so the loser's close tore down the winner's
// session. Generations are now per pane, so the realistic trigger — type a
// target, press Enter, notice the typo, retype, press Enter again — opens two
// independent panes, and a close for one must leave the other untouched.
func TestRawForwardConnect_AttemptsAreIndependent(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{} // non-nil is enough; the returned cmd is never executed
	a.rawModal.Show("cccc0000000000000000000000000000")
	a.rawModal.SetSpec("127.0.0.1:6379")

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if cmd == nil {
		t.Fatal("first enter with a valid spec should dispatch a connect")
	}
	first := a.rawModal.ActivePane().gen

	a.rawModal.MovePane(-99) // back to [+ new] to start another
	a.rawModal.SetSpec("127.0.0.1:6380")
	m, cmd = a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if cmd == nil {
		t.Fatal("a second target should dispatch its own connect")
	}
	second := a.rawModal.ActivePane().gen

	if first == second {
		t.Fatalf("both attempts share generation %d — a close for one would tear down the other", first)
	}
	if a.rawModal.PaneCount() != 2 {
		t.Fatalf("PaneCount = %d, want 2", a.rawModal.PaneCount())
	}

	a.Update(RawForwardClosedMsg{Gen: first, Reason: "first died"})
	if p := a.rawModal.PaneForGen(second); p == nil || p.note == "first died" {
		t.Fatalf("a close for the first attempt leaked into the second: %+v", p)
	}
}

// An invalid target must not create a pane: a pane the operator cannot close
// because it never had a connection is worse than an error line.
func TestRawForwardConnect_BadTargetCreatesNoPane(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{}
	a.rawModal.Show("dddd0000000000000000000000000000")
	a.rawModal.SetSpec("garbage")

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if cmd != nil {
		t.Fatal("a bad target must not dispatch a connect")
	}
	if a.rawModal.PaneCount() != 0 {
		t.Fatalf("PaneCount = %d, want 0", a.rawModal.PaneCount())
	}
}

func TestRawModalViewShowsTabsAndActiveTarget(t *testing.T) {
	m := NewRawConnectModal()
	m.Show("3777c91ae235bdcc1f18db0b1d33d183")
	m.AddPane("3777c91ae235bdcc1f18db0b1d33d183", "127.0.0.1", 8080, 1)
	m.SetConn(1, nil, nil, "connected (fwd 7)")
	m.AddPane("3777c91ae235bdcc1f18db0b1d33d183", "10.0.0.2", 22, 2)
	m.MarkClosed(2, "connection closed")
	m.MovePane(-1) // back onto the 8080 pane

	v := m.View()
	for _, want := range []string{"+ new", "127.0.0.1:8080", "10.0.0.2:22", "connected (fwd 7)", "ctrl+x close", "ctrl+r hex", "esc hide"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q:\n%s", want, v)
		}
	}
}

func TestHexToBytes(t *testing.T) {
	ok := map[string]string{
		"48656c":      "Hel",
		"48 65 6c":    "Hel",
		" 48\t65\n6c": "Hel",
		"00ff":        "\x00\xff",
	}
	for in, want := range ok {
		got, err := hexToBytes(in)
		if err != nil || string(got) != want {
			t.Errorf("hexToBytes(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "4", "48656", "zz", "48 6g"} {
		if got, err := hexToBytes(in); err == nil {
			t.Errorf("hexToBytes(%q) = %q, want an error", in, got)
		}
	}
}

// The terminator rule, pinned without a connection: text gets the selected
// terminator, hex gets nothing added under ANY selection — appending to an
// exact byte sequence defeats the only reason to reach for hex.
func TestEntryBytesTerminatorRule(t *testing.T) {
	text := map[rawNewline]string{
		rawNewlineCRLF: "PING\r\n",
		rawNewlineLF:   "PING\n",
		rawNewlineNone: "PING",
	}
	for nl, want := range text {
		got, err := entryBytes("PING", false, nl)
		if err != nil || string(got) != want {
			t.Errorf("text entry under %s = %q, %v; want %q", nl.label(), got, err, want)
		}
		got, err = entryBytes("50494e47", true, nl)
		if err != nil || string(got) != "PING" {
			t.Errorf("hex entry under %s = %q, %v; want \"PING\" with no terminator", nl.label(), got, err)
		}
	}
}

// ctrl+o walks CRLF → LF → none → CRLF, matching the WebUI's selector, and the
// label is what the footer shows.
func TestRawModalNewlineCycle(t *testing.T) {
	a := New(Config{})
	a.rawModal.Show("task-1")
	a.rawModal.AddPane("task-1", "127.0.0.1", 8080, 1)
	a.rawModal.SetConn(1, nil, nil, "connected")

	for _, want := range []string{"LF", "none", "CRLF"} {
		m, _ := a.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
		a = m.(*App)
		if got := a.rawModal.NewlineLabel(); got != want {
			t.Fatalf("after ctrl+o: %s, want %s", got, want)
		}
	}
	if !strings.Contains(a.rawModal.View(), "ctrl+o CRLF") {
		t.Errorf("footer does not show the current terminator:\n%s", a.rawModal.View())
	}
}

// `x` used to close the pane, which made the letter untypable in the entry
// line. Every pane action is a chord now.
func TestRawModalPrintableKeysReachTheEntryLine(t *testing.T) {
	a := New(Config{})
	a.rawModal.Show("task-1")
	a.rawModal.AddPane("task-1", "127.0.0.1", 8080, 1)
	a.rawModal.SetConn(1, nil, nil, "connected")

	for _, r := range "xq" {
		m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		a = m.(*App)
	}
	if a.rawModal.PaneCount() != 1 {
		t.Fatalf("a printable key closed the pane (PaneCount=%d)", a.rawModal.PaneCount())
	}
	if got := a.rawModal.Spec(); got != "xq" {
		t.Errorf("entry line = %q, want \"xq\"", got)
	}
}

func TestRawModalHexToggleAndClose(t *testing.T) {
	a := New(Config{})
	a.rawModal.Show("task-1")
	a.rawModal.AddPane("task-1", "127.0.0.1", 8080, 1)
	a.rawModal.SetConn(1, nil, nil, "connected")

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	a = m.(*App)
	if !a.rawModal.InHex() {
		t.Fatal("ctrl+r did not turn hex entry on")
	}
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	a = m.(*App)
	if a.rawModal.PaneCount() != 0 {
		t.Errorf("ctrl+x did not close the pane")
	}
}
