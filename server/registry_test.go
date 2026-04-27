package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRegistryAddFindRemove(t *testing.T) {
	r := NewRegistry()
	now := time.Now()

	r.Add(&RunnerEntry{
		ID:           "A",
		Hostname:     "hostA",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  now,
		LastSeen:     now,
	})

	entry, ok := r.Get("A")
	if !ok {
		t.Fatal("expected Get(\"A\") ok=true, got false")
	}
	if len(entry.AllowedRoots) == 0 || entry.AllowedRoots[0] != "/x" {
		t.Fatalf("expected AllowedRoots[\"/x\"], got %v", entry.AllowedRoots)
	}

	r.Remove("A")

	_, ok = r.Get("A")
	if ok {
		t.Fatal("expected Get(\"A\") ok=false after Remove, got true")
	}
}

func TestRegistrySetLastSeen(t *testing.T) {
	r := NewRegistry()
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)

	r.Add(&RunnerEntry{
		ID:           "A",
		Hostname:     "hostA",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  t0,
		LastSeen:     t0,
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
		ID:           "A",
		Hostname:     "hostA",
		AllowedRoots: []string{"/original"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Now(),
	})

	got, ok := r.Get("A")
	if !ok {
		t.Fatal("expected Get ok=true")
	}

	// Mutate the returned value snapshot; the registry must not be affected.
	got.Hostname = "poison"

	second, _ := r.Get("A")
	if second.Hostname != "hostA" {
		t.Fatalf("registry was poisoned by mutating returned snapshot: got Hostname=%q, want \"hostA\"", second.Hostname)
	}
}

func TestRegistryBindTaskAtCapacity(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
	})
	if !r.BindTask("A", "t1") {
		t.Fatal("expected first BindTask to succeed")
	}
	if r.BindTask("A", "t2") {
		t.Fatal("expected second BindTask to fail at capacity")
	}
	r.UnbindTask("A", "t1")
	if !r.BindTask("A", "t2") {
		t.Fatal("expected BindTask to succeed after UnbindTask")
	}
}

func TestRegistryUnbindTaskIdempotent(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 2,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
	})
	r.UnbindTask("A", "absent") // double-release safe
	r.BindTask("A", "t1")
	r.UnbindTask("A", "t1")
	r.UnbindTask("A", "t1") // idempotent on already-unbound
}

func TestRegistryBindTaskRace(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
	})
	const N = 64
	results := make(chan bool, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- r.BindTask("A", fmt.Sprintf("t%d", i))
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for ok := range results {
		if ok {
			successes++
		}
	}
	if successes != 4 {
		t.Fatalf("expected exactly 4 successful binds (MaxTasks), got %d", successes)
	}
}

// TestRegistryStatusMethod verifies the Status() method derives status from connection + capacity.
func TestRegistryStatusMethod(t *testing.T) {
	now := time.Now()

	// No conn = Offline
	e := &RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
		Conn: nil,
	}
	if s := e.Status(); s.String() != "Offline" {
		t.Fatalf("expected Offline when Conn==nil, got %v", s)
	}

	// With a mock conn and no active tasks = Idle
	e.Conn = &fakeConn{}
	e.MaxTasks = 2
	if s := e.Status(); s.String() != "Idle" {
		t.Fatalf("expected Idle, got %v", s)
	}

	// Fill to capacity = Busy
	e.ActiveTasks["t1"] = struct{}{}
	e.ActiveTasks["t2"] = struct{}{}
	if s := e.Status(); s.String() != "Busy" {
		t.Fatalf("expected Busy at capacity, got %v", s)
	}
}

func TestRegistryCandidatesPrefixMatch(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "gmkhost", AllowedRoots: []string{"/home/kforfk/workspace"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})
	r.Add(&RunnerEntry{ID: "B", Hostname: "raspi", AllowedRoots: []string{"/home/pi/workspace"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})

	cs := r.Candidates("/home/kforfk/workspace/repo1", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	if len(cs) != 1 || cs[0].ID != "A" {
		t.Fatalf("expected only A, got %v", cs)
	}
	cs = r.Candidates("/home/pi/workspace/foo", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	if len(cs) != 1 || cs[0].ID != "B" {
		t.Fatalf("expected only B, got %v", cs)
	}
	cs = r.Candidates("/etc/passwd", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	if len(cs) != 0 {
		t.Fatalf("expected no candidates, got %v", cs)
	}
}

func TestRegistryCandidatesAmbiguous(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})
	r.Add(&RunnerEntry{ID: "B", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})
	cs := r.Candidates("/shared/foo", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	if len(cs) != 2 {
		t.Fatalf("expected 2 candidates (ambiguous), got %d", len(cs))
	}
}

func TestRegistryCandidatesCapacityAgnostic(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{"existing": {}}, ConnectedAt: now, LastSeen: now})
	cs := r.Candidates("/x/repo", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	if len(cs) != 1 {
		t.Fatalf("Candidates must include at-capacity runners (caller filters), got %d", len(cs))
	}
}

func TestRegistryCandidatesSelectorByHostname(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "gmkhost", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})
	r.Add(&RunnerEntry{ID: "B", Hostname: "raspi", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	h := protocol.Hostname{}
	h.SetName([]byte("gmkhost"))
	sel.SetHostname(h)

	cs := r.Candidates("/x/repo", sel)
	if len(cs) != 1 || cs[0].ID != "A" {
		t.Fatalf("expected only A (gmkhost), got %v", cs)
	}
}
