package tui

import (
	"sort"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// helpLocalVerbs are the cmdline verbs that live in this package rather than
// the declaration: `caps` / `scope` set the default a spawn carries when the
// command line does not name its own, and no other surface holds that state
// as a command (the WebUI holds it as chips), so the shared form is still
// being designed -- `caps set-defaults`.
//
// The list used to also carry clear / refresh / quit / help / trsf / diag /
// repo on the grounds that only this surface has them. That is what
// Surfaces: TUI says, and saying it in the table instead deleted this list's
// other half, a hand-written parse switch, and a hand-written action type per
// verb. (`ssh-gateway start|stop|status` left the same way.)
//
// Listed rather than skipped, because the help must still describe them: an
// operator reading it cannot tell which half of the cmdline a verb came from.
var helpLocalVerbs = []string{"caps", "scope"}

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
	// Head words AND full two-word paths. Matching the head alone is the
	// p.split(" ")[0] defect this work removed from the WebUI's startup
	// assertion and from the CLI's usage guard: `session frobnicate <id>`
	// passed on the strength of the word `session`.
	families := map[string]bool{}
	for _, p := range verb.PathsForSurface(verb.TUI) {
		fs := strings.Fields(p)
		reachable[fs[0]] = true
		if len(fs) >= 2 {
			families[fs[0]] = true
			reachable[fs[0]+" "+fs[1]] = true
		}
	}
	for _, l := range helpLocalVerbs {
		reachable[l] = true
	}
	// `caps` / `scope` take a bare mask and `ssh-gateway` a bare sub-verb, all
	// TUI session state with no declared path; `git`'s id sits between the
	// family word and the sub-verb.
	for _, sub := range []string{
		"caps set", "caps set-parent", "caps --on-resume", "scope subtree",
		"ssh-gateway start", "ssh-gateway stop", "ssh-gateway status",
	} {
		reachable[sub] = true
	}
	families["git"] = false
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
			continue
		}
		if len(first) < 2 || !families[w] {
			continue
		}
		for _, sub := range strings.Split(strings.Trim(first[1], "[]{}()"), "|") {
			sub = strings.Trim(sub, "[]{}()<>")
			// `<name>` and `[<name>]` are arguments, not sub-verbs.
			if sub == "" || strings.HasPrefix(sub, "-") || sub == strings.ToUpper(sub) ||
				strings.HasPrefix(first[1], "<") || strings.HasPrefix(first[1], "[<") {
				continue
			}
			if !reachable[w+" "+sub] && !reachable[sub] {
				t.Errorf("the help line %q names %q, and the cmdline has no such sub-verb", l, w+" "+sub)
			}
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

// TestEveryTuiVerbHasADescription is the control the help's own comment
// claims. Without it a path added to the table gets a synopsis line and NO
// description, and TestHelpDescribesEveryDeclaredVerb passes anyway --
// because a generated synopsis always mentions the verb. The completeness
// that used to be interesting (is it listed?) became free the moment the list
// was generated; what is left to check is whether the line SAYS anything.
func TestEveryTuiVerbHasADescription(t *testing.T) {
	for _, path := range verb.PathsForSurface(verb.TUI) {
		if strings.TrimSpace(tuiVerbHelp[path]) == "" {
			t.Errorf("`%s` is declared for the TUI and tuiVerbHelp says nothing about it.\n"+
				"The synopsis is generated, so the line appears either way -- and an "+
				"operator reading a bare synopsis learns the flags and not the point.", path)
		}
	}
}

// The other direction: an entry for a path this surface does not declare is a
// description of something nobody can type. It survived in the hand-written
// list as `ssh-gateway [start|stop|status]` long after the paths were split.
func TestTuiVerbHelpNamesNothingUndeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, p := range verb.PathsForSurface(verb.TUI) {
		declared[p] = true
	}
	for path := range tuiVerbHelp {
		if !declared[path] {
			t.Errorf("tuiVerbHelp describes %q, which the TUI does not declare", path)
		}
	}
}
