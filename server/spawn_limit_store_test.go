package server

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func sameSpawnLimit(a, b protocol.SpawnLimit) bool {
	return a.Limited() == b.Limited() && a.Value() == b.Value()
}

// limitedChild creates a task under creator ("" = operator) through the
// checked entry point, with the given limits.
func limitedChild(s *TaskStore, creator string, depth, max protocol.SpawnLimit) (string, error) {
	var c protocol.TaskID
	if creator != "" {
		c = taskIDFromHex(creator)
	}
	return s.CreateWithinLimits("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, c, "",
		protocol.RunnerSelector{}, nil, authority.Grant{SpawnDepth: depth, MaxDescendants: max}, "")
}

func mustChild(t *testing.T, s *TaskStore, creator string, depth, max protocol.SpawnLimit) string {
	t.Helper()
	id, err := limitedChild(s, creator, depth, max)
	if err != nil {
		t.Fatalf("create under %.8s: %v", creator, err)
	}
	return id
}

func wantLimitErr(t *testing.T, err error, want SpawnLimitError) {
	t.Helper()
	var got *SpawnLimitError
	if !errors.As(err, &got) {
		t.Fatalf("err = %v, want a *SpawnLimitError", err)
	}
	if *got != want {
		t.Errorf("err = %+v, want %+v", *got, want)
	}
}

func TestCreateWithinLimitsDepth(t *testing.T) {
	s := NewTaskStore()
	zero := mustChild(t, s, "", authority.LimitOf(0), authority.Unbounded())
	before := len(s.List(0))
	_, err := limitedChild(s, zero, authority.Unbounded(), authority.Unbounded())
	wantLimitErr(t, err, SpawnLimitError{Depth: true})
	if len(s.List(0)) != before {
		t.Error("a refused creation added a row")
	}
	if err.Error() != "spawn limit: your spawn depth is 0, so this task may create no tasks" {
		t.Errorf("text = %q", err.Error())
	}
	one := mustChild(t, s, "", authority.LimitOf(1), authority.Unbounded())
	mustChild(t, s, one, authority.Unbounded(), authority.Unbounded())
	free := mustChild(t, s, "", authority.Unbounded(), authority.Unbounded())
	mustChild(t, s, free, authority.Unbounded(), authority.Unbounded())
}

func TestCreateWithinLimitsDescendants(t *testing.T) {
	s := NewTaskStore()
	a := mustChild(t, s, "", authority.Unbounded(), authority.LimitOf(2))
	c1 := mustChild(t, s, a, authority.Unbounded(), authority.Unbounded())
	mustChild(t, s, a, authority.Unbounded(), authority.Unbounded())

	_, err := limitedChild(s, a, authority.Unbounded(), authority.Unbounded())
	wantLimitErr(t, err, SpawnLimitError{Distance: 0, Count: 2, Max: 2})
	if err.Error() != "spawn limit: your own descendant limit is reached (2/2); resume an existing task, or ask the operator to prune or raise the limit" {
		t.Errorf("text = %q", err.Error())
	}

	// Ending a task frees nothing: the record still counts (D2).
	s.Finish(c1, 0, nil)
	_, err = limitedChild(s, a, authority.Unbounded(), authority.Unbounded())
	wantLimitErr(t, err, SpawnLimitError{Distance: 0, Count: 2, Max: 2})

	// A child creating counts against its parent's limit.
	_, err = limitedChild(s, c1, authority.Unbounded(), authority.Unbounded())
	wantLimitErr(t, err, SpawnLimitError{Distance: 1, Count: 2, Max: 2})
	if err.Error() != "spawn limit: your parent's descendant limit is reached (2/2); resume an existing task, or ask the operator to prune or raise the limit" {
		t.Errorf("text = %q", err.Error())
	}

	// Pruning the ended record frees its place.
	if n, _, _ := s.PruneByIDs(nil, []string{c1}, ""); n != 1 {
		t.Fatal("prune did not remove the ended child")
	}
	mustChild(t, s, a, authority.Unbounded(), authority.Unbounded())
}

func TestCreateWithinLimitsDistanceText(t *testing.T) {
	s := NewTaskStore()
	a := mustChild(t, s, "", authority.Unbounded(), authority.LimitOf(3))
	b := mustChild(t, s, a, authority.Unbounded(), authority.Unbounded())
	c := mustChild(t, s, b, authority.Unbounded(), authority.Unbounded())
	d := mustChild(t, s, c, authority.Unbounded(), authority.Unbounded())
	_, err := limitedChild(s, d, authority.Unbounded(), authority.Unbounded())
	wantLimitErr(t, err, SpawnLimitError{Distance: 3, Count: 3, Max: 3})
	if err.Error() != "spawn limit: an ancestor's (3 levels up) descendant limit is reached (3/3); resume an existing task, or ask the operator to prune or raise the limit" {
		t.Errorf("text = %q", err.Error())
	}
}

// A child's own larger limit does not widen its parent's (D6).
func TestCreateWithinLimitsChildLimitDoesNotWiden(t *testing.T) {
	s := NewTaskStore()
	a := mustChild(t, s, "", authority.Unbounded(), authority.LimitOf(2))
	c := mustChild(t, s, a, authority.Unbounded(), authority.LimitOf(10))
	mustChild(t, s, c, authority.Unbounded(), authority.Unbounded())
	_, err := limitedChild(s, c, authority.Unbounded(), authority.Unbounded())
	wantLimitErr(t, err, SpawnLimitError{Distance: 1, Count: 2, Max: 2})
}

// An operator-created task has no ancestors; a full subtree elsewhere is
// irrelevant to it (D7).
func TestCreateWithinLimitsOperatorRoot(t *testing.T) {
	s := NewTaskStore()
	a := mustChild(t, s, "", authority.Unbounded(), authority.LimitOf(0))
	if _, err := limitedChild(s, a, authority.Unbounded(), authority.Unbounded()); err == nil {
		t.Fatal("A with max 0 created a child")
	}
	mustChild(t, s, "", authority.Unbounded(), authority.Unbounded())
}

// Reparenting into a full subtree is allowed and leaves it over its limit;
// the next creation under it is refused (D4).
func TestCreateWithinLimitsReparentOverLimit(t *testing.T) {
	s := NewTaskStore()
	a := mustChild(t, s, "", authority.Unbounded(), authority.LimitOf(1))
	mustChild(t, s, a, authority.Unbounded(), authority.Unbounded())
	stray := mustChild(t, s, "", authority.Unbounded(), authority.Unbounded())
	if _, ok := s.SetParent(stray, taskIDFromHex(a)); !ok {
		t.Fatal("set parent failed")
	}
	_, err := limitedChild(s, a, authority.Unbounded(), authority.Unbounded())
	wantLimitErr(t, err, SpawnLimitError{Distance: 0, Count: 2, Max: 1})
}

// Two creations racing for the last place: exactly one wins (D11).
func TestCreateWithinLimitsRace(t *testing.T) {
	s := NewTaskStore()
	a := mustChild(t, s, "", authority.Unbounded(), authority.LimitOf(1))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := limitedChild(s, a, authority.Unbounded(), authority.Unbounded()); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Errorf("%d creations took a single place", ok)
	}
}

// Limits written at spawn, kept through a caps-only caps set, and rewritten by
// a re-granting resume all survive a restart.
func TestLimitsSurviveReplay(t *testing.T) {
	s, path := provenanceStore(t)
	a := mustChild(t, s, "", authority.LimitOf(3), authority.LimitOf(5))
	s.SetCaps(a, GrantUpdate{Caps: true, To: authority.Grant{Caps: protocol.Capability_Cancel}}, AuthorityWrite{})

	b := mustChild(t, s, "", authority.LimitOf(3), authority.Unbounded())
	s.Finish(b, 0, nil)
	if _, err := s.Resume(b, "p2", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
		GrantUpdate{SpawnDepth: true, MaxDescendants: true, To: authority.Grant{SpawnDepth: authority.LimitOf(1), MaxDescendants: authority.LimitOf(9)}},
		protocol.TaskKind_Oneshot, "", protocol.TaskID{}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	fresh := NewTaskStore()
	fresh.ReplayEvents(events)
	for _, c := range []struct {
		id         string
		depth, max protocol.SpawnLimit
	}{{a, authority.LimitOf(3), authority.LimitOf(5)}, {b, authority.LimitOf(1), authority.LimitOf(9)}} {
		got, ok := fresh.Get(c.id)
		if !ok {
			t.Fatalf("%.8s lost on replay", c.id)
		}
		if !sameSpawnLimit(got.SpawnDepth, c.depth) || !sameSpawnLimit(got.MaxDescendants, c.max) {
			t.Errorf("%.8s replayed depth %s max %s, want %s %s", c.id,
				authority.LimitLabel(got.SpawnDepth), authority.LimitLabel(got.MaxDescendants),
				authority.LimitLabel(c.depth), authority.LimitLabel(c.max))
		}
	}
}

func TestLegacyRecordReplaysUnbounded(t *testing.T) {
	var legacy WALEvent
	if err := json.Unmarshal([]byte(`{"type":"task_created","task_id":"`+boundX+`","repo_path":"/r","ts":1}`), &legacy); err != nil {
		t.Fatal(err)
	}
	fresh := NewTaskStore()
	fresh.ReplayEvents([]WALEvent{legacy})
	got, ok := fresh.Get(boundX)
	if !ok {
		t.Fatal("legacy record lost")
	}
	if got.SpawnDepth.Limited() || got.MaxDescendants.Limited() {
		t.Error("a record from before the limits replayed limited")
	}
}

func TestDescendantCountsAndSpawnRoom(t *testing.T) {
	s := NewTaskStore()
	a := mustChild(t, s, "", authority.Unbounded(), authority.LimitOf(3))
	c := mustChild(t, s, a, authority.Unbounded(), authority.LimitOf(10))
	mustChild(t, s, c, authority.Unbounded(), authority.Unbounded())
	counts := s.DescendantCounts()
	if counts[a] != 2 || counts[c] != 1 {
		t.Errorf("counts a=%d c=%d, want 2 1", counts[a], counts[c])
	}
	if got := s.SpawnRoom(c); !sameSpawnLimit(got, authority.LimitOf(1)) {
		t.Errorf("room under a full-ish A = %s, want 1", authority.LimitLabel(got))
	}
	free := mustChild(t, s, "", authority.Unbounded(), authority.Unbounded())
	if got := s.SpawnRoom(free); got.Limited() {
		t.Errorf("room with no limit anywhere = %s, want unbounded", authority.LimitLabel(got))
	}
}
