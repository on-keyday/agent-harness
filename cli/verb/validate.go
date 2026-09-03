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
			return fmt.Errorf("%s: --%s is required", name, f.Name)
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
			return fmt.Errorf("%s: --%s %q (want %s)", name, f.Name, got, strings.Join(f.OneOf, ", "))
		}
	}

	for _, group := range v.Exclusive {
		if named := b.namedIn(group); len(named) > 1 {
			return fmt.Errorf("%s: %s are mutually exclusive", name, dashList(named))
		}
	}
	for _, group := range v.ExactlyOne {
		named := b.namedIn(group)
		if len(named) != 1 {
			return fmt.Errorf("%s: pass exactly one of %s", name, dashList(group))
		}
	}
	for _, group := range v.AtLeastOne {
		if len(b.namedIn(group)) == 0 {
			return fmt.Errorf("%s: pass at least one of %s", name, dashList(group))
		}
	}
	for dependent, needed := range v.Requires {
		if b.Set[dependent] && !b.Set[needed] {
			return fmt.Errorf("%s: --%s needs --%s", name, dependent, needed)
		}
	}

	if v.Trailing != nil && v.Trailing.Required && b.Trail == "" {
		return fmt.Errorf("%s: %s is required\n%s", name, v.Trailing.Name, v.Usage())
	}
	return nil
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

// dashList renders flag names as the operator wrote them.
func dashList(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, "--"+n)
	}
	return strings.Join(out, ", ")
}
