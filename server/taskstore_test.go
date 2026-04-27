package server

import (
	"os"
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
	id := s.Create("/repo", "prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
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
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})

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
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
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
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})

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
	a := s.Create("/x", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
	b := s.Create("/x", "b", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
	_ = s.Create("/y", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})

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
		ids[i] = s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
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
	id := s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
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
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})

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
	id := s.Create("/original", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})

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
	id := s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
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
	a := s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
	b := s.Create("/r", "q", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
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

func TestTaskStorePruneTerminal(t *testing.T) {
	s := NewTaskStore()

	// 1: queued (still active — must NOT be pruned)
	idQueued := s.Create("/r", "still-queued", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})

	// 2: succeeded long ago (should be pruned)
	idOldSucc := s.Create("/r", "old-success", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
	s.Assign(idOldSucc, "runner-x", "/wt-1")
	s.Finish(idOldSucc, 0, nil)
	oldTime := time.Now().Add(-48 * time.Hour)
	got, _ := s.Get(idOldSucc)
	*s.tasks[got.ID].EndedAt = oldTime

	// 3: failed recently (must NOT be pruned)
	idRecentFail := s.Create("/r", "recent-fail", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
	s.Assign(idRecentFail, "runner-x", "/wt-2")
	s.Finish(idRecentFail, 7, nil)

	// 4: cancelled long ago (should be pruned)
	idOldCancel := s.Create("/r", "old-cancel", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
	s.Cancel(idOldCancel)
	*s.tasks[idOldCancel].EndedAt = oldTime

	cutoff := time.Now().Add(-24 * time.Hour)
	logDir := t.TempDir()
	// Drop a sentinel log file that should be removed.
	logPath := filepath.Join(logDir, idOldSucc+".log")
	if err := os.WriteFile(logPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed := s.PruneTerminal(cutoff, logDir)
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	if _, ok := s.Get(idOldSucc); ok {
		t.Errorf("old-succeeded task should be gone")
	}
	if _, ok := s.Get(idOldCancel); ok {
		t.Errorf("old-cancelled task should be gone")
	}
	if _, ok := s.Get(idQueued); !ok {
		t.Errorf("queued task should be retained")
	}
	if _, ok := s.Get(idRecentFail); !ok {
		t.Errorf("recent failed task should be retained")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("log file should be removed, got err=%v", err)
	}
	if got := len(s.List(0)); got != 2 {
		t.Errorf("List(0) length=%d, want 2", got)
	}
}

func TestReplayHandlesPruned(t *testing.T) {
	s := NewTaskStore()
	exit := int32(0)
	events := []WALEvent{
		{Type: "task_created", TaskID: "abc", RepoPath: "/r", Prompt: "p", Ts: 100},
		{Type: "task_finished", TaskID: "abc", ExitCode: &exit, Ts: 200},
		{Type: "task_pruned", TaskID: "abc", Ts: 300},
		{Type: "task_created", TaskID: "xyz", RepoPath: "/r", Prompt: "q", Ts: 400},
	}
	s.ReplayEvents(events)
	if _, ok := s.Get("abc"); ok {
		t.Errorf("pruned task abc should not survive replay")
	}
	if _, ok := s.Get("xyz"); !ok {
		t.Errorf("xyz should survive replay")
	}
	if got := len(s.List(0)); got != 1 {
		t.Errorf("List(0) length=%d, want 1", got)
	}
}

// mustHostname is a test helper that builds a protocol.Hostname from a plain string.
func mustHostname(t *testing.T, s string) protocol.Hostname {
	t.Helper()
	var h protocol.Hostname
	if !h.SetName([]byte(s)) {
		t.Fatalf("SetName(%q) failed", s)
	}
	return h
}

func TestTaskStoreAddCarriesSelectorAndBoundRunner(t *testing.T) {
	ts := NewTaskStore()
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	sel.SetHostname(mustHostname(t, "gmkhost"))
	taskID := ts.Create("/x/repo", "hello", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		"runner-A", sel)
	got, ok := ts.Get(taskID)
	if !ok {
		t.Fatal("Get failed")
	}
	if got.BoundRunnerID != "runner-A" {
		t.Fatalf("BoundRunnerID=%q want runner-A", got.BoundRunnerID)
	}
	if got.Selector.Kind != protocol.RunnerSelectorKind_ByHostname {
		t.Fatalf("Selector.Kind=%v want ByHostname", got.Selector.Kind)
	}
}

func TestTaskStoreMarkFailedTransitions(t *testing.T) {
	ts := NewTaskStore()
	id := ts.Create("/x", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		"", protocol.RunnerSelector{})
	ts.Assign(id, "runner-x", "/wt")
	ts.MarkFailed(id, "runner_disconnected")
	got, _ := ts.Get(id)
	if got.Status != protocol.TaskStatus_Failed {
		t.Fatalf("status=%v want Failed", got.Status)
	}
	if string(got.DiffInfo) != "runner_disconnected" {
		t.Fatalf("DiffInfo=%q want runner_disconnected", string(got.DiffInfo))
	}
}

func TestTaskStoreMarkFailedIdempotentOnTerminal(t *testing.T) {
	ts := NewTaskStore()
	id := ts.Create("/x", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		"", protocol.RunnerSelector{})
	ts.Finish(id, 0, nil) // already terminal (Succeeded)
	ts.MarkFailed(id, "runner_disconnected")
	got, _ := ts.Get(id)
	if got.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("MarkFailed should be no-op on terminal state, got %v", got.Status)
	}
}

