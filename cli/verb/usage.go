package verb

import "strings"

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
		b.WriteString(" [")
		b.WriteString(dashList([]string{f.Name}))
		if f.Type != FlagBool {
			b.WriteString(" ")
			b.WriteString(strings.ToUpper(f.Name))
		}
		b.WriteString("]")
	}
	for _, a := range v.Args {
		b.WriteString(" <")
		b.WriteString(a.Name)
		b.WriteString(">")
		if a.Variadic {
			b.WriteString("...")
		}
	}
	if v.Trailing != nil {
		b.WriteString(" <")
		b.WriteString(v.Trailing.Name)
		b.WriteString(">...")
	}
	return b.String()
}
