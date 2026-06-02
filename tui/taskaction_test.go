package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestResumeReattachAction(t *testing.T) {
	detached := &protocol.TaskInfo{Status: protocol.TaskStatus_Detached}
	detached.SetDetachable(true)
	runningDetachable := &protocol.TaskInfo{Status: protocol.TaskStatus_Running}
	runningDetachable.SetDetachable(true)
	runningOneshot := &protocol.TaskInfo{Status: protocol.TaskStatus_Running}

	if got := resumeReattachAction(nil, true); got.Kind != actionNone {
		t.Errorf("nil: want actionNone, got %v", got.Kind)
	}
	for _, wc := range []bool{true, false} {
		if got := resumeReattachAction(detached, wc); got.Kind != actionReattach {
			t.Errorf("detached wc=%v: want actionReattach, got %v", wc, got.Kind)
		}
		// Running + detachable → takeover reattach (matches WebUI gate).
		if got := resumeReattachAction(runningDetachable, wc); got.Kind != actionReattach {
			t.Errorf("running+detachable wc=%v: want actionReattach, got %v", wc, got.Kind)
		}
	}
	for _, st := range []protocol.TaskStatus{
		protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled,
	} {
		task := &protocol.TaskInfo{Status: st}
		if got := resumeReattachAction(task, true); got.Kind != actionResume ||
			len(got.ResumeArgs) != 1 || got.ResumeArgs[0] != "--continue" {
			t.Errorf("status=%v r: want resume [--continue], got %v %v", st, got.Kind, got.ResumeArgs)
		}
		if got := resumeReattachAction(task, false); got.Kind != actionResume || got.ResumeArgs != nil {
			t.Errorf("status=%v R: want resume nil, got %v %v", st, got.Kind, got.ResumeArgs)
		}
	}
	// Running but non-detachable (oneshot) has no PTY to attach → actionNone.
	if got := resumeReattachAction(runningOneshot, true); got.Kind != actionNone {
		t.Errorf("running+oneshot: want actionNone, got %v", got.Kind)
	}
}
