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
	id := s.Create("/repo", "prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
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
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)

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
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
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
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)

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

// TestTaskStoreFinishAsymmetricIdempotency captures the intentional
// asymmetric idempotency of Finish: Cancelled IS overwritten (because
// Cancel is just a "SIGTERM in flight" marker — the runner's actual
// exit code is the real outcome), but Succeeded/Failed are NOT
// overwritten (those are final outcomes set by a prior Finish, and a
// duplicate or late-arriving Finish must not corrupt them).
func TestTaskStoreFinishAsymmetricIdempotency(t *testing.T) {
	t.Run("overwrites_Cancelled", func(t *testing.T) {
		s := NewTaskStore()
		id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
		s.Cancel(id)
		s.Finish(id, 0, nil)
		entry, _ := s.Get(id)
		if entry.Status != protocol.TaskStatus_Succeeded {
			t.Fatalf("Cancelled→Finish(0): want Succeeded, got %v", entry.Status)
		}
	})

	t.Run("noop_after_Succeeded", func(t *testing.T) {
		s := NewTaskStore()
		id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
		s.Finish(id, 0, nil)
		// Late-arriving Finish with non-zero exit must NOT corrupt the real outcome.
		s.Finish(id, 1, []byte("late"))
		entry, _ := s.Get(id)
		if entry.Status != protocol.TaskStatus_Succeeded {
			t.Fatalf("Succeeded→Finish(1): want Succeeded, got %v", entry.Status)
		}
		if entry.ExitCode == nil || *entry.ExitCode != 0 {
			t.Fatalf("ExitCode should still be 0, got %v", entry.ExitCode)
		}
	})

	t.Run("noop_after_Failed", func(t *testing.T) {
		s := NewTaskStore()
		id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
		s.Finish(id, 1, []byte("first"))
		s.Finish(id, 0, nil)
		entry, _ := s.Get(id)
		if entry.Status != protocol.TaskStatus_Failed {
			t.Fatalf("Failed→Finish(0): want Failed, got %v", entry.Status)
		}
	})
}

// TestTaskStoreTerminalClearsIsAttached verifies that transitioning to a
// terminal state clears IsAttached so `session ls` doesn't show stale
// attached=true on dead sessions.
func TestTaskStoreTerminalClearsIsAttached(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   func(*TaskStore, string)
	}{
		{"Finish_zero", func(s *TaskStore, id string) { s.Finish(id, 0, nil) }},
		{"Finish_nonzero", func(s *TaskStore, id string) { s.Finish(id, 1, nil) }},
		{"Cancel", func(s *TaskStore, id string) { s.Cancel(id) }},
		{"MarkFailed", func(s *TaskStore, id string) { s.MarkFailed(id, "x") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewTaskStore()
			id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
			s.MarkAttached(id, true)
			tc.op(s, id)
			entry, _ := s.Get(id)
			if entry.IsAttached {
				t.Fatalf("IsAttached should be false after %s, got true", tc.name)
			}
		})
	}
}

func TestTaskStoreNextQueuedForRepo(t *testing.T) {
	s := NewTaskStore()
	a := s.Create("/x", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	b := s.Create("/x", "b", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	_ = s.Create("/y", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)

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
		ids[i] = s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
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

// PruneByIDs must fire OnPrune for each removed task (so the server can publish
// a TaskPruned event and clients drop the row). PruneTerminal shares the same
// capture-then-fire-after-unlock code path.
func TestTaskStore_PruneByIDsFiresOnPrune(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	s.MarkFailed(id, "done") // terminal, so prune (force=false) removes it

	var pruned []string
	s.OnPrune = func(pid string) { pruned = append(pruned, pid) }

	removed, _, _ := s.PruneByIDs([]string{id}, false, "")
	if removed != 1 {
		t.Fatalf("removed=%d want 1", removed)
	}
	if len(pruned) != 1 || pruned[0] != id {
		t.Fatalf("OnPrune fired %v, want [%s]", pruned, id)
	}
	if _, ok := s.Get(id); ok {
		t.Fatal("task still present after prune")
	}
}

// An active (non-terminal) task skipped by PruneByIDs must NOT fire OnPrune.
func TestTaskStore_PruneSkipsActiveNoOnPrune(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	// left Queued (non-terminal)

	fired := false
	s.OnPrune = func(string) { fired = true }

	removed, skippedActive, _ := s.PruneByIDs([]string{id}, false, "")
	if removed != 0 || skippedActive != 1 {
		t.Fatalf("removed=%d skippedActive=%d want 0/1", removed, skippedActive)
	}
	if fired {
		t.Fatal("OnPrune must not fire for a skipped active task")
	}
}

func TestTaskStoreCancelRunning(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
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
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)

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
	id := s.Create("/original", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)

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
	id := s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
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
	if string(got.ErrorMsg) != "done" {
		t.Fatalf("DiffInfo lost: %q", got.ErrorMsg)
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
	a := s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	b := s.Create("/r", "q", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
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
	if string(got.ErrorMsg) != "server_restart" {
		t.Fatalf("expected ErrorMsg=server_restart, got %q", got.ErrorMsg)
	}
}

func TestTaskStorePruneTerminal(t *testing.T) {
	s := NewTaskStore()

	// 1: queued (still active — must NOT be pruned)
	idQueued := s.Create("/r", "still-queued", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)

	// 2: succeeded long ago (should be pruned)
	idOldSucc := s.Create("/r", "old-success", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	s.Assign(idOldSucc, "runner-x", "/wt-1")
	s.Finish(idOldSucc, 0, nil)
	oldTime := time.Now().Add(-48 * time.Hour)
	got, _ := s.Get(idOldSucc)
	*s.tasks[got.ID].EndedAt = oldTime

	// 3: failed recently (must NOT be pruned)
	idRecentFail := s.Create("/r", "recent-fail", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	s.Assign(idRecentFail, "runner-x", "/wt-2")
	s.Finish(idRecentFail, 7, nil)

	// 4: cancelled long ago (should be pruned)
	idOldCancel := s.Create("/r", "old-cancel", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
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

func TestTaskStorePruneByIDs(t *testing.T) {
	s := NewTaskStore()

	// One running, one terminal, one we'll request that doesn't exist.
	idActive := s.Create("/r", "still-running", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	s.Assign(idActive, "runner-x", "/wt-1")
	// Status stays Running

	idTerminal := s.Create("/r", "done", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	s.Assign(idTerminal, "runner-x", "/wt-2")
	s.Finish(idTerminal, 0, nil)

	// Keepalive task that shouldn't be touched.
	idKeep := s.Create("/r", "untouched", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	s.Assign(idKeep, "runner-x", "/wt-3")
	s.Finish(idKeep, 0, nil)

	missingID := "deadbeefdeadbeefdeadbeefdeadbeef"

	logDir := t.TempDir()
	// Sentinel log file for the terminal task — should be removed.
	if err := os.WriteFile(filepath.Join(logDir, idTerminal+".log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pass 1: force=false should refuse the active task and report missing.
	removed, active, missing := s.PruneByIDs([]string{idActive, idTerminal, missingID}, false, logDir)
	if removed != 1 || active != 1 || missing != 1 {
		t.Fatalf("PruneByIDs(force=false): removed=%d active=%d missing=%d, want 1/1/1", removed, active, missing)
	}
	if _, ok := s.Get(idActive); !ok {
		t.Errorf("active task should be retained without --force")
	}
	if _, ok := s.Get(idTerminal); ok {
		t.Errorf("terminal task should be pruned")
	}
	if _, ok := s.Get(idKeep); !ok {
		t.Errorf("unrelated task should be retained")
	}

	// Pass 2: force=true should now remove the active one too.
	removed, active, missing = s.PruneByIDs([]string{idActive}, true, logDir)
	if removed != 1 || active != 0 || missing != 0 {
		t.Fatalf("PruneByIDs(force=true): removed=%d active=%d missing=%d, want 1/0/0", removed, active, missing)
	}
	if _, ok := s.Get(idActive); ok {
		t.Errorf("active task should be force-pruned")
	}
	if got := len(s.List(0)); got != 1 {
		t.Errorf("List(0) length=%d, want 1 (only %s remains)", got, idKeep)
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
		protocol.TaskID{}, "runner-A", sel, nil, protocol.Capability_All)
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
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	ts.Assign(id, "runner-x", "/wt")
	ts.MarkFailed(id, "runner_disconnected")
	got, _ := ts.Get(id)
	if got.Status != protocol.TaskStatus_Failed {
		t.Fatalf("status=%v want Failed", got.Status)
	}
	if string(got.ErrorMsg) != "runner_disconnected" {
		t.Fatalf("DiffInfo=%q want runner_disconnected", string(got.ErrorMsg))
	}
}

func TestTaskStoreMarkFailedIdempotentOnTerminal(t *testing.T) {
	ts := NewTaskStore()
	id := ts.Create("/x", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	ts.Finish(id, 0, nil) // already terminal (Succeeded)
	ts.MarkFailed(id, "runner_disconnected")
	got, _ := ts.Get(id)
	if got.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("MarkFailed should be no-op on terminal state, got %v", got.Status)
	}
}

// createDetachableTask is a helper that creates a task, marks it detachable,
// and assigns it to a runner (transitioning it to Running).
func createDetachableTask(ts *TaskStore) string {
	id := ts.Create("/repo", "detach-test", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	ts.SetDetachableFlag(id, true)
	ts.Assign(id, "runner-det", "/wt/det")
	return id
}

func TestTaskStore_DetachedTransitions(t *testing.T) {
	t.Run("Running_to_Detached", func(t *testing.T) {
		ts := NewTaskStore()
		id := createDetachableTask(ts)

		if err := ts.SetDetached(id); err != nil {
			t.Fatalf("SetDetached: %v", err)
		}
		info, ok := ts.Get(id)
		if !ok {
			t.Fatal("Get returned ok=false after SetDetached")
		}
		if info.Status != protocol.TaskStatus_Detached {
			t.Fatalf("expected Detached, got %v", info.Status)
		}
		if info.DetachedAt == 0 {
			t.Fatal("DetachedAt should be set after SetDetached")
		}
		if info.IsAttached {
			t.Fatal("IsAttached should be false after SetDetached")
		}
	})

	t.Run("SetDetached_requires_Running", func(t *testing.T) {
		ts := NewTaskStore()
		id := ts.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
		// Task is Queued, not Running — must fail.
		if err := ts.SetDetached(id); err == nil {
			t.Fatal("SetDetached on Queued task should return error, got nil")
		}
	})

	t.Run("SetDetached_unknown_task", func(t *testing.T) {
		ts := NewTaskStore()
		if err := ts.SetDetached("nonexistent"); err == nil {
			t.Fatal("SetDetached on unknown task should return error, got nil")
		}
	})

	t.Run("Detached_to_Running_via_Assign", func(t *testing.T) {
		ts := NewTaskStore()
		id := createDetachableTask(ts)
		ts.SetDetached(id) //nolint:errcheck

		// Re-attach: Assign from Detached state.
		ts.Assign(id, "runner-det", "/wt/det")
		info, ok := ts.Get(id)
		if !ok {
			t.Fatal("Get returned ok=false")
		}
		if info.Status != protocol.TaskStatus_Running {
			t.Fatalf("expected Running after re-attach, got %v", info.Status)
		}
		if info.DetachedAt != 0 {
			t.Fatalf("DetachedAt should be cleared on re-attach, got %d", info.DetachedAt)
		}
		if !info.IsAttached {
			t.Fatal("IsAttached should be true after re-attach via Assign")
		}
	})

	t.Run("MarkAttached_Detached_to_Running", func(t *testing.T) {
		ts := NewTaskStore()
		id := createDetachableTask(ts)
		ts.SetDetached(id) //nolint:errcheck

		// MarkAttached on a Detached task transitions it to Running.
		if !ts.MarkAttached(id, true) {
			t.Fatal("MarkAttached returned false for existing task")
		}
		info, _ := ts.Get(id)
		if info.Status != protocol.TaskStatus_Running {
			t.Fatalf("expected Running after MarkAttached, got %v", info.Status)
		}
		if info.DetachedAt != 0 {
			t.Fatalf("DetachedAt should be cleared, got %d", info.DetachedAt)
		}
	})

	t.Run("Detached_to_Cancelled", func(t *testing.T) {
		ts := NewTaskStore()
		id := createDetachableTask(ts)
		ts.SetDetached(id) //nolint:errcheck

		ts.Cancel(id)
		info, _ := ts.Get(id)
		if info.Status != protocol.TaskStatus_Cancelled {
			t.Fatalf("expected Cancelled, got %v", info.Status)
		}
		if info.EndedAt == nil {
			t.Fatal("EndedAt should be set after Cancel")
		}
	})

	t.Run("Detached_to_Succeeded", func(t *testing.T) {
		ts := NewTaskStore()
		id := createDetachableTask(ts)
		ts.SetDetached(id) //nolint:errcheck

		ts.Finish(id, 0, nil)
		info, _ := ts.Get(id)
		if info.Status != protocol.TaskStatus_Succeeded {
			t.Fatalf("expected Succeeded, got %v", info.Status)
		}
	})

	t.Run("Detached_to_Failed_via_Finish", func(t *testing.T) {
		ts := NewTaskStore()
		id := createDetachableTask(ts)
		ts.SetDetached(id) //nolint:errcheck

		ts.Finish(id, 1, []byte("timeout"))
		info, _ := ts.Get(id)
		if info.Status != protocol.TaskStatus_Failed {
			t.Fatalf("expected Failed, got %v", info.Status)
		}
	})

	t.Run("Detached_to_Failed_via_MarkFailed", func(t *testing.T) {
		ts := NewTaskStore()
		id := createDetachableTask(ts)
		ts.SetDetached(id) //nolint:errcheck

		ts.MarkFailed(id, "runner_unreachable")
		info, _ := ts.Get(id)
		if info.Status != protocol.TaskStatus_Failed {
			t.Fatalf("expected Failed, got %v", info.Status)
		}
		if string(info.ErrorMsg) != "runner_unreachable" {
			t.Fatalf("ErrorMsg=%q want runner_unreachable", string(info.ErrorMsg))
		}
	})
}

func TestTaskStore_DetachableFlag(t *testing.T) {
	ts := NewTaskStore()
	id := ts.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)

	// Default: not detachable.
	got, _ := ts.Get(id)
	if got.Detachable {
		t.Fatal("new task should not be detachable by default")
	}

	// Set detachable.
	if !ts.SetDetachableFlag(id, true) {
		t.Fatal("SetDetachableFlag returned false for existing task")
	}
	got, _ = ts.Get(id)
	if !got.Detachable {
		t.Fatal("Detachable should be true after SetDetachableFlag")
	}

	// Unknown task returns false.
	if ts.SetDetachableFlag("nope", true) {
		t.Fatal("SetDetachableFlag should return false for unknown task")
	}
}

func TestTaskStore_SetRingBufferBytes(t *testing.T) {
	ts := NewTaskStore()
	id := ts.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)

	if !ts.SetRingBufferBytes(id, 4096) {
		t.Fatal("SetRingBufferBytes returned false for existing task")
	}
	got, _ := ts.Get(id)
	if got.RingBufferBytes != 4096 {
		t.Fatalf("RingBufferBytes=%d want 4096", got.RingBufferBytes)
	}

	if ts.SetRingBufferBytes("nope", 100) {
		t.Fatal("SetRingBufferBytes should return false for unknown task")
	}
}

func TestTaskStore_MarkAttached(t *testing.T) {
	ts := NewTaskStore()
	id := createDetachableTask(ts)

	// Default: not attached (task just assigned, no explicit mark).
	got, _ := ts.Get(id)
	if got.IsAttached {
		t.Fatal("IsAttached should be false by default")
	}

	// Mark attached.
	if !ts.MarkAttached(id, true) {
		t.Fatal("MarkAttached returned false")
	}
	got, _ = ts.Get(id)
	if !got.IsAttached {
		t.Fatal("IsAttached should be true after MarkAttached(true)")
	}
	// Status must remain Running (was already Running, not Detached).
	if got.Status != protocol.TaskStatus_Running {
		t.Fatalf("status should stay Running, got %v", got.Status)
	}

	// Mark detached via MarkAttached (does NOT change status from Running).
	if !ts.MarkAttached(id, false) {
		t.Fatal("MarkAttached(false) returned false")
	}
	got, _ = ts.Get(id)
	if got.IsAttached {
		t.Fatal("IsAttached should be false after MarkAttached(false)")
	}
	// Status stays Running — MarkAttached(false) does not transition to Detached;
	// use SetDetached for that.
	if got.Status != protocol.TaskStatus_Running {
		t.Fatalf("status should still be Running after MarkAttached(false), got %v", got.Status)
	}

	// Unknown task.
	if ts.MarkAttached("nope", true) {
		t.Fatal("MarkAttached should return false for unknown task")
	}
}

func TestResumeRecordsResumedByKind(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "runner1", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	s.Assign(id, "runner1", "/wt")
	s.Finish(id, 0, nil)
	if _, err := s.Resume(id, "p2", nil, protocol.RunnerSelector{}, "runner1", protocol.ClientKind_Agent); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ := s.Get(id)
	if got.OriginKind != protocol.ClientKind_Cli {
		t.Fatalf("origin should stay cli, got %v", got.OriginKind)
	}
	if got.ResumedByKind != protocol.ClientKind_Agent {
		t.Fatalf("resumed_by should be agent, got %v", got.ResumedByKind)
	}
}

// TestWALReplayRestoresAttribution verifies that CreatorTaskID and
// ResumedByKind survive a full WAL persist → ReadWAL → ReplayEvents
// round-trip. CreatorTaskID must be non-zero and unchanged after a
// resume; ResumedByKind must reflect the most recent resumer.
func TestWALReplayRestoresAttribution(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "attr.log")
	wal, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}

	// Build a non-zero creator task ID.
	var creatorID protocol.TaskID
	creatorID.Id[0] = 0xCA
	creatorID.Id[1] = 0xFE

	s := NewTaskStore()
	s.SetWAL(wal)

	// Create a task with kind=agent and a non-zero creator task id.
	id := s.Create("/repo", "agent-prompt",
		protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent,
		creatorID,
		"",
		protocol.RunnerSelector{},
		nil,
		protocol.Capability_All,
	)

	// Finish it (terminal state is required before Resume).
	s.Assign(id, "runner-x", "/wt/x")
	s.Finish(id, 0, nil)

	// Resume with resumer kind = ClientKind_Tui.
	if _, err := s.Resume(id, "agent-prompt-v2", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Tui); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	wal.Close() //nolint:errcheck

	// Read and replay into a fresh store.
	events, readErr := ReadWAL(walPath)
	if readErr != nil {
		t.Fatalf("ReadWAL: %v", readErr)
	}

	s2 := NewTaskStore()
	s2.ReplayEvents(events)

	got, ok := s2.Get(id)
	if !ok {
		t.Fatalf("task %q not found after replay", id)
	}

	// CreatorTaskID must be non-zero and match the original.
	if got.CreatorTaskID.Id == ([16]byte{}) {
		t.Fatal("CreatorTaskID is zero after WAL replay — not persisted or not restored")
	}
	if got.CreatorTaskID.Id != creatorID.Id {
		t.Fatalf("CreatorTaskID mismatch: got %x, want %x", got.CreatorTaskID.Id, creatorID.Id)
	}

	// ResumedByKind must reflect the resumer set during Resume.
	if got.ResumedByKind != protocol.ClientKind_Tui {
		t.Fatalf("ResumedByKind=%v after replay, want Tui", got.ResumedByKind)
	}
}

func TestCreateRecordsCreatorTaskID(t *testing.T) {
	s := NewTaskStore()
	var creator protocol.TaskID
	creator.Id = [16]byte{0xAA, 0xBB}
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, creator, "runner1", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	got, _ := s.Get(id)
	if got.CreatorTaskID.Id != creator.Id {
		t.Fatalf("creator = %x, want %x", got.CreatorTaskID.Id, creator.Id)
	}
	var zero protocol.TaskID
	id2 := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, zero, "runner1", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	got2, _ := s.Get(id2)
	if got2.CreatorTaskID.Id != ([16]byte{}) {
		t.Fatalf("operator creator should be zero, got %x", got2.CreatorTaskID.Id)
	}
	s.Assign(id, "runner1", "/wt")
	s.Finish(id, 0, nil)
	if _, err := s.Resume(id, "p2", nil, protocol.RunnerSelector{}, "runner1", protocol.ClientKind_Agent); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got3, _ := s.Get(id)
	if got3.CreatorTaskID.Id != creator.Id {
		t.Fatalf("resume changed creator: %x", got3.CreatorTaskID.Id)
	}
}

func TestCapabilitiesPersistAndReplay(t *testing.T) {
	caps := protocol.Capability_Spawn | protocol.Capability_FileRead
	ev := WALEvent{Type: "task_created", TaskID: "abc", Capabilities: uint32(caps)}
	b, err := ev.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got WALEvent
	if err := got.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if protocol.Capability(got.Capabilities) != caps {
		t.Fatalf("caps round-trip = %#x, want %#x", got.Capabilities, caps)
	}
}
