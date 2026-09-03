package verb

import (
	"flag"
	"reflect"
	"sort"
	"testing"
)

// legacyFlagSets pairs a verb path with the FlagSet the pre-migration code
// built for it. Scaffolding with a defined end, and the end has arrived: the
// legacy parsers are deleted, so there is no longer a ground truth to add an
// entry FROM. What remains is one verb of the thirteen that declare aliases,
// and this file cannot grow past it.
//
// Read that as the scope, not as coverage. The case that motivated the whole
// alias rule -- `session send`'s --enter and -e being two flags, not a long
// form and its short form -- is pinned permanently and by behaviour in
// trailing_test.go, which is where to add the next one.
var legacyFlagSets = map[string]func() *flag.FlagSet{
	// cmd/harness-cli/main.go's prune, as it stood before the migration.
	"prune": func() *flag.FlagSet {
		fs := flag.NewFlagSet("prune", flag.ContinueOnError)
		fs.Duration("before", 0, "")
		force := fs.Bool("force", false, "")
		fs.BoolVar(force, "f", false, "")
		return fs
	},
}

// TestDeclaredAliasesMatchLegacyGrouping derives the ground truth for which
// spellings are one flag from the legacy FlagSet, by grouping names whose
// flag.Flag.Value is the same pointer. stdlib's newBoolValue is
// (*boolValue)(p) -- a pointer conversion, not an allocation -- so two
// bindings of one variable share a pointer while two independent fs.Bool calls
// do not.
//
// This exists because `-e` is NOT short for `--enter`: cmd/harness-cli's
// `session send` registers --enter (append a carriage return) and -e
// (interpret backslash escapes) as two independent flags, while `session new`
// binds --detach and -d to one variable and `git diff` does the same for
// --staged and --cached. Declaring the first pair as an alias would turn
// `session send -e '...'` into a spurious Enter typed into a live PTY -- and
// it would compile, review cleanly, and pass every existing test.
func TestDeclaredAliasesMatchLegacyGrouping(t *testing.T) {
	for _, v := range Verbs {
		build, have := legacyFlagSets[v.FlagSetName()]
		if !have {
			continue // not yet migrated, or its legacy parser is already gone
		}
		want := groupByValuePointer(build())
		got := groupByValuePointer(v.NewFlagSet(flag.ContinueOnError))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: alias grouping differs.\n declared: %v\n   legacy: %v\n"+
				"Two spellings belong together only when the legacy code bound them to ONE "+
				"variable. Sharing a prefix is not evidence: session send's --enter and -e "+
				"are different flags.", v.FlagSetName(), got, want)
		}
	}
}

// groupByValuePointer returns the name groups, each sorted, ordered by their
// first name, so two FlagSets compare regardless of registration order.
func groupByValuePointer(fs *flag.FlagSet) [][]string {
	byPtr := map[uintptr][]string{}
	fs.VisitAll(func(f *flag.Flag) {
		p := reflect.ValueOf(f.Value).Pointer()
		byPtr[p] = append(byPtr[p], f.Name)
	})
	out := make([][]string, 0, len(byPtr))
	for _, names := range byPtr {
		sort.Strings(names)
		out = append(out, names)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
