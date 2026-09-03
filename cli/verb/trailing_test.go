package verb

import (
	"flag"
	"io"
	"testing"
)

// TestSessionSendEnterAndEAreDifferentFlags is the test this whole alias rule
// exists for.
//
// cmd/harness-cli/session.go registers --enter (append a carriage return) and
// -e (interpret backslash escapes) as two independent flags. Anything that
// paired short with long by spelling would merge them, and `session send -e
// 'x'` would then type a spurious Enter into a LIVE PTY -- while compiling,
// reviewing cleanly, and passing every test that did not check this.
func TestSessionSendEnterAndEAreDifferentFlags(t *testing.T) {
	sp, ok := Lookup("session", "send")
	if !ok {
		t.Fatal("session send is not in the table")
	}
	send := func(t *testing.T, args ...string) SendAction {
		t.Helper()
		fs := sp.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		b, err := sp.Parse(fs, args)
		if err != nil {
			t.Fatalf("Parse(%q): %v", args, err)
		}
		act, err := sp.Build(b)
		if err != nil {
			t.Fatalf("Build(%q): %v", args, err)
		}
		return act.(SendAction)
	}

	if a := send(t, "-e", idA, "hi"); !a.Interp || a.Enter {
		t.Errorf("-e set Enter=%v Interp=%v; -e is escape interpretation, NOT Enter", a.Enter, a.Interp)
	}
	if a := send(t, "--enter", idA, "hi"); !a.Enter || a.Interp {
		t.Errorf("--enter set Enter=%v Interp=%v; --enter appends a CR and interprets nothing", a.Enter, a.Interp)
	}
	if a := send(t, "-e", "--enter", idA, "hi"); !a.Enter || !a.Interp {
		t.Errorf("both flags together set Enter=%v Interp=%v; want both", a.Enter, a.Interp)
	}
	// And the declared alias grouping must show them apart.
	fs := sp.NewFlagSet(flag.ContinueOnError)
	groups := groupByValuePointer(fs)
	for _, g := range groups {
		if len(g) > 1 {
			for _, n := range g {
				if n == "e" || n == "enter" {
					t.Errorf("enter/e share a value with %v; they are separate flags", g)
				}
			}
		}
	}
}

// TestTrailingVerbsKeepTextLiteral: a '-'-leading word in the tail is text,
// not a flag, which is the whole reason these verbs cannot permute.
func TestTrailingVerbsKeepTextLiteral(t *testing.T) {
	sp, _ := Lookup("session", "send")
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, []string{"--enter", idA, "--not-a-flag", "-x"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := mustBuild(t, sp, b).(SendAction)
	if a.Text != "--not-a-flag -x" {
		t.Errorf("Text = %q, want the '-'-leading words kept literally", a.Text)
	}
	if !a.Enter {
		t.Error("--enter before the id should still be read")
	}
}

// TestAgentSendIsTrailing pins the pair the existing guard could not see:
// `agent send` and `agent dispatch` take a joined-positional payload, so they
// are Trailing verbs, and cli/flagorder_test.go's allowlist never listed them
// because they read their positionals inside resolvePayload rather than off
// the FlagSet.
func TestAgentSendIsTrailing(t *testing.T) {
	for _, path := range [][]string{{"agent", "send"}, {"agent", "dispatch"}} {
		sp, ok := Lookup(path...)
		if !ok {
			t.Fatalf("%v is not in the table", path)
		}
		if sp.Trailing == nil {
			t.Errorf("%s: takes a joined-positional payload, so it must be Trailing", sp.FlagSetName())
		}
	}
}
