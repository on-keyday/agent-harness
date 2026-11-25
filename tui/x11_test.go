package tui

import (
	"strings"
	"testing"
)

const x11TaskID = "0123456789abcdef0123456789abcdef"

// A detach (Ctrl+]) used to cancel the X11 forward, and with it every GUI
// program the session had open. The forward now lives in the registry like a
// `b` forward and a detach leaves it alone.
func TestDetachLeavesTheX11ForwardRunning(t *testing.T) {
	a := New(Config{})
	cancelled := false
	a.activeForwards[7] = &PortForwardSession{ID: 7, TaskID: x11TaskID, Direction: ForwardX11, X11Display: 10,
		Cancel: func() { cancelled = true }}
	a.Update(InteractiveDoneMsg{TaskID: x11TaskID})
	if cancelled {
		t.Fatal("the detach cancelled the X11 forward")
	}
	if a.activeForwards[7] == nil {
		t.Fatal("the detach dropped the X11 forward from the registry")
	}
}

// A workspace apply reconciles only the forwards it can name; an X11 tunnel
// is not one of them, so it must never be adopted or stopped by one.
func TestX11ForwardIsNoWorkspaceForward(t *testing.T) {
	if _, ok := sessionIdentity(&PortForwardSession{Direction: ForwardX11, Spec: ":10 -> localhost:0"}); ok {
		t.Fatal("an X11 forward has a workspace identity")
	}
}

// The runner closes the forward when its task ends; the serve goroutine's
// PortForwardStoppedMsg is what removes it here.
func TestX11ForwardStopsWithItsTask(t *testing.T) {
	a := New(Config{})
	a.activeForwards[7] = &PortForwardSession{ID: 7, TaskID: x11TaskID, Direction: ForwardX11, X11Display: 10}
	a.Update(PortForwardStoppedMsg{ID: 7, TaskID: x11TaskID})
	if a.activeForwards[7] != nil {
		t.Fatal("a stopped X11 forward is still registered")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "forward stopped") {
		t.Fatalf("no stop line: %v", a.cmdresult.lines)
	}
}
