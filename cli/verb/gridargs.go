package verb

import (
	"fmt"
	"strings"
)

// GridArgsUsage is the one-line usage shared by every surface that accepts the
// grid argument grammar.
const GridArgsUsage = "grid: usage: grid [<task-id>...] | grid --under <task-id> [--descendants]"

// gridMode is the grid verb's scope mode, which falls out of --under,
// --descendants and the positionals together rather than out of any one of
// them.
//
// Rebuilt into ParseGridArgs's own argument form rather than duplicated: that
// function is what `workspace save` validates a stored selection against, and
// one grammar means one place that decides. It is also where --descendants
// without --under and --under WITH ids are refused, so this returns their
// errors rather than restating them.
func gridMode(b Bound) (GridScopeMode, error) {
	args := make([]string, 0, len(b.Args)+3)
	if u := b.Str("under"); u != "" {
		args = append(args, "--under", u)
	}
	if b.Bool("descendants") {
		args = append(args, "--descendants")
	}
	args = append(args, b.Args...)
	mode, _, _, err := ParseGridArgs(args)
	return mode, err
}

// ParseGridArgs parses the `grid` command's arguments into the three values
// GridSet consumes. It lives here rather than in the TUI because the workspace
// config accepts the same grammar and must not carry a second copy of it: a
// mirror has no way to fail loudly when the grammar grows.
func ParseGridArgs(args []string) (GridScopeMode, string, []string, error) {
	mode, anchor, descendants := GridAll, "", false
	var ids []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--descendants":
			descendants = true
		case "--under":
			if i+1 >= len(args) {
				return mode, "", nil, fmt.Errorf("grid: --under needs a task id")
			}
			i++
			anchor = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return mode, "", nil, fmt.Errorf("grid: unknown flag %q\n%s", args[i], GridArgsUsage)
			}
			ids = append(ids, args[i])
		}
	}

	switch {
	case anchor != "":
		if len(ids) > 0 {
			return mode, "", nil, fmt.Errorf("grid: --under names one subtree; drop the extra ids\n%s", GridArgsUsage)
		}
		mode = GridSubtree
		if descendants {
			mode = GridDescendants
		}
	case descendants:
		return mode, "", nil, fmt.Errorf("grid: --descendants needs --under <task-id> to take the descendants OF\n%s", GridArgsUsage)
	case len(ids) > 0:
		mode = GridIds
	}
	return mode, anchor, ids, nil
}

// GridArgsString renders a grid selection back into the argument string
// ParseGridArgs accepts. It is the other half of one grammar: `workspace save`
// writes what an operator could have typed, and the round trip is pinned by
// TestGridArgsRoundTrip.
func GridArgsString(mode GridScopeMode, anchor string, ids []string) string {
	switch mode {
	case GridSubtree:
		return "--under " + anchor
	case GridDescendants:
		return "--under " + anchor + " --descendants"
	case GridIds:
		return strings.Join(ids, " ")
	}
	return ""
}
