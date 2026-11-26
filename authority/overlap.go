package authority

import "github.com/on-keyday/agent-harness/runner/protocol"

// OverlapBits are the bits that can change what an action does from outside
// it: writing the files the command reads, typing into the task's session
// (exec_control implies exec_cowrite). An unconditioned exec_run is not
// listed: one scope carries at most one override per bit, so a grant cannot
// hold exec_run both conditioned and not.
const OverlapBits = protocol.Capability_FileWrite | protocol.Capability_ExecCowrite | protocol.Capability_ExecControl

// ActionOverlap is the § Surfaces warning of the grant conditions spec: the
// bits of OverlapBits that caps holds over a target an ActionSet-conditioned
// exec_run also reaches. An action is only as narrow as the files it reads, so
// a holder of such a bit there can change what the action runs. 0 when
// exec_run is not held or not ActionSet-conditioned. Target sets are scope's,
// over children (a client passes the tasks it can see).
func ActionOverlap(selfHex string, caps protocol.Capability, s Scope, children map[string][]string) protocol.Capability {
	run := protocol.Capability_ExecRun
	if caps&run == 0 {
		return 0
	}
	if c := s.CondFor(run); c == nil || c.Kind != protocol.ConditionKind_ActionSet {
		return 0
	}
	rAll, rSet := ActionSet(selfHex, s, run, children)
	var out protocol.Capability
	for _, bit := range []protocol.Capability{protocol.Capability_FileWrite, protocol.Capability_ExecCowrite, protocol.Capability_ExecControl} {
		if caps&bit == 0 {
			continue
		}
		all, set := ActionSet(selfHex, s, bit, children)
		if setsMeet(rAll, rSet, all, set) {
			out |= bit
		}
	}
	return out
}

func setsMeet(aAll bool, a map[string]bool, bAll bool, b map[string]bool) bool {
	nonEmpty := func(m map[string]bool) bool {
		for _, in := range m {
			if in {
				return true
			}
		}
		return false
	}
	switch {
	case aAll && bAll:
		return true
	case aAll:
		return nonEmpty(b)
	case bAll:
		return nonEmpty(a)
	}
	for id, in := range a {
		if in && b[id] {
			return true
		}
	}
	return false
}
