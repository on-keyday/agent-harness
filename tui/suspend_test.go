package tui

import (
	"strings"
	"testing"
)

// Terminal handovers cannot nest: ReleaseTerminal/RestoreTerminal are not
// re-entrant. Keys cannot reach Update while the terminal is released (the
// input reader is cancelled), but MESSAGES can — a second in-flight
// OpenInteractive RPC, dispatched before the first was attached, resolves into
// a second InteractiveReadyMsg mid-handover. Under the old tea.Exec path the
// frozen Update loop queued it until the session ended; the loop keeps running
// now, so it must be refused here instead of racing the first child for stdin.
func TestInteractiveReadyMsg_RefusedWhileTerminalReleased(t *testing.T) {
	a := New(Config{})
	a.termReleased = true

	x11Cancelled := false
	m, cmd := a.Update(InteractiveReadyMsg{
		TaskID:    "0123456789abcdef",
		X11Cancel: func() { x11Cancelled = true },
	})
	a = m.(*App)

	if cmd != nil {
		t.Error("a second handover must not be started while the terminal is released")
	}
	if !a.termReleased {
		t.Error("the in-progress handover's flag was cleared by the refused one")
	}
	if !x11Cancelled {
		t.Error("the refused session's X11 forward goroutine was leaked (X11Cancel not called)")
	}
	if a.x11Cancel != nil {
		t.Error("the refused session overwrote the live session's x11Cancel")
	}
	found := false
	for _, line := range a.cmdresult.lines {
		if strings.Contains(line, "terminal is busy") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a 'terminal is busy' cmdresult line, got %q", a.cmdresult.lines)
	}
}

// The flag must survive a full open→done cycle, or a later attach is refused
// forever. InteractiveDoneMsg is what the handover Cmd posts on return.
func TestInteractiveDoneMsg_ReleasesTheTerminalFlag(t *testing.T) {
	a := New(Config{})
	a.termReleased = true
	m, _ := a.Update(InteractiveDoneMsg{TaskID: "0123456789abcdef"})
	if m.(*App).termReleased {
		t.Error("termReleased still set after InteractiveDoneMsg: no further attach could start")
	}
}

// Same for the external editor, which shares the flag with the attach path.
func TestFileEditExternal_RefusedWhileTerminalReleased(t *testing.T) {
	a := New(Config{})
	a.termReleased = true
	m, cmd := a.Update(FileEditExternalMsg{Name: "x.txt", Text: "hi"})
	a = m.(*App)
	if cmd != nil {
		t.Error("editor handover must not start while the terminal is released")
	}
	if a.fileEditTmpPath != "" {
		t.Errorf("refused editor still wrote a temp file: %q", a.fileEditTmpPath)
	}
}

func TestFileEditExecDoneMsg_ReleasesTheTerminalFlag(t *testing.T) {
	a := New(Config{})
	a.termReleased = true
	m, _ := a.Update(fileEditExecDoneMsg{path: "/nonexistent/harness-test"})
	if m.(*App).termReleased {
		t.Error("termReleased still set after fileEditExecDoneMsg")
	}
}
