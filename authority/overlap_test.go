package authority

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The warning fires when a bit that can change an action's inputs reaches a
// target the ActionSet-conditioned exec_run reaches, and only then.
func TestActionOverlap(t *testing.T) {
	const self, child, other = "aa", "bb", "cc"
	children := map[string][]string{self: {child}}
	runOn := func(ids ...string) ScopeOverride {
		return ScopeOverride{Caps: protocol.Capability_ExecRun, Base: protocol.ScopeBase_None, ExcludeSelf: true, IDs: ids,
			Cond: &Condition{Kind: protocol.ConditionKind_ActionSet, Actions: [][16]byte{{1}}}}
	}
	caps := protocol.Capability_ExecRun | protocol.Capability_FileWrite
	inside := Scope{Base: protocol.ScopeBase_Subtree, Overrides: []ScopeOverride{runOn(child)}}
	if got := ActionOverlap(self, caps, inside, children); got != protocol.Capability_FileWrite {
		t.Fatalf("child in file_write's subtree: %s, want file_write", CapsLabel(got))
	}
	outside := Scope{Base: protocol.ScopeBase_Subtree, Overrides: []ScopeOverride{runOn(other)}}
	if got := ActionOverlap(self, caps, outside, children); got != 0 {
		t.Fatalf("exec_run on a task outside file_write's subtree: %s, want none", CapsLabel(got))
	}
	plain := Scope{Base: protocol.ScopeBase_Subtree}
	if got := ActionOverlap(self, caps, plain, children); got != 0 {
		t.Fatalf("unconditioned exec_run: %s, want none", CapsLabel(got))
	}
	if got := ActionOverlap(self, protocol.Capability_FileWrite, inside, children); got != 0 {
		t.Fatalf("exec_run not held: %s, want none", CapsLabel(got))
	}
	ctl := protocol.Capability_ExecRun | protocol.Capability_ExecControl
	if got := ActionOverlap(self, ctl, inside, children); got != protocol.Capability_ExecControl {
		t.Fatalf("exec_control: %s, want exec_control", CapsLabel(got))
	}
}
