package verb

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// Every cross-flag rule that used to live inside a hand-written Build, pinned
// as a line the parse must refuse.
//
// This file exists because of how those rules were lost. Rewriting ten Builds
// into declarations dropped two of `session batch`'s three rules and one of
// `agent batch`'s, and nothing about deleting a Build says a rule went with
// it: the code compiles, the tests pass, and the option quietly starts being
// accepted. Both were found by reading the deleted code against the new
// declaration -- by hand, once, which is not a thing that keeps working.
//
// A rule with no test here is a rule the next refactor deletes for free.
func TestDeclaredRulesRefuseWhatTheBuildsRefused(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		verb string
		args []string
		want string // a substring the message must carry
	}{
		// forward: -W owns the foreground, -L/-R are listeners.
		{"forward", []string{id, "-W", "h:1", "-L", "1:h:2"}, "mutually exclusive"},
		{"forward", []string{id, "-W", "h:1", "-R", "1:h:2"}, "mutually exclusive"},
		{"forward", []string{id}, "at least one of -L, -R, -W"},
		{"forward", []string{id, "-L", "1:h:2", "--http-path", "/x"}, "-W"},

		// forward tap: one direction, one render mode.
		{"forward tap", []string{"7", "--dir", "sideways"}, "want to-target"},
		{"forward tap", []string{"7", "--text", "--json"}, "mutually exclusive"},
		{"forward tap", []string{"7", "--raw"}, "explicit --dir"},

		// session send: the snapshot knobs mean nothing without --snapshot,
		// and every orphan is named at once.
		{"session send", []string{"--rows", "10", "--style", id, "x"}, "--rows, --style need --snapshot"},
		{"session send", []string{id}, "is required"},

		{"session exec", []string{id}, "is required"},
		{"session stream turn", []string{id}, "is required"},

		// caps set: something to change, and a base to narrow.
		{"caps set", []string{id}, "at least one of --caps, --scope"},
		{"caps set", []string{id, "--caps", "none", "--scope-for", "spawn=none"}, "needs --scope"},

		// caps set-parent: one destination, not none and not two.
		{"caps set-parent", []string{id}, "exactly one"},
		{"caps set-parent", []string{id, "--none", "--swap"}, "exactly one"},

		// spawn: the prompt, the terminal rules, the one-runner rule.
		{"submit", []string{"--repo", "/r"}, "a prompt is required"},
		{"session new", []string{"--repo", "/r", "--x11", "--detach"}, "mutually exclusive"},
		{"session new", []string{"--repo", "/r", "--stream"}, "needs --detach"},
		{"session new", []string{"--repo", "/r", "--stream", "--detach", "--x11"}, "mutually exclusive"},
		{"session new", []string{"--repo", "/r", "--x11", "--x11-display", "100"}, "0..99"},
		{"session new", []string{"--repo", "/r", "--runner", "a", "--host", "b"}, "mutually exclusive"},
		{"interactive", []string{"--repo", "/r", "--runner", "a", "--ip", "1.2.3.4"}, "mutually exclusive"},

		// grid: --descendants needs something to take the descendants OF, and
		// --under names ONE subtree.
		{"grid", []string{"--descendants"}, "needs --under"},
		{"grid", []string{"--under", id, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, "drop the extra ids"},

		// A kill with no id is a mistyped line, not a request to kill nothing.
		{"exec kill", nil, "at least"},
		{"forward kill", nil, "at least"},

		// session snapshot: three refusals that were deleted with its Build and
		// only came back after an audit read the deleted lines.
		{"session snapshot", []string{id, "--raw", "--ansi"}, "mutually exclusive"},
		{"session snapshot", []string{id, "--json", "--ansi"}, "mutually exclusive"},
		{"session snapshot", []string{id, "--detect-agent", "codex"}, "needs --detect"},

		// session stream approve: --message is the DENY reason.
		{"session stream approve", []string{id, "req-1", "--allow", "--message", "why"}, "mutually exclusive"},

		// forward tap --max-bytes is uint32 on the wire; a larger number was
		// silently truncated (4294967297 read as a 1-byte cut).
		{"forward tap", []string{"7", "--max-bytes", "4294967297"}, "out of range"},

		// board retract has no whole-topic form, so --seq 0 is not an answer.
		{"board retract", []string{"chat.abcd1234", "--seq", "0"}, "non-zero"},

		// The agent verbs name a topic or their own, never both.
		{"agent subscribe", []string{"--self", "--topic", "chat.abcd1234"}, "mutually exclusive"},
	} {
		v, ok := Lookup(strings.Fields(tc.verb)...)
		if !ok {
			t.Errorf("%s: not in the table", tc.verb)
			continue
		}
		err := parseAndBuild(v, tc.args)
		if err == nil {
			t.Errorf("`%s %s` is accepted; it was refused before the migration (want %q)",
				tc.verb, strings.Join(tc.args, " "), tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("`%s %s`: %v\n  does not carry %q", tc.verb, strings.Join(tc.args, " "), err, tc.want)
		}
	}
}

// TestDeclaredRulesAcceptTheOrdinaryForms is the other half: a rule written
// one notch too strict refuses a line that always worked, and a test that only
// checks refusals cannot see it.
func TestDeclaredRulesAcceptTheOrdinaryForms(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		verb string
		args []string
	}{
		{"forward", []string{id, "-L", "1:h:2", "-R", "2:h:3"}}, // -L WITH -R is ordinary
		{"forward", []string{id, "-W", "h:1", "--http-path", "/x"}},
		{"forward tap", []string{"7"}}, // no mode named = hex
		{"forward tap", []string{"7", "--raw", "--dir", "to-target"}},
		{"session send", []string{id, "hello"}},
		{"session send", []string{"--snapshot", "--rows", "10", id, "hello"}},
		{"submit", []string{"--repo", "/r", "--task", "do it"}},
		{"submit", []string{"--repo", "/r", "do", "it"}}, // the prompt as trailing words
		{"session new", []string{"--repo", "/r", "--stream", "--detach"}},
		{"grid", nil},
		{"grid", []string{"--under", id, "--descendants"}},
		{"grid", []string{id}},
		{"caps set", []string{id, "--scope", "subtree", "--scope-for", "spawn=none"}},
		{"caps set-parent", []string{id, "--none"}},
		{"session snapshot", []string{id, "--raw"}},
		{"session snapshot", []string{id, "--detect", "--detect-agent", "codex"}},
		{"session stream approve", []string{id, "req-1", "--allow"}},
		{"session stream approve", []string{id, "req-1", "--deny", "--message", "no"}},
		{"session stream approve", []string{id, "req-1", "--allow", "--suggestion", "0"}},
		{"forward tap", []string{"7", "--max-bytes", "4096"}},
		{"board retract", []string{"chat.abcd1234", "--seq", "5"}},
	} {
		v, ok := Lookup(strings.Fields(tc.verb)...)
		if !ok {
			t.Errorf("%s: not in the table", tc.verb)
			continue
		}
		if err := parseAndBuild(v, tc.args); err != nil {
			t.Errorf("`%s %s` is refused: %v", tc.verb, strings.Join(tc.args, " "), err)
		}
	}
}

// parseAndBuild runs the whole path a surface runs: parse, the declared
// checks, Validate, then the generated build.
func parseAndBuild(v VerbSpec, args []string) error {
	fs := v.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := v.Parse(fs, args)
	if err != nil {
		return err
	}
	_, err = v.BuildFunc()(b)
	return err
}
