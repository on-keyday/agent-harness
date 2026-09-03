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

	// Optional is a fixed position that may be omitted. Unlike Variadic it
	// keeps its index, so a verb can take two of them: `git diff [base]
	// [target]` counts revisions the way git does -- none, one, or two -- and
	// each lands in its own field rather than being interpreted from a slice.
	// Every Optional must follow the required ones.
	Optional bool

	// MaxCount caps a variadic positional. Zero means unbounded. `board
	// subscribers` takes at most one topic and `git diff` at most two
	// revisions -- both were `if len(b.Args) > N` inside a Build, which is a
	// count the declaration already knew.
	MaxCount int

	// Surfaces narrower than the verb's requires a reason: `file push` takes
	// one fewer positional in a browser, which has no local path to name.
	Surfaces      Surface
	SurfaceReason string

	// Field is the Action field this positional lands in. Empty means the
	// generator skips it, which is how a verb whose Build interprets its
	// positionals (git counts revisions its own way) opts out.
	Field string
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

	// Custom supplies a flag.Value for options the stdlib types cannot carry:
	// --agent-arg accumulates one entry per occurrence, and --scope-for parses
	// and merges on every Set so an overlapping capability list is refused at
	// the flag rather than one round trip later. New() returns a fresh value
	// per FlagSet; Get() reads it back after parsing.
	Custom *CustomValue

	// Resolve names the tiers consulted when the flag is absent, in order.
	// The CLI reads flag > env > workspace config; the TUI and WebUI supply a
	// surface context instead (the TUI's defaultRepo, the WebUI's dropdowns).
	// Declared here because all three had their own version of this ladder.
	Resolve []Tier

	// WidensIfUnset marks a flag whose ABSENCE makes the operation cover more.
	// `board purge <topic> --seq N` -- the line the help text printed -- left
	// --seq at its zero value, which is the whole-topic form, and destroyed
	// two messages on a live board. Stated here so the property lives with the
	// flag instead of in a comment beside the parser.
	WidensIfUnset bool

	Surfaces      Surface
	SurfaceReason string

	// Field is the Action field this flag lands in. The generator writes both
	// the struct field and the assignment, so the two cannot disagree -- the
	// seam where `ls --filtered` had no field to land in and `agent wait
	// --timeout` lost its default to a type mismatch.
	//
	// Empty means the flag is not carried into the Action by the generator.
	// That is a real case (--hex names the default render mode) and it must be
	// spelled with FieldReason.
	Field       string
	FieldReason string

	// PresenceField carries whether the operator TYPED the flag, not its
	// value. `agent send --data ""` is an explicit empty body and a missing
	// --data is not, and the spawn flags whose zero value is meaningful
	// ("none" caps, "subtree" scope) need the same distinction so a resume
	// does not re-grant on a flag nobody passed.
	PresenceField string

	// PresenceAlso names a SECOND flag whose presence ORs into
	// PresenceField. --scope and --scope-for are one half of the authority:
	// naming either makes the scope explicit, and letting a lone --scope-for
	// ride the session default would write an authority half typed and half
	// inherited.
	PresenceAlso string

	// Required refuses the verb when the flag is absent. `board retract --seq`
	// is required because there is no whole-topic retract, unlike purge.
	Required bool

	// FieldType overrides the Go type the generator gives Field. A wire struct
	// wanting uint32 where the flag parses as uint is the whole use: without
	// it every consumer casts, and one that forgets reads a different number.
	FieldType string

	// OneOf restricts a string flag to a vocabulary. A value outside it is
	// refused with the list, rather than passed through to mean whatever the
	// consumer makes of it -- `forward tap --dir sideways` used to reach the
	// server.
	OneOf []string
}

// Modes turns a set of mutually exclusive bool flags into one string field.
type Modes struct {
	// Field is the Action field carrying the chosen name.
	Field string
	// Names are the flag names, which are also the values Field takes.
	Names []string
	// Default is the name when the operator picked none. It must be one of
	// Names: a mode flag that names the default still has to be writable,
	// which is why --hex exists at all.
	Default string
}

// CustomValue is a flag whose value type the stdlib does not cover.
type CustomValue struct {
	New func() flag.Value
	Get func(flag.Value) any
}

// Tier is one step of a flag's fallback ladder.
type Tier struct {
	// Env names an environment variable; SurfaceContext takes the value the
	// calling surface injected under the flag's own name. Exactly one is set.
	Env            string
	SurfaceContext bool
	// Workspace names a key in .harness/config, resolved by the caller because
	// the file is read once per process and this package parses only.
	Workspace string
}

// Trailing describes free-form words after the positionals. Non-nil means the
// verb CANNOT permute: a '-'-leading word is indistinguishable from a flag, so
// flags must precede the text.
type Trailing struct {
	Name   string
	Reason string

	// Field is the Action field the joined text lands in; FieldArgs is the one
	// the word list lands in. A verb reads whichever it means.
	Field     string
	FieldArgs string

	// Required refuses an empty tail. Sending nothing is a mistyped command,
	// not a no-op send.
	Required bool

	// JoinWhen names a bool flag that collapses the tail into ONE word.
	// `exec --shell` is the case: the operator asked for shell
	// interpretation, so those words were never an argv to preserve, and
	// joining them anywhere else would lose the boundaries the runner needs.
	JoinWhen string

	// List keeps the tail as separate words in Bound.TrailArgs instead of only
	// joining it into Bound.Trail. `exec <task-id> -- <cmd> [args...]` hands
	// the runner an argv, and joining it would lose the word boundaries the
	// runner needs; `session send` wants the joined text instead. Both forms
	// are always populated, so a Build takes whichever it means.
	List bool

	// AfterSeparator means a literal `--` MAY introduce the tail, and that
	// anything before it is still positional. The separator is optional: the
	// CLI's exec required it and the TUI's did not, so accepting both is a
	// widening that reconciles them. Without this flag the tail simply starts
	// once the declared positionals are satisfied.
	AfterSeparator bool
}

// VerbSpec is one verb path's whole grammar.
type VerbSpec struct {
	Path     []string
	Surfaces Surface
	Args     []Arg
	Flags    []Flag
	Trailing *Trailing

	// PathspecField is the Action field the trailing `-- <path>` lands in.
	PathspecField string

	// Pathspec means the verb accepts a trailing `-- <path>...`, peeled BEFORE
	// flags are read because Go's flag package consumes a bare "--" as its
	// end-of-flags marker and the path would silently vanish. The result
	// arrives in Bound.Pathspec, so a Build that accepts the path either way
	// (git file) can see both and refuse the ambiguous case.
	Pathspec bool

	// Action names the type this verb builds. The generator emits the struct
	// and the assignment; a verb whose fields are shared with another (session
	// snapshot and session await-idle both build SessionAction) names the same
	// type and the generator unions their fields.
	Action string

	// ExtraFields are Action fields the PARSE does not fill: each surface sets
	// them after Build. `git <task-id> diff` is the case -- the id sits between
	// the family word and the sub-verb, so every surface peels it before the
	// shared parse and writes it back afterwards. Declared here so the
	// generated struct has somewhere for it to go.
	ExtraFields map[string]string // field name -> Go type

	// Const carries values that are fixed per verb rather than parsed --
	// SessionAction.Sub is "snapshot" for one path and "await-idle" for
	// another. Generated as literal assignments.
	Const map[string]string

	// Modes is a group of bool flags naming ONE choice out of several --
	// `forward tap --hex|--text|--raw|--json` is the case. Declared rather
	// than written as four bool fields plus a hand-rolled count, because the
	// consumer wants the chosen name and every hand-rolled version of this
	// spelled the exclusivity check again.
	Modes *Modes

	// Validate is the residue: rules no attribute expresses. It runs on Bound
	// AFTER the declarative checks and BEFORE the Action is built, which is
	// what keeps this file free of the generated types -- and therefore able
	// to compile when the generated file is absent, so the generator can run.
	Validate func(Bound) error

	// Exclusive lists flag groups where naming more than one is refused.
	Exclusive [][]string
	// ExactlyOne lists groups where naming exactly one is required.
	ExactlyOne [][]string
	// AtLeastOne lists groups where naming none is refused.
	AtLeastOne [][]string
	// Requires maps a flag to the one it cannot be used without.
	Requires map[string]string

	// MinArgs refuses a verb whose variadic positional came back empty:
	// `exec kill` and `forward kill` with no id are mistyped lines, not
	// requests to kill nothing.
	MinArgs int

	Examples []string

	// narrowedFor records which surface For() produced this spec for, so
	// BuildFunc can pick the matching generated build. Unexported: it is
	// bookkeeping, not part of the declaration.
	narrowedFor string

	// Build is the hand-written path, kept for the verbs whose repacking is
	// not mechanical. Read it through BuildFunc, never directly: when it is
	// nil the generated build applies, and a caller that ranges over Verbs
	// rather than going through Lookup would otherwise dereference nil.
	Build func(Bound) (Action, error)
}

// BuildFunc returns the build this verb uses: its own when it has one, the
// generated one otherwise.
//
// Every caller goes through this rather than touching Build, because the two
// sources are not interchangeable at the field level -- Lookup used to patch
// Build in, which worked until a test ranged over Verbs directly and found
// nil. One accessor is the fix; a second patching site is not.
func (v VerbSpec) BuildFunc() func(Bound) (Action, error) {
	if v.Build != nil {
		return v.Build
	}
	// Keyed by (path, surface): a positional the declaration narrows away
	// shifts every index after it, so `file push` writes b.Args[2] into
	// RemoteDst on the CLI and b.Args[1] on the WebUI, which has no local path.
	// The narrowed spec remembers which surface it came from.
	if b, ok := generatedBuilds[v.FlagSetName()+"\x00"+v.narrowedFor]; ok {
		return b
	}
	// A spec that was never narrowed -- a test, or a caller that skipped For --
	// gets the build for the first surface this verb declares, in CLI, TUI,
	// WebUI order. Defaulting to CLI alone left `workspace apply` (TUI-only)
	// with no build at all, which reads as a verb nobody wired.
	for _, sf := range []struct {
		s    Surface
		name string
	}{{CLI, "cli"}, {TUI, "tui"}, {WebUI, "webui"}} {
		if !v.Surfaces.Has(sf.s) {
			continue
		}
		if b, ok := generatedBuilds[v.FlagSetName()+"\x00"+sf.name]; ok {
			return b
		}
	}
	return nil
}

// Bound is a parsed command before Build: the neutral form the wasm bridge
// hands to JS, so no per-action marshaller is needed.
type Bound struct {
	Path  []string
	Args  []string
	Flags map[string]any
	Set   map[string]bool // flags the caller actually supplied
	Trail string

	// Pathspec is whatever followed `--`, joined with spaces. Empty when the
	// verb does not take one or none was given.
	Pathspec string

	// TrailArgs is the trailing tail as separate words; Trail is the same
	// words joined. A verb that hands an argv onward reads TrailArgs.
	TrailArgs []string

	// Custom holds the parsed values of Custom flags, by canonical name.
	Custom map[string]any
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
