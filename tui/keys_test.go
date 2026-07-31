package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// TestMainKeysAreAllDocumented is the guard the keys.go split exists for:
// every key the main view binds must appear in mainKeyBindings, so it reaches
// the `?` popup. Adding a field to mainKeys without a table row fails here
// instead of silently producing an undiscoverable key.
func TestMainKeysAreAllDocumented(t *testing.T) {
	documented := map[string]bool{}
	for _, b := range mainKeyBindings {
		for _, k := range b.Keys {
			documented[k] = true
		}
	}
	v := reflect.ValueOf(mainKeys)
	for i := 0; i < v.NumField(); i++ {
		key := v.Field(i).String()
		if !documented[key] {
			t.Errorf("mainKeys.%s = %q has no row in mainKeyBindings (it would be missing from `?`)",
				v.Type().Field(i).Name, key)
		}
	}
}

// TestMainKeysAreUnique catches two actions claiming the same character —
// the dispatcher would run whichever guard comes first and the other binding
// would be dead.
func TestMainKeysAreUnique(t *testing.T) {
	seen := map[string]string{}
	v := reflect.ValueOf(mainKeys)
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		key := v.Field(i).String()
		if prev, dup := seen[key]; dup {
			t.Errorf("key %q bound twice: mainKeys.%s and mainKeys.%s", key, prev, name)
		}
		seen[key] = name
	}
}

// TestFooterHintsFitsWidth is the regression test for the overflow: the hint
// was a fixed ~270-char string while the layout budgets one row for it, so on
// any real terminal it wrapped and pushed the view off-screen.
//
// Measured under BOTH East-Asian width readings. `·`, `…` and the arrows are
// Ambiguous-width, so the same string is 125 cells to one terminal and 135 to
// another; a hint that only fits under the narrow reading still overflows a
// CJK terminal.
func TestFooterHintsFitsWidth(t *testing.T) {
	narrow := &runewidth.Condition{EastAsianWidth: false}
	wide := &runewidth.Condition{EastAsianWidth: true}
	for _, f := range []focus{focusRunners, focusTasks, focusLogs, focusNotify, focusCmdresult, focusCmdline} {
		for _, w := range []int{80, 100, 120, 137, 200, 400} {
			got := footerHints(f, w)
			if strings.Contains(got, "\n") {
				t.Errorf("focus=%d width=%d: hint contains a newline: %q", f, w, got)
			}
			for name, cond := range map[string]*runewidth.Condition{"narrow": narrow, "wide": wide} {
				if n := cond.StringWidth(got); n > w {
					t.Errorf("focus=%d width=%d: hint is %d cells wide under %s east-asian width: %q",
						f, w, n, name, got)
				}
			}
		}
	}
}

// TestFooterHintsKeepsEscapeHatch — whatever gets dropped, the way to see the
// rest (`?`) and the way out (`q`) must stay on screen.
func TestFooterHintsKeepsEscapeHatch(t *testing.T) {
	for _, f := range []focus{focusRunners, focusTasks, focusLogs, focusCmdline} {
		got := footerHints(f, 80)
		for _, want := range []string{mainKeys.Help + " keys", mainKeys.Quit + " quit"} {
			if !strings.Contains(got, want) {
				t.Errorf("focus=%d: hint %q is missing %q", f, got, want)
			}
		}
	}
}

// TestFooterHintsIsContextual — the pane-specific keys shown are the ones that
// actually do something in the focused pane.
func TestFooterHintsIsContextual(t *testing.T) {
	tasks := footerHints(focusTasks, 120)
	logs := footerHints(focusLogs, 120)

	if !strings.Contains(tasks, mainKeys.Cancel+" cancel") {
		t.Errorf("tasks focus should advertise cancel, got %q", tasks)
	}
	if strings.Contains(logs, mainKeys.Cancel+" cancel") {
		t.Errorf("logs focus must not advertise the tasks-only cancel key, got %q", logs)
	}
	if !strings.Contains(logs, mainKeys.LogFilter+" filter") {
		t.Errorf("logs focus should advertise the filter key, got %q", logs)
	}
	// The command line takes literal text, so no global single-letter key
	// applies there — the dispatcher guards them all with focus != cmdline.
	if cmd := footerHints(focusCmdline, 120); strings.Contains(cmd, mainKeys.Submit+" submit") {
		t.Errorf("cmdline focus must not advertise global letter keys, got %q", cmd)
	}
}

// TestKeyHelpBodyListsEveryBinding — the `?` popup is the complete list; the
// footer is allowed to drop entries only because this one never does.
func TestKeyHelpBodyListsEveryBinding(t *testing.T) {
	body := keyHelpBody()
	for _, b := range mainKeyBindings {
		if !strings.Contains(body, b.Long) {
			t.Errorf("keyHelpBody() is missing %q (keys %v)", b.Long, b.Keys)
		}
		for _, k := range b.Keys {
			if !strings.Contains(body, k) {
				t.Errorf("keyHelpBody() never mentions key %q", k)
			}
		}
	}
}

// TestViewFooterFitsTerminal complements TestViewHeightFitsTerminal: that one
// counts newlines, so a single over-long line (exactly the footer bug) is
// invisible to it — the terminal wraps it at display time, not in the string.
//
// Scoped to the footer row; TestViewFitsTerminalWidth covers the whole frame.
func TestViewFooterFitsTerminal(t *testing.T) {
	for _, w := range []int{80, 100, 160} {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			a := New(Config{Server: "ws://test:8080", DefaultRepo: ""})
			a.width = w
			a.height = 40
			a.layout()

			lines := strings.Split(a.View(), "\n")
			footer := lines[len(lines)-1]
			if n := lipgloss.Width(footer); n > w {
				t.Errorf("footer is %d cells wide (terminal is %d): %q", n, w, footer)
			}
		})
	}
}
