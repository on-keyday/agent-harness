package server

import (
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func bgSpec(deadline time.Duration) RunSpec {
	return RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{
		Deadline: deadline, TurnEnd: protocol.TurnEndSignal_Lifecycle,
		Quiet: time.Minute, Blocked: 2 * time.Minute, QueueTimeout: 3 * time.Minute,
		Rows: 40, Cols: 120,
	}}
}

func createBG(t *testing.T, s *TaskStore, kind protocol.TaskKind, run RunSpec) string {
	t.Helper()
	id, err := s.CreateWithinLimits("/repo", "triage", kind, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "claude", run)
	if err != nil {
		t.Fatalf("CreateWithinLimits: %v", err)
	}
	return id
}

func resumeWith(t *testing.T, s *TaskStore, id string, run RunSpec) {
	t.Helper()
	if _, err := s.Resume(id, "again", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
		GrantUpdate{}, protocol.TaskKind_Interactive, "claude", protocol.TaskID{}, true, run); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestBackgroundCreateStoresConditionsAndQueueDeadline(t *testing.T) {
	s := NewTaskStore()
	before := time.Now().UnixNano()
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	e, _ := s.Get(id)
	if e.RunMode != protocol.RunMode_Background || e.Cond.Deadline != time.Hour || e.Cond.Rows != 40 {
		t.Fatalf("entry = mode %v cond %+v", e.RunMode, e.Cond)
	}
	if e.QueueDeadlineAt < before+int64(3*time.Minute) || e.DeadlineAt != 0 {
		t.Fatalf("queue deadline %d (want >= %d), deadline %d (want 0 until placed)", e.QueueDeadlineAt, before+int64(3*time.Minute), e.DeadlineAt)
	}
}

func TestAssignResolvesTheDeadlineFromRunStart(t *testing.T) {
	s := NewTaskStore()
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Assign(id, testRunnerID("r1"), "", false)
	e, _ := s.Get(id)
	if e.StartedAt == nil || e.DeadlineAt-e.StartedAt.UnixNano() != int64(time.Hour) {
		t.Fatalf("deadline %d started %v", e.DeadlineAt, e.StartedAt)
	}
}

func TestResumeReusesNoConditions(t *testing.T) {
	s := NewTaskStore()
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	first := bgSpec(2 * time.Hour)
	first.Cond.Quiet = time.Minute
	resumeWith(t, s, id, first)
	s.Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	resumeWith(t, s, id, RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}.WithDefaults(protocol.TaskKind_Interactive)})
	e, _ := s.Get(id)
	if e.Cond.Quiet != 5*time.Minute || e.Cond.QueueTimeout != 0 || e.QueueDeadlineAt != 0 {
		t.Fatalf("second resume kept the first's values: %+v queue deadline %d", e.Cond, e.QueueDeadlineAt)
	}
	if e.EndReason != protocol.EndReason_None || e.CancelledBy != (protocol.TaskID{}) {
		t.Fatalf("resume left the previous run's end: %v by %v", e.EndReason, e.CancelledBy)
	}
}

func TestWithDefaultsStreamAnyMeansLifecycle(t *testing.T) {
	c := RunConditions{}.WithDefaults(protocol.TaskKind_Stream)
	if c.TurnEnd != protocol.TurnEndSignal_Lifecycle || c.Quiet != 5*time.Minute || c.Blocked != 5*time.Minute {
		t.Fatalf("stream defaults = %+v", c)
	}
	if c := (RunConditions{}).WithDefaults(protocol.TaskKind_Interactive); c.TurnEnd != protocol.TurnEndSignal_Any {
		t.Fatalf("interactive default turn end = %v", c.TurnEnd)
	}
}

func TestCancelRecordsReasonAndActor(t *testing.T) {
	s := NewTaskStore()
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	by := protocol.TaskID{Id: [16]byte{7}}
	s.Cancel(id, protocol.EndReason_Cancelled, by)
	e, _ := s.Get(id)
	if e.Status != protocol.TaskStatus_Cancelled || e.EndReason != protocol.EndReason_Cancelled || e.CancelledBy != by {
		t.Fatalf("got status %v reason %v by %v", e.Status, e.EndReason, e.CancelledBy)
	}
}

func TestFinishAfterCancelKeepsTheCancelReason(t *testing.T) {
	s := NewTaskStore()
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Assign(id, testRunnerID("r1"), "", false)
	s.Cancel(id, protocol.EndReason_TurnDoneLifecycle, protocol.TaskID{})
	s.Finish(id, 0, nil)
	e, _ := s.Get(id)
	if e.EndReason != protocol.EndReason_TurnDoneLifecycle {
		t.Fatalf("end reason = %v", e.EndReason)
	}
}

func TestFinishMinusOneIsFailedReason(t *testing.T) {
	s := NewTaskStore()
	a := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	b := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Finish(a, -1, []byte("prompt_unsupported: x"))
	s.Finish(b, 3, nil)
	if e, _ := s.Get(a); e.EndReason != protocol.EndReason_Failed {
		t.Fatalf("exit -1 reason = %v", e.EndReason)
	}
	if e, _ := s.Get(b); e.EndReason != protocol.EndReason_ProcessExit {
		t.Fatalf("exit 3 reason = %v", e.EndReason)
	}
}

func TestMarkFailedRunnerDisconnectedIsFailedReason(t *testing.T) {
	s := NewTaskStore()
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Assign(id, testRunnerID("r1"), "", false)
	s.MarkFailed(id, "runner_disconnected")
	e, _ := s.Get(id)
	if e.Status != protocol.TaskStatus_Failed || e.EndReason != protocol.EndReason_Failed || string(e.ErrorMsg) != "runner_disconnected" {
		t.Fatalf("got %v %v %q", e.Status, e.EndReason, e.ErrorMsg)
	}
	if due, next := s.BackgroundDeadlines(time.Now().Add(2 * time.Hour).UnixNano()); len(due) != 0 || next != 0 {
		t.Fatalf("a failed task still has a deadline: due %v next %d", due, next)
	}
}

func TestSwitchRunModeRefusesQueuedAndOneshot(t *testing.T) {
	s := NewTaskStore()
	q := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	if _, err := s.SwitchRunMode(q, protocol.RunMode_Foreground, RunConditions{}, 1); !errors.Is(err, ErrSwitchQueued) {
		t.Fatalf("queued: err = %v", err)
	}
	o := createBG(t, s, protocol.TaskKind_Oneshot, RunSpec{})
	s.Assign(o, testRunnerID("r1"), "", false)
	if _, err := s.SwitchRunMode(o, protocol.RunMode_Background, RunConditions{Deadline: time.Hour}, 1); !errors.Is(err, ErrSwitchNotSession) {
		t.Fatalf("oneshot: err = %v", err)
	}
	s.Assign(q, testRunnerID("r1"), "", false)
	s.Cancel(q, protocol.EndReason_Cancelled, protocol.TaskID{})
	if _, err := s.SwitchRunMode(q, protocol.RunMode_Foreground, RunConditions{}, 1); !errors.Is(err, ErrSwitchNotActive) {
		t.Fatalf("terminal: err = %v", err)
	}
}

func TestSwitchRunModeSetsAndClearsTheDeadline(t *testing.T) {
	s := NewTaskStore()
	id := createBG(t, s, protocol.TaskKind_Interactive, RunSpec{})
	s.Assign(id, testRunnerID("r1"), "", false)
	now := time.Now().UnixNano()
	e, err := s.SwitchRunMode(id, protocol.RunMode_Background, RunConditions{Deadline: time.Minute}, now)
	if err != nil || e.RunMode != protocol.RunMode_Background || e.DeadlineAt != now+int64(time.Minute) || e.ModeSwitchedAt != now {
		t.Fatalf("to background: %v %+v", err, e)
	}
	e, err = s.SwitchRunMode(id, protocol.RunMode_Foreground, RunConditions{}, now+1)
	if err != nil || e.RunMode != protocol.RunMode_Foreground || e.DeadlineAt != 0 {
		t.Fatalf("to foreground: %v %+v", err, e)
	}
}

func TestBackgroundDeadlinesReportsDueAndNext(t *testing.T) {
	s := NewTaskStore()
	run := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Assign(run, testRunnerID("r1"), "", false)
	queued := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour)) // queue timeout 3m
	e, _ := s.Get(run)
	due, next := s.BackgroundDeadlines(e.DeadlineAt)
	if len(due) != 2 {
		t.Fatalf("due = %+v", due)
	}
	got := map[string]protocol.EndReason{}
	for _, d := range due {
		got[d.ID] = d.Reason
	}
	if got[run] != protocol.EndReason_Deadline || got[queued] != protocol.EndReason_QueueTimeout || next != 0 {
		t.Fatalf("due %v next %d", got, next)
	}
	q, _ := s.Get(queued)
	if _, next := s.BackgroundDeadlines(0); next != q.QueueDeadlineAt {
		t.Fatalf("next = %d, want the queue deadline %d (earliest)", next, q.QueueDeadlineAt)
	}
}

func TestRunFieldsSurviveWALReplay(t *testing.T) {
	s, path := storeWithWAL(t)
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Assign(id, testRunnerID("r1"), "", false)
	switchedAt := time.Now().UnixNano()
	if _, err := s.SwitchRunMode(id, protocol.RunMode_Background, RunConditions{Deadline: 2 * time.Hour, TurnEnd: protocol.TurnEndSignal_Quiet, Quiet: 7 * time.Minute, Rows: 40, Cols: 120}, switchedAt); err != nil {
		t.Fatal(err)
	}
	by := protocol.TaskID{Id: [16]byte{9}}
	s.Cancel(id, protocol.EndReason_Deadline, by)
	want, _ := s.Get(id)

	queued := createBG(t, s, protocol.TaskKind_Stream, bgSpec(time.Hour))
	wantQ, _ := s.Get(queued)

	failed := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Assign(failed, testRunnerID("r1"), "", false)
	s.MarkFailed(failed, "runner_disconnected")

	got := replayInto(t, path)
	g, _ := got.Get(id)
	if g.RunMode != want.RunMode || g.Cond != want.Cond || g.DeadlineAt != want.DeadlineAt ||
		g.ModeSwitchedAt != switchedAt || g.EndReason != protocol.EndReason_Deadline || g.CancelledBy != by {
		t.Fatalf("replayed %+v\nwant     %+v", g, want)
	}
	gq, _ := got.Get(queued)
	if gq.RunMode != protocol.RunMode_Background || gq.Cond != wantQ.Cond || gq.QueueDeadlineAt != wantQ.QueueDeadlineAt || gq.Status != protocol.TaskStatus_Queued {
		t.Fatalf("queued replayed %+v\nwant %+v", gq, wantQ)
	}
	if gf, _ := got.Get(failed); gf.EndReason != protocol.EndReason_Failed {
		t.Fatalf("failed replayed reason %v", gf.EndReason)
	}

	// A resume after replay starts clean, and its own values replay too.
	s2, path2 := storeWithWAL(t)
	id2 := createBG(t, s2, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s2.Cancel(id2, protocol.EndReason_Cancelled, protocol.TaskID{})
	resumeWith(t, s2, id2, bgSpec(3*time.Hour))
	if r, _ := replayInto(t, path2).Get(id2); r.Cond.Deadline != 3*time.Hour || r.EndReason != protocol.EndReason_None {
		t.Fatalf("resumed replay %+v", r)
	}
}

// The replay sweep that fails a task left Running records the end reason too.
func TestReplaySweepSetsFailedReason(t *testing.T) {
	s, path := storeWithWAL(t)
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Assign(id, testRunnerID("r1"), "", false)
	if e, _ := replayInto(t, path).Get(id); e.Status != protocol.TaskStatus_Failed || e.EndReason != protocol.EndReason_Failed {
		t.Fatalf("swept %v %v", e.Status, e.EndReason)
	}
}
