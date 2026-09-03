// Package verb holds the single declaration of this project's command
// grammar: one VerbSpec per verb path, from which the CLI builds its FlagSet,
// the TUI parses, and the WebUI parses through the wasm bridge.
//
// It is deliberately BELOW package cli (see import_test.go): cli parses the
// board family itself, so cli imports verb and never the reverse.
//
// Parse only. What a surface DOES with a parsed Action -- stdout and an exit
// code, a tea.Cmd, a DOM update -- stays in that surface.
package verb

import "flag"

// Surface is the set of front-ends a verb or option is reachable from.
// Which surfaces a verb appears on is part of the declaration, not an
// invariant over it: `trsf` is TUI-only and `board` is CLI-only, both on
// purpose.
type Surface uint8

const (
	CLI Surface = 1 << iota
	TUI
	WebUI
)

// Has reports whether s includes every bit of want.
func (s Surface) Has(want Surface) bool { return s&want == want }

// Action is what a parsed command becomes. The marker is EXPORTED because
// surface-local actions live in their own packages -- tui's ClearAction and
// QuitAction cannot implement an unexported method declared here.
type Action interface{ IsAction() }

// ActionMarker is embedded by every action type to satisfy Action.
type ActionMarker struct{}

// IsAction marks the embedding type as an Action.
func (ActionMarker) IsAction() {}

// ArgType names how a positional is validated.
type ArgType int

const (
	ArgString ArgType = iota
	ArgTaskID         // 32 hex
	ArgTopic
	ArgUint
)

// Arg is one positional argument.
type Arg struct {
	Name     string
	Type     ArgType
	Variadic bool // absorbs the rest; only valid as the last Arg

	// Surfaces narrower than the verb's requires a reason: `file push` takes
	// one fewer positional in a browser, which has no local path to name.
	Surfaces      Surface
	SurfaceReason string
}

// FlagType names a flag's value type.
type FlagType int

const (
	FlagBool FlagType = iota
	FlagString
	FlagUint
	FlagUint64
	FlagDuration
)

// Flag is one option.
type Flag struct {
	Name string

	// Aliases are additional spellings for THIS flag. Declared explicitly and
	// never inferred from spelling: `session send` registers --enter (append a
	// carriage return) and -e (interpret backslash escapes) as two independent
	// flags, while `session new` binds --detach and -d to one variable and
	// `git diff` does the same for --staged and --cached. Pairing short with
	// long by shape would merge the first pair and inject a spurious Enter
	// into a live PTY.
	Aliases []string

	Type    FlagType
	Default any
	Help    string

	// WidensIfUnset marks a flag whose ABSENCE makes the operation cover more.
	// `board purge <topic> --seq N` -- the line the help text printed -- left
	// --seq at its zero value, which is the whole-topic form, and destroyed
	// two messages on a live board. Stated here so the property lives with the
	// flag instead of in a comment beside the parser.
	WidensIfUnset bool

	Surfaces      Surface
	SurfaceReason string
}

// Trailing describes free-form words after the positionals. Non-nil means the
// verb CANNOT permute: a '-'-leading word is indistinguishable from a flag, so
// flags must precede the text.
type Trailing struct {
	Name   string
	Reason string
}

// VerbSpec is one verb path's whole grammar.
type VerbSpec struct {
	Path     []string
	Surfaces Surface
	Args     []Arg
	Flags    []Flag
	Trailing *Trailing
	Examples []string
	Build    func(Bound) (Action, error)
}

// Bound is a parsed command before Build: the neutral form the wasm bridge
// hands to JS, so no per-action marshaller is needed.
type Bound struct {
	Path  []string
	Args  []string
	Flags map[string]any
	Set   map[string]bool // flags the caller actually supplied
	Trail string
}

// Str returns a string flag's value.
func (b Bound) Str(name string) string {
	v, _ := b.Flags[name].(string)
	return v
}

// Bool returns a bool flag's value.
func (b Bound) Bool(name string) bool {
	v, _ := b.Flags[name].(bool)
	return v
}

// FlagSetName is the name a verb's FlagSet carries, and the key the alias
// guard matches a legacy FlagSet by.
func (v VerbSpec) FlagSetName() string {
	out := ""
	for i, p := range v.Path {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

// ErrorHandling is re-exported so callers need not import flag for the one
// argument NewFlagSet takes: the CLI wants ExitOnError, the TUI
// ContinueOnError with a discarded writer.
type ErrorHandling = flag.ErrorHandling
