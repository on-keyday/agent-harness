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
			!got.ResumeConversation {
			t.Errorf("status=%v r: want resume conversation, got %v %v", st, got.Kind, got.ResumeConversation)
		}
		if got := resumeReattachAction(task, false); got.Kind != actionResume || got.ResumeConversation {
			t.Errorf("status=%v R: want resume without conversation, got %v %v", st, got.Kind, got.ResumeConversation)
		}
	}
	// Running but non-detachable (oneshot) has no PTY to attach → actionNone.
	if got := resumeReattachAction(runningOneshot, true); got.Kind != actionNone {
		t.Errorf("running+oneshot: want actionNone, got %v", got.Kind)
	}
}
