package authority

import (
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// CoversNewChild reports whether a task the caller is about to create — its
// direct child — would fall inside the caller's target set for c. Decided from
// the scope alone because the child does not exist yet: a subtree base (with or
// without self) and a global one reach every child, while a none base reaches
// only listed ids, and an id list cannot name a task that has no id yet.
// TestScopeCoversNewChildAgreesWithActionSet holds it to ActionSet.
func CoversNewChild(s Scope, c protocol.Capability) bool {
	base, _, _ := s.ForCap(c)
	return base == protocol.ScopeBase_Subtree || base == protocol.ScopeBase_Global
}

// DescendantsOf marks self and every task reachable through the creator index
// into allowed.
//
// visited is deliberately separate from allowed. allowed arrives pre-seeded
// with self and the scope's explicit ids, so reusing it as the visited set
// would make the walk skip its own root on the first pop and return without
// ever expanding a single child.
func DescendantsOf(children map[string][]string, selfHex string, allowed map[string]bool) {
	visited := make(map[string]bool, len(children)+1)
	queue := []string{selfHex}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		allowed[cur] = true
		queue = append(queue, children[cur]...)
	}
}

// ActionSet is the target set scope gives the bit want, keyed by the task
// (server's scopeSetWith resolves a connection to its principal and calls it).
// The board's reach source calls it for board_peer (reach spec D16), so the
// board and the control plane cannot disagree about what a scope covers.
func ActionSet(callerHex string, scope Scope, want protocol.Capability, children map[string][]string) (all bool, allowed map[string]bool) {
	base, excludeSelf, ids := scope.ForCap(want)
	if base == protocol.ScopeBase_Global {
		return true, nil
	}
	allowed = map[string]bool{}
	if !excludeSelf {
		allowed[callerHex] = true
	}
	for _, id := range ids {
		allowed[id] = true
	}
	if base == protocol.ScopeBase_Subtree {
		// DescendantsOf seeds from callerHex and marks it, so exclude_self has
		// to be applied AFTER the walk. Deleting first would make the walk skip
		// its own root and return without expanding a single child — the same
		// trap the visited/allowed split in DescendantsOf documents.
		DescendantsOf(children, callerHex, allowed)
		if excludeSelf {
			delete(allowed, callerHex)
		}
	}
	return false, allowed
}

// VisibleSet is what a task with this scope may SEE:
//
//	{self} ∪ baseSet(VisRank) ∪ VisIDs ∪ IDs ∪ every override's IDs
//
// Self is unconditional: ExcludeSelf removes self from an ACTION set only.
// Action ids join without being repeated in VisIDs, because an id written into
// a grant was disclosed by the granter. server's visibilitySet calls it for a
// connection's principal.
func VisibleSet(callerHex string, scope Scope, children map[string][]string) (all bool, allowed map[string]bool) {
	visRank := scope.VisRank()
	if visRank == protocol.ScopeBase_Global {
		return true, nil
	}
	allowed = map[string]bool{callerHex: true}
	for _, id := range scope.VisIDs {
		allowed[id] = true
	}
	for _, id := range scope.IDs {
		allowed[id] = true
	}
	for _, o := range scope.Overrides {
		for _, id := range o.IDs {
			allowed[id] = true
		}
	}
	if visRank == protocol.ScopeBase_Subtree {
		DescendantsOf(children, callerHex, allowed)
	}
	return false, allowed
}
