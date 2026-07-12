package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestResumeReattachAction(t *testing.T) {
	detached := &protocol.TaskInfo{Status: protocol.TaskStatus_Detached, Kind: protocol.TaskKind_Interactive}
	detached.SetDetachable(true)
	runningDetachable := &protocol.TaskInfo{Status: protocol.TaskStatus_Running, Kind: protocol.TaskKind_Interactive}
	runningDetachable.SetDetachable(true)
	// A row stubbed from a tasks.status event: kind+status only, NO detachable
	// bit (TaskStatusEvent doesn't carry it). Must still be reattachable — the
	// server, not this local bit, is the authority (regression: real `session
	// new` sessions were refused until a snapshot refresh happened to land).
	runningEventStub := &protocol.TaskInfo{Status: protocol.TaskStatus_Running, Kind: protocol.TaskKind_Interactive}
	runningOneshot := &protocol.TaskInfo{Status: protocol.TaskStatus_Running}

	if got := resumeReattachAction(nil, true); got.Kind != actionNone {
		t.Errorf("nil: want actionNone, got %v", got.Kind)
	}
	for _, wc := range []bool{true, false} {
		if got := resumeReattachAction(detached, wc); got.Kind != actionReattach {
			t.Errorf("detached wc=%v: want actionReattach, got %v", wc, got.Kind)
		}
		// Running + interactive → takeover reattach (matches WebUI gate).
		if got := resumeReattachAction(runningDetachable, wc); got.Kind != actionReattach {
			t.Errorf("running+detachable wc=%v: want actionReattach, got %v", wc, got.Kind)
		}
		if got := resumeReattachAction(runningEventStub, wc); got.Kind != actionReattach {
			t.Errorf("running+event-stub wc=%v: want actionReattach, got %v", wc, got.Kind)
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
	// Running one-shot has no PTY to attach → actionNone, and the hint must
	// explain the one-shot case specifically (cancel-then-resume takeover
	// path), not the generic line.
	runningOneshot.Kind = protocol.TaskKind_Oneshot
	if got := resumeReattachAction(runningOneshot, true); got.Kind != actionNone ||
		!strings.Contains(got.Hint, "one-shot") {
		t.Errorf("running+oneshot: want actionNone with one-shot hint, got %v %q", got.Kind, got.Hint)
	}
	// Queued → actionNone with the queued explanation.
	queued := &protocol.TaskInfo{Status: protocol.TaskStatus_Queued}
	if got := resumeReattachAction(queued, true); got.Kind != actionNone ||
		!strings.Contains(got.Hint, "queued") {
		t.Errorf("queued: want actionNone with queued hint, got %v %q", got.Kind, got.Hint)
	}
}
