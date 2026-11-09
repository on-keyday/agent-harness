package authority

import (
	"strconv"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A task's two spawn limits (docs/superpowers/specs/2026-11-09-spawn-limits-design.md)
// are protocol.SpawnLimit values: limited = 0 is unbounded, which is the zero
// value. These helpers are the only arithmetic on them.

// Unbounded is the absent limit.
func Unbounded() protocol.SpawnLimit { return protocol.SpawnLimit{} }

// LimitOf is the limit n.
func LimitOf(n uint32) protocol.SpawnLimit {
	var l protocol.SpawnLimit
	l.SetLimited(true)
	l.SetValue(n)
	return l
}

// DecLimit is l one generation down: unbounded stays unbounded, 0 stays 0.
func DecLimit(l protocol.SpawnLimit) protocol.SpawnLimit {
	if !l.Limited() || l.Value() == 0 {
		return l
	}
	return LimitOf(l.Value() - 1)
}

// MinLimit is the tighter of a and b; unbounded is the identity.
func MinLimit(a, b protocol.SpawnLimit) protocol.SpawnLimit {
	switch {
	case !a.Limited():
		return b
	case !b.Limited():
		return a
	case b.Value() < a.Value():
		return b
	default:
		return a
	}
}

// LimitLabel renders l as the flags take it: "unbounded" or the decimal.
func LimitLabel(l protocol.SpawnLimit) string {
	if !l.Limited() {
		return "unbounded"
	}
	return strconv.FormatUint(uint64(l.Value()), 10)
}

// ChildSpawnDepth is the spawn_depth a task granted by g gets:
// min(requested, g's own minus one), so the limit only shrinks down a chain.
// A nil Granter is an operator, the root, whose depth is unbounded.
func (g *Granter) ChildSpawnDepth(requested protocol.SpawnLimit) protocol.SpawnLimit {
	if g == nil {
		return requested
	}
	return MinLimit(requested, DecLimit(g.held.SpawnDepth))
}
