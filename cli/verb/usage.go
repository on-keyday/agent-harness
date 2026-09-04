package verb

import (
	"fmt"
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
		// Aliases included: `-r` is how `file push -r` is typed, and a
		// synopsis naming only --recursive describes a longer spelling of a
		// flag whose short form is the one in every example. Joined with |
		// rather than dashList's comma, because inside brackets a comma reads
		// as "and also".
		names := append([]string{f.Name}, f.Aliases...)
		b.WriteString(strings.ReplaceAll(dashList(names), ", ", "|"))
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

// FamilyNotes is the prose that belongs to a whole first word rather than to
// one of its paths: what `git` reads and when, what `file` paths are relative
// to. Keyed by the first word, printed once after that family's last verb.
//
// Separate from VerbSpec.Notes because the alternative is repeating five lines
// on each of `git`'s six paths, and a reader who meets the same paragraph six
// times stops reading it -- which is how the one line that DID differ per verb
// (diff's revision counting) got skipped.
var FamilyNotes = map[string][]string{}

// UsageLines renders one verb's help block: the generated synopsis, then its
// declared notes, indented under it.
func (v VerbSpec) UsageLines() []string {
	out := []string{strings.TrimPrefix(v.Usage(), "usage: ")}
	out = append(out, v.Notes...)
	return out
}

// ConstName is the name of the generated constant for one declared
// discriminator value: ConstName("Sub", "stream-turn") is "SubStreamTurn".
//
// Exported and living here rather than in the generator because two things
// need it and they must not disagree: the generator, which emits the
// constant, and the guard that tells a caller which one to write instead of a
// literal. A guard naming a constant that does not exist is worse than no
// guard.
func ConstName(field, value string) string {
	var b strings.Builder
	b.WriteString(field)
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '-' || r == '_' }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// MidPathIDVerbs maps each family whose task id is typed before the sub-verb
// to that family's declared sub-verbs, in table order.
//
// Callers need both halves: the family word to recognise the line, and the
// sub-verb list to say what was expected when the word after the id is not
// one of them.
func MidPathIDVerbs(sf Surface) map[string][]string {
	out := map[string][]string{}
	for _, v := range Verbs {
		if !v.IDBeforeSub || !v.Surfaces.Has(sf) || len(v.Path) < 2 {
			continue
		}
		out[v.Path[0]] = append(out[v.Path[0]], v.Path[1])
	}
	return out
}

// PeelMidPathID turns `git <task-id> log --max 5` into the path-first tokens
// the generic parsers accept, plus the id that was in the middle.
//
// ok is false for a line this does not apply to. A line whose family DOES
// match but whose shape is wrong comes back with an error naming the declared
// sub-verbs -- from the table, so the list cannot drift from what parses.
func PeelMidPathID(sf Surface, tokens []string) (rest []string, id string, ok bool, err error) {
	if len(tokens) == 0 {
		return nil, "", false, nil
	}
	subs, isFamily := MidPathIDVerbs(sf)[tokens[0]]
	if !isFamily {
		return nil, "", false, nil
	}
	if len(tokens) < 3 {
		return nil, "", true, fmt.Errorf("usage: %s <task-id> {%s} ...",
			tokens[0], strings.Join(subs, " | "))
	}
	for _, sub := range subs {
		if tokens[2] == sub {
			return append([]string{tokens[0], tokens[2]}, tokens[3:]...), tokens[1], true, nil
		}
	}
	return nil, "", true, fmt.Errorf("%s: unknown sub-verb %q (%s)",
		tokens[0], tokens[2], strings.Join(subs, " | "))
}
