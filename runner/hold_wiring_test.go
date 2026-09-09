package runner

import (
	"context"
	"testing"
)

// Config.Tasks without Config.ProcessCtx is a configuration that ARMS holds and
// then kills every child anyway: taskRootCtx falls back to the connection's
// context, so the task dies with the link it was supposed to outlive.
//
// This exists because that is exactly what shipped for three live test runs.
// The registry assignment landed in the Session literal and the context one did
// not, so every symptom pointed at the relay — which was working perfectly —
// while the tasks were still rooted at the connection.
func TestSessionRootsTasksAtTheProcessWhenAHoldRegistryIsWired(t *testing.T) {
	processCtx, processCancel := context.WithCancel(context.Background())
	defer processCancel()
	connCtx, connCancel := context.WithCancel(context.Background())

	s := &Session{reg: NewTaskRegistry(), processCtx: processCtx}
	root := s.taskRootCtx(connCtx)
	connCancel()
	if root.Err() != nil {
		t.Fatal("a task's root context died with the CONNECTION — the hold is inert")
	}
	processCancel()
	if root.Err() == nil {
		t.Error("a task's root context outlived the PROCESS")
	}
}

// And with no registry the old behaviour must be exact: tasks die with the
// connection, which is what every non-holding deployment relies on.
func TestSessionKeepsConnectionRootedTasksWithoutARegistry(t *testing.T) {
	connCtx, connCancel := context.WithCancel(context.Background())
	s := &Session{}
	root := s.taskRootCtx(connCtx)
	connCancel()
	if root.Err() == nil {
		t.Error("without a registry a task must still die with its connection")
	}
}
