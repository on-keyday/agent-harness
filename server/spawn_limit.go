package server

import (
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Spawn limits (docs/superpowers/specs/2026-11-09-spawn-limits-design.md):
// a task's spawn_depth is how many more generations may be created below it,
// and its max_descendants how many records its subtree may hold, itself
// excluded. Both are checked when a task is created, against the creator and
// every ancestor present in the store, under the store's lock.

// SpawnLimitError is a creation a spawn limit refused. Its text carries no
// "spawn limit" prefix: it travels as error_msg beside the spawn_limit
// status, and every client prints the status first. Depth: the creator's
// spawn_depth is 0. Otherwise the ancestor Distance levels above the creator
// (0 = the creator itself) holds Count records against a limit of Max.
type SpawnLimitError struct {
	Depth      bool
	Distance   int
	Count, Max uint32
}

func (e *SpawnLimitError) Error() string {
	if e.Depth {
		return "your spawn depth is 0, so this task may create no tasks"
	}
	var whose string
	switch e.Distance {
	case 0:
		whose = "your own"
	case 1:
		whose = "your parent's"
	default:
		whose = fmt.Sprintf("an ancestor's (%d levels up)", e.Distance)
	}
	return fmt.Sprintf("%s descendant limit is reached (%d/%d); resume an existing task, or ask the operator to prune or raise the limit",
		whose, e.Count, e.Max)
}

// CreateWithinLimits is Create behind the spawn-limit check, both under one
// hold of s.mu so two creations cannot take the same last place. It is the
// only creation production code uses (TestOnlyCreateWithinLimitsInProduction);
// Create stays unchecked for tests that build trees directly.
func (s *TaskStore) CreateWithinLimits(repo, prompt string, kind protocol.TaskKind, origin protocol.ClientKind, creatorTaskID protocol.TaskID, boundRunnerID string, selector protocol.RunnerSelector, extraArgs []string, g authority.Grant, agentProfile string, run RunSpec) (string, error) {
	s.mu.Lock()
	if err := s.checkSpawnLimitsLocked(creatorTaskID); err != nil {
		s.mu.Unlock()
		return "", err
	}
	// The child's depth against the creator's as it stands now: the caller
	// computed g from a read taken before this lock, and a caps set in
	// between must not leave the child deeper than creator - 1.
	if creatorTaskID.Id != ([16]byte{}) {
		if c, ok := s.tasks[hex.EncodeToString(creatorTaskID.Id[:])]; ok {
			g.SpawnDepth = authority.MinLimit(g.SpawnDepth, authority.DecLimit(c.SpawnDepth))
		}
	}
	id := s.createLocked(repo, prompt, kind, origin, creatorTaskID, boundRunnerID, selector, extraArgs, g, agentProfile, run)
	onCreate := s.OnCreate
	s.mu.Unlock()
	if onCreate != nil {
		onCreate(id)
	}
	return id, nil
}

// CheckSpawnLimits is the check alone, for refusing early (before runner
// resolution). It is advisory: only CreateWithinLimits' own check, under the
// lock that inserts, decides.
func (s *TaskStore) CheckSpawnLimits(creator protocol.TaskID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkSpawnLimitsLocked(creator)
}

// checkSpawnLimitsLocked refuses a creation by creator. An all-zero creator
// is the operator: no ancestors, unbounded depth.
func (s *TaskStore) checkSpawnLimitsLocked(creator protocol.TaskID) error {
	if creator.Id == ([16]byte{}) {
		return nil
	}
	chain := s.ancestorsLocked(hex.EncodeToString(creator.Id[:]))
	if len(chain) == 0 {
		return nil
	}
	if d := chain[0].SpawnDepth; d.Limited() && d.Value() == 0 {
		return &SpawnLimitError{Depth: true}
	}
	counts := s.descendantCountsLocked()
	for dist, a := range chain {
		if m := a.MaxDescendants; m.Limited() && counts[a.ID]+1 > m.Value() {
			return &SpawnLimitError{Distance: dist, Count: counts[a.ID], Max: m.Value()}
		}
	}
	return nil
}

// ancestorsLocked is id and its ancestors, nearest first, following the
// current parent links through tasks present in the store. A pruned parent
// ends the walk (spec D10).
func (s *TaskStore) ancestorsLocked(id string) []*TaskEntry {
	var out []*TaskEntry
	seen := map[string]bool{}
	for cur, ok := s.tasks[id]; ok && !seen[cur.ID]; {
		seen[cur.ID] = true
		out = append(out, cur)
		if cur.CreatorTaskID.Id == ([16]byte{}) {
			break
		}
		cur, ok = s.tasks[hex.EncodeToString(cur.CreatorTaskID.Id[:])]
	}
	return out
}

// descendantCountsLocked is, per task, the records in its subtree, itself
// excluded, under the current parent links.
func (s *TaskStore) descendantCountsLocked() map[string]uint32 {
	children := make(map[string][]string, len(s.tasks))
	for id, t := range s.tasks {
		if t.CreatorTaskID.Id != ([16]byte{}) {
			p := hex.EncodeToString(t.CreatorTaskID.Id[:])
			children[p] = append(children[p], id)
		}
	}
	counts := make(map[string]uint32, len(s.tasks))
	visiting := map[string]bool{}
	var count func(id string) uint32
	count = func(id string) uint32 {
		if n, done := counts[id]; done {
			return n
		}
		if visiting[id] { // a parent cycle; set_parent refuses them, count nothing twice
			return 0
		}
		visiting[id] = true
		var n uint32
		for _, c := range children[id] {
			n += 1 + count(c)
		}
		counts[id] = n
		return n
	}
	for id := range s.tasks {
		count(id)
	}
	return counts
}

// DescendantCounts is descendantCountsLocked for callers outside the lock.
func (s *TaskStore) DescendantCounts() map[string]uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.descendantCountsLocked()
}

// SpawnStatus is id's descendant count and its room -- how many more tasks
// it may create now: the minimum, over id and its ancestors with a limited
// max_descendants, of max - descendants, floored at 0; unbounded when none is
// limited. One hold of the lock, so whoami's two numbers cannot disagree.
func (s *TaskStore) SpawnStatus(id string) (descendants uint32, room protocol.SpawnLimit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := s.descendantCountsLocked()
	descendants = counts[id]
	room = authority.Unbounded()
	for _, a := range s.ancestorsLocked(id) {
		if m := a.MaxDescendants; m.Limited() {
			left := uint32(0)
			if counts[a.ID] < m.Value() {
				left = m.Value() - counts[a.ID]
			}
			room = authority.MinLimit(room, authority.LimitOf(left))
		}
	}
	return descendants, room
}
