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
	if len(plan.Stop) != 1 || plan.Stop[0].ID != 2 {
		var got []int
		for _, s := range plan.Stop {
			got = append(got, s.ID)
		}
		sort.Ints(got)
		t.Errorf("Stop = %v, want only session 2 (workspace-owned, no longer declared)", got)
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
	if len(plan.Stop) != 1 || plan.Stop[0].ID != 1 {
		t.Errorf("Stop = %+v, want the undeclared -R stopped", plan.Stop)
	}
}

func TestPlanForwardsOnAnEmptyActiveSetStartsAll(t *testing.T) {
	declared := []string{"-L 3000:127.0.0.1:3000", "-R 8080:127.0.0.1:8080"}
	plan := planForwards(declared, map[int]*PortForwardSession{}, wsTaskA)
	if len(plan.Start) != 2 || len(plan.Stop) != 0 {
		t.Errorf("reconnect case: Start=%q Stop=%d, want both started and nothing stopped", plan.Start, len(plan.Stop))
	}
}

// Dropping every forward from the file must stop the ones the workspace owns
// and still leave a hand-started one alone.
func TestPlanForwardsWithNothingDeclared(t *testing.T) {
	active := map[int]*PortForwardSession{
		1: {ID: 1, TaskID: wsTaskA, Spec: "3000:127.0.0.1:3000", Direction: ForwardLocal, FromWorkspace: true},
		2: {ID: 2, TaskID: wsTaskA, Spec: "9999:127.0.0.1:9999", Direction: ForwardLocal},
	}
	plan := planForwards(nil, active, wsTaskA)
	if len(plan.Start) != 0 {
		t.Errorf("Start = %q, want nothing", plan.Start)
	}
	if len(plan.Stop) != 1 || plan.Stop[0].ID != 1 {
		t.Errorf("Stop = %+v, want only the workspace-owned one", plan.Stop)
	}
}
