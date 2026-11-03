package verb

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// Every operator-facing listing takes --json, so a hand that learned it on
// one is not refused by another. The generated usage put them side by side
// and the odd ones out became readable:
//
//	ls          [--json] [--tree]
//	conns       [--json] [--follow|-f]
//	exec ls     [--task TASK] [--json]
//	forward ls  [--task TASK] [--json]
//	session ls                            <- JSON either way, and refused it
//	board topics                          <- text only, while board read had it
//	board subscribers                     <- likewise
//
// The agent family has its own test below.
func TestEveryOperatorListingTakesJSON(t *testing.T) {
	for _, path := range []string{
		"ls", "conns", "exec ls", "forward ls", "session ls",
		"board topics", "board read", "board subscribers",
	} {
		v, ok := Lookup(strings.Fields(path)...)
		if !ok {
			t.Errorf("%s: not in the table", path)
			continue
		}
		found := false
		for _, f := range v.Flags {
			if f.Name == "json" {
				found = true
				if strings.TrimSpace(f.Help) == "" {
					t.Errorf("%s: --json has no help", path)
				}
			}
		}
		if !found {
			t.Errorf("`%s` has no --json. Every other operator listing takes one, "+
				"so a hand that types it here gets `flag provided but not defined` "+
				"from the one verb that differs.", path)
		}
	}
}

// session ls is the one that takes it WITHOUT changing anything, and that has
// to be said out loud. An option accepted and dropped in silence is the
// defect this table exists to remove; the cure is to describe it.
func TestSessionLsSaysItsJSONFlagChangesNothing(t *testing.T) {
	v, ok := Lookup("session", "ls")
	if !ok {
		t.Fatal("session ls is not in the table")
	}
	for _, f := range v.Flags {
		if f.Name != "json" {
			continue
		}
		if !strings.Contains(f.Help, "either way") {
			t.Errorf("session ls --json help does not say the output is unchanged: %q\n"+
				"A flag that does nothing must SAY so, or it reads as one that was dropped.", f.Help)
		}
		return
	}
	t.Fatal("session ls has no --json")
}

// The agent read verbs emit JSON Lines with no text form, so they were once
// left without --json on the grounds that there was nothing to opt out of.
// But the skill teaches `agent inbox --json` (inbox already took the flag),
// and an agent carrying that to `agent subscriptions` was refused with
// `flag provided but not defined: -json` (2026-11-03). Each one accepts it,
// says it changes nothing, and parses with it. `agent thread` is not here:
// its --json does switch the output from text.
func TestAgentJSONOnlyVerbsAcceptJSONAndSaySo(t *testing.T) {
	for _, tc := range []struct {
		sub  string
		args []string // the least line the verb's rules accept
	}{
		{"inbox", nil},
		{"wait", nil},
		{"topics", nil},
		{"archives", []string{"--topic", "t"}},
		{"subscriptions", nil},
		{"contacts", nil},
		{"retained", nil},
		{"read", []string{"--topic", "t"}},
	} {
		v, ok := Lookup("agent", tc.sub)
		if !ok {
			t.Errorf("agent %s: not in the table", tc.sub)
			continue
		}
		var help string
		for _, f := range v.Flags {
			if f.Name == "json" {
				help = f.Help
			}
		}
		if !strings.Contains(help, "either way") {
			t.Errorf("agent %s --json: help %q does not say the output is unchanged", tc.sub, help)
		}
		sp := v.For(CLI)
		fs := sp.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		b, err := sp.Parse(fs, append(append([]string{}, tc.args...), "--json"))
		if err != nil {
			t.Errorf("`agent %s --json`: %v", tc.sub, err)
			continue
		}
		if _, err := sp.BuildFunc()(b); err != nil {
			t.Errorf("`agent %s --json`: build: %v", tc.sub, err)
		}
	}
}

// And it must actually parse, on both surfaces that declare the verb.
func TestSessionLsAcceptsJSON(t *testing.T) {
	for _, sf := range []Surface{CLI, TUI} {
		if _, err := ParseCmdSessionLs(sf, []string{"--json"}, nil); err != nil {
			t.Errorf("surface %v: `session ls --json`: %v", sf, err)
		}
		if _, err := ParseCmdSessionLs(sf, nil, nil); err != nil {
			t.Errorf("surface %v: bare `session ls`: %v", sf, err)
		}
	}
}
