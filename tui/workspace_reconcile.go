package tui

import "github.com/on-keyday/agent-harness/cli/workspace"

// forwardPlan is the difference between what a workspace declares for one task
// and what is already running for it.
type forwardPlan struct {
	Start   []string              // declared values with nothing running
	Release []*PortForwardSession // workspace-owned sessions no longer declared
}

// sessionIdentity is the forward a session is, by the key a declaration is
// compared with. PortForwardSession.Spec holds the spec without its flag and
// Direction holds the flag, so both go in: matching the spec alone would let a
// -R satisfy a declared -L. An ip link is never a declarable forward.
func sessionIdentity(s *PortForwardSession) (workspace.ForwardKey, bool) {
	if s.Direction == ForwardLink {
		return workspace.ForwardKey{}, false
	}
	k, err := workspace.ForwardIdentity(s.Direction.flag() + " " + s.Spec)
	return k, err == nil
}

// planForwards computes the difference for one task. Reconciling rather than
// restarting is what makes `workspace apply` usable as the recovery from a port
// conflict: the forwards that ARE working must survive the apply that retries
// the one that is not.
//
// Declarations and sessions are compared by workspace.ForwardIdentity, never
// by spelling: a session typed as `3000` and the long form a save renders are
// one forward, and comparing strings stopped the one to bind the other.
//
// A workspace-owned session the file no longer declares is RELEASED, not
// stopped (operator, 2026-10-29): removing a declaration means "do not bring
// it back", the same as un-keep, wherever it was removed from — a CLI
// `workspace forward rm` included, which cannot reach this process's
// bookkeeping. Stopping is `x`, `forward kill` or `workspace detach --stop`.
// A forward the operator started by hand still SATISFIES a declaration with
// its identity, since binding a second copy could only fail on the port; it
// is not adopted.
func planForwards(declared []string, active map[int]*PortForwardSession, taskID string) forwardPlan {
	want := make(map[workspace.ForwardKey]bool, len(declared))
	for _, value := range declared {
		if k, err := workspace.ForwardIdentity(value); err == nil {
			want[k] = true
		}
	}

	running := make(map[workspace.ForwardKey]bool, len(active))
	var plan forwardPlan
	for _, s := range active {
		if s.TaskID != taskID {
			continue
		}
		k, ok := sessionIdentity(s)
		if !ok {
			continue
		}
		if s.FromWorkspace && !want[k] {
			plan.Release = append(plan.Release, s)
			continue
		}
		running[k] = true
	}

	// A value that does not parse is skipped: Validate rejected it before any
	// apply ran, so reaching here means the file was not the one validated.
	for _, value := range declared {
		k, err := workspace.ForwardIdentity(value)
		if err != nil {
			continue
		}
		if !running[k] {
			plan.Start = append(plan.Start, value)
		}
	}
	return plan
}
