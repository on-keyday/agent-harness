//go:build !js

package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// These properties used to be checked against helpers in main.go --
// isTaskIDLike, forwardWConflictsWithLR, tapMode -- that the binary stopped
// calling when the forward family moved onto the declaration. The tests
// passed and the code was unreachable, which is the worst of both: nothing
// reported the dead helper, and nothing covered the live rule.
//
// Same properties, asserted through the parse an operator actually reaches.

// `forward ls` / `kill` / `tap` are their own declared paths, so a sub-verb
// can never be read as the task id of a bare `forward`.
func TestForwardSubVerbsAreNotTaskIDs(t *testing.T) {
	id := strings.Repeat("ab", 16)
	// Each sub-verb with the arguments IT declares -- `ls` takes none (it
	// filters with --task), the other two take an id of their own kind.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"forward", "ls"}, "verb.ForwardLsAction"},
		{[]string{"forward", "kill", "7"}, "verb.ForwardKillAction"},
		{[]string{"forward", "tap", "7"}, "verb.ForwardTapAction"},
	} {
		act, handled, err := verb.ParseCLICommand(tc.args, nil)
		if !handled || err != nil {
			t.Errorf("%v: handled=%t err=%v", tc.args, handled, err)
			continue
		}
		if _, isOpen := act.(verb.ForwardOpenAction); isOpen {
			t.Errorf("%v parsed as the OPEN form, so the sub-verb was read as a task id", tc.args)
		}
		if got := fmt.Sprintf("%T", act); got != tc.want {
			t.Errorf("%v = %s, want %s", tc.args, got, tc.want)
		}
	}
	// And the open form still takes one.
	act, _, err := verb.ParseCLICommand([]string{"forward", id, "-L", "1:h:2"}, nil)
	if err != nil {
		t.Fatalf("forward <id> -L: %v", err)
	}
	if fo, ok := act.(verb.ForwardOpenAction); !ok || fo.TaskID != id {
		t.Fatalf("open form = %#v, want TaskID %s", act, id)
	}
}

// -W is the stdio forward: no local listener, this process's stdin/stdout IS
// the endpoint. Combining it with -L or -R asks for two different things on
// one connection, and the declaration refuses it.
func TestForwardWConflictsWithLR(t *testing.T) {
	id := strings.Repeat("cd", 16)
	if _, _, err := verb.ParseCLICommand([]string{"forward", id, "-W", "h:1"}, nil); err != nil {
		t.Fatalf("-W alone must parse: %v", err)
	}
	for _, other := range [][]string{{"-L", "1:h:2"}, {"-R", "1:h:2"}} {
		args := append([]string{"forward", id, "-W", "h:1"}, other...)
		if _, _, err := verb.ParseCLICommand(args, nil); err == nil {
			t.Errorf("-W with %s parsed; it must be refused", other[0])
		}
	}
}

// The four render flags name one mode. Two at once is a contradiction, not a
// ranking, and the default is hex.
func TestForwardTapRenderModes(t *testing.T) {
	parse := func(t *testing.T, flags ...string) verb.ForwardTapAction {
		t.Helper()
		act, _, err := verb.ParseCLICommand(append([]string{"forward", "tap", "7"}, flags...), nil)
		if err != nil {
			t.Fatalf("forward tap %v: %v", flags, err)
		}
		return act.(verb.ForwardTapAction)
	}
	if m := tapModeByName(parse(t).Mode); m != cli.TapHex {
		t.Errorf("no flag = %v, want hex", m)
	}
	if m := tapModeByName(parse(t, "--json").Mode); m != cli.TapJSON {
		t.Errorf("--json = %v, want json", m)
	}
	if _, _, err := verb.ParseCLICommand([]string{"forward", "tap", "7", "--hex", "--raw"}, nil); err == nil {
		t.Fatal("--hex with --raw must be refused, not silently ranked")
	}
}
