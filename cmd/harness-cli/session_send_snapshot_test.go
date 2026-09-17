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

// The RENDER flags are rejected without --snapshot BEFORE anything dials, so
// this needs no server: the check sits between flag parsing and the usage
// check, which is also why a zero ConnectionID is safe here.
//
// The property under test is the one this repo keeps re-learning: a typed
// option either takes effect or errors. Each of these shapes a screen, so with
// no screen there is nothing for it to take effect on, and silently doing
// neither is worse than either.
//
// --settle-ms was in this list until 2026-09-18 and is deliberately not any
// more; see TestSessionSendSettleMsWaitsWithoutSnapshot for why the same rule
// moved it out.
func TestSessionSendRejectsRenderFlagsWithoutSnapshot(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"rows", []string{"--rows", "10", id, "x"}, "--rows"},
		{"cols", []string{"--cols", "10", id, "x"}, "--cols"},
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

// --settle-ms is a DURATION, not a property of a render: time passes whether or
// not the screen is photographed, so it takes effect on its own rather than
// being refused. The same "takes effect or errors" rule that refuses the
// others is what admits this one.
//
// It was refused, and the refusal cost more than it saved: the send did not
// happen either, and the send is this verb's job. An operator — or an agent
// that discards stderr — then sees a command that did nothing at all, with the
// one line explaining why thrown away. That happened repeatedly.
func TestSessionSendSettleMsWaitsWithoutSnapshot(t *testing.T) {
	// A deliberately WRONG arity so the run stops at the usage check instead of
	// dialling. Reaching that error is the assertion: the render-flag guard did
	// not fire on --settle-ms.
	err := sendLine("--settle-ms", "10", id0)
	if err == nil {
		t.Fatal("want the usage error, got none")
	}
	// The GUARD's own wording, not the bare word "--snapshot": the usage text
	// lists every flag, so the looser check passes for the wrong reason —
	// which is what the neighbouring test says three lines up and what I wrote
	// anyway on the first try.
	if strings.Contains(err.Error(), "needs --snapshot") {
		t.Errorf("--settle-ms was refused for want of --snapshot: %v", err)
	}
	if !strings.Contains(err.Error(), "usage: session send") {
		t.Errorf("want the usage error, got %v", err)
	}
}

// Unset means no extra wait. The flag's default moved to 0 for exactly this:
// a plain send must not silently gain the 1500ms the snapshot path collects
// for, which is what a non-zero default would have done the moment the wait
// stopped being gated on --snapshot.
func TestSessionSendSettleMsDefaultsToNoWait(t *testing.T) {
	a, err := verb.ParseCmdSessionSend(verb.CLI, []string{id0, "x"}, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.SettleMs != 0 {
		t.Errorf("SettleMs = %d with the flag unset, want 0: a plain send would wait for it", a.SettleMs)
	}
}
