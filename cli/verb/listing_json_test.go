package verb

import (
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
// The agent family is deliberately absent: those verbs run inside a task's
// shell and emit JSON Lines by construction, with no text form to opt out of.
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
