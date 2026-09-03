package verb

import (
	"fmt"
	"strings"
)

// checkDeclared runs the validation the DECLARATION expresses, before any
// hand-written Validate and before the Action is built.
//
// These five shapes covered 30 of the 39 hand-written rules when they were
// counted. Writing them as attributes rather than as code in each Build is
// what lets the generator emit the build at all -- and it makes the error
// wording uniform, where the hand-written ones each spelled the verb name
// again and could spell it wrong.
func (v VerbSpec) checkDeclared(b Bound) error {
	name := v.FlagSetName()

	for _, f := range v.Flags {
		if f.Required && !b.Set[f.Name] {
			return fmt.Errorf("%s: %s is required", name, dashList([]string{f.Name}))
		}
		if len(f.OneOf) == 0 || !b.Set[f.Name] {
			continue
		}
		got := b.Str(f.Name)
		ok := false
		for _, want := range f.OneOf {
			if got == want {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s: %s %q (want %s)", name, dashList([]string{f.Name}), got, strings.Join(f.OneOf, ", "))
		}
	}

	for i, a := range v.Args {
		if len(a.OneOfArg) == 0 || i >= len(b.Args) {
			continue
		}
		got, ok := b.Args[i], false
		for _, want := range a.OneOfArg {
			if got == want {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s: <%s> %q (want %s)", name, a.Name, got, strings.Join(a.OneOfArg, ", "))
		}
	}
	if v.Modes != nil {
		if named := v.namedIn(b, v.Modes.Names); len(named) > 1 {
			return fmt.Errorf("%s: %s are mutually exclusive", name, dashList(named))
		}
	}
	for _, r := range v.Exclusive {
		if named := v.namedIn(b, r.Flags); len(named) > 1 {
			return fmt.Errorf("%s: %s are mutually exclusive%s", name, v.nameList(named), why(r))
		}
	}
	for _, r := range v.ExactlyOne {
		if named := v.namedIn(b, r.Flags); len(named) != 1 {
			return fmt.Errorf("%s: pass exactly one of %s%s", name, v.nameList(r.Flags), why(r))
		}
	}
	for _, r := range v.AtLeastOne {
		if len(v.namedIn(b, r.Flags)) == 0 {
			return fmt.Errorf("%s: pass at least one of %s%s", name, v.nameList(r.Flags), why(r))
		}
	}
	for _, r := range v.Requires {
		if b.Set[r.Needs] {
			continue
		}
		// Every orphan at once: fixing them one per run is how a caller
		// discovers the second one only after correcting the first.
		if orphans := v.namedIn(b, r.Flags); len(orphans) > 0 {
			verb := "needs"
			if len(orphans) > 1 {
				verb = "need"
			}
			return fmt.Errorf("%s: %s %s %s%s", name, v.nameList(orphans), verb,
				v.nameList([]string{r.Needs}), why(Rule{Reason: r.Reason}))
		}
	}

	if v.Trailing != nil && v.Trailing.Required && b.Trail == "" {
		return fmt.Errorf("%s: %s is required\n%s", name, v.Trailing.Name, v.Usage())
	}
	return nil
}

// why renders a rule's reason, or nothing when it has none.
func why(r Rule) string {
	if r.Reason == "" {
		return ""
	}
	return " (" + r.Reason + ")"
}

// namedIn returns which of these the caller actually supplied. A name is
// looked up as a flag first and as a POSITIONAL second, so a rule can span
// both -- `prune` is a choice between naming ids and naming a cutoff, and
// only one of those is a flag.
func (v VerbSpec) namedIn(b Bound, group []string) []string {
	var out []string
	for _, n := range group {
		if b.Set[n] {
			out = append(out, n)
			continue
		}
		for _, a := range v.Args {
			if a.Name == n && len(b.Args) > 0 {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// nameList renders a rule's names the way the operator types them: a flag
// with its dashes, a POSITIONAL in angle brackets. A group may span both, and
// printing `--task-id` for a positional sends the reader looking for a flag
// that does not exist -- the same failure dashList's one-dash rule exists to
// avoid for -W.
func (v VerbSpec) nameList(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		positional := false
		for _, a := range v.Args {
			if a.Name == n {
				positional = true
				break
			}
		}
		if positional {
			out = append(out, "<"+n+">")
			continue
		}
		out = append(out, dashList([]string{n}))
	}
	return strings.Join(out, ", ")
}

// dashList renders flag names as the operator wrote them -- ONE dash for a
// single-letter flag, because `forward -W host:port` is how it is typed and an
// error naming `--W` sends the reader looking for a flag that does not exist.
func dashList(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if len(n) == 1 {
			out = append(out, "-"+n)
			continue
		}
		out = append(out, "--"+n)
	}
	return strings.Join(out, ", ")
}
