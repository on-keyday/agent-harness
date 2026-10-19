package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The audit's scope kind restates which bits no task scope bounds; this keeps
// it in step with the classification the gates are checked against.
func TestAuditScopeKindAgreesWithTargetClasses(t *testing.T) {
	for bit, class := range capTargetClasses {
		noTarget := class.kind == capNoTargetResolution
		if (verb.CapScopeKindOf(bit) == verb.ScopeNone) != noTarget {
			t.Errorf("%s: audit scope kind ScopeNone=%v, server capNoTargetResolution=%v", bit, verb.CapScopeKindOf(bit) == verb.ScopeNone, noTarget)
		}
	}
}

// The audit's "also done by" notes restate the implications the gates apply.
func TestAuditImpliedByAgreesWithGates(t *testing.T) {
	var someID protocol.TaskID
	someID.Id[0] = 1
	mask := map[protocol.Capability]protocol.Capability{
		protocol.Capability_ExecView:    attachModeCap(protocol.AttachMode_View),
		protocol.Capability_ExecCowrite: attachModeCap(protocol.AttachMode_Cowrite),
		protocol.Capability_ResumeExact: resumeRequestCap(someID, true),
	}
	for bit, m := range mask {
		impliers := verb.CapImpliedBy(bit)
		if len(impliers) == 0 {
			t.Errorf("%s: no implications listed", bit)
		}
		for _, imp := range impliers {
			if !hasAnyCap(imp, m) {
				t.Errorf("%s is listed as implied by %s, but the gate does not accept %s", bit, imp, imp)
			}
		}
	}
}
