package tui

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// Every key this TUI binds is declared in this file, once. The dispatchers in
// app.go compare against these fields instead of inline string literals, and
// the footer hint line plus the `?` popup both render from mainKeyBindings —
// so a key cannot exist in the dispatcher while being missing from the help,
// which is exactly the drift that kept happening when the literals were
// scattered across a 400-line key switch.
//
// mainKeys is a struct rather than a const block on purpose: keys_test.go
// enumerates its fields with reflect and fails when a field has no matching
// row in mainKeyBindings. A const block cannot be enumerated, so the guard
// would not exist.

// mainKeyMap holds the keys the main view (no modal open) dispatches on.
type mainKeyMap struct {
	Quit            string
	Help            string
	Submit          string
	Session         string
	Interactive     string
	Detail          string
	Grid            string
	GridSubtree     string
	GridDescendants string
	Conns           string
	Board           string
	Tree            string
	Forwards        string
	Execs           string
	FilePicker      string
	Git             string
	LogFilter       string

	Cancel                 string
	ReGrant                string
	SetParent              string
	ResumeAssignedContinue string
	ResumeAssignedFresh    string
	ResumeAnyContinue      string
	ResumeAnyFresh         string
	ViewOnly               string
	AwaitIdle              string
	AwaitIdleNotify        string
	ForwardLocal           string
	ForwardLocalStop       string
	ForwardRemote          string
	ForwardRemoteStop      string
	RawConnect             string
}

var mainKeys = mainKeyMap{
	Quit:            "q",
	Help:            "?",
	Submit:          "s",
	Session:         "S",
	Interactive:     "i",
	Detail:          "d",
	Grid:            "g",
	GridSubtree:     "z",
	GridDescendants: "Z",
	Conns:           "C",
	Board:           "O",
	Tree:            "T",
	Forwards:        "f",
	Execs:           "e",
	FilePicker:      "F",
	Git:             "G",
	LogFilter:       "/",

	Cancel:                 "c",
	ReGrant:                "a",
	SetParent:              "A",
	ResumeAssignedContinue: "r",
	ResumeAssignedFresh:    "R",
	ResumeAnyContinue:      "u",
	ResumeAnyFresh:         "U",
	ViewOnly:               "v",
	AwaitIdle:              "w",
	AwaitIdleNotify:        "W",
	ForwardLocal:           "p",
	ForwardLocalStop:       "P",
	ForwardRemote:          "b",
	ForwardRemoteStop:      "B",
	RawConnect:             "t",
}

// modalKeyMap holds keys dispatched by app.go while a full-screen overlay is
// open. They are deliberately separate from mainKeys: the same character can
// mean different things in a modal (`r` refreshes the board, but resumes a
// task in the main view), and modals render their own footers.
// A modal key must not collide with the viewport's own scroll bindings
// (pgup/pgdown, space, f, b, u, d, j, k, h, l and the ctrl forms). The App
// intercepts these before the viewport ever sees them, so a collision does not
// merely shadow scrolling — it silently runs the modal's action instead.
// `b` used to be set-base and `u` up-one-repo; paging up in a long diff
// therefore moved the baseline, and half-paging up left the nested repository.
// git_keys_test.go asserts the two sets stay disjoint.
type modalKeyMap struct {
	ConfirmYes       string
	ConfirmYesUpper  string
	ConfirmNo        string
	ConfirmNoUpper   string
	Escape           string
	ForwardKill      string
	ForwardTap       string
	BoardRefresh     string
	BoardPurgeTopic  string
	BoardPurgeMsg    string
	BoardRetractMsg  string
	BoardSubscribers string
	GitSetBase       string
	GitStatus        string
	GitNextFile      string
	GitPrevFile      string
	GitSubmodule     string
	GitUp            string
	GitOpenFile      string
}

var modalKeys = modalKeyMap{
	ConfirmYes:      "y",
	ConfirmYesUpper: "Y",
	ConfirmNo:       "n",
	ConfirmNoUpper:  "N",
	Escape:          "esc",
	ForwardKill:     "x",
	// `t` rather than a shift-variant of the kill key: tapping is a read, not
	// a stronger form of killing, and the two should not look like a pair.
	ForwardTap:      "t",
	BoardRefresh:    "r",
	BoardPurgeTopic: "x",
	BoardPurgeMsg:   "X",
	// Withdraw, not destroy: its own letter rather than a shift-variant of the
	// purge keys, because the two differ in what survives, not in how much of
	// the topic they take.
	BoardRetractMsg:  "w",
	BoardSubscribers: "s",
	GitSetBase:       "B",
	GitStatus:        "s",
	GitNextFile:      "n",
	GitPrevFile:      "N",
	GitSubmodule:     "m",
	GitUp:            "backspace",
	GitOpenFile:      "o",
}

// keyScope is a bitmask of the panes a main-view binding applies to. The
// dispatcher's focus guards are the authority; these bits mirror them so the
// footer only advertises keys that actually do something right now.
type keyScope uint8

const (
	scopeRunners keyScope = 1 << iota
	scopeTasks
	scopeLogs
	scopeNotify
	scopeCmdresult
	scopeCmdline
)

const (
	scopeAllPanes = scopeRunners | scopeTasks | scopeLogs | scopeNotify | scopeCmdresult | scopeCmdline
	// scopeGlobal matches the dispatcher's `a.focus != focusCmdline` guard:
	// these keys work from any pane except the command line, where they must
	// stay typeable text.
	scopeGlobal = scopeAllPanes &^ scopeCmdline
)

// keyBinding is one row of help. Keys carries the exact key values so
// keys_test.go can prove every declared key is documented; Short is the
// footer wording and Long the `?` popup wording.
type keyBinding struct {
	Keys  []string
	Scope keyScope
	Short string
	Long  string
}

// isGlobal reports whether the binding applies to every pane. The footer
// lists pane-specific bindings first, so the keys that only work right here
// survive truncation on a narrow terminal.
func (b keyBinding) isGlobal() bool { return b.Scope == scopeGlobal }

// mainKeyBindings is the help table. Order within a scope is the order the
// footer uses. mainKeys.Quit / mainKeys.Help are pinned to the footer's tail
// by footerHints and so carry no Short text here.
var mainKeyBindings = []keyBinding{
	// --- tasks pane ---
	{Keys: []string{"enter"}, Scope: scopeTasks, Short: "enter follow", Long: "follow the selected task's log"},
	{Keys: []string{mainKeys.ResumeAssignedContinue, mainKeys.ResumeAssignedFresh}, Scope: scopeTasks,
		Short: "r/R assigned resume", Long: "reattach / resume on the assigned runner (r keeps the agent's conversation, R starts fresh). On a live EVENT-STREAM task it opens the chat view instead — that kind has no terminal to take over, but it is driven from there"},
	{Keys: []string{mainKeys.ResumeAnyContinue, mainKeys.ResumeAnyFresh}, Scope: scopeTasks,
		Short: "u/U any resume", Long: "same as r/R but unpinned — any runner may take it"},
	{Keys: []string{mainKeys.ViewOnly}, Scope: scopeTasks, Short: "v view-only", Long: "attach read-only (no input forwarded)"},
	{Keys: []string{mainKeys.Cancel}, Scope: scopeTasks, Short: "c cancel", Long: "cancel the selected task"},
	{Keys: []string{mainKeys.ReGrant}, Scope: scopeTasks, Short: "a re-grant", Long: "open the re-grant picker for the selected task's caps/scope (operator-only)"},
	{Keys: []string{mainKeys.SetParent}, Scope: scopeTasks, Short: "A set parent", Long: "re-point the selected task's parent link (root / swap / another task; operator-only)"},
	{Keys: []string{mainKeys.AwaitIdle, mainKeys.AwaitIdleNotify}, Scope: scopeTasks,
		Short: "w/W await-idle", Long: "arm a one-shot idle watcher (W also notifies the operator)"},
	{Keys: []string{mainKeys.FilePicker}, Scope: scopeTasks, Short: "F files", Long: "open the file picker on the selected task's worktree"},
	{Keys: []string{mainKeys.Git}, Scope: scopeTasks, Short: "G git", Long: "browse the selected task's git state — commit log, diff, status — read-only, without touching its shell"},
	{Keys: []string{mainKeys.ForwardLocal, mainKeys.ForwardLocalStop}, Scope: scopeTasks,
		Short: "p/P L-forward", Long: "start / stop a local port forward for the selected task"},
	{Keys: []string{mainKeys.ForwardRemote, mainKeys.ForwardRemoteStop}, Scope: scopeTasks,
		Short: "b/B R-forward", Long: "start / stop a remote port forward for the selected task"},
	{Keys: []string{mainKeys.RawConnect}, Scope: scopeTasks, Short: "t raw connect", Long: "raw-connect to a forwarded port"},
	{Keys: []string{mainKeys.GridSubtree, mainKeys.GridDescendants}, Scope: scopeTasks, Short: "z/Z subtree grid",
		Long: "grid of the selected task's subtree — z includes the task itself, Z is its descendants only (for when you are watching that one elsewhere); g is the whole fleet"},

	// --- logs pane ---
	{Keys: []string{"left", "right"}, Scope: scopeLogs, Short: "←/→ scroll", Long: "scroll the log horizontally"},
	{Keys: []string{mainKeys.LogFilter}, Scope: scopeLogs, Short: "/ filter", Long: "filter the log by substring (enter applies, esc cancels/clears)"},

	// --- cmdline ---
	{Keys: []string{"up", "down"}, Scope: scopeCmdline, Short: "↑/↓ history", Long: "walk the command history"},
	{Keys: []string{"enter"}, Scope: scopeCmdline, Short: "enter run", Long: "run the typed command"},

	// --- runners + tasks ---
	{Keys: []string{mainKeys.Detail}, Scope: scopeRunners | scopeTasks, Short: "d detail", Long: "detail popup for the selected runner / task"},

	// --- global (every pane but the command line) ---
	{Keys: []string{"tab", "shift+tab"}, Scope: scopeGlobal, Short: "tab focus", Long: "cycle focus between panes (shift-tab reverses)"},
	{Keys: []string{mainKeys.Submit}, Scope: scopeGlobal, Short: "s submit", Long: "open the submit popup (one-shot task)"},
	{Keys: []string{mainKeys.Session}, Scope: scopeGlobal, Short: "S session", Long: "open a new detachable session"},
	{Keys: []string{mainKeys.Interactive}, Scope: scopeGlobal, Short: "i interactive", Long: "open an interactive session in the default repo"},
	{Keys: []string{mainKeys.Grid}, Scope: scopeGlobal, Short: "g grid", Long: "live session viewer grid"},
	{Keys: []string{mainKeys.Conns}, Scope: scopeGlobal, Short: "C conns", Long: "connections view"},
	{Keys: []string{mainKeys.Board}, Scope: scopeGlobal, Short: "O board", Long: "agentboard topics view"},
	{Keys: []string{mainKeys.Tree}, Scope: scopeGlobal, Short: "T tree", Long: "toggle the task list between flat and creator-tree order"},
	{Keys: []string{mainKeys.Forwards}, Scope: scopeGlobal, Short: "f forwards", Long: "port-forward list (x kills the selected row, t taps its traffic)"},
	{Keys: []string{mainKeys.Execs}, Scope: scopeGlobal, Short: "e execs", Long: "running-exec list (x kills the selected row)"},
	{Keys: []string{mainKeys.Help}, Scope: scopeGlobal, Long: "this key list"},
	{Keys: []string{mainKeys.Quit}, Scope: scopeGlobal, Long: "quit"},
}

// scopeForFocus maps the focused pane to its scope bit.
func scopeForFocus(f focus) keyScope {
	switch f {
	case focusRunners:
		return scopeRunners
	case focusTasks:
		return scopeTasks
	case focusLogs:
		return scopeLogs
	case focusNotify:
		return scopeNotify
	case focusCmdresult:
		return scopeCmdresult
	case focusCmdline:
		return scopeCmdline
	}
	return 0
}

const hintSep = " · "

// footerWidth measures the hint conservatively. The separator `·` (U+00B7),
// the `…` marker and the arrow glyphs are all East-Asian *Ambiguous*: a CJK
// terminal draws them 2 cells wide, while go-runewidth's locale sniffing may
// report 1 (or vice versa — the TUI process and the terminal need not agree
// on the locale at all). Budgeting at the wider reading means the line can
// only ever come out narrower than the terminal, never wider, which is the
// failure being fixed here. ~10 ambiguous runes per hint, so the cost of
// being wrong in the safe direction is ~10 unused cells.
var footerWidth = &runewidth.Condition{EastAsianWidth: true}

// footerHints renders the one-line hint for the focused pane, never wider
// than width cells. The layout budgets exactly one row for the footer
// (App.layout), so overflowing it wraps and pushes the whole view off-screen
// — hence the hard clamp rather than a best-effort join.
//
// Ordering is deliberate: pane-specific bindings first, then global ones,
// with `? keys · q quit` pinned to the tail. What gets dropped on a narrow
// terminal is therefore the part `?` can still show, and the escape hatch is
// always visible. A `…` marks that something was dropped.
func footerHints(f focus, width int) string {
	scope := scopeForFocus(f)
	tail := []string{mainKeys.Help + " keys", mainKeys.Quit + " quit"}
	tailWidth := footerWidth.StringWidth(strings.Join(tail, hintSep))
	if width <= tailWidth {
		return clipLine(strings.Join(tail, hintSep), 0, width)
	}

	var picked []string
	budget := width - tailWidth
	truncated := false
	for _, pass := range []bool{false, true} { // pane-specific first, then global
		for _, b := range mainKeyBindings {
			if b.Short == "" || b.Scope&scope == 0 || b.isGlobal() != pass {
				continue
			}
			cost := footerWidth.StringWidth(b.Short) + footerWidth.StringWidth(hintSep)
			if cost > budget {
				truncated = true
				continue
			}
			picked = append(picked, b.Short)
			budget -= cost
		}
	}
	if truncated {
		const ell = "…"
		need := footerWidth.StringWidth(ell) + footerWidth.StringWidth(hintSep)
		for budget < need && len(picked) > 0 {
			last := picked[len(picked)-1]
			budget += footerWidth.StringWidth(last) + footerWidth.StringWidth(hintSep)
			picked = picked[:len(picked)-1]
		}
		if budget >= need {
			picked = append(picked, ell)
		}
	}
	return strings.Join(append(picked, tail...), hintSep)
}

// keyHelpBody renders the full key list for the `?` popup — every binding,
// grouped by where it applies. The footer shows a width-limited subset; this
// is the complete one, so nothing is ever merely hidden.
func keyHelpBody() string {
	groups := []struct {
		title string
		scope keyScope
		only  bool // list bindings whose scope is exactly this group's
	}{
		{title: "global ", scope: scopeGlobal},
		{title: "runners", scope: scopeRunners, only: true},
		{title: "tasks  ", scope: scopeTasks, only: true},
		{title: "logs   ", scope: scopeLogs, only: true},
		{title: "cmdline", scope: scopeCmdline, only: true},
	}
	var sb strings.Builder
	for _, g := range groups {
		first := true
		for _, b := range mainKeyBindings {
			switch {
			case g.only && b.isGlobal():
				continue
			case g.only && b.Scope&g.scope == 0:
				continue
			case !g.only && !b.isGlobal():
				continue
			}
			label := g.title
			if !first {
				label = strings.Repeat(" ", runewidth.StringWidth(g.title))
			}
			first = false
			sb.WriteString(label + "  " + runewidth.FillRight(strings.Join(b.Keys, ", "), 18) + b.Long + "\n")
		}
		if !first {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("modals   esc closes · forwards: x kill (y/n confirms) · board: r reload, s subscribers, x purge topic, X purge message, w retract message")
	return sb.String()
}
