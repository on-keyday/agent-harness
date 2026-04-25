package server

import (
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var hexRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestTaskStoreCreate(t *testing.T) {
	s := NewTaskStore()
	before := time.Now()
	id := s.Create("/repo", "prompt")
	after := time.Now()

	if len(id) != 32 {
		t.Fatalf("expected id length 32, got %d", len(id))
	}
	if !hexRE.MatchString(id) {
		t.Fatalf("expected 32 lowercase hex chars, got %q", id)
	}

	entry, ok := s.Get(id)
	if !ok {
		t.Fatalf("Get(%q) returned ok=false, want true", id)
	}
	if entry.Status != protocol.TaskStatus_Queued {
		t.Fatalf("expected Status=Queued, got %v", entry.Status)
	}
	if entry.Prompt != "prompt" {
		t.Fatalf("expected Prompt=%q, got %q", "prompt", entry.Prompt)
	}
	if entry.RepoPath != "/repo" {
		t.Fatalf("expected RepoPath=%q, got %q", "/repo", entry.RepoPath)
	}
	// CreatedAt must fall within the before/after sandwich around Create.
	if entry.CreatedAt.Before(before) || entry.CreatedAt.After(after) {
		t.Fatalf("CreatedAt %v not in [%v, %v]", entry.CreatedAt, before, after)
	}
}

func TestTaskStoreAssignAndFinish(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p")

	s.Assign(id, "runner-1", "/tmp/wt")

	entry, ok := s.Get(id)
	if !ok {
		t.Fatal("Get after Assign returned ok=false")
	}
	if entry.Status != protocol.TaskStatus_Running {
		t.Fatalf("expected Status=Running, got %v", entry.Status)
	}
	if entry.AssignedTo != "runner-1" {
		t.Fatalf("expected AssignedTo=%q, got %q", "runner-1", entry.AssignedTo)
	}
	if entry.WorktreeDir != "/tmp/wt" {
		t.Fatalf("expected WorktreeDir=%q, got %q", "/tmp/wt", entry.WorktreeDir)
	}
	if entry.StartedAt == nil {
		t.Fatal("expected StartedAt non-nil after Assign")
	}

	s.Finish(id, 0, []byte{})

	entry, ok = s.Get(id)
	if !ok {
		t.Fatal("Get after Finish returned ok=false")
	}
	if entry.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("expected Status=Succeeded, got %v", entry.Status)
	}
	if entry.ExitCode == nil {
		t.Fatal("expected ExitCode non-nil after Finish")
	}
	if *entry.ExitCode != 0 {
		t.Fatalf("expected *ExitCode=0, got %d", *entry.ExitCode)
	}
	if entry.EndedAt == nil {
		t.Fatal("expected EndedAt non-nil after Finish")
	}
}

func TestTaskStoreFinishNonZero(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p")
	s.Assign(id, "r", "/wt")
	s.Finish(id, 7, nil)

	entry, ok := s.Get(id)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if entry.Status != protocol.TaskStatus_Failed {
		t.Fatalf("expected Status=Failed, got %v", entry.Status)
	}
	if entry.ExitCode == nil {
		t.Fatal("expected ExitCode non-nil")
	}
	if *entry.ExitCode != 7 {
		t.Fatalf("expected *ExitCode=7, got %d", *entry.ExitCode)
	}
}

func TestTaskStoreCancel(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p")

	s.Cancel(id)

	entry, ok := s.Get(id)
	if !ok {
		t.Fatal("Get after Cancel returned ok=false")
	}
	if entry.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("expected Status=Cancelled, got %v", entry.Status)
	}
	if entry.EndedAt == nil {
		t.Fatal("expected EndedAt non-nil after Cancel")
	}

	// Idempotent: second Cancel must not panic and status stays Cancelled.
	s.Cancel(id)
	entry2, _ := s.Get(id)
	if entry2.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("expected Status=Cancelled after second Cancel, got %v", entry2.Status)
	}
}

func TestTaskStoreNextQueuedForRepo(t *testing.T) {
	s := NewTaskStore()
	a := s.Create("/x", "a")
	b := s.Create("/x", "b")
	_ = s.Create("/y", "c")

	got, ok := s.NextQueuedForRepo("/x")
	if !ok {
		t.Fatal("expected ok=true for /x, got false")
	}
	if got.ID != a {
		t.Fatalf("expected earliest id %q, got %q", a, got.ID)
	}

	s.Assign(a, "r", "/wt")

	got, ok = s.NextQueuedForRepo("/x")
	if !ok {
		t.Fatal("expected ok=true for /x after assigning a, got false")
	}
	if got.ID != b {
		t.Fatalf("expected id %q after a assigned, got %q", b, got.ID)
	}

	_, ok = s.NextQueuedForRepo("/z")
	if ok {
		t.Fatal("expected ok=false for /z, got true")
	}
}

func TestTaskStoreListLimit(t *testing.T) {
	s := NewTaskStore()
	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		ids[i] = s.Create("/repo", "p")
	}

	all := s.List(0)
	if len(all) != 5 {
		t.Fatalf("List(0) expected 5 entries, got %d", len(all))
	}
	// Verify insertion order.
	for i, e := range all {
		if e.ID != ids[i] {
			t.Fatalf("List(0)[%d]: expected id %q, got %q", i, ids[i], e.ID)
		}
	}

	recent := s.List(3)
	if len(recent) != 3 {
		t.Fatalf("List(3) expected 3 entries, got %d", len(recent))
	}
	// Most-recent 3 in insertion order = ids[2], ids[3], ids[4].
	expected := ids[2:]
	for i, e := range recent {
		if e.ID != expected[i] {
			t.Fatalf("List(3)[%d]: expected id %q, got %q", i, expected[i], e.ID)
		}
	}
}

func TestTaskStoreCancelRunning(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/r", "p")
	s.Assign(id, "runner-x", "/wt")
	// sanity: now Running
	if got, _ := s.Get(id); got.Status != protocol.TaskStatus_Running {
		t.Fatalf("expected Running, got %v", got.Status)
	}
	s.Cancel(id)
	got, _ := s.Get(id)
	if got.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("expected Cancelled after cancelling a Running task, got %v", got.Status)
	}
	if got.EndedAt == nil {
		t.Fatalf("EndedAt should be set after Cancel")
	}
}

func TestTaskStoreSetWorktreeDir(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p")

	ok := s.SetWorktreeDir(id, "/new/wt")
	if !ok {
		t.Fatal("expected SetWorktreeDir to return true for existing task, got false")
	}

	entry, _ := s.Get(id)
	if entry.WorktreeDir != "/new/wt" {
		t.Fatalf("expected WorktreeDir=%q after SetWorktreeDir, got %q", "/new/wt", entry.WorktreeDir)
	}

	// Returns false for unknown task.
	if s.SetWorktreeDir("nonexistent", "/wt") {
		t.Fatal("expected SetWorktreeDir to return false for unknown task, got true")
	}
}

func TestTaskStoreReadIsSnapshot(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/original", "p")

	got, ok := s.Get(id)
	if !ok {
		t.Fatal("expected Get ok=true")
	}

	// Mutate the returned value snapshot; the store must not be affected.
	got.RepoPath = "/poison"

	second, _ := s.Get(id)
	if second.RepoPath != "/original" {
		t.Fatalf("store was poisoned by mutating returned snapshot: got RepoPath=%q, want \"/original\"", second.RepoPath)
	}
}

func TestTaskStoreWALWriteAndReplay(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "events.log")
	wal, _ := OpenWAL(walPath)

	s := NewTaskStore()
	s.SetWAL(wal)
	id := s.Create("/r", "p")
	before := time.Now()
	s.Assign(id, "runner-x", "/tmp/wt")
	s.Finish(id, 0, []byte("done"))
	after := time.Now()
	wal.Close() //nolint:errcheck

	// Re-open and replay
	events, _ := ReadWAL(walPath)
	s2 := NewTaskStore()
	s2.ReplayEvents(events)
	got, ok := s2.Get(id)
	if !ok || got.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("replay: %+v ok=%v", got, ok)
	}
	if string(got.DiffInfo) != "done" {
		t.Fatalf("DiffInfo lost: %q", got.DiffInfo)
	}
	if got.StartedAt == nil {
		t.Fatal("StartedAt lost in replay")
	}
	if got.StartedAt.Before(before) || got.StartedAt.After(after) {
		t.Fatalf("StartedAt %v not in [%v, %v]", got.StartedAt, before, after)
	}
}

func TestTaskStoreOnCreateFires(t *testing.T) {
	s := NewTaskStore()
	var got []string
	s.OnCreate = func(id string) { got = append(got, id) }
	a := s.Create("/r", "p")
	b := s.Create("/r", "q")
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("got %v, expected [%s, %s]", got, a, b)
	}
}

func TestTaskStoreReplayMarksRunningAsFailed(t *testing.T) {
	s := NewTaskStore()
	events := []WALEvent{
		{Type: "task_created", TaskID: "abc", RepoPath: "/r", Prompt: "p", Ts: 100},
		{Type: "task_assigned", TaskID: "abc", RunnerID: "r1", WorktreeDir: "/wt", Ts: 200},
		// No task_finished — simulates server-restart while task was running
	}
	s.ReplayEvents(events)
	got, _ := s.Get("abc")
	if got.Status != protocol.TaskStatus_Failed {
		t.Fatalf("expected Failed, got %v", got.Status)
	}
}
