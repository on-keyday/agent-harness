//go:build !js

package main

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// whoami reads its own grant-condition bodies through the listing (the
// whoami answer carries only their markers), so its declaration must name the
// List it sends: the command line refuses an undeclared kind before the wire
// (cli.WithExpectedKinds), and the bodies then read "(condition unknown)" on a
// task that has them (found live on a dummy, 2026-11-26).
func TestWhoamiDeclaresTheListItReadsConditionsThrough(t *testing.T) {
	sp, ok := cliSpecFor([]string{"whoami"})
	if !ok {
		t.Fatal("whoami has no declaration")
	}
	has := map[protocol.TaskControlKind]bool{}
	for _, k := range sp.Sends {
		has[k] = true
	}
	for _, k := range []protocol.TaskControlKind{protocol.TaskControlKind_Whoami, protocol.TaskControlKind_List} {
		if !has[k] {
			t.Errorf("whoami's Sends lacks %v", k)
		}
	}
}
