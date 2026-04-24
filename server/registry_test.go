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

	oldest := r.OldestIdleForRepo("/x")
	if oldest == nil {
		t.Fatal("expected non-nil result from OldestIdleForRepo")
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

	result := r.OldestIdleForRepo("/x")
	if result != nil {
		t.Fatalf("expected nil from OldestIdleForRepo when no Idle runners, got %v", result)
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
