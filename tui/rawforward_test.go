package tui

import (
	"bytes"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
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
	if other := m.PaneForGen(8); other == nil || len(other.output()) != 0 {
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

// The ring keeps BOTH ends. A pure keep-the-newest rule dropped the status
// line and headers of any response bigger than the ring, leaving a body with
// no indication of what it answered — the operator hit exactly that: "<html>
// タグがスクロールバックしても見えない". The middle that goes is reported, so a
// truncated view cannot read as a complete one.
func TestRawPane_RingKeepsHeadAndTail(t *testing.T) {
	var p rawPane
	head := []byte("HTTP/1.1 200 OK\r\nHEAD-MARKER\r\n")
	p.AppendOutput(head)
	p.AppendOutput(bytes.Repeat([]byte("M"), 2*rawTUIRingBytes))
	tail := []byte("TAIL-MARKER")
	p.AppendOutput(tail)

	out := p.output()
	if !bytes.HasPrefix(out, head) {
		t.Errorf("the front of the response was dropped; output starts %q", out[:min(40, len(out))])
	}
	if !bytes.HasSuffix(out, tail) {
		t.Errorf("newest bytes must survive; output ends %q", out[max(0, len(out)-16):])
	}
	if !bytes.Contains(out, []byte("bytes elided")) {
		t.Error("a dropped middle must be reported, not silent")
	}
	if len(p.head)+len(p.tail) > rawTUIRingBytes {
		t.Errorf("retained %d bytes, past the %d cap", len(p.head)+len(p.tail), rawTUIRingBytes)
	}
}

// Nothing dropped means nothing to say about it.
func TestRawPane_NoMarkerWhenNothingElided(t *testing.T) {
	var p rawPane
	p.AppendOutput([]byte("HTTP/1.1 200 OK\r\n\r\npong"))
	if got := string(p.output()); got != "HTTP/1.1 200 OK\r\n\r\npong" {
		t.Errorf("output = %q, want it verbatim", got)
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
	if got := a.rawModal.ActivePane().output(); len(got) != 0 {
		t.Fatalf("stale data applied: %q", got)
	}
	a.Update(RawForwardClosedMsg{Gen: 1, Reason: "stale close"})
	if !a.rawModal.ActivePane().live {
		t.Fatal("a close for an unknown generation must not mark a live pane closed")
	}

	a.Update(RawForwardDataMsg{Gen: 2, Data: []byte("fresh")})
	if got := string(a.rawModal.ActivePane().output()); got != "fresh" {
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

// The modal is framed and fills the terminal, like the forwards list and the
// grid. Before SetSize existed it sized itself to its longest line and
// lipgloss.Place centred that, so it read as a floating note rather than a
// panel — the operator's words were 淡泊すぎ.
func TestRawModalViewFillsTheTerminal(t *testing.T) {
	m := NewRawConnectModal()
	m.SetSize(100, 24)
	m.Show("a69a7c86528a0000000000000000000a")
	m.AddPane("a69a7c86528a0000000000000000000a", "localhost", 8765, 23)
	m.SetConn(23, nil, nil, "connected (fwd 23)")

	lines := strings.Split(m.View(), "\n")
	if !strings.HasPrefix(lines[0], "╭") || !strings.HasPrefix(lines[len(lines)-1], "╰") {
		t.Fatalf("view is not framed:\n%s", m.View())
	}
	if n := lipgloss.Width(lines[0]); n < 90 || n > 100 {
		t.Errorf("frame is %d cells wide in a 100-cell terminal", n)
	}
	for i, l := range lines {
		if n := lipgloss.Width(l); n > 100 {
			t.Errorf("line %d is %d cells wide, past the terminal", i, n)
		}
	}
	if len(lines) < 20 {
		t.Errorf("view is %d rows tall in a 24-row terminal; it should fill", len(lines))
	}
	if !strings.Contains(m.View(), "(1 pane)") {
		t.Errorf("header does not report the pane count:\n%s", m.View())
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

// The entry line has two jobs — a target on [+ new], bytes on a pane — and one
// textinput. It must say which, or a connected pane invites the operator to
// type a target into the send line (which is what it did).
func TestRawModalEntryLineDescribesItsMode(t *testing.T) {
	m := NewRawConnectModal()
	m.SetSize(100, 24)
	m.Show("task-1")

	if v := m.View(); !strings.Contains(v, "target > ") || !strings.Contains(v, "host:port") {
		t.Errorf("[+ new] must ask for a target:\n%s", v)
	}

	m.AddPane("task-1", "localhost", 8765, 1)
	m.SetConn(1, nil, nil, "connected (fwd 1)")
	v := m.View()
	if !strings.Contains(v, "send CRLF > ") || !strings.Contains(v, "bytes to send") {
		t.Errorf("a live pane must offer to send, with the terminator shown:\n%s", v)
	}
	if strings.Contains(v, "host:port") {
		t.Errorf("a live pane still advertises a target placeholder:\n%s", v)
	}

	m.ToggleHex()
	if v := m.View(); !strings.Contains(v, "send hex > ") {
		t.Errorf("hex mode not reflected in the entry line:\n%s", v)
	}
	m.ToggleHex()
	m.CycleNewline()
	if v := m.View(); !strings.Contains(v, "send LF > ") {
		t.Errorf("terminator change not reflected in the entry line:\n%s", v)
	}

	m.MarkClosed(1, "connection closed")
	if v := m.View(); !strings.Contains(v, "closed  > ") {
		t.Errorf("a closed pane must not look like it can send:\n%s", v)
	}
}

// The builder's errors already start with "http:" — wrapping them produced
// "http: http: path ...".
func TestRawModalFormErrorIsNotDoublePrefixed(t *testing.T) {
	a := New(Config{})
	a.rawModal.Show("task-1")
	a.rawModal.AddPane("task-1", "localhost", 8765, 1)
	a.rawModal.SetConn(1, nil, nil, "connected")
	a.rawModal.ToggleForm()
	a.rawModal.SetFormForTest("GET", "", "", "")

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	note := a.rawModal.ActivePane().note
	if strings.Contains(note, "http: http:") {
		t.Errorf("note is double-prefixed: %q", note)
	}
	if !strings.HasPrefix(note, "http:") {
		t.Errorf("note lost its prefix: %q", note)
	}
}

// The far end sends arbitrary bytes into a bordered panel. An ESC sequence
// among them repositions the cursor and the frame gets drawn over, so the
// panel visibly falls apart; a lone CR does the same to one line.
func TestSanitizeOutputCannotMoveTheCursor(t *testing.T) {
	got := sanitizeOutput([]byte("HTTP/1.1 200 OK\r\nX: \x1b[2J\x1b[H\r ok\x00\x7f\n"))
	if strings.ContainsAny(got, "\x1b\x00\x7f\r") {
		t.Errorf("control bytes survived: %q", got)
	}
	if !strings.HasPrefix(got, "HTTP/1.1 200 OK\n") {
		t.Errorf("CRLF should collapse to LF, got %q", got)
	}
	if !strings.Contains(got, "ok") || !strings.Contains(got, "X: ") {
		t.Errorf("printable text was lost: %q", got)
	}
	if strings.Contains(got, "\n\n") {
		t.Errorf("CRLF collapse should not double the newline: %q", got)
	}
}

// File operations need a worktree the runner still holds; the server answers
// NoSuchTask outside Running/Detached. Opening the picker for a terminal task
// produced a modal whose first listing failed.
func TestTaskSessionAlive(t *testing.T) {
	live := []protocol.TaskStatus{protocol.TaskStatus_Running, protocol.TaskStatus_Detached}
	for _, s := range live {
		if !taskSessionAlive(s) {
			t.Errorf("%s should count as a live session", taskStatusStr(s))
		}
	}
	dead := []protocol.TaskStatus{
		protocol.TaskStatus_Queued, protocol.TaskStatus_Succeeded,
		protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled,
	}
	for _, s := range dead {
		if taskSessionAlive(s) {
			t.Errorf("%s must not offer file operations", taskStatusStr(s))
		}
	}
}
