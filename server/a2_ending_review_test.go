package server

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Ending review I1: a task may not switch ITSELF in either direction — a
// background→background self-switch would let it replace its own deadline.
func TestSelfSwitchToBackgroundRefused(t *testing.T) {
	h := newTestHandler(t)
	conn, self := modeAgentConn(t, h, "9180")
	h.Tasks.Assign(self, testRunnerID("r1"), "", false)
	if _, err := h.Tasks.SwitchRunMode(self, protocol.RunMode_Background, RunConditions{Deadline: time.Hour}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	before, _ := h.Tasks.Get(self)
	cond := &protocol.BackgroundConditions{DeadlineMs: maxConditionMs, TurnEnd: protocol.TurnEndSignal_Off}
	if r := sessionMode(t, h, conn, self, true, cond); r.Status != protocol.SessionModeStatus_SelfSwitchRefused {
		t.Fatalf("status %v", r.Status)
	}
	if after, _ := h.Tasks.Get(self); after.DeadlineAt != before.DeadlineAt {
		t.Fatal("the task moved its own deadline")
	}
}

// Ending review I2a/I2b: across a deliberate restart the run keeps its input
// floor and the time its program state was really reported, not the time the
// capture was loaded.
func TestRestartCarriesInputFloorAndProgramAt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: NewSessionRegistry()}
	id := createBG(t, h.Tasks, protocol.TaskKind_Interactive,
		RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}.WithDefaults(protocol.TaskKind_Interactive)})
	h.Tasks.Assign(id, testRunnerID("r1"), "", false)
	e, _ := h.Tasks.Get(id)
	runStart := e.StartedAt.UnixNano()

	before := newFakeStream(t)
	old := NewSessionMux(ctx, id, protocol.TaskKind_Interactive, before, NewRingBuffer(1<<20), SessionHooks{})
	defer old.Stop()
	before.QueueRead(makeWireFrame(1, []byte("\x1b]7501;state=idle\x1b\\")))
	waitFor(t, func() bool { s, _, _ := old.Program(); return s == protocol.ProgramState_Idle })
	facts := old.runFacts(runStart)
	facts.LastInput = runStart + 500 // a human typed in this run
	programAt, _ := old.ProgramStamps()

	time.Sleep(5 * time.Millisecond)
	fresh := NewSessionMux(ctx, id, protocol.TaskKind_Interactive, newFakeStream(t), NewRingBuffer(1<<20), SessionHooks{})
	defer fresh.Stop()
	if _, _, err := fresh.loadHeldCapture(old.heldCapture()); err != nil {
		t.Fatal(err)
	}
	fresh.carryRunFacts(facts)
	in, ok := h.runInputsOf(id, fresh)
	if !ok {
		t.Fatal("not active")
	}
	if in.LastInput != facts.LastInput {
		t.Errorf("LastInput %d, want the carried %d", in.LastInput, facts.LastInput)
	}
	if in.ProgAt != programAt {
		t.Errorf("ProgAt %d, want the pre-restart %d (not the load time)", in.ProgAt, programAt)
	}
}

// Ending review M1: a deadline fire that loses a race with a switch to
// foreground must not cancel the now-foreground task.
func TestDeadlineFireAfterSwitchToForegroundDoesNotCancel(t *testing.T) {
	h := newTestHandler(t)
	id := createBG(t, h.Tasks, protocol.TaskKind_Interactive, RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Millisecond}})
	h.Tasks.Assign(id, testRunnerID("r1"), "", false)
	time.Sleep(3 * time.Millisecond)
	due, _ := h.Tasks.BackgroundDeadlines(time.Now().UnixNano())
	if len(due) != 1 {
		t.Fatalf("due %v", due)
	}
	if _, err := h.Tasks.SwitchRunMode(id, protocol.RunMode_Foreground, RunConditions{}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	h.endBackgroundRun(due[0])
	if e, _ := h.Tasks.Get(id); e.Status == protocol.TaskStatus_Cancelled {
		t.Fatalf("a stale deadline fire cancelled a foreground task (%v)", e.EndReason)
	}
}
