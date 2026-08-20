package server

import (
	"path/filepath"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func newTaskForSkills(t *testing.T, s *TaskStore) string {
	t.Helper()
	return s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Tui,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil,
		protocol.Capability_All, defaultScope(), "claude")
}

// A queued task has no runner yet, so it has nothing to declare. False here
// means "unknown", which is why the display surfaces word it that way rather
// than as a positive "bare agent".
func TestSkillsInjectedIsFalseBeforeAssign(t *testing.T) {
	s := NewTaskStore()
	id := newTaskForSkills(t, s)
	got, ok := s.Get(id)
	if !ok {
		t.Fatal("Get after Create returned ok=false")
	}
	if got.SkillsInjected {
		t.Error("a Queued task claims skills_injected before any runner was chosen")
	}
}

func TestAssignStampsSkillsInjected(t *testing.T) {
	s := NewTaskStore()
	id := newTaskForSkills(t, s)
	s.Assign(id, "runner-x", "/wt", true)
	got, _ := s.Get(id)
	if !got.SkillsInjected {
		t.Error("Assign did not stamp the runner's skills_injected onto the task")
	}
}

// A Detached task can come back on a DIFFERENT runner. The stale value would
// then describe a worktree nobody is running in, so re-attach re-stamps it even
// though it writes no WAL event (same rule the existing StartedAt handling
// follows: runtime state flips, persisted state does not).
func TestReattachRestampsSkillsInjected(t *testing.T) {
	s := NewTaskStore()
	id := newTaskForSkills(t, s)
	s.Assign(id, "runner-injecting", "/wt", true)
	s.SetDetached(id)
	s.Assign(id, "runner-bare", "/wt", false)
	got, _ := s.Get(id)
	if got.SkillsInjected {
		t.Error("re-attach on a non-injecting runner left the old true in place")
	}
}

// The whole point of putting the bit on the task: it must outlive the runner it
// came from, because the runner record is gone by the time an operator looks at
// a Failed row — and a confined caller never had the runner list at all.
func TestSkillsInjectedSurvivesWALReplay(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "skills.log")
	wal, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	s := NewTaskStore()
	s.SetWAL(wal)
	id := newTaskForSkills(t, s)
	s.Assign(id, "runner-x", "/wt", true)
	if err := wal.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	events, err := ReadWAL(walPath)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	s2 := NewTaskStore()
	s2.ReplayEvents(events)
	got, ok := s2.Get(id)
	if !ok {
		t.Fatalf("replay: task %q not found", id)
	}
	if !got.SkillsInjected {
		t.Error("skills_injected was lost across a WAL persist -> replay round trip")
	}
}

// A task_assigned record written before this field existed has no key at all.
// It must replay as false — "we cannot say" — not as a fabricated true.
func TestLegacyAssignedRecordReplaysAsNotInjected(t *testing.T) {
	s := NewTaskStore()
	s.ReplayEvents([]WALEvent{
		{Type: "task_created", TaskID: "legacy00000000000000000000000001", RepoPath: "/repo", Prompt: "p"},
		{Type: "task_assigned", TaskID: "legacy00000000000000000000000001", RunnerID: "runner-x", WorktreeDir: "/wt"},
	})
	got, ok := s.Get("legacy00000000000000000000000001")
	if !ok {
		t.Fatal("legacy task not found after replay")
	}
	if got.SkillsInjected {
		t.Error("a legacy task_assigned event replayed as injected")
	}
}

// The single upward hop a confined caller gets on its own creator is stripped
// of everything that says WHERE the parent runs. skills_injected is a property
// of the parent's RUNNER, so it goes with AssignedTo.
func TestRedactParentClearsSkillsInjected(t *testing.T) {
	var info protocol.TaskInfo
	info.SetSkillsInjected(true)
	redactParentTaskInfo(&info)
	if info.SkillsInjected() {
		t.Error("the redacted parent row still reports its runner's skills_injected")
	}
}
