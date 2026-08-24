package tui

import "strings"

// forwardPlan is the difference between what a workspace declares for one task
// and what is already running for it.
type forwardPlan struct {
	Start []string              // declared values with nothing running
	Stop  []*PortForwardSession // workspace-owned sessions no longer declared
}

// forwardKey identifies a forward by the pair a session actually records:
// PortForwardSession.Spec holds the spec without its flag, Direction holds the
// flag. Matching on the spec alone would let a -R satisfy a declared -L.
type forwardKey struct {
	dir  ForwardDirection
	spec string
}

// splitForwardValue peels the -L/-R flag off a config value. A value that does
// not split is skipped by the caller: Validate rejected it before any apply
// ran, so reaching here means the file was not the one that was validated.
func splitForwardValue(value string) (forwardKey, bool) {
	flag, spec, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok {
		return forwardKey{}, false
	}
	dir := ForwardLocal
	if flag == "-R" {
		dir = ForwardRemote
	}
	return forwardKey{dir: dir, spec: strings.TrimSpace(spec)}, true
}

// planForwards computes the difference for one task. Reconciling rather than
// restarting is what makes `workspace apply` usable as the recovery from a port
// conflict: the forwards that ARE working must survive the apply that retries
// the one that is not.
//
// Only workspace-owned sessions are candidates for Stop. A forward the operator
// started by hand belongs to the operator.
func planForwards(declared []string, active map[int]*PortForwardSession, taskID string) forwardPlan {
	want := make(map[forwardKey]bool, len(declared))
	for _, value := range declared {
		if k, ok := splitForwardValue(value); ok {
			want[k] = true
		}
	}

	running := make(map[forwardKey]bool, len(active))
	var plan forwardPlan
	for _, s := range active {
		if s.TaskID != taskID || !s.FromWorkspace {
			continue // another task's, or the operator's own — not ours to touch
		}
		k := forwardKey{dir: s.Direction, spec: s.Spec}
		if !want[k] {
			plan.Stop = append(plan.Stop, s)
			continue
		}
		running[k] = true
	}

	for _, value := range declared {
		k, ok := splitForwardValue(value)
		if !ok {
			continue
		}
		if !running[k] {
			plan.Start = append(plan.Start, value)
		}
	}
	return plan
}
