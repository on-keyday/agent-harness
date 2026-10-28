package tui

import (
	"sort"
	"testing"
)

const (
	wsTaskA = "3f2a9c00000000000000000000000001"
	wsTaskB = "7b1e000000000000000000000000000f"
)

func TestPlanForwards(t *testing.T) {
	active := map[int]*PortForwardSession{
		1: {ID: 1, TaskID: wsTaskA, Spec: "3000:127.0.0.1:3000", Direction: ForwardLocal, FromWorkspace: true},
		2: {ID: 2, TaskID: wsTaskA, Spec: "5432:127.0.0.1:5432", Direction: ForwardLocal, FromWorkspace: true},
		3: {ID: 3, TaskID: wsTaskA, Spec: "9999:127.0.0.1:9999", Direction: ForwardLocal},
		4: {ID: 4, TaskID: wsTaskB, Spec: "3000:127.0.0.1:3000", Direction: ForwardLocal, FromWorkspace: true},
	}
	declared := []string{
		"-L 3000:127.0.0.1:3000", // already running, workspace-owned → leave alone
		"-R 8080:127.0.0.1:8080", // not running → start
	}

	plan := planForwards(declared, active, wsTaskA)

	if len(plan.Start) != 1 || plan.Start[0] != "-R 8080:127.0.0.1:8080" {
		t.Errorf("Start = %q, want only the -R", plan.Start)
	}
	if len(plan.Release) != 1 || plan.Release[0].ID != 2 {
		var got []int
		for _, s := range plan.Release {
			got = append(got, s.ID)
		}
		sort.Ints(got)
		t.Errorf("Release = %v, want only session 2 (workspace-owned, no longer declared)", got)
	}
}

// A -L and a -R with the same spec text are different forwards; matching on the
// spec alone would treat one as satisfying the other.
func TestPlanForwardsDistinguishesDirection(t *testing.T) {
	active := map[int]*PortForwardSession{
		1: {ID: 1, TaskID: wsTaskA, Spec: "3000:127.0.0.1:3000", Direction: ForwardRemote, FromWorkspace: true},
	}
	plan := planForwards([]string{"-L 3000:127.0.0.1:3000"}, active, wsTaskA)
	if len(plan.Start) != 1 {
		t.Errorf("Start = %q, want the -L started despite a -R with the same spec", plan.Start)
	}
	if len(plan.Release) != 1 || plan.Release[0].ID != 1 {
		t.Errorf("Release = %+v, want the undeclared -R released", plan.Release)
	}
}

func TestPlanForwardsOnAnEmptyActiveSetStartsAll(t *testing.T) {
	declared := []string{"-L 3000:127.0.0.1:3000", "-R 8080:127.0.0.1:8080"}
	plan := planForwards(declared, map[int]*PortForwardSession{}, wsTaskA)
	if len(plan.Start) != 2 || len(plan.Release) != 0 {
		t.Errorf("reconnect case: Start=%q Release=%d, want both started and nothing released", plan.Start, len(plan.Release))
	}
}

// Dropping every forward from the file releases the ones the workspace owns
// and still leaves a hand-started one alone.
func TestPlanForwardsWithNothingDeclared(t *testing.T) {
	active := map[int]*PortForwardSession{
		1: {ID: 1, TaskID: wsTaskA, Spec: "3000:127.0.0.1:3000", Direction: ForwardLocal, FromWorkspace: true},
		2: {ID: 2, TaskID: wsTaskA, Spec: "9999:127.0.0.1:9999", Direction: ForwardLocal},
	}
	plan := planForwards(nil, active, wsTaskA)
	if len(plan.Start) != 0 {
		t.Errorf("Start = %q, want nothing", plan.Start)
	}
	if len(plan.Release) != 1 || plan.Release[0].ID != 1 {
		t.Errorf("Release = %+v, want only the workspace-owned one", plan.Release)
	}
}

// A session typed as `3000` and a declaration in the long form a save renders
// are the same forward. Compared as strings they were not, and an apply
// stopped the running one and bound the declared one in the same pass.
func TestPlanForwardsMatchesByIdentity(t *testing.T) {
	active := map[int]*PortForwardSession{
		1: {ID: 1, TaskID: wsTaskA, Spec: "3000", Direction: ForwardLocal, FromWorkspace: true},
	}
	plan := planForwards([]string{"-L 127.0.0.1:3000:127.0.0.1:3000"}, active, wsTaskA)
	if len(plan.Start) != 0 || len(plan.Release) != 0 {
		t.Errorf("Start=%q Release=%d, want nothing — the running forward IS the declared one", plan.Start, len(plan.Release))
	}
}

// A forward the operator started by hand satisfies a declaration with the same
// identity: binding a second copy would only fail on the port. It is not
// adopted, so a later un-declare never stops it.
func TestPlanForwardsHandStartedSatisfies(t *testing.T) {
	s := &PortForwardSession{ID: 1, TaskID: wsTaskA, Spec: "3000", Direction: ForwardLocal}
	plan := planForwards([]string{"-L 3000"}, map[int]*PortForwardSession{1: s}, wsTaskA)
	if len(plan.Start) != 0 || len(plan.Release) != 0 {
		t.Errorf("Start=%q Release=%d, want nothing", plan.Start, len(plan.Release))
	}
	if s.FromWorkspace {
		t.Error("planning adopted a hand-started forward")
	}
}
