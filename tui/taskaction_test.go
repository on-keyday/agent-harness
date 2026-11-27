package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestResumeReattachAction(t *testing.T) {
	detached := &protocol.TaskInfo{Status: protocol.TaskStatus_Detached, Kind: protocol.TaskKind_Interactive}
	runningDetachable := &protocol.TaskInfo{Status: protocol.TaskStatus_Running, Kind: protocol.TaskKind_Interactive}
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
	// Running oneshot is a live session kind → actionReattach.
	runningOneshot.Kind = protocol.TaskKind_Oneshot
	if got := resumeReattachAction(runningOneshot, true); got.Kind != actionReattach {
		t.Errorf("running+oneshot: want actionReattach, got %v %q", got.Kind, got.Hint)
	}
	// Queued → actionNone with the queued explanation.
	queued := &protocol.TaskInfo{Status: protocol.TaskStatus_Queued}
	if got := resumeReattachAction(queued, true); got.Kind != actionNone ||
		!strings.Contains(got.Hint, "queued") {
		t.Errorf("queued: want actionNone with queued hint, got %v %q", got.Kind, got.Hint)
	}
	// A LIVE event-stream session has no seat to take, but r still DRIVES it:
	// the chat view sends turns and answers approvals. The generic fallback
	// names only take-over and resume, which reads as "nothing to do here" —
	// how an operator concluded the TUI could not reach this kind at all.
	for _, st := range []protocol.TaskStatus{
		protocol.TaskStatus_Running, protocol.TaskStatus_Detached,
	} {
		stream := &protocol.TaskInfo{Status: st, Kind: protocol.TaskKind_Stream}
		if got := resumeReattachAction(stream, true); got.Kind != actionChat {
			t.Errorf("stream status=%v: want actionChat, got %v %q", st, got.Kind, got.Hint)
		}
	}
	// Once it ENDS, the kind stops mattering: a finished stream task resumes
	// like any other, so it must NOT be captured by the live-stream case.
	endedStream := &protocol.TaskInfo{Status: protocol.TaskStatus_Succeeded, Kind: protocol.TaskKind_Stream}
	if got := resumeReattachAction(endedStream, true); got.Kind != actionResume {
		t.Errorf("ended stream: want actionResume, got %v %q", got.Kind, got.Hint)
	}
}

// D (X11) on each kind of row: reattach only where this TUI holds the
// tunnel, restart (after a y/n) a live session without one, resume a finished
// task with its conversation, and say why for everything else.
func TestX11OpenAction(t *testing.T) {
	inter := func(s protocol.TaskStatus) *protocol.TaskInfo {
		return &protocol.TaskInfo{Status: s, Kind: protocol.TaskKind_Interactive}
	}
	bg := inter(protocol.TaskStatus_Running)
	bg.RunMode = protocol.RunMode_Background
	cases := []struct {
		name string
		t    *protocol.TaskInfo
		held bool
		want taskActionKind
		hint string
	}{
		{"nil", nil, false, actionNone, "no task selected"},
		{"running, held", inter(protocol.TaskStatus_Running), true, actionReattach, ""},
		{"detached, held", inter(protocol.TaskStatus_Detached), true, actionReattach, ""},
		{"running, not held", inter(protocol.TaskStatus_Running), false, actionX11Restart, ""},
		{"detached, not held", inter(protocol.TaskStatus_Detached), false, actionX11Restart, ""},
		{"background", bg, false, actionNone, "background"},
		{"stream", &protocol.TaskInfo{Status: protocol.TaskStatus_Running, Kind: protocol.TaskKind_Stream}, false, actionNone, "terminal"},
		{"queued", inter(protocol.TaskStatus_Queued), false, actionNone, "queued"},
	}
	for _, c := range cases {
		got := x11OpenAction(c.t, c.held)
		if got.Kind != c.want || !strings.Contains(got.Hint, c.hint) {
			t.Errorf("%s: got %+v, want kind %v hint ~%q", c.name, got, c.want, c.hint)
		}
	}
	for _, st := range []protocol.TaskStatus{protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled} {
		if got := x11OpenAction(inter(st), false); got.Kind != actionResume || !got.ResumeConversation {
			t.Errorf("%v: got %+v, want a resume with the conversation", st, got)
		}
	}
	done := inter(protocol.TaskStatus_Cancelled)
	done.RunMode = protocol.RunMode_Background
	if got := x11OpenAction(done, false); got.Kind != actionNone || !strings.Contains(got.Hint, "background") {
		t.Errorf("finished background task: got %+v, want a hint (a background task resumes in background)", got)
	}
}
