package verb

import (
	"strconv"
	"strings"
)

// Usage renders the verb's synopsis from the declaration.
//
// Generated rather than written by hand because a hand-written usage line
// drifts from the parser: `board purge <topic> --seq N` is what the help text
// told operators to type, and stdlib parsing left --seq unread, so the call
// fell through to the whole-topic form and destroyed the ring it was asked to
// take one message from. Printing and parsing now come from one source, and
// TestExamplesParse holds every documented invocation to that.
func (v VerbSpec) Usage() string {
	var b strings.Builder
	b.WriteString("usage: ")
	b.WriteString(v.FlagSetName())
	for _, f := range v.Flags {
		// One dash for a single letter. `forward -W host:port` is how it is
		// typed, and a usage line printing --W describes a flag that does not
		// exist -- which is the `board purge` failure shape exactly: a
		// documented invocation the parser refuses.
		// Brackets mean "may be omitted", so a Required flag must not wear
		// them: `board retract [--seq SEQ]` described the one call this verb
		// refuses, and it is the verb whose whole reason for Required is that
		// the zero value would retract a topic-full of other agents' messages.
		open, close := "[", "]"
		if f.Required {
			open, close = "", ""
		}
		b.WriteString(" " + open)
		b.WriteString(dashList([]string{f.Name}))
		if f.Type != FlagBool {
			b.WriteString(" ")
			b.WriteString(strings.ToUpper(f.Name))
		}
		b.WriteString(close)
	}
	for _, a := range v.Args {
		// Three shapes, not two. A variadic capped at one is an OPTIONAL
		// SINGLE value -- `skill [<name>]`, `workspace show [<name>]` -- and
		// printing it as `<name>...` invited a second word the parse refuses.
		// An Optional keeps its index and is likewise bracketed: `git diff
		// [<base>] [<target>]` counts revisions the way git does, and printing
		// them as required described a call the CLI has never made.
		switch {
		case a.Variadic && a.MaxCount == 1, a.Optional:
			b.WriteString(" [<" + a.Name + ">]")
		case a.Variadic:
			b.WriteString(" <" + a.Name + ">...")
			if a.MaxCount > 1 {
				b.WriteString(" (at most " + itoa(a.MaxCount) + ")")
			}
		default:
			b.WriteString(" <" + a.Name + ">")
		}
	}
	if v.Trailing != nil {
		b.WriteString(" <")
		b.WriteString(v.Trailing.Name)
		b.WriteString(">...")
	}
	return b.String()
}

func itoa(n int) string { return strconv.Itoa(n) }
