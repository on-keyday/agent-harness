package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// cancelFixture is one runner with one Running task dispatched to it, the
// TaskStore wired to the Dispatcher the way server.New wires them, and a
// ticket registered for the task.
type cancelFixture struct {
	d        *Dispatcher
	tasks    *TaskStore
	board    *agentboard.Board
	fc       *fakeConn
	identity protocol.RunnerID
	taskID   string
}

func newCancelFixture(t *testing.T, delays []time.Duration) *cancelFixture {
	t.Helper()
	reg := NewRegistry()
	tasks := NewTaskStore()
	board := agentboard.New(agentboard.Config{})
	d := &Dispatcher{Registry: reg, Tasks: tasks, Board: board, CancelResendDelays: delays}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-30")}
	identity := testRunnerID(fc.id.String())
	reg.Add(&RunnerEntry{
		ID:               fc.id,
		Identity:         identity,
		Hostname:         "host",
		AllowedRoots:     []string{"/repo"},
		MaxTasks:         2,
		ActiveTasks:      map[string]struct{}{},
		ConnectedAt:      time.Unix(1, 0),
		LastTaskActivity: time.Unix(1, 0),
		Conn:             fc,
	})
	taskID := tasks.Create("/repo", "work", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	tasks.Assign(taskID, identity, "", false)
	reg.BindTask(fc.id, taskID)
	boardRegisterTask(board, identity, taskID, [16]byte{1, 2, 3}, "claude")

	tasks.OnCancel = d.OnCancel
	tasks.OnCancelRepeated = d.OnCancel
	return &cancelFixture{d: d, tasks: tasks, board: board, fc: fc, identity: identity, taskID: taskID}
}

func (f *cancelFixture) cancelsSent(t *testing.T) int {
	t.Helper()
	n := 0
	for _, m := range f.fc.Sent() {
		if classifyRunnerRequest(m) == "cancel" {
			n++
		}
	}
	return n
}

// waitCancels polls until at least want CancelTasks were sent.
func (f *cancelFixture) waitCancels(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.cancelsSent(t) < want {
		if time.Now().After(deadline) {
			t.Fatalf("sent %d CancelTask, want at least %d", f.cancelsSent(t), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// The ticket goes at the cancel, not at the TaskFinished: the CancelTask is
// one unacknowledged datagram, and measured with it dropped the agent of a
// Cancelled row went on publishing to the board.
func TestOnCancelRevokesTicketBeforeAnyTaskFinished(t *testing.T) {
	f := newCancelFixture(t, []time.Duration{time.Hour})
	if _, ok := boardTaskTicket(f.board, f.identity, taskIDFromHex(f.taskID)); !ok {
		t.Fatal("fixture: ticket not registered")
	}
	f.tasks.Cancel(f.taskID)
	if _, ok := boardTaskTicket(f.board, f.identity, taskIDFromHex(f.taskID)); ok {
		t.Fatal("ticket still valid after cancel, with no TaskFinished received")
	}
}

// Unanswered, the CancelTask is resent; once a TaskFinished lands (the row
// leaves Cancelled), the resends stop.
func TestCancelResentUntilTaskFinished(t *testing.T) {
	f := newCancelFixture(t, []time.Duration{
		5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond,
		200 * time.Millisecond, 200 * time.Millisecond,
	})
	f.tasks.Cancel(f.taskID)
	f.waitCancels(t, 3) // the first send plus two resends

	f.tasks.Finish(f.taskID, 0, nil)
	settled := f.cancelsSent(t)
	time.Sleep(500 * time.Millisecond) // past both remaining delays
	if got := f.cancelsSent(t); got > settled+1 {
		// +1: a resend already past its status check when Finish ran.
		t.Fatalf("resends continued after TaskFinished: %d then %d", settled, got)
	}
}

// The schedule is finite: an unanswered cancel stops after the last delay.
func TestCancelResendGivesUp(t *testing.T) {
	f := newCancelFixture(t, []time.Duration{time.Millisecond, time.Millisecond})
	f.tasks.Cancel(f.taskID)
	f.waitCancels(t, 3)
	time.Sleep(50 * time.Millisecond)
	if got := f.cancelsSent(t); got != 3 {
		t.Fatalf("sent %d CancelTask, want 3 (one send + two resends)", got)
	}
}

// Cancelling an already-Cancelled task resends the CancelTask. It used to be a
// no-op, so a cancel whose datagram was lost could not be retried at all.
func TestRepeatedCancelResends(t *testing.T) {
	f := newCancelFixture(t, []time.Duration{}) // no automatic resends
	published := 0
	onCancel := f.tasks.OnCancel
	f.tasks.OnCancel = func(id string) { published++; onCancel(id) }

	f.tasks.Cancel(f.taskID)
	f.tasks.Cancel(f.taskID)
	f.waitCancels(t, 2)
	if published != 1 {
		t.Fatalf("OnCancel fired %d times; the transition must happen once", published)
	}
}

// A Queued task has nothing on a runner to cancel, even though handleSubmit
// records the candidate runner (BoundRunnerID) at submit. Sending it a
// CancelTask is now answered with TaskFinished(-1) — the runner does not have
// the task — which would turn the operator's Cancelled into Failed.
func TestCancelOfQueuedTaskSendsNothing(t *testing.T) {
	f := newCancelFixture(t, []time.Duration{})
	queued := f.tasks.Create("/repo", "work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, f.fc.id.String(), protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	before := f.cancelsSent(t)
	f.tasks.Cancel(queued)
	if got := f.cancelsSent(t); got != before {
		t.Fatalf("cancelling a Queued task sent %d CancelTask", got-before)
	}
}
