package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
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
	openX11Session = func(c *cli.Client, repo string, sel cli.SelectorOpts, retryUnpinned bool, extraArgs []string, resumeTaskID string, displayN int, forwardID int, program *tea.Program, auth Authority, capsOverride, conv, exact bool, agent string, rm cli.RunModeFlags) tea.Cmd {
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

type x11Open struct {
	resume  string
	display int
	conv    bool
	sel     cli.SelectorOpts
	fwdID   int
	retry   bool
}

// stubX11Open records what reaches the X11 open seam.
func stubX11Open(t *testing.T) *[]x11Open {
	var got []x11Open
	old := openX11Session
	t.Cleanup(func() { openX11Session = old })
	openX11Session = func(c *cli.Client, repo string, sel cli.SelectorOpts, retryUnpinned bool, extraArgs []string, resumeTaskID string, displayN int, forwardID int, program *tea.Program, auth Authority, capsOverride, conv, exact bool, agent string, rm cli.RunModeFlags) tea.Cmd {
		got = append(got, x11Open{resume: resumeTaskID, display: displayN, conv: conv, sel: sel, fwdID: forwardID, retry: retryUnpinned})
		return nil
	}
	return &got
}

// x11App seeds one interactive task in st and selects it.
func x11App(t *testing.T, st protocol.TaskStatus) (*App, protocol.TaskInfo) {
	a := New(Config{})
	var ti protocol.TaskInfo
	ti.Id.Id[0] = 0x58
	ti.Kind = protocol.TaskKind_Interactive
	ti.Status = st
	ti.AssignedTo.Id[0] = 0x07
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{ti}})
	return a, ti
}

func x11Out(a *App) string { return strings.Join(a.cmdresult.lines, "\n") }

// stubProbe makes D's X-server probe answer display/found/err. DISPLAY is
// saved and emptied first: a found display is set in this process.
func stubProbe(t *testing.T, display string, found bool, err error) {
	t.Setenv("DISPLAY", "")
	old := probeX11Display
	t.Cleanup(func() { probeX11Display = old })
	probeX11Display = func() (string, bool, error) { return display, found, err }
}

// pressD presses D and, when D answers with its probe, delivers the probe's
// result as the runtime would. It returns the command that follows. Only for
// rows where D probes: any other command is returned unrun.
func pressD(t *testing.T, a *App) tea.Cmd {
	t.Helper()
	_, cmd := a.Update(runeKey('D'))
	if cmd == nil {
		return nil
	}
	m, ok := cmd().(x11ProbedMsg)
	if !ok {
		t.Fatal("D did not probe for the X server")
	}
	_, cmd = a.Update(m)
	return cmd
}

func TestDReattachesWhenThisTUIHoldsTheTunnel(t *testing.T) {
	got := stubX11Open(t)
	a, ti := x11App(t, protocol.TaskStatus_Running)
	a.activeForwards[1] = &PortForwardSession{ID: 1, TaskID: FormatTaskID(ti.Id), Direction: ForwardX11, X11Display: 10}
	_, cmd := a.Update(runeKey('D'))
	if cmd == nil {
		t.Fatal("D on a session whose tunnel this TUI holds did nothing")
	}
	if len(*got) != 0 || a.x11RestartAsk != "" {
		t.Fatalf("D reopened or asked instead of reattaching: opens=%v ask=%q", *got, a.x11RestartAsk)
	}
}

func TestDResumesAFinishedTaskWithX11(t *testing.T) {
	stubProbe(t, "localhost:0", false, nil)
	got := stubX11Open(t)
	a, ti := x11App(t, protocol.TaskStatus_Cancelled)
	pressD(t, a)
	// retry: a runner that restarted has a new RunnerID, so the pin fails on
	// exactly the tasks a restart left to resume; r falls back to Any, and so
	// does D.
	want := x11Open{resume: FormatTaskID(ti.Id), display: 10, conv: true, sel: resumeSelectorOpts(ti.AssignedTo), fwdID: (*got)[0].fwdID, retry: true}
	if len(*got) != 1 || (*got)[0] != want {
		t.Fatalf("opens = %+v; want [%+v]", *got, want)
	}
}

func TestDAsksBeforeRestarting(t *testing.T) {
	stubProbe(t, "localhost:0", false, nil)
	got := stubX11Open(t)
	a, ti := x11App(t, protocol.TaskStatus_Running)
	a.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	cmd := pressD(t, a)
	if a.x11RestartAsk != FormatTaskID(ti.Id) || cmd != nil {
		t.Fatalf("D did not just ask: ask=%q cmd=%v", a.x11RestartAsk, cmd != nil)
	}
	if !strings.Contains(a.View(), "restart") {
		t.Fatal("the question is not on screen")
	}
	// q must not quit under a pending destructive question.
	_, cmd = a.Update(runeKey('q'))
	if cmd != nil {
		t.Fatalf("q under the question returned a command (quit?)")
	}
	if a.x11RestartAsk != "" || len(a.x11Restarts) != 0 || len(*got) != 0 {
		t.Fatalf("a non-y answer restarted something: ask=%q pending=%v opens=%v", a.x11RestartAsk, a.x11Restarts, *got)
	}
	if !strings.Contains(x11Out(a), "nothing restarted") {
		t.Fatalf("no 'nothing restarted' line: %s", x11Out(a))
	}
}

// No X server answering is a refusal before the question: a restart then
// would kill the agent's turn for an X11 session that cannot work.
func TestDRefusesWhenNoXServerAnswers(t *testing.T) {
	stubProbe(t, "", false, errors.New("x11: nothing answered at DISPLAY=localhost:0: is the X server running?"))
	got := stubX11Open(t)
	a, _ := x11App(t, protocol.TaskStatus_Running)
	pressD(t, a)
	if a.x11RestartAsk != "" {
		t.Fatal("D asked to restart with no X server answering: the agent would be killed for nothing")
	}
	if !strings.Contains(x11Out(a), "is the X server running") {
		t.Fatalf("the refusal is not shown: %s", x11Out(a))
	}
	b, _ := x11App(t, protocol.TaskStatus_Cancelled)
	pressD(t, b)
	if len(*got) != 0 {
		t.Fatalf("D resumed a finished task with no X server answering: %v", *got)
	}
}

// On Windows the TUI finds an X server nobody told it about; it must use it
// for the open (DISPLAY in this process) and say which one it took.
func TestDUsesTheXServerItFound(t *testing.T) {
	stubProbe(t, "localhost:2", true, nil)
	got := stubX11Open(t)
	a, _ := x11App(t, protocol.TaskStatus_Cancelled)
	pressD(t, a)
	if d := os.Getenv("DISPLAY"); d != "localhost:2" {
		t.Fatalf("DISPLAY = %q; want the found localhost:2 set for the open", d)
	}
	if !strings.Contains(x11Out(a), "using DISPLAY=localhost:2") {
		t.Fatalf("the found display is not reported: %s", x11Out(a))
	}
	if len(*got) != 1 {
		t.Fatalf("opens = %v; want the resume", *got)
	}
}

func TestDRestartCancelsThenResumes(t *testing.T) {
	stubProbe(t, "localhost:0", false, nil)
	got := stubX11Open(t)
	a, ti := x11App(t, protocol.TaskStatus_Running)
	id := FormatTaskID(ti.Id)
	pressD(t, a)
	_, cmd := a.Update(runeKey('y'))
	if cmd == nil || !a.x11Restarts[id] {
		t.Fatalf("y did not cancel: cmd=%v pending=%v", cmd != nil, a.x11Restarts)
	}
	_, cmd = a.Update(runeKey('D'))
	if cmd != nil || a.x11RestartAsk != "" || !strings.Contains(x11Out(a), "already restarting") {
		t.Fatalf("a second D while restarting: cmd=%v ask=%q out=%s", cmd != nil, a.x11RestartAsk, x11Out(a))
	}
	if len(*got) != 0 {
		t.Fatal("reopened before the task finished")
	}
	a.Update(TaskEventMsg{Event: protocol.TaskStatusEvent{TaskId: ti.Id, TaskStatus: protocol.TaskStatus_Cancelled, Kind: protocol.StatusEventKind_TaskEnded}})
	if len(*got) != 1 || (*got)[0].resume != id || !(*got)[0].conv {
		t.Fatalf("opens after the cancel = %+v; want one resume of %s with the conversation", *got, id)
	}
	if len(a.x11Restarts) != 0 {
		t.Fatalf("the restart is still pending: %v", a.x11Restarts)
	}
}

func TestDRestartGivesUpOnCancelError(t *testing.T) {
	a, ti := x11App(t, protocol.TaskStatus_Running)
	id := FormatTaskID(ti.Id)
	a.x11Restarts[id] = true
	a.Update(CancelResultMsg{IDPrefix: id, Resolved: id, Err: errors.New("denied")})
	if len(a.x11Restarts) != 0 || !strings.Contains(x11Out(a), "denied") {
		t.Fatalf("pending=%v out=%s", a.x11Restarts, x11Out(a))
	}
}

func TestDRestartGivesUpAfterTimeout(t *testing.T) {
	got := stubX11Open(t)
	a, ti := x11App(t, protocol.TaskStatus_Running)
	id := FormatTaskID(ti.Id)
	a.x11Restarts[id] = true
	a.Update(x11RestartTimeoutMsg{TaskID: id})
	if len(a.x11Restarts) != 0 || !strings.Contains(x11Out(a), "did not finish") {
		t.Fatalf("pending=%v out=%s", a.x11Restarts, x11Out(a))
	}
	a.Update(TaskEventMsg{Event: protocol.TaskStatusEvent{TaskId: ti.Id, TaskStatus: protocol.TaskStatus_Cancelled, Kind: protocol.StatusEventKind_TaskEnded}})
	if len(*got) != 0 {
		t.Fatal("a restart given up on still reopened the task")
	}
}

// The start modal bumps nextForwardID and then uses it; an X11 open that used
// the counter's current value and then bumped it took the SAME id, and the
// second PortForwardStartedMsg overwrote the first registry entry. One
// allocator serves every site now.
func TestX11ForwardIDDoesNotReuseAModalForwardsID(t *testing.T) {
	stubProbe(t, "localhost:0", false, nil)
	got := stubX11Open(t)
	a, _ := x11App(t, protocol.TaskStatus_Cancelled)
	// As the modal leaves it after starting a `b` forward: bumped to 5, used 5.
	a.nextForwardID = 5
	a.activeForwards[5] = &PortForwardSession{ID: 5, TaskID: x11TaskID, Direction: ForwardRemote}
	pressD(t, a)
	if len(*got) != 1 || (*got)[0].fwdID == 5 {
		t.Fatalf("the X11 open took registry id %v, which the modal forward holds", *got)
	}
}

// The typed line names its runner on purpose; widening it silently would
// spawn somewhere the operator did not ask for.
func TestTuiSessionNewX11KeepsItsRunner(t *testing.T) {
	got := stubX11Open(t)
	a := New(Config{DefaultRepo: "/r"})
	_ = (tuiVerbs{a}).SessionNew(verb.SpawnAction{Repo: "/r", X11: true, X11Display: 10, Runner: "abc"})
	if len(*got) != 1 || (*got)[0].retry {
		t.Fatalf("opens = %+v; want one without the unpinned retry", *got)
	}
}

func TestWithResumeRetry(t *testing.T) {
	noPause := func(int) bool { return true }
	pinned := cli.SelectorOpts{Runner: "07000000000000000000000000000000"}
	var tried []cli.SelectorOpts
	open := func(o cli.SelectorOpts) error {
		tried = append(tried, o)
		if o.Runner != "" {
			return fmt.Errorf("open: %w", cli.ErrPinnedNotFound)
		}
		return nil
	}
	if err := withResumeRetry(pinned, true, open, noPause); err != nil || len(tried) != 2 || tried[1] != (cli.SelectorOpts{}) {
		t.Fatalf("retry=true: err=%v tried=%v; want a second, unpinned try that succeeds", err, tried)
	}
	tried = nil
	if err := withResumeRetry(pinned, false, open, noPause); !errors.Is(err, cli.ErrPinnedNotFound) || len(tried) != 1 {
		t.Fatalf("retry=false: err=%v tried=%v; want the pinned failure, no second try", err, tried)
	}
	tried = nil
	other := func(o cli.SelectorOpts) error { tried = append(tried, o); return errors.New("refused") }
	if err := withResumeRetry(pinned, true, other, noPause); err == nil || len(tried) != 1 {
		t.Fatalf("another error: err=%v tried=%v; want no retry", err, tried)
	}
}

// D resumes the moment its cancel shows the task finished, and the server
// refuses that while the previous run is still stopping on its runner ("retry
// in a few seconds"). D retries it, paused between tries, until the pause
// gives up; r's operator did that by hand without noticing.
func TestWithResumeRetryWaitsOutAStoppingRun(t *testing.T) {
	stopping := fmt.Errorf("open: %w", cli.ErrResumeNotTerminal)
	tries, pauses := 0, 0
	open := func(cli.SelectorOpts) error {
		tries++
		if tries < 3 {
			return stopping
		}
		return nil
	}
	pause := func(int) bool { pauses++; return true }
	if err := withResumeRetry(cli.SelectorOpts{}, true, open, pause); err != nil || tries != 3 || pauses != 2 {
		t.Fatalf("err=%v tries=%d pauses=%d; want success on the third try after two pauses", err, tries, pauses)
	}
	tries = 0
	always := func(cli.SelectorOpts) error { tries++; return stopping }
	giveUp := func(attempt int) bool { return attempt < 3 }
	if err := withResumeRetry(cli.SelectorOpts{}, true, always, giveUp); !errors.Is(err, cli.ErrResumeNotTerminal) || tries != 3 {
		t.Fatalf("err=%v tries=%d; want the stopping refusal once the pause gives up after 3 tries", err, tries)
	}
	tries = 0
	if err := withResumeRetry(cli.SelectorOpts{}, false, always, giveUp); tries != 1 || err == nil {
		t.Fatalf("retry=false: tries=%d err=%v; want no retry", tries, err)
	}
}

// A forward the runner or server closed ends its serve goroutine, but the
// ctx it was started under, and the status drainer on it, stayed open until
// the TUI exited. An X11 tunnel now ends that way once per session.
func TestStoppedForwardReleasesItsContext(t *testing.T) {
	a := New(Config{})
	cancelled := false
	a.activeForwards[7] = &PortForwardSession{ID: 7, TaskID: x11TaskID, Direction: ForwardX11, X11Display: 10,
		Cancel: func() { cancelled = true }}
	a.Update(PortForwardStoppedMsg{ID: 7, TaskID: x11TaskID})
	if !cancelled {
		t.Fatal("the stopped forward's context was left open")
	}
}

// `display` is the TUI process's own DISPLAY: shown bare, set with a value
// checked the way D checks one, and nothing set when the check refuses.
func TestDisplayCommandShowsAndSets(t *testing.T) {
	t.Setenv("DISPLAY", "")
	a := New(Config{})
	h := tuiVerbs{a}
	h.Display(verb.ScreenAction{})
	if !strings.Contains(x11Out(a), "unset") {
		t.Fatalf("bare display with nothing set: %s", x11Out(a))
	}
	h.Display(verb.ScreenAction{Arg: "localhost:1"})
	if d := os.Getenv("DISPLAY"); d != "localhost:1" {
		t.Fatalf("DISPLAY = %q after `display localhost:1`", d)
	}
	h.Display(verb.ScreenAction{Arg: "nonsense"})
	if d := os.Getenv("DISPLAY"); d != "localhost:1" {
		t.Fatalf("a refused value was set: DISPLAY = %q", d)
	}
	if !strings.Contains(x11Out(a), "nonsense") {
		t.Fatalf("the refusal does not name the value: %s", x11Out(a))
	}
}
