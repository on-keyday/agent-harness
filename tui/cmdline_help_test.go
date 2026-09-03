package tui

import (
	"sort"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// helpLocalVerbs are the cmdline verbs that live in this package rather than
// the declaration. Two kinds: screen-state operations another surface has no
// equivalent of (clear, refresh, trsf, diag), and this TUI's own session
// state -- `caps` / `scope` set the default a spawn carries when the command
// line does not name its own, and `repo` sets the default repo those spawns
// use. (`ssh-gateway start|stop|status` used to be here too; it is declared
// now, as three TUI-only paths beside the CLI's foreground form.)
//
// Listed rather than skipped, because the help must still describe them: an
// operator reading it cannot tell which half of the cmdline a verb came from.
var helpLocalVerbs = []string{
	"clear", "refresh", "quit", "help", "trsf", "diag",
	"caps", "scope", "repo",
}

// TestHelpDescribesEveryDeclaredVerb holds the `help` body to the declaration.
//
// The CLI's usage() drifted exactly this way and it took counting to notice:
// `caps set-parent`, `file edit`, `file new`, `forward tap`, `session resize`,
// the whole `session stream` namespace and four `agent` sub-verbs existed in
// code and were absent from the help. A help text does not merely describe
// invocations that fail -- it omits operations that work, and an operator
// reading it concludes they do not exist.
//
// The match is on the line's leading words, and a verb path may be
// interrupted: `git <task-id> log` describes `git log`. So a path counts as
// described when every word of it appears, in order, in one line's head.
func TestHelpDescribesEveryDeclaredVerb(t *testing.T) {
	lines := cmdlineHelpLines()
	if len(lines) == 0 {
		t.Fatal("the help body is empty")
	}

	var undescribed []string
	for _, path := range verb.PathsForSurface(verb.TUI) {
		if !describedBy(lines, strings.Fields(path)) {
			undescribed = append(undescribed, path)
		}
	}
	for _, local := range helpLocalVerbs {
		if !describedBy(lines, []string{local}) {
			undescribed = append(undescribed, local+" (TUI-local)")
		}
	}
	sort.Strings(undescribed)
	for _, p := range undescribed {
		t.Errorf("`%s` is reachable from the TUI cmdline and the help never describes it.\n"+
			"Add a line to cmdlineHelpLines. An operation missing from the help is one an "+
			"operator concludes does not exist.", p)
	}
}

// describedBy reports whether one help line's head contains every word of the
// verb path, in order. The words need not be adjacent: the task id sits in the
// middle of `git <task-id> diff`, and `exec ls [-task <id>] | exec kill <id>`
// describes two paths on one line.
func describedBy(lines []string, path []string) bool {
	for _, l := range lines {
		head := l
		if i := strings.Index(l, " - "); i >= 0 {
			head = l[:i]
		}
		words := strings.FieldsFunc(head, func(r rune) bool {
			return r == ' ' || r == '|' || r == '[' || r == ']' || r == '(' || r == ')'
		})
		i := 0
		for _, w := range words {
			if i < len(path) && w == path[i] {
				i++
			}
		}
		if i == len(path) {
			return true
		}
	}
	return false
}

// TestHelpNamesNothingUnreachable is the other direction: a help line whose
// head names a verb the cmdline cannot parse tells the operator to type
// something that errors. `ssh-gateway` was in the placeholder while its CLI
// form had moved to a separate path, which is the shape this catches.
func TestHelpNamesNothingUnreachable(t *testing.T) {
	reachable := map[string]bool{}
	for _, p := range verb.PathsForSurface(verb.TUI) {
		reachable[strings.Fields(p)[0]] = true
	}
	for _, l := range helpLocalVerbs {
		reachable[l] = true
	}
	// Aliases the cmdline accepts, plus the heads of the help's continuation
	// and key-hint lines, which describe a verb rather than naming one.
	for _, extra := range []string{
		"sync", "exit",
		"F", "picker", "push/pull", "--shell", "--sshd-parent",
		"submit|interactive|session", // one line covering the three spawn verbs
	} {
		reachable[extra] = true
	}

	for _, l := range cmdlineHelpLines() {
		head := strings.TrimSpace(l)
		if head == "" || strings.HasPrefix(head, "commands:") || strings.HasPrefix(head, "-") {
			continue
		}
		first := strings.Fields(head)
		if len(first) == 0 {
			continue
		}
		w := strings.Trim(first[0], "[]()|")
		if !reachable[w] {
			t.Errorf("the help line %q starts with %q, which the cmdline does not accept.\n"+
				"A help that names an unreachable verb is worse than one that omits a "+
				"reachable one: the operator types it and gets an error.", l, w)
		}
	}
}

// TestPlaceholderNamesNothingUnreachable applies the same one-way check to the
// cmdline's placeholder text.
//
// It is a width-limited SUMMARY, so it does not have to name every verb -- the
// help does that. What it must not do is name one the cmdline cannot parse:
// the placeholder is the first thing an operator reads, and a verb listed
// there is a promise.
func TestPlaceholderNamesNothingUnreachable(t *testing.T) {
	reachable := map[string]bool{}
	for _, p := range verb.PathsForSurface(verb.TUI) {
		reachable[strings.Fields(p)[0]] = true
	}
	for _, l := range helpLocalVerbs {
		reachable[l] = true
	}
	reachable["sync"], reachable["exit"] = true, true

	for _, part := range strings.Split(cmdlinePlaceholder, "/") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) == 0 {
			continue
		}
		if !reachable[f[0]] {
			t.Errorf("the placeholder names %q, which the cmdline does not accept", f[0])
		}
	}
}
