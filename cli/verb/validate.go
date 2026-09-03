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

	if v.Modes != nil {
		if named := b.namedIn(v.Modes.Names); len(named) > 1 {
			return fmt.Errorf("%s: %s are mutually exclusive", name, dashList(named))
		}
	}
	for _, r := range v.Exclusive {
		if named := b.namedIn(r.Flags); len(named) > 1 {
			return fmt.Errorf("%s: %s are mutually exclusive%s", name, dashList(named), why(r))
		}
	}
	for _, r := range v.ExactlyOne {
		if named := b.namedIn(r.Flags); len(named) != 1 {
			return fmt.Errorf("%s: pass exactly one of %s%s", name, dashList(r.Flags), why(r))
		}
	}
	for _, r := range v.AtLeastOne {
		if len(b.namedIn(r.Flags)) == 0 {
			return fmt.Errorf("%s: pass at least one of %s%s", name, dashList(r.Flags), why(r))
		}
	}
	for _, r := range v.Requires {
		if b.Set[r.Needs] {
			continue
		}
		// Every orphan at once: fixing them one per run is how a caller
		// discovers the second one only after correcting the first.
		if orphans := b.namedIn(r.Flags); len(orphans) > 0 {
			verb := "needs"
			if len(orphans) > 1 {
				verb = "need"
			}
			return fmt.Errorf("%s: %s %s %s%s", name, dashList(orphans), verb,
				dashList([]string{r.Needs}), why(Rule{Reason: r.Reason}))
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

// namedIn returns which of these flags the caller actually supplied.
func (b Bound) namedIn(group []string) []string {
	var out []string
	for _, n := range group {
		if b.Set[n] {
			out = append(out, n)
		}
	}
	return out
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
