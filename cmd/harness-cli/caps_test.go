//go:build !js

package main

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Whether --caps was TYPED is not the same question as what it parsed to:
// `--caps none` and an omitted --caps both mean Capability_None, and on a
// --resume the first re-grants that mask while the second keeps the task's
// persisted caps. Getting them confused silently widens or narrows a resumed
// task's authority.
//
// This used to be checked against capsExplicitlySet, a helper reading a
// hand-built flag.FlagSet that the binary stopped constructing when the spawn
// verbs moved onto the declaration. The presence bit is PresenceField in the
// table now; the property is the same, so it is asserted where it lives.
func TestCapsPresenceIsSeparateFromItsValue(t *testing.T) {
	id := strings.Repeat("ab", 16)
	parse := func(t *testing.T, args ...string) verb.SpawnAction {
		t.Helper()
		act, handled, err := verb.ParseCLICommand(append([]string{"submit"}, args...), nil)
		if !handled || err != nil {
			t.Fatalf("submit %v: handled=%t err=%v", args, handled, err)
		}
		return act.(verb.SpawnAction)
	}

	omitted := parse(t, "--repo", "/r", "hello")
	if omitted.CapsPresent {
		t.Error("no --caps: CapsPresent must be false")
	}
	if omitted.Caps != nil {
		t.Errorf("no --caps: Caps = %v, want nil", *omitted.Caps)
	}

	// The case the two questions come apart on: an explicit "none".
	explicit := parse(t, "--repo", "/r", "--caps", "none", "hello")
	if !explicit.CapsPresent {
		t.Error("--caps none: CapsPresent must be true — it was typed")
	}
	if explicit.Caps == nil || *explicit.Caps != protocol.Capability_None {
		t.Errorf("--caps none: Caps = %v, want an explicit None", explicit.Caps)
	}

	// And a resume is where the difference is spent: only a typed --caps
	// re-grants, which spawnOpts reads off exactly this bit.
	resumed := parse(t, "--resume", id, "--caps", "none", "hello")
	if !spawnOpts(resumed).ResumeCapsOverride {
		t.Error("--resume with a typed --caps must re-grant that mask")
	}
	kept := parse(t, "--resume", id, "hello")
	if spawnOpts(kept).ResumeCapsOverride {
		t.Error("--resume without --caps must keep the task's persisted caps")
	}
}
