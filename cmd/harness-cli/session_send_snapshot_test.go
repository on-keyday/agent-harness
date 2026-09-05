//go:build !js

package main

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/objtrsf/objproto"
)

// sendLine drives the line through the SAME parse the dispatch uses, then the
// same body. It used to call runSessionSend, a wrapper that parsed the argv
// itself -- and once the CLI moved onto the generated dispatch, nothing but
// this test called it. A test is not coverage of a path the binary no longer
// takes.
func sendLine(args ...string) error {
	a, err := verb.ParseCmdSessionSend(verb.CLI, args, nil)
	if err != nil {
		return err
	}
	return runSessionSendWith(objproto.ConnectionID{}, a)
}

// The snapshot-only flags are rejected without --snapshot BEFORE anything
// dials, so this needs no server: the check sits between flag parsing and the
// usage check, which is also why a zero ConnectionID is safe here.
//
// The property under test is the one this repo keeps re-learning: a typed
// option either takes effect or errors. `--settle-ms 5000` without --snapshot
// reads exactly like "wait 5s then show me", and silently doing neither is
// worse than either.
func TestSessionSendRejectsSnapshotFlagsWithoutSnapshot(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"rows", []string{"--rows", "10", id, "x"}, "--rows"},
		{"cols", []string{"--cols", "10", id, "x"}, "--cols"},
		{"settle-ms", []string{"--settle-ms", "10", id, "x"}, "--settle-ms"},
		{"style", []string{"--style", id, "x"}, "--style"},
		{"several at once", []string{"--rows", "10", "--style", id, "x"}, "--rows, --style"},
	} {
		err := sendLine(tc.args...)
		if err == nil {
			t.Errorf("%s: want an error, got none", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %q", tc.name, err, tc.want)
		}
		if !strings.Contains(err.Error(), "--snapshot") {
			t.Errorf("%s: error %q does not say which flag would make it apply", tc.name, err)
		}
	}
}

// A flag left at its default is not "given": fs.Visit is what tells the two
// apart, and getting it wrong would reject every plain send.
func TestSessionSendPlainFormIsNotRejected(t *testing.T) {
	// No snapshot flags, but a deliberately WRONG arity so the run stops at the
	// usage check instead of dialling. Reaching that error is the assertion:
	// the stray-flag guard did not fire.
	err := sendLine("-enter", id0)
	if err == nil {
		t.Fatal("want the usage error, got none")
	}
	// Match the guard's own wording, not the word "--snapshot": the usage text
	// mentions the flag too, so the looser check passes for the wrong reason.
	if strings.Contains(err.Error(), "take effect only with") {
		t.Errorf("plain send was rejected as if it carried snapshot flags: %v", err)
	}
	if !strings.Contains(err.Error(), "usage: session send") {
		t.Errorf("want the usage error, got %v", err)
	}
}

const id0 = "0123456789abcdef0123456789abcdef"
