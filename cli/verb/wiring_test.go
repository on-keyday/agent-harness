package verb

import (
	"flag"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// The declaration and the Action it builds are two hand-written halves wired
// by a hand-written Build. That is the seam this file watches.
//
// Two ways they drift apart, both silent:
//
//   - A flag is declared and Build never reads it. The option parses, the
//     operator sees no error, and nothing happens.
//   - Build reads a name the declaration does not have. Bound.Str and
//     Bound.Bool are map lookups whose comma-ok is discarded, so a renamed or
//     mistyped flag yields "" / false rather than a failure.
// A third shape -- an Action field no Build ever fills -- is NOT checked here.
// The probe for it could not tell "nothing sets this" from "the all-flags form
// was refused by a cross-flag rule", and reported 30 fields, every one noise.
// It is the shape a generator removes rather than one a test should chase.
//
// Neither of the two is a compile error, and the differential tests cannot see
// them either: they compare one Build against the legacy parser it replaced,
// so a flag both sides ignore agrees perfectly.

// TestEveryDeclaredFlagIsReadByItsBuild parses a line that sets each flag to a
// non-zero value and checks the built Action changed. A flag whose value never
// reaches the Action is one the operator can type to no effect.
func TestEveryDeclaredFlagIsReadByItsBuild(t *testing.T) {
	for _, v := range Verbs {
		for _, f := range v.Flags {
			if _, exempt := flagNotInAction[v.FlagSetName()+"."+f.Name]; exempt {
				continue
			}
			// On the surface the FLAG is declared for, not the verb's first.
			// `ls --filtered` exists only in a browser, and building it with
			// the CLI's build reads as a flag nobody carries -- which is the
			// correct answer for the CLI and the wrong question to ask there.
			for _, sf := range []Surface{CLI, TUI, WebUI} {
				if !v.Surfaces.Has(sf) || (f.Surfaces != 0 && !f.Surfaces.Has(sf)) {
					continue
				}
				nv := v.For(sf)
				base, ok := buildWith(t, nv, nil)
				if !ok {
					continue // the verb needs positionals; covered by the arity form below
				}
				with, ok := buildWith(t, nv, &f)
				if !ok {
					continue
				}
				if reflect.DeepEqual(base, with) {
					t.Errorf("%s (%v): --%s is declared but its value never reaches the Action.\n"+
						"Setting it changes nothing the build produces, so an operator can type it "+
						"and get silence. Either give it a Field, or add %q to flagNotInAction "+
						"with the reason.",
						v.FlagSetName(), sf, f.Name, v.FlagSetName()+"."+f.Name)
				}
			}
		}
	}
}

// flagNotInAction names flags whose effect is deliberately not visible in the
// built Action, with the reason.
var flagNotInAction = map[string]string{
	// --hex IS the default render mode, so naming it produces the same Action
	// as leaving it out. It exists to be writable, not to change anything.
	"forward tap.hex": "hexdump is the default mode; --hex names what already happens",
}

// buildWith parses a synthesised command line for v -- the required
// positionals, plus one flag set to a non-zero value when f is non-nil -- and
// returns the built Action. ok is false when the verb cannot be synthesised
// (free-form trailing text, custom values), which the other tests cover.
func buildWith(t *testing.T, v VerbSpec, f *Flag) (any, bool) {
	t.Helper()
	var args []string
	if f != nil {
		if f.Custom != nil {
			return nil, false // a custom value's shape is verb-specific
		}
		switch f.Type {
		case FlagBool:
			if d, _ := f.Default.(bool); d {
				return nil, false // a bool defaulting true cannot be raised
			}
			args = append(args, "--"+f.Name)
		case FlagString:
			args = append(args, "--"+f.Name, "probe-value")
		case FlagUint, FlagUint64:
			args = append(args, "--"+f.Name, "7")
		case FlagDuration:
			args = append(args, "--"+f.Name, "3m")
		}
	}
	args = append(args, positionalsFor(v)...)
	if v.Trailing != nil {
		args = append(args, "trailing", "words")
	}

	fs := v.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := v.Parse(fs, args)
	if err != nil {
		return nil, false
	}
	act, err := v.BuildFunc()(b)
	if err != nil {
		return nil, false
	}
	return act, true
}

// positionalsFor synthesises one acceptable value per required positional.
func positionalsFor(v VerbSpec) []string {
	var out []string
	for _, a := range v.Args {
		if a.Variadic {
			continue
		}
		switch a.Type {
		case ArgTaskID:
			out = append(out, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		case ArgUint:
			out = append(out, "1")
		case ArgTopic:
			out = append(out, "chat.abcd1234")
		default:
			out = append(out, "probe-arg")
		}
	}
	return out
}

// TestBuildReadsNoUndeclaredFlagName is the other direction: a Build that
// looks up a name the verb does not declare gets a zero value with no error,
// so a rename in the table leaves the Build silently reading nothing.
//
// Checked by parsing with EVERY declared flag set to a distinctive value and
// then asserting the built Action carries no zero-valued string field that a
// declared flag should have filled. It is coarse -- it cannot see a lookup
// whose result is discarded -- so the exact-name check lives in
// TestBuildLookupNamesAreDeclared, which reads the source.
func TestBuildReadsNoUndeclaredFlagName(t *testing.T) {
	for _, v := range Verbs {
		if v.Trailing != nil {
			continue
		}
		var args []string
		for _, f := range v.Flags {
			if f.Custom != nil {
				continue
			}
			switch f.Type {
			case FlagBool:
				if d, _ := f.Default.(bool); !d {
					args = append(args, "--"+f.Name)
				}
			case FlagString:
				args = append(args, "--"+f.Name, "probe-"+f.Name)
			case FlagUint, FlagUint64:
				args = append(args, "--"+f.Name, "7")
			case FlagDuration:
				args = append(args, "--"+f.Name, "3m")
			}
		}
		args = append(args, positionalsFor(v)...)

		fs := v.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		b, err := v.Parse(fs, args)
		if err != nil {
			continue
		}
		act, err := v.BuildFunc()(b)
		if err != nil {
			continue // a cross-flag rule refused the all-flags-set form
		}
		// Every declared string flag's probe value must appear somewhere in
		// the built action; one that does not is either unread (the test
		// above) or read under a different name.
		rv := reflect.ValueOf(act)
		var seen []string
		for i := 0; i < rv.NumField(); i++ {
			if s, ok := rv.Field(i).Interface().(string); ok && s != "" {
				seen = append(seen, s)
			}
		}
		for _, f := range v.Flags {
			if f.Type != FlagString || f.Custom != nil {
				continue
			}
			if _, exempt := flagNotInAction[v.FlagSetName()+"."+f.Name]; exempt {
				continue
			}
			want := "probe-" + f.Name
			found := false
			for _, s := range seen {
				if strings.Contains(s, want) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: --%s parses but its value is not in the built Action (looked for %q among %q).\n"+
					"Bound.Str is a map lookup whose comma-ok is discarded, so reading a name the "+
					"declaration does not have returns \"\" silently -- which is what a rename in "+
					"the table leaves behind.",
					v.FlagSetName(), f.Name, want, seen)
			}
		}
	}
}

// TestNoVerbDeclaresAFlagItCannotType catches a Default whose type does not
// match the declared FlagType: NewFlagSet's type switch falls through to the
// zero value, so `Default: 400` on a FlagUint (an int, not a uint) silently
// becomes 0.
func TestNoVerbDeclaresAFlagItCannotType(t *testing.T) {
	for _, v := range Verbs {
		for _, f := range v.Flags {
			if f.Default == nil || f.Custom != nil {
				continue
			}
			var want string
			switch f.Type {
			case FlagBool:
				_, ok := f.Default.(bool)
				want = boolWant(ok, "bool")
			case FlagString:
				_, ok := f.Default.(string)
				want = boolWant(ok, "string")
			case FlagUint:
				_, ok := f.Default.(uint)
				want = boolWant(ok, "uint")
			case FlagUint64:
				_, ok := f.Default.(uint64)
				want = boolWant(ok, "uint64")
			case FlagDuration:
				_, ok := f.Default.(time.Duration)
				want = boolWant(ok, "time.Duration")
			}
			if want != "" {
				t.Errorf("%s: --%s has Default %#v, but its FlagType needs a %s.\n"+
					"NewFlagSet's type switch falls through on a mismatch, so the flag "+
					"silently defaults to the zero value instead.",
					v.FlagSetName(), f.Name, f.Default, want)
			}
		}
	}
}

func boolWant(ok bool, typ string) string {
	if ok {
		return ""
	}
	return typ
}

// TestWiringExemptionsAreLive keeps the two allowlists from outliving what
// they describe.
func TestWiringExemptionsAreLive(t *testing.T) {
	declared := map[string]bool{}
	for _, v := range Verbs {
		for _, f := range v.Flags {
			declared[v.FlagSetName()+"."+f.Name] = true
		}
	}
	var stale []string
	for key, reason := range flagNotInAction {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: an exemption needs a reason", key)
		}
		if !declared[key] {
			stale = append(stale, key+" (no such flag on that verb)")
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("stale exemption: %s", s)
	}
}
