package verb

import (
	"flag"
	"fmt"
	"strings"
	"time"
)

// NewFlagSet registers this verb's flags, aliases included, on a fresh FlagSet
// named after the verb path.
//
// Every spelling of a flag binds to ONE variable, which is what makes an alias
// an alias rather than a second flag that happens to look related. It is also
// what the migration-lifetime alias guard reads back: two names sharing a
// flag.Flag.Value pointer are one flag, two independent registrations are not.
//
// eh differs per surface and always has: the CLI takes ExitOnError because a
// bad command line should end the process with usage, while the TUI takes
// ContinueOnError with a discarded writer because a typo there is a line in a
// results pane, not an exit.
func (v VerbSpec) NewFlagSet(eh ErrorHandling) *flag.FlagSet {
	fs := flag.NewFlagSet(v.FlagSetName(), eh)
	for _, f := range v.Flags {
		names := append([]string{f.Name}, f.Aliases...)
		if f.Custom != nil {
			// One value shared by every spelling, same as the typed cases:
			// --agent-arg and its deprecated alias --claude-arg accumulate
			// into ONE list, in the order the flags appear.
			val := f.Custom.New()
			for _, n := range names {
				fs.Var(val, n, f.Help)
			}
			continue
		}
		switch f.Type {
		case FlagBool:
			p := new(bool)
			if d, ok := f.Default.(bool); ok {
				*p = d
			}
			for _, n := range names {
				fs.BoolVar(p, n, *p, f.Help)
			}
		case FlagString:
			p := new(string)
			if d, ok := f.Default.(string); ok {
				*p = d
			}
			for _, n := range names {
				fs.StringVar(p, n, *p, f.Help)
			}
		case FlagUint:
			p := new(uint)
			if d, ok := f.Default.(uint); ok {
				*p = d
			}
			for _, n := range names {
				fs.UintVar(p, n, *p, f.Help)
			}
		case FlagUint64:
			p := new(uint64)
			if d, ok := f.Default.(uint64); ok {
				*p = d
			}
			for _, n := range names {
				fs.Uint64Var(p, n, *p, f.Help)
			}
		case FlagDuration:
			p := new(time.Duration)
			if d, ok := f.Default.(time.Duration); ok {
				*p = d
			}
			for _, n := range names {
				fs.DurationVar(p, n, *p, f.Help)
			}
		}
	}
	return fs
}

// Parse runs fs over args and returns the neutral Bound.
//
// Permuted unless Trailing is set: with stdlib flag alone, parsing stops at
// the first non-flag token and a flag written after a positional is silently
// dropped -- which is how `board purge <topic> --seq N`, the exact line the
// help text printed, fell through to the whole-topic form and destroyed two
// messages on a live board. A verb WITH Trailing cannot permute, because a
// '-'-leading word in free-form text is indistinguishable from a flag.
func (v VerbSpec) Parse(fs *flag.FlagSet, args []string) (Bound, error) {
	pathspec := ""
	if v.Pathspec {
		args, pathspec = SplitPathspec(args)
	}
	var (
		positionals []string
		err         error
	)
	if v.Trailing == nil {
		positionals, err = ParsePermuted(fs, args)
	} else {
		err = fs.Parse(args)
		positionals = fs.Args()
	}
	if err != nil {
		// Named HERE, once. `flag`'s own message is "flag provided but not
		// defined: -x" with no hint which verb refused it, and the callers
		// were split: four wrapped it with the verb name and eleven returned
		// it bare -- so the same mistake read differently depending on where
		// it was typed, and the four produced "agent subscribe: agent
		// subscribe: …" once the declared checks started naming it too.
		return Bound{}, fmt.Errorf("%s: %w", v.FlagSetName(), err)
	}

	b := Bound{
		Path:     v.Path,
		Pathspec: pathspec,
		Flags:    map[string]any{},
		Set:      map[string]bool{},
	}
	// Canonical names only: an alias is a spelling, not a key. Reading the
	// value off the canonical registration is enough because NewFlagSet binds
	// every spelling to one variable.
	canonical := map[string]string{}
	for _, f := range v.Flags {
		if fl := fs.Lookup(f.Name); fl != nil {
			if f.Custom != nil {
				if b.Custom == nil {
					b.Custom = map[string]any{}
				}
				b.Custom[f.Name] = f.Custom.Get(fl.Value)
			} else {
				b.Flags[f.Name] = flagValue(fl)
			}
		}
		canonical[f.Name] = f.Name
		for _, a := range f.Aliases {
			canonical[a] = f.Name
		}
	}
	fs.Visit(func(fl *flag.Flag) {
		if name, ok := canonical[fl.Name]; ok {
			b.Set[name] = true
		}
	})

	if v.Trailing != nil {
		fixed := v.fixedArgs()
		if len(positionals) < fixed {
			return Bound{}, fmt.Errorf("%s", v.Usage())
		}
		b.Args = positionals[:fixed]
		tail := positionals[fixed:]
		if v.Trailing.AfterSeparator {
			// Everything up to `--` is still positional; the tail is what
			// follows it. Without the separator there is no tail at all, which
			// is how `exec <id>` with no command is refused rather than run.
			sep := -1
			for i, a := range tail {
				if a == "--" {
					sep = i
					break
				}
			}
			if sep >= 0 {
				b.Args = append(b.Args, tail[:sep]...)
				tail = tail[sep+1:]
			}
		}
		b.TrailArgs = tail
		b.Trail = strings.Join(tail, " ")
		if err := v.check(b); err != nil {
			return Bound{}, err
		}
		return b, nil
	}
	if err := v.checkArity(len(positionals)); err != nil {
		return Bound{}, err
	}
	b.Args = positionals
	if err := v.check(b); err != nil {
		return Bound{}, err
	}
	return b, nil
}

// check runs the declared validation, then the hand-written residue.
func (v VerbSpec) check(b Bound) error {
	if err := v.checkDeclared(b); err != nil {
		return err
	}
	if v.Validate != nil {
		return v.Validate(b)
	}
	return nil
}

// fixedArgs counts the non-variadic positionals.
// fixedArgs counts the positionals that must be present.
func (v VerbSpec) fixedArgs() int {
	n := 0
	for _, a := range v.Args {
		if !a.Variadic && !a.Optional {
			n++
		}
	}
	return n
}

// maxArgs is the most positionals this verb accepts, or -1 when variadic.
func (v VerbSpec) maxArgs() int {
	n := 0
	for _, a := range v.Args {
		if a.Variadic {
			if a.MaxCount > 0 {
				n += a.MaxCount
				continue
			}
			return -1
		}
		n++
	}
	return n
}

// checkArity replaces the per-verb `len(pargs) != 3` checks the three surfaces
// each wrote by hand -- and supplies one where a surface had none: tui's
// parsePrune had no check at all, so a dropped flag arrived as a task id.
func (v VerbSpec) checkArity(n int) error {
	fixed := v.fixedArgs()
	variadic, maxCount := false, 0
	for _, a := range v.Args {
		if a.Variadic {
			variadic = true
			maxCount = a.MaxCount
		}
	}
	_ = variadic
	_ = maxCount
	if n < fixed {
		return fmt.Errorf("%s", v.Usage())
	}
	if n < v.MinArgs {
		what := "argument"
		if len(v.Args) > 0 {
			what = v.Args[len(v.Args)-1].Name
		}
		plural := ""
		if v.MinArgs > 1 {
			plural = "s"
		}
		return fmt.Errorf("%s: needs at least %d %s%s\n%s",
			v.FlagSetName(), v.MinArgs, what, plural, v.Usage())
	}
	if max := v.maxArgs(); max >= 0 && n > max {
		return fmt.Errorf("%s: at most %d positional(s), got %d\n%s", v.FlagSetName(), max, n, v.Usage())
	}
	return nil
}

// flagValue reads a parsed flag's typed value. Every stdlib flag Value
// implements Getter; the fallback is for a custom flag.Value that does not.
func flagValue(fl *flag.Flag) any {
	if g, ok := fl.Value.(flag.Getter); ok {
		return g.Get()
	}
	return fl.Value.String()
}

// For returns this spec narrowed to one surface: flags and positionals whose
// own Surfaces exclude s are dropped.
//
// Needed because narrowing is real, not cosmetic. `file push` takes
// <task-id> <local-src> <dst> on the CLI and <task-id> <dst> in a browser,
// which has no local path to name -- so the same verb has a different arity
// per surface, and a parser that did not know which surface it was serving
// would reject one of them.
//
// A zero Surfaces on a Flag or Arg means "wherever the verb goes", which is
// the common case; only a deliberate narrowing sets it, and the invariant
// test requires a reason when it does.
func (v VerbSpec) For(s Surface) VerbSpec {
	out := v
	switch s {
	case CLI:
		out.narrowedFor = "cli"
	case TUI:
		out.narrowedFor = "tui"
	case WebUI:
		out.narrowedFor = "webui"
	}
	out.Flags = nil
	for _, f := range v.Flags {
		if f.Surfaces == 0 || f.Surfaces.Has(s) {
			out.Flags = append(out.Flags, f)
		}
	}
	out.Args = nil
	for _, a := range v.Args {
		if a.Surfaces == 0 || a.Surfaces.Has(s) {
			out.Args = append(out.Args, a)
		}
	}
	return out
}

// Resolve applies a flag's fallback ladder: the value as parsed if the caller
// supplied it, otherwise each declared tier in order.
//
// It exists because all three surfaces had their own version of this. The CLI
// called cliopts.ResolveString(flag, env), the TUI passed defaultRepo as the
// FlagSet's default, and the WebUI read a dropdown -- three ladders for one
// question, which is why --config's workspace tier reached only the CLI.
//
// env and ws are supplied by the caller: this package parses and does not read
// the environment or the config file.
func (v VerbSpec) Resolve(b Bound, name string, env func(string) string, ws func(string) string, ctx map[string]string) string {
	if b.Set[name] {
		return b.Str(name)
	}
	var f *Flag
	for i := range v.Flags {
		if v.Flags[i].Name == name {
			f = &v.Flags[i]
			break
		}
	}
	if f == nil {
		return b.Str(name)
	}
	for _, t := range f.Resolve {
		switch {
		case t.Env != "" && env != nil:
			if val := env(t.Env); val != "" {
				return val
			}
		case t.Workspace != "" && ws != nil:
			if val := ws(t.Workspace); val != "" {
				return val
			}
		case t.SurfaceContext && ctx != nil:
			if val := ctx[name]; val != "" {
				return val
			}
		}
	}
	return b.Str(name)
}
