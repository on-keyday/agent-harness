package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// -h on the cmdline is a request that was ANSWERED. It arrived here as an
// error and was rendered in ErrorStyle behind an "error: " prefix, so the
// operator who asked what a verb's flags do was told they had made a mistake —
// and the answer was one long line the panel folded at the left margin.
//
// Driven through Update rather than ParseCommand alone: the parse has returned
// *verb.HelpRequested all along, and the defect was entirely in what this
// surface did with it.
func TestCmdlineHelpIsOutputNotAnError(t *testing.T) {
	a := New(Config{})
	a.focus = focusCmdline
	// A verb this surface actually declares: `session send` is CLI-only, and a
	// help test that names an unreachable verb tests the "unknown sub-verb"
	// path instead of the one it means to.
	a.cmdline.SetValue("file push -h")
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)

	got := strings.Join(a.cmdresult.lines, "\n")
	if strings.Contains(got, "error:") {
		t.Errorf("-h must not be rendered as an error:\n%s", got)
	}
	for _, want := range []string{"flags:", "--route", "--recursive, -r"} {
		if !strings.Contains(got, want) {
			t.Errorf("cmdresult does not mention %q:\n%s", want, got)
		}
	}
}

// A real parse error is still an error. The branch above must not swallow the
// case it was added next to.
func TestCmdlineParseErrorStaysAnError(t *testing.T) {
	a := New(Config{})
	a.focus = focusCmdline
	a.cmdline.SetValue("file push --zzz-no-such-flag a b c")
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)

	if got := strings.Join(a.cmdresult.lines, "\n"); !strings.Contains(got, "error:") {
		t.Errorf("an undeclared flag must still report an error, got:\n%s", got)
	}
}

// The panel's width is what the block wraps to, so a description stays inside
// the panel instead of being folded by it. The CLI asks its terminal; this asks
// the viewport it will be printed into.
func TestCmdlineHelpWrapsToThePanel(t *testing.T) {
	a := New(Config{})
	a.cmdresult.SetSize(70, 20)
	a.focus = focusCmdline
	a.cmdline.SetValue("file push -h")
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)

	for _, l := range a.cmdresult.lines[1:] { // [0] is the echoed input
		if strings.HasPrefix(l, "file push [") {
			continue // the synopsis is a grammar and is not folded
		}
		if len(l) > 70 {
			t.Errorf("line wider than the panel (70):\n%q", l)
		}
	}
}

// ParseCommand keeps returning the typed error the surfaces branch on. Stated
// separately because the two tests above would still pass if it degraded to a
// plain error carrying the same text.
func TestParseCommandReturnsTypedHelp(t *testing.T) {
	_, err := ParseCommand("file push -h", "")
	var help *verb.HelpRequested
	if !errors.As(err, &help) {
		t.Fatalf("want *verb.HelpRequested, got %T (%v)", err, err)
	}
}
