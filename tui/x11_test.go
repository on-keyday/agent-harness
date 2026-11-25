package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
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

func x11Held(a *App, id, n int) {
	a.activeForwards[id] = &PortForwardSession{ID: id, TaskID: x11TaskID, Direction: ForwardX11, X11Display: n}
}

// Every --x11 open used to default to display 10, so a second X11 task on
// the same runner could not bind 6010. Held and in-flight numbers are skipped.
func TestX11DisplaySkipsHeldAndReserved(t *testing.T) {
	a := New(Config{})
	x11Held(a, 1, 10)
	if n, ok := a.pickX11Display(10); !ok || n != 11 {
		t.Fatalf("pick = %d,%v; want 11 (10 is held)", n, ok)
	}
	if n, ok := a.pickX11Display(10); !ok || n != 12 {
		t.Fatalf("pick = %d,%v; want 12 (11 is reserved by the open in flight)", n, ok)
	}
}

func TestX11DisplayHonoursAHigherRequest(t *testing.T) {
	a := New(Config{})
	if n, ok := a.pickX11Display(15); !ok || n != 15 {
		t.Fatalf("pick = %d,%v; want 15", n, ok)
	}
}

func TestX11DisplayRunsOut(t *testing.T) {
	a := New(Config{})
	for n := 10; n <= 99; n++ {
		x11Held(a, n, n)
	}
	if n, ok := a.pickX11Display(10); ok {
		t.Fatalf("pick = %d with every display from 10 to 99 held", n)
	}
}

// A failed open must not keep its number: the Ready handler releases it on
// success and on error alike.
func TestX11DisplayIsReleasedWhenTheOpenFails(t *testing.T) {
	a := New(Config{})
	if n, _ := a.pickX11Display(10); n != 10 {
		t.Fatalf("first pick = %d", n)
	}
	a.Update(InteractiveReadyMsg{X11Display: 10, Err: errors.New("refused")})
	if n, ok := a.pickX11Display(10); !ok || n != 10 {
		t.Fatalf("pick after the failed open = %d,%v; want 10 again", n, ok)
	}
}

// The TUI command line's `session new --x11` goes through the same pick, with
// --x11-display (default 10) as the lowest number it may take.
func TestTuiSessionNewX11UsesAFreeDisplay(t *testing.T) {
	var got []int
	old := openX11Session
	defer func() { openX11Session = old }()
	openX11Session = func(c *cli.Client, repo string, sel cli.SelectorOpts, extraArgs []string, resumeTaskID string, displayN int, forwardID int, program *tea.Program, auth Authority, capsOverride, conv, exact bool, agent string, rm cli.RunModeFlags) tea.Cmd {
		got = append(got, displayN)
		return nil
	}
	a := New(Config{DefaultRepo: "/r"})
	x11Held(a, 1, 10)
	_ = (tuiVerbs{a}).SessionNew(verb.SpawnAction{Repo: "/r", X11: true, X11Display: 10})
	if len(got) != 1 || got[0] != 11 {
		t.Fatalf("displays opened = %v; want [11]", got)
	}
}
