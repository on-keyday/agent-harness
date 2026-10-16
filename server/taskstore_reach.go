package server

import (
	"encoding/hex"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// storeReach is the agentboard's view of the task store: who created whom,
// and what each task holds. Each call takes the store's read lock once. See
// docs/superpowers/specs/2026-10-16-agentboard-reach-design.md.
type storeReach struct{ s *TaskStore }

func (r storeReach) entry(tid protocol.TaskID) (*TaskEntry, bool) {
	e, ok := r.s.tasks[hex.EncodeToString(tid.Id[:])]
	return e, ok
}

func (r storeReach) CreatorOf(tid protocol.TaskID) (protocol.TaskID, bool) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	if e, ok := r.entry(tid); ok {
		return e.CreatorTaskID, true
	}
	return protocol.TaskID{}, false
}

func (r storeReach) CapsOf(tid protocol.TaskID) (protocol.Capability, bool) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	if e, ok := r.entry(tid); ok {
		return e.Capabilities, true
	}
	return protocol.Capability_None, false
}

// HasChild scans the store: about 27 ns a task, measured in
// server/reach_bench_test.go and inside the budget the reach spec §6 set.
func (r storeReach) HasChild(parent protocol.TaskID, short string) bool {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	for _, e := range r.s.tasks {
		if e.CreatorTaskID == parent && strings.HasPrefix(e.ID, short) {
			return true
		}
	}
	return false
}

// PeerCovers asks actionSet — the control plane's own scope evaluation — for
// tid's board_peer target set, and matches short against it. The child index
// is built only for a subtree base, the one form that walks it.
func (r storeReach) PeerCovers(tid protocol.TaskID, short string) bool {
	e, ok := r.s.Get(hex.EncodeToString(tid.Id[:]))
	if !ok {
		return false
	}
	var children map[string][]string
	if base, _, _ := e.Scope.ForCap(protocol.Capability_BoardPeer); base == protocol.ScopeBase_Subtree {
		children = childIndexOf(r.s)
	}
	all, allowed := actionSet(e.ID, e.Scope, protocol.Capability_BoardPeer, children)
	if all {
		return true
	}
	for id := range allowed {
		if strings.HasPrefix(id, short) {
			return true
		}
	}
	return false
}

// PeerGlobal reports a global board_peer scope, the form that opens free-form
// topics (reach spec D16).
func (r storeReach) PeerGlobal(tid protocol.TaskID) bool {
	e, ok := r.s.Get(hex.EncodeToString(tid.Id[:]))
	if !ok {
		return false
	}
	base, _, _ := e.Scope.ForCap(protocol.Capability_BoardPeer)
	return base == protocol.ScopeBase_Global
}

func (r storeReach) Len() int {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	return len(r.s.tasks)
}
