package verb

import (
	"flag"
	"io"
	"os"
	"reflect"
	"regexp"
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
			// A Required flag has no baseline to compare against -- omitting
			// it is refused by construction -- so the static check above is
			// the whole check for it.
			if f.Required {
				continue
			}
			// A flag carrying neither a Field nor a place in a Modes group is
			// one the generator writes nothing for. It must say why -- the
			// four `forward tap` render modes and `grid --descendants` are the
			// real cases, and each names the group that carries it instead.
			if f.Field == "" && f.PresenceField == "" && !inModes(v, f.Name) {
				if strings.TrimSpace(f.FieldReason) == "" {
					t.Errorf("%s: --%s has no Field and no FieldReason.\n"+
						"The generator emits an assignment per Field, so a flag without one "+
						"parses and reaches nothing. Give it a Field, or a FieldReason saying "+
						"what carries it.", v.FlagSetName(), f.Name)
				}
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
				base, ok := buildWith(t, nv, &f, false)
				if !ok {
					t.Errorf("%s (%v): --%s could not be probed -- no line satisfying the "+
						"declared rules could be synthesised. That used to be a silent skip, "+
						"and it hid five verbs entirely.", v.FlagSetName(), sf, f.Name)
					continue
				}
				with, ok := buildWith(t, nv, &f, true)
				if !ok {
					continue // a cross-flag rule refuses the flag in this shape
				}
				if !changedSomething(base, with, f, v) {
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

// inModes reports whether a flag is one of a Modes group's names.
func inModes(v VerbSpec, name string) bool {
	if v.Modes == nil {
		return false
	}
	for _, n := range v.Modes.Names {
		if n == name {
			return true
		}
	}
	return false
}

// changedSomething compares the fields this flag is supposed to move, not the
// whole struct. A whole-struct compare reported --allow as unread on `session
// stream approve`, because the baseline that satisfies its ExactlyOne group
// has to name --deny, and both builds then differ in two places at once.
func changedSomething(base, with any, f Flag, v VerbSpec) bool {
	names := []string{}
	if f.Field != "" {
		names = append(names, f.Field)
	}
	if f.PresenceField != "" {
		names = append(names, f.PresenceField)
	}
	if inModes(v, f.Name) {
		names = append(names, v.Modes.Field)
	}
	bv, wv := reflect.ValueOf(base), reflect.ValueOf(with)
	for _, n := range names {
		bf, wf := bv.FieldByName(n), wv.FieldByName(n)
		if !bf.IsValid() || !wf.IsValid() {
			continue
		}
		if !reflect.DeepEqual(bf.Interface(), wf.Interface()) {
			return true
		}
	}
	return false
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
func buildWith(t *testing.T, v VerbSpec, f *Flag, set bool) (any, bool) {
	t.Helper()
	var args []string
	if f != nil && set {
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
			args = append(args, "--"+f.Name, probeValue(*f))
		case FlagUint, FlagUint64:
			args = append(args, "--"+f.Name, "7")
		case FlagDuration:
			args = append(args, "--"+f.Name, "3m")
		}
	}
	// A form the DECLARED rules accept. Without this the bare probe is refused
	// for any verb carrying ExactlyOne / AtLeastOne / MinArgs -- and buildWith
	// returned ok=false, which the caller silently skipped. Measured before
	// the fix: 5 verbs with not one flag checked (`forward`, `caps set`, `caps
	// set-parent`, `session stream approve`, `board retract`) and 99 of 403
	// (verb, flag) pairs dropped, including -L / -R / -W on the verb that
	// opens listeners. The comment there named a fallback "below" that does
	// not exist.
	args = append(args, satisfyingFlags(v, f, set)...)
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

// satisfyingFlags adds the minimum the declared rules demand, skipping
// anything that would conflict with the flag being probed.
func satisfyingFlags(v VerbSpec, probe *Flag, probeSet bool) []string {
	probeName := ""
	if probe != nil {
		probeName = probe.Name
	}
	conflicts := func(name string) bool {
		if name == probeName {
			// Only when the probe is actually on the line. In the BASE form
			// it is not, so `forward -W`'s baseline may legitimately be -L --
			// treating them as conflicting left that verb with no baseline at
			// all, which read as "could not be probed".
			return probeSet
		}
		if !probeSet {
			return false
		}
		for _, r := range v.Exclusive {
			var hasProbe, hasName bool
			for _, n := range r.Flags {
				hasProbe = hasProbe || n == probeName
				hasName = hasName || n == name
			}
			if hasProbe && hasName {
				return true
			}
		}
		return false
	}
	var out []string
	add := func(names []string) {
		if probeSet {
			for _, n := range names {
				if n == probeName {
					return // the probe itself satisfies this group
				}
			}
		}
		for _, n := range names {
			// Never the probe itself: the base form exists to isolate it.
			if n == probeName || conflicts(n) {
				continue
			}
			for _, f := range v.Flags {
				if f.Name != n {
					continue
				}
				switch f.Type {
				case FlagBool:
					if d, _ := f.Default.(bool); !d {
						out = append(out, "--"+n)
					}
				case FlagString:
					out = append(out, "--"+n, probeValue(f))
				case FlagUint, FlagUint64:
					out = append(out, "--"+n, "1")
				case FlagDuration:
					out = append(out, "--"+n, "1m")
				}
			}
			return
		}
	}
	for _, r := range v.ExactlyOne {
		add(r.Flags)
	}
	for _, r := range v.AtLeastOne {
		add(r.Flags)
	}
	// A probed flag that Requires another needs that other one too -- but only
	// when the probe is on the line. Adding it to the BASE pulled -W onto
	// `forward`'s baseline, where it is exclusive with the -L that satisfies
	// the at-least-one rule, and the baseline stopped parsing.
	if probeSet {
		for _, r := range v.Requires {
			for _, n := range r.Flags {
				if n == probeName {
					add([]string{r.Needs})
				}
			}
		}
	}
	return out
}

// probeValue is a value the flag will actually accept. A few flags carry a
// GRAMMAR rather than a string -- Convert parses them -- so "probe-value"
// makes the build fail and the probe read as unsatisfiable.
func probeValue(f Flag) string {
	if len(f.OneOf) > 0 {
		// Not OneOf[0] blindly: `notify --level info` IS the default, so
		// setting it produced the same Action and read as unwired.
		def, _ := f.Default.(string)
		for _, v := range f.OneOf {
			if v != def {
				return v
			}
		}
		return f.OneOf[0]
	}
	if v, ok := probeValues[f.Name]; ok {
		return v
	}
	return "probe-value"
}

var probeValues = map[string]string{
	"caps":      "none",
	"scope":     "subtree",
	"scope-for": "spawn=none",
	"resize":    "40x150",
	"size":      "40x150",
}

// positionalsFor synthesises one acceptable value per required positional.
func positionalsFor(v VerbSpec) []string {
	var out []string
	for _, a := range v.Args {
		if a.Variadic {
			// MinArgs demands at least one; a variadic that came back empty is
			// refused, and the probe was skipped rather than satisfied.
			for i := len(out); i < v.MinArgs; i++ {
				out = append(out, valueFor(a))
			}
			continue
		}
		out = append(out, valueFor(a))
	}
	return out
}

func valueFor(a Arg) string {
	switch a.Type {
	case ArgTaskID:
		return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	case ArgUint:
		return "1"
	case ArgTopic:
		return "chat.abcd1234"
	}
	return "probe-arg"
}

// TestBuildLookupNamesAreDeclared is the other direction, and the docstring
// above promised it by name long before it existed: `grep` for it returned one
// hit, that promise.
//
// Bound.Str / Bool / Flags / Set / Custom are map lookups whose comma-ok is
// discarded, so a name the declaration does not have yields "" or false with
// no error. In GENERATED code that cannot happen -- the key and the Field come
// from one Flag -- but every hand-written reader is exposed: Validate funcs,
// the Derived and Convert helpers, and the surfaces.
//
// So this reads the source rather than probing behaviour. The version it
// replaces built an all-flags-set line and looked for each probe value in the
// result, which skipped every Trailing verb outright and gave up whenever a
// cross-flag rule refused the all-flags form: measured, it reached 20 of 80
// verb paths and could not see `notify`'s title read as "titel".
func TestBuildLookupNamesAreDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, v := range Verbs {
		for _, f := range v.Flags {
			declared[f.Name] = true
			for _, a := range f.Aliases {
				declared[a] = true
			}
		}
	}
	// b.Str("x") / b.Bool("x") / b.Flags["x"] / b.Set["x"] / b.Custom["x"],
	// however the receiver is spelled.
	pat := regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\.(?:Str|Bool)\("([^"]+)"\)|\b[A-Za-z_][A-Za-z0-9_]*\.(?:Flags|Set|Custom)\["([^"]+)"\]`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(e.Name())
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, m := range pat.FindAllStringSubmatch(string(src), -1) {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if declared[name] {
				continue
			}
			t.Errorf("%s: reads %q off a Bound, and no verb declares a flag by that name.\n"+
				"These are map lookups with the comma-ok discarded, so the value is \"\" or "+
				"false and nothing fails -- which is what a rename in the table leaves behind.",
				e.Name(), name)
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
