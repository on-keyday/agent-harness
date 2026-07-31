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

// openViewer is the operator's path to a pane: list → n → target → Enter, then
// the viewer for the pane that just opened.
func openViewer(t *testing.T, target string) *App {
	t.Helper()
	a := New(Config{})
	a.client = &cli.Client{} // non-nil is enough; the returned cmd is never run
	a.rawModal.SetSize(100, 24)
	a.rawModal.Show("task-1")
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	a = m.(*App)
	if a.rawModal.Mode() != rawModeNew {
		t.Fatalf("n did not open the target prompt (mode=%d)", a.rawModal.Mode())
	}
	a.rawModal.SetSpec(target)
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if cmd == nil {
		t.Fatalf("a valid target should dispatch a connect")
	}
	if a.rawModal.Mode() != rawModeView {
		t.Fatalf("connecting should open the viewer (mode=%d)", a.rawModal.Mode())
	}
	return a
}

// The list is home: esc from the viewer returns to it with the pane still
// connected, and esc from the list hides the modal without closing anything.
func TestRawModalListIsHome(t *testing.T) {
	a := openViewer(t, "127.0.0.1:6379")
	gen := a.rawModal.ActivePane().gen

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	a = m.(*App)
	if a.rawModal.Mode() != rawModeList {
		t.Fatalf("esc from the viewer should return to the list")
	}
	if a.rawModal.PaneForGen(gen) == nil {
		t.Error("returning to the list must not close the pane")
	}

	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	a = m.(*App)
	if a.rawModal.IsOpen() {
		t.Error("esc from the list should hide the modal")
	}
	if a.rawModal.PaneCount() != 1 {
		t.Error("hiding must not close panes")
	}
}

// Enter on a row opens that row's pane, and the list reports each pane's state
// and byte counts.
func TestRawModalListOpensTheSelectedRow(t *testing.T) {
	a := openViewer(t, "127.0.0.1:6379")
	a.Update(RawForwardDataMsg{Gen: a.rawModal.ActivePane().gen, Data: []byte("hello")})

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	a = m.(*App)
	v := a.rawModal.View()
	for _, want := range []string{"127.0.0.1:6379", "5B", "Enter: open", "n: new"} {
		if !strings.Contains(v, want) {
			t.Errorf("list is missing %q:\n%s", want, v)
		}
	}

	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if a.rawModal.Mode() != rawModeView {
		t.Fatal("Enter on a row should open the viewer")
	}
	if !strings.Contains(a.rawModal.View(), "hello") {
		t.Errorf("the viewer does not show the pane's output:\n%s", a.rawModal.View())
	}
}

// Two attempts get their own generations and their own panes, so a close for
// one cannot tear down the other.
func TestRawForwardConnect_AttemptsAreIndependent(t *testing.T) {
	a := openViewer(t, "127.0.0.1:6379")
	first := a.rawModal.ActivePane().gen

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEsc}) // back to the list
	a = m.(*App)
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	a = m.(*App)
	a.rawModal.SetSpec("127.0.0.1:6380")
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if cmd == nil {
		t.Fatal("the second target should dispatch its own connect")
	}
	second := a.rawModal.ActivePane().gen

	if first == second {
		t.Fatalf("both attempts share generation %d", first)
	}
	if a.rawModal.PaneCount() != 2 {
		t.Fatalf("PaneCount = %d, want 2", a.rawModal.PaneCount())
	}
	a.Update(RawForwardClosedMsg{Gen: first, Reason: "first died"})
	if p := a.rawModal.PaneForGen(second); p == nil || p.note == "first died" {
		t.Fatalf("a close for the first attempt leaked into the second: %+v", p)
	}
}

// A bad target must not create a pane: one that never had a connection is a
// row the operator cannot do anything with.
func TestRawForwardConnect_BadTargetCreatesNoPane(t *testing.T) {
	a := New(Config{})
	a.client = &cli.Client{}
	a.rawModal.Show("task-1")
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	a = m.(*App)
	a.rawModal.SetSpec("garbage")
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)
	if cmd != nil || a.rawModal.PaneCount() != 0 {
		t.Fatalf("a bad target dispatched or created a pane (cmd=%v panes=%d)", cmd != nil, a.rawModal.PaneCount())
	}
}

// In the viewer the entry line says what Enter will do, and printable keys
// reach it — no letter is a command there.
func TestRawModalViewerEntryLine(t *testing.T) {
	a := openViewer(t, "127.0.0.1:6379")
	a.rawModal.SetConn(a.rawModal.ActivePane().gen, nil, nil, "connected")

	if v := a.rawModal.View(); !strings.Contains(v, "send CRLF > ") {
		t.Errorf("viewer entry line does not describe the send:\n%s", v)
	}
	for _, r := range "xnq" {
		m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		a = m.(*App)
	}
	if a.rawModal.PaneCount() != 1 {
		t.Fatalf("a printable key acted as a command (panes=%d)", a.rawModal.PaneCount())
	}
	if got := a.rawModal.Spec(); got != "xnq" {
		t.Errorf("entry line = %q, want \"xnq\"", got)
	}

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	a = m.(*App)
	if v := a.rawModal.View(); !strings.Contains(v, "send hex > ") {
		t.Errorf("ctrl+r did not switch the entry line to hex:\n%s", v)
	}
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	a = m.(*App)
	if v := a.rawModal.View(); !strings.Contains(v, "ctrl+o LF") {
		t.Errorf("ctrl+o did not cycle the terminator:\n%s", v)
	}
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	a = m.(*App)
	if a.rawModal.PaneCount() != 0 || a.rawModal.Mode() != rawModeList {
		t.Errorf("ctrl+x should close the pane and return to the list")
	}
}

// The modal is framed and fills the terminal, like the forwards list.
func TestRawModalViewFillsTheTerminal(t *testing.T) {
	m := NewRawConnectModal()
	m.SetSize(100, 24)
	m.Show("a69a7c86528a0000000000000000000a")
	m.AddPane("a69a7c86528a0000000000000000000a", "localhost", 8765, 23)
	m.BackToList()

	lines := strings.Split(m.View(), "\n")
	if !strings.HasPrefix(lines[0], "╭") || !strings.HasPrefix(lines[len(lines)-1], "╰") {
		t.Fatalf("view is not framed:\n%s", m.View())
	}
	for i, l := range lines {
		if n := lipgloss.Width(l); n > 100 {
			t.Errorf("line %d is %d cells wide, past the terminal", i, n)
		}
	}
	if len(lines) < 20 {
		t.Errorf("view is %d rows tall in a 24-row terminal; it should fill", len(lines))
	}
}

// Every screen fills the frame. A modal that shrinks when you press `n` reads
// as a different window opening rather than the same one changing mode.
func TestRawModalScreensAreTheSameHeight(t *testing.T) {
	m := NewRawConnectModal()
	m.SetSize(100, 24)
	m.Show("task-1")
	m.AddPane("task-1", "localhost", 8765, 1)
	m.SetConn(1, nil, nil, "connected")

	heights := map[string]int{}
	m.BackToList()
	heights["list"] = len(strings.Split(m.View(), "\n"))
	m.OpenSelected()
	heights["view"] = len(strings.Split(m.View(), "\n"))
	m.BeginNew()
	heights["new"] = len(strings.Split(m.View(), "\n"))

	for name, h := range heights {
		if h != heights["list"] {
			t.Errorf("%s screen is %d rows, list is %d — the frame must not jump", name, h, heights["list"])
		}
	}
}
