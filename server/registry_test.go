package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRegistryAddFindRemove(t *testing.T) {
	r := NewRegistry()
	now := time.Now()

	r.Add(&RunnerEntry{
		ID:          "A",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: now,
		LastSeen:    now,
	})

	entry, ok := r.Get("A")
	if !ok {
		t.Fatal("expected Get(\"A\") ok=true, got false")
	}
	if entry.RepoPath != "/x" {
		t.Fatalf("expected RepoPath \"/x\", got %q", entry.RepoPath)
	}

	r.Remove("A")

	_, ok = r.Get("A")
	if ok {
		t.Fatal("expected Get(\"A\") ok=false after Remove, got true")
	}
}

func TestRegistryIdleByRepo(t *testing.T) {
	r := NewRegistry()

	r.Add(&RunnerEntry{
		ID:          "A",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Busy,
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
	})
	r.Add(&RunnerEntry{
		ID:          "B",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Unix(2, 0),
		LastSeen:    time.Unix(2, 0),
	})
	r.Add(&RunnerEntry{
		ID:          "C",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
	})

	oldest, ok := r.OldestIdleForRepo("/x")
	if !ok {
		t.Fatal("expected ok=true from OldestIdleForRepo, got false")
	}
	if oldest.ID != "C" {
		t.Fatalf("expected ID \"C\" (earliest ConnectedAt among Idle), got %q", oldest.ID)
	}
}

func TestRegistryNoIdle(t *testing.T) {
	r := NewRegistry()

	r.Add(&RunnerEntry{
		ID:          "A",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Busy,
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
	})

	_, ok := r.OldestIdleForRepo("/x")
	if ok {
		t.Fatal("expected ok=false from OldestIdleForRepo when no Idle runners, got true")
	}
}

func TestRegistrySetStatus(t *testing.T) {
	r := NewRegistry()
	now := time.Now()

	r.Add(&RunnerEntry{
		ID:          "A",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: now,
		LastSeen:    now,
	})

	beforeCall := time.Now()
	r.SetStatus("A", protocol.RunnerStatus_Busy, "task-1")

	entry, ok := r.Get("A")
	if !ok {
		t.Fatal("expected Get(\"A\") ok=true")
	}
	if entry.Status != protocol.RunnerStatus_Busy {
		t.Fatalf("expected Status=Busy, got %v", entry.Status)
	}
	if entry.CurrentTask != "task-1" {
		t.Fatalf("expected CurrentTask=\"task-1\", got %q", entry.CurrentTask)
	}
	if entry.LastSeen.Before(beforeCall) {
		t.Fatalf("expected LastSeen >= call time, but LastSeen=%v, beforeCall=%v", entry.LastSeen, beforeCall)
	}
}

func TestRegistrySetLastSeen(t *testing.T) {
	r := NewRegistry()
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)

	r.Add(&RunnerEntry{
		ID:          "A",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: t0,
		LastSeen:    t0,
	})

	ok := r.SetLastSeen("A", t1)
	if !ok {
		t.Fatal("expected SetLastSeen to return true for registered runner, got false")
	}

	entry, _ := r.Get("A")
	if !entry.LastSeen.Equal(t1) {
		t.Fatalf("expected LastSeen=%v after SetLastSeen, got %v", t1, entry.LastSeen)
	}

	// Returns false for unknown runner.
	if r.SetLastSeen("nonexistent", t1) {
		t.Fatal("expected SetLastSeen to return false for unknown runner, got true")
	}
}

func TestRegistryReadIsSnapshot(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunnerEntry{
		ID:          "A",
		RepoPath:    "/original",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Now(),
	})

	got, ok := r.Get("A")
	if !ok {
		t.Fatal("expected Get ok=true")
	}

	// Mutate the returned value snapshot; the registry must not be affected.
	got.RepoPath = "/poison"

	second, _ := r.Get("A")
	if second.RepoPath != "/original" {
		t.Fatalf("registry was poisoned by mutating returned snapshot: got RepoPath=%q, want \"/original\"", second.RepoPath)
	}
}
