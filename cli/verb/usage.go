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
		// Aliases included: `-r` is how `file push -r` is typed, and a
		// synopsis naming only --recursive describes a longer spelling of a
		// flag whose short form is the one in every example. The spelling
		// itself comes from flagLabel, shared with the flags block below;
		// joined with | rather than its comma, because inside brackets a comma
		// reads as "and also".
		b.WriteString(strings.ReplaceAll(flagLabel(f), ", ", "|"))
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

// flagLabel is how one flag is spelled wherever it is LISTED: every accepted
// spelling, then the value placeholder for a flag that takes one.
//
// Built once because two renderers spell it -- Usage() inside brackets with |
// between the spellings, HelpBlock in a column with dashList's comma -- and a
// synopsis naming a flag differently from the list underneath it describes two
// flags where the parser has one.
func flagLabel(f Flag) string {
	lbl := dashList(append([]string{f.Name}, f.Aliases...))
	if f.Type != FlagBool {
		lbl += " " + strings.ToUpper(f.Name)
	}
	return lbl
}

// helpFlagColumn caps how far the description column is pushed right. A verb
// whose longest spelling is wider than this puts that one flag on its own
// line, rather than indenting every other description past it: `--agent-arg,
// --claude-arg AGENT-ARG` is 35 columns and would otherwise move `--task`'s
// description a third of the way across the terminal.
const helpFlagColumn = 24

// HelpBlock renders the answer to `<verb> -h`: everything UsageLines carries,
// then one line per declared flag with the Help the declaration holds.
//
// That Help reached no operator before. `--help` rendered UsageLines and
// nothing called the flag package's PrintDefaults, so every declared
// description -- including the one that says which of splice / forwarded /
// direct a transfer takes and what each costs, in measured numbers -- was
// written, reviewed and maintained where only the FlagSet could read it. An
// author who noticed wrote the flag's prose into Notes instead, which is how a
// flag's description came to sit in two places with the copy beside the flag
// being the unread one.
//
// Deliberately NOT in the full listings (the CLI's usage(), the TUI's help,
// HelpLines for the WebUI): those are one line per verb across every declared
// path, and every flag's description there would bury the grammar it exists to
// explain. Detail is what naming ONE verb buys.
//
// width is the terminal or panel width the caller knows and this package does
// not. 0 means do not wrap: what a pipe gets, because the reader's pager
// decides how to fold, and what the browser passes, because its output pane is
// a <pre> that scrolls rather than wraps -- the same treatment every other long
// line printed there already gets.
func (v VerbSpec) HelpBlock(width int) []string {
	lines := v.UsageLines()
	out := []string{lines[0]}
	for _, n := range lines[1:] {
		out = append(out, wrapHanging("  ", 2, width, n)...)
	}
	if len(v.Flags) == 0 {
		return out
	}
	// The column is the widest spelling that FITS it, not the widest spelling:
	// one long flag must not push every description right.
	labels := make([]string, len(v.Flags))
	namew := 0
	for i, f := range v.Flags {
		labels[i] = flagLabel(f)
		if n := len(labels[i]); n > namew && n <= helpFlagColumn {
			namew = n
		}
	}
	indent := 2 + namew + 2
	out = append(out, "", "flags:")
	for i, f := range v.Flags {
		if len(labels[i]) > namew {
			out = append(out, "  "+labels[i])
			out = append(out, wrapHanging(strings.Repeat(" ", indent), indent, width, f.Help)...)
			continue
		}
		first := "  " + labels[i] + strings.Repeat(" ", namew-len(labels[i])+2)
		out = append(out, wrapHanging(first, indent, width, f.Help)...)
	}
	return out
}

// wrapHanging lays text out with `first` in front of its first line and
// `indent` spaces in front of every continuation, so a description stays in
// its column instead of wrapping back to the left margin.
//
// width <= 0 returns one line: a surface that does not know its width must not
// have one guessed for it. A guessed column count is wrong for whatever folds
// the line afterwards, and being wrong there is worse than not folding.
func wrapHanging(first string, indent, width int, text string) []string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return []string{strings.TrimRight(first, " ")}
	}
	// Under 20 columns of room the wrap produces two or three words a line,
	// which is harder to read than letting the terminal fold it.
	if width <= 0 || width-indent < 20 {
		return []string{first + text}
	}
	var out []string
	line, prefix, started := first, strings.Repeat(" ", indent), false
	for _, w := range strings.Fields(text) {
		switch {
		case !started:
			line += w
			started = true
		case len(line)+1+len(w) > width:
			out = append(out, line)
			line = prefix + w
		default:
			line += " " + w
		}
	}
	return append(out, line)
}

// HelpLines renders every verb declared for one surface: the generated
// synopsis, then that verb's declared Notes indented under it.
//
// It exists because a surface that cannot loop over the table itself has to
// restate it, and a restatement rots. The WebUI held the whole command list
// twice by hand — index.html's placeholder and main.js — and `--via <cid>`
// survived there after the flag started taking a runner identity, while the CLI
// and TUI were already correct because their usage is generated. The wasm
// bridge calls this and hands the lines to JS, so the browser reads the same
// declaration the parser does.
//
// Deliberately plainer than the CLI's usage() and the TUI's cmdlineHelpLines:
// those interleave family prose and key bindings that only they have. What is
// shared is the part that must not disagree — which verbs exist, how they are
// spelled, and what their flags are called.
func HelpLines(s Surface) []string {
	var out []string
	for _, path := range PathsForSurface(s) {
		sp, ok := Lookup(strings.Fields(path)...)
		if !ok {
			continue
		}
		lines := sp.For(s).UsageLines()
		out = append(out, "  "+lines[0])
		for _, n := range lines[1:] {
			out = append(out, "      "+n)
		}
	}
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
