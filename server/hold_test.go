package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// storeWithWAL builds a TaskStore backed by a real WAL in a temp dir, so a test
// can assert what a NEXT server would replay rather than only what this one
// holds in memory.
func storeWithWAL(t *testing.T) (*TaskStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	wal, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	s := NewTaskStore()
	s.SetWAL(wal)
	return s, path
}

func replayInto(t *testing.T, path string) *TaskStore {
	t.Helper()
	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	fresh := NewTaskStore()
	fresh.ReplayEvents(events)
	return fresh
}

func runningTask(t *testing.T, s *TaskStore, runner protocol.RunnerID) string {
	t.Helper()
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	s.Assign(id, runner, "/wt", false)
	return id
}

func TestMarkHoldSurvivesAReplayAsHeld(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)

	deadline := time.Now().Add(90 * time.Second).UnixNano()
	if err := s.MarkHold(id, rid.Hex(), "cafe", deadline); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	fresh := replayInto(t, path)
	got, ok := fresh.Get(id)
	if !ok {
		t.Fatal("task absent after replay")
	}
	if got.Status != protocol.TaskStatus_Held {
		t.Errorf("status = %v, want Held", got.Status)
	}
	if got.HoldID != "cafe" || got.HoldDeadline != deadline {
		t.Errorf("hold = %q/%d, want cafe/%d", got.HoldID, got.HoldDeadline, deadline)
	}
}

// The race the ack window opens: a TaskFinished can land while acks are being
// collected. Because replay is order-sensitive, a task_held written after that
// task's task_finished would win and the next server would offer a task whose
// child has exited. MarkHold refuses instead.
func TestMarkHoldRefusesATaskThatJustFinished(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	s.Finish(id, 0, nil)

	if err := s.MarkHold(id, rid.Hex(), "cafe", time.Now().UnixNano()); err == nil {
		t.Fatal("MarkHold accepted a finished task")
	}
	fresh := replayInto(t, path)
	got, _ := fresh.Get(id)
	if got.Status == protocol.TaskStatus_Held {
		t.Error("a finished task replayed as Held — the hold record was written anyway")
	}
}

func TestMarkReadoptedClosesTheHoldForASecondRestart(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	if err := s.MarkHold(id, rid.Hex(), "cafe", time.Now().Add(time.Minute).UnixNano()); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	if err := s.MarkReadopted(id, rid.Hex(), "cafe"); err != nil {
		t.Fatalf("MarkReadopted: %v", err)
	}

	fresh := replayInto(t, path)
	got, _ := fresh.Get(id)
	// NOT Running: ReplayEvents' own tail sweeps a still-Running task to
	// Failed("server_restart"), and that is correct here — by the time this
	// record is replayed the task was running again with no hold. What must
	// hold is that it is no longer HELD and carries no hold id, because that
	// is what stops a second restart from offering it for re-adoption against
	// the previous hold.
	if got.Status == protocol.TaskStatus_Held {
		t.Errorf("status = Held after a readopt — the hold was never closed")
	}
	if got.HoldID != "" || got.HoldDeadline != 0 {
		t.Errorf("hold not cleared: %q/%d — a second restart would re-offer this task",
			got.HoldID, got.HoldDeadline)
	}
}

// A cancel while held must win on replay, since that is the only thing that
// tells the next server not to offer the task (CancelTask itself can never be
// delivered to a held task's runner).
func TestCancelAfterHoldWinsOnReplay(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	if err := s.MarkHold(id, rid.Hex(), "cafe", time.Now().Add(time.Minute).UnixNano()); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	s.Cancel(id)

	fresh := replayInto(t, path)
	got, _ := fresh.Get(id)
	if got.Status != protocol.TaskStatus_Cancelled {
		t.Errorf("status = %v, want Cancelled", got.Status)
	}
}

// An older binary ignores an unknown record type rather than failing the file.
// The rollback claim in the spec's §4 rests on this, so it is measured here
// rather than asserted there.
func TestUnknownWALRecordTypeIsIgnoredOnReplay(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	if err := s.wal.Write(WALEvent{Type: "task_invented_later", TaskID: id, Ts: time.Now().UnixNano()}); err != nil {
		t.Fatalf("write: %v", err)
	}

	fresh := replayInto(t, path)
	got, ok := fresh.Get(id)
	if !ok {
		t.Fatal("an unknown record type cost the whole file")
	}
	// Failed("server_restart") is replay's own verdict on a task that was
	// Running when the server went away; what this test is about is that the
	// unknown record cost nothing — the task is still there and its status is
	// the one the KNOWN records produce.
	if got.Status != protocol.TaskStatus_Failed {
		t.Errorf("status = %v, want Failed (the unknown record must be inert)", got.Status)
	}
}

// The sweep at the end of ReplayEvents forces every still-Running task to
// Failed("server_restart"). Held must be outside it: that status is the one
// claim the sweep exists to deny for everything else — the child is alive on a
// runner that agreed to keep it. If this ever goes red, every held task comes
// back Failed and the whole feature is inert while looking implemented.
func TestReplaySweepDoesNotFailAHeldTask(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	held := runningTask(t, s, rid)
	interrupted := runningTask(t, s, rid)
	deadline := time.Now().Add(90 * time.Second).UnixNano()
	if err := s.MarkHold(held, rid.Hex(), "cafe", deadline); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	fresh := replayInto(t, path)
	if got, _ := fresh.Get(held); got.Status != protocol.TaskStatus_Held {
		t.Errorf("held task status = %v, want Held", got.Status)
	}
	// The control: a task that was merely Running IS swept, so the test proves
	// an exemption rather than the absence of a sweep.
	if got, _ := fresh.Get(interrupted); got.Status != protocol.TaskStatus_Failed {
		t.Errorf("un-held task status = %v, want Failed — the sweep did not run at all", got.Status)
	}
}
