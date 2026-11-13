package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Review I1: an in-case refusal of a prompted open must not block the
// connection's receive loop waiting for the prompt stream, whose frames that
// same loop delivers.
func TestDeniedPromptedOpenDoesNotBlockTheReceiveLoop(t *testing.T) {
	h := newTestHandler(t)
	conn, _ := modeAgentConn(t, h, "9170") // cancel only: no exec_control, no spawn
	oi := detachedOpen(0, 0)
	oi.SetDetached(false)
	oi.SetPromptPresent(true)
	oi.SetPromptStreamId(5)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: 9}}
	req.SetOpenInteractive(*oi)
	start := time.Now()
	h.Handle(conn, encodeTaskControlRequest(t, req))
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Handle returned after %v: the denial waited on the prompt stream", d)
	}
}

// Review I2: background conditions on an open that resolves to foreground are
// refused, not dropped — or an unattended session starts with no deadline.
func TestConditionsWithoutBackgroundRefused(t *testing.T) {
	h := newTestHandler(t)
	openRunner(t, h)
	oi := detachedOpen(0, 0)
	oi.SetConditionsPresent(true)
	oi.SetConditions(protocol.BackgroundConditions{DeadlineMs: 3600000})
	if r := openBG(h, oi, "p"); r.Status != protocol.OpenInteractiveStatus_ConditionsWithoutBackground {
		t.Fatalf("conditions without run mode: %v", r.Status)
	}
	bgBitOnly := detachedOpen(0, 0)
	bgBitOnly.SetBackground(true)
	if r := openBG(h, bgBitOnly, "p"); r.Status != protocol.OpenInteractiveStatus_ConditionsWithoutBackground {
		t.Fatalf("background bit without run_mode_set: %v", r.Status)
	}
	if n := len(h.Tasks.List(0)); n != 0 {
		t.Fatalf("a refused open left %d tasks", n)
	}
}

// Review M1: Tick's placement does not revive a task cancelled between its
// queue snapshot and Assign.
func TestAssignIfQueuedRefusesCancelled(t *testing.T) {
	s := NewTaskStore()
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Cancel(id, protocol.EndReason_QueueTimeout, protocol.TaskID{})
	if s.AssignIfQueued(id, testRunnerID("r1"), false) {
		t.Fatal("AssignIfQueued assigned a cancelled task")
	}
	if e, _ := s.Get(id); e.Status != protocol.TaskStatus_Cancelled || e.DeadlineAt != 0 {
		t.Fatalf("status %v deadline %d", e.Status, e.DeadlineAt)
	}
	q := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	if !s.AssignIfQueued(q, testRunnerID("r1"), false) {
		t.Fatal("AssignIfQueued refused a queued task")
	}
}

// Review M2: condition values out of range are refused, not wrapped.
func TestConditionsOutOfRangeRefused(t *testing.T) {
	h := newTestHandler(t)
	for name, c := range map[string]protocol.BackgroundConditions{
		"deadline overflow":      {DeadlineMs: 1 << 63},
		"deadline wraps short":   {DeadlineMs: 9223372036854776},
		"queue timeout overflow": {DeadlineMs: 1000, QueueTimeoutMs: 1 << 62},
	} {
		oi := bgOpen("claude")
		oi.SetConditions(c)
		if r := openBG(h, oi, "p"); r.Status != protocol.OpenInteractiveStatus_DeadlineRequired {
			t.Errorf("%s: %v", name, r.Status)
		}
	}
	oi := bgOpen("claude")
	oi.SetConditions(protocol.BackgroundConditions{DeadlineMs: 1000, TurnEnd: protocol.TurnEndSignal(99)})
	if r := openBG(h, oi, "p"); r.Status != protocol.OpenInteractiveStatus_TurnEndUnsupported {
		t.Errorf("undefined turn end: %v", r.Status)
	}
}

// Review M4: resuming a background task that is still running answers
// resume_not_terminal before anything about the request's own fields.
func TestBackgroundResumeOfRunningTaskIsNotTerminal(t *testing.T) {
	h := newTestHandler(t)
	id := createBG(t, h.Tasks, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	h.Tasks.Assign(id, testRunnerID("r1"), "", false)
	oi := resumeReq(id, detachedOpen(0, 0)) // no prompt, no deadline
	if r := openBG(h, oi, ""); r.Status != protocol.OpenInteractiveStatus_ResumeNotTerminal {
		t.Fatalf("status %v", r.Status)
	}
}

// Review M5: X11 cannot ride a background open (nobody hosts the tunnel).
func TestBackgroundOpenWithX11Refused(t *testing.T) {
	h := newTestHandler(t)
	oi := bgOpen("claude")
	oi.SetX11Enabled(true)
	oi.SetX11(protocol.X11Forward{Display: 10})
	if r := openBG(h, oi, "p"); r.Status == protocol.OpenInteractiveStatus_Ok {
		t.Fatal("a background open with X11 was accepted")
	}
}

// Review M8: a foreground session task left Queued by a crash between its
// create and its assign is failed by the replay sweep, not stranded (Tick no
// longer places foreground sessions).
func TestReplaySweepFailsQueuedForegroundSession(t *testing.T) {
	s, path := storeWithWAL(t)
	fg := s.Create("/r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	bg := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	one := s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	got := replayInto(t, path)
	if e, _ := got.Get(fg); e.Status != protocol.TaskStatus_Failed || e.EndReason != protocol.EndReason_Failed {
		t.Fatalf("foreground session: %v %v", e.Status, e.EndReason)
	}
	for _, id := range []string{bg, one} {
		if e, _ := got.Get(id); e.Status != protocol.TaskStatus_Queued {
			t.Fatalf("%s: %v, want still Queued", id, e.Status)
		}
	}
	_ = objproto.ConnectionID{}
}
