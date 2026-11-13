package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A run is one open of a task: its creation, then each resume (pillar A spec,
// A-D20). The number names a run's record.

func runNumberTask(t *testing.T, s *TaskStore) string {
	t.Helper()
	return s.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "")
}

func resumeForRun(t *testing.T, s *TaskStore, id string) {
	t.Helper()
	s.Finish(id, 0, nil)
	if _, err := s.Resume(id, "p", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli, GrantUpdate{},
		protocol.TaskKind_Interactive, "", protocol.TaskID{}, false, RunSpec{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

func runOf(t *testing.T, s *TaskStore, id string) uint32 {
	t.Helper()
	e, ok := s.Get(id)
	if !ok {
		t.Fatalf("task %s missing", id)
	}
	return e.Run
}

func TestRunNumberCountsCreateAndResumes(t *testing.T) {
	s := NewTaskStore()
	id := runNumberTask(t, s)
	if got := runOf(t, s, id); got != 1 {
		t.Fatalf("after create: run %d, want 1", got)
	}
	resumeForRun(t, s, id)
	resumeForRun(t, s, id)
	if got := runOf(t, s, id); got != 3 {
		t.Fatalf("after two resumes: run %d, want 3", got)
	}
}

// A WAL written before A1 has no run field: replay counts the opens.
func TestRunNumberReplaysFromRecordsWithoutIt(t *testing.T) {
	s := NewTaskStore()
	id := "3123456789abcdef0123456789abcdef"
	s.ReplayEvents([]WALEvent{
		{Type: "task_created", TaskID: id, Ts: 1},
		{Type: "task_resumed", TaskID: id, Ts: 2},
		{Type: "task_resumed", TaskID: id, Ts: 3},
	})
	if got := runOf(t, s, id); got != 3 {
		t.Fatalf("replayed run %d, want 3", got)
	}
}

func TestRunNumberSurvivesReplay(t *testing.T) {
	s := NewTaskStore()
	id := "4123456789abcdef0123456789abcdef"
	s.ReplayEvents([]WALEvent{
		{Type: "task_created", TaskID: id, Run: 1, Ts: 1},
		{Type: "task_resumed", TaskID: id, Run: 7, Ts: 2},
	})
	if got := runOf(t, s, id); got != 7 {
		t.Fatalf("replayed run %d, want the recorded 7", got)
	}
}

// A hold and its re-adoption continue the same run, so its record continues
// too (A-D20, Review Focus 1).
func TestRunNumberKeptAcrossHoldAndReadopt(t *testing.T) {
	s := NewTaskStore()
	id := runNumberTask(t, s)
	resumeForRun(t, s, id)
	s.Assign(id, protocol.RunnerID{}, "/wt", false)
	if err := s.MarkHold(id, "aa", "bb", 1); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := s.MarkReadopted(id, "aa", "bb", true); err != nil {
		t.Fatalf("readopt: %v", err)
	}
	if got := runOf(t, s, id); got != 2 {
		t.Fatalf("after hold and readopt: run %d, want 2", got)
	}
}
