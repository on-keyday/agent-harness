package main

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// usageText is everything `harness-cli` prints when it is asked what it can
// do: the top-level list plus the four sub-usages it defers to.
func usageText() string {
	var b bytes.Buffer
	usageTo(&b)
	// The sub-usages describe `harness-cli <ns> <sub>` and print only the sub
	// half, so the namespace is prefixed back on. Without it every `agent *`
	// path read as undescribed while agentUsage names all twelve.
	for ns, fn := range map[string]func(io.Writer){
		"server": serverUsageTo, "board": boardUsageTo,
		"agent": agentUsageTo, "workspace": workspaceUsageTo,
	} {
		var sub bytes.Buffer
		fn(&sub)
		for _, l := range strings.Split(sub.String(), "\n") {
			b.WriteString("  " + ns + " " + strings.TrimSpace(l) + "\n")
		}
	}
	return b.String()
}

// TestUsageDescribesEveryDeclaredCliVerb is the CLI's half of the guard the
// TUI has had since tui/cmdline_help_test.go.
//
// The spec's Problem section MEASURED this defect and it was never fixed:
// `caps set-parent`, `file edit`, `file new`, `forward tap`, `session resize`,
// the whole `session stream` namespace and four `agent` sub-verbs existed in
// code and were absent from usage(). A help text does not merely describe
// invocations that fail -- it omits operations that work, and an operator
// reading it concludes they do not exist.
//
// D12 says usage is generated from the declaration. The prose here is worth
// keeping -- it explains what --caps means on a resume, which no generator
// knows -- so what is mechanical is the COVERAGE: every declared CLI path
// must appear, and this is what says so.
func TestUsageDescribesEveryDeclaredCliVerb(t *testing.T) {
	lines := strings.Split(usageText(), "\n")
	var undescribed []string
	for _, path := range verb.PathsForSurface(verb.CLI) {
		if !usageDescribes(lines, strings.Fields(path)) {
			undescribed = append(undescribed, path)
		}
	}
	sort.Strings(undescribed)
	for _, p := range undescribed {
		t.Errorf("`harness-cli %s` works and usage() never names it.\n"+
			"Add a line. An operation missing from the help is one an operator "+
			"concludes does not exist.", p)
	}
}

// usageDescribes reports whether one line's head contains every word of the
// path, in order. The words need not be adjacent: `git <task-id> log`
// describes `git log`, and one line can cover several sub-verbs.
func usageDescribes(lines []string, path []string) bool {
	for _, l := range lines {
		head := l
		if i := strings.Index(l, "  "); i > 2 {
			head = l[:i]
		}
		words := strings.FieldsFunc(head, func(r rune) bool {
			return r == ' ' || r == '|' || r == '[' || r == ']' || r == '(' || r == ')' || r == '{' || r == '}'
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

// TestUsageNamesNothingUnreachable is the other direction, and the more
// dangerous one: the `board purge` incident was a usage line the parser
// REFUSED. An operator types what the help prints.
//
// The check is on the first word of each subcommand line, which is what a
// reader takes as the verb. `file pull` gained --offset and --length in the
// migration and usage() documented neither -- absence the test above catches;
// a line naming something that does not parse is what this one catches.
func TestUsageNamesNothingUnreachable(t *testing.T) {
	reachable := map[string]bool{}
	for _, p := range verb.PathsForSurface(verb.CLI) {
		reachable[strings.Fields(p)[0]] = true
	}
	// Process-global options and the namespace headings, which are not verbs.
	for _, extra := range []string{
		"harness-cli", "usage:", "Global", "Subcommands:", "Env-primary", "HARNESS_AUTH_TICKET",
		"server", "board", "agent", "workspace", "--server-cid", "--ws-path", "--config", "--workspace",
	} {
		reachable[extra] = true
	}
	for _, l := range strings.Split(usageText(), "\n") {
		if !strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "    ") {
			continue // continuation lines describe, they do not name
		}
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		w := strings.Trim(f[0], "[]()|<>")
		if w == "" || strings.HasPrefix(w, "-") {
			continue
		}
		if !reachable[w] {
			t.Errorf("usage() prints a line starting with %q, which harness-cli does not accept:\n  %s\n"+
				"A help that names an unreachable verb is worse than one that omits a "+
				"reachable one: the operator types it and gets an error.", w, strings.TrimSpace(l))
		}
	}
}
