package verb

import (
	"flag"
	"reflect"
	"sort"
)

// TestDeclaredAliasesMatchLegacyGrouping used to live here: it compared the
// alias groups each verb declares against the FlagSet the pre-migration code
// built, grouping names whose flag.Flag.Value is the same pointer. Its last
// subject was prune's --force / -f, and prune lost --force (2026-11-09: the
// server never forgets a live task), so the comparison had nothing left to
// check and was removed. The case that motivated the alias rule -- `session
// send`'s --enter and -e being two flags, not a long form and its short form
// -- is pinned by behaviour in trailing_test.go, which still uses the helper
// below.

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
