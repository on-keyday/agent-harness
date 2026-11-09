package verb

import (
	"fmt"
	"strconv"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The three converters --caps, --scope and --scope-for name from the
// declaration. They exist because those flags carry a GRAMMAR rather than a
// string, and the grammar lives here (ParseCaps / ParseScope / ParseScopeFor)
// rather than in the generator, which knows nothing about capabilities.
//
// Each returns a POINTER or a slice, so "the operator said nothing" stays
// distinct from "the operator said none" -- the zero Capability is no
// authority at all and the zero TaskScope is the subtree, both meaningful,
// which is why a resume must not re-grant on a flag nobody passed.

// parseCapsFlag is --caps.
func parseCapsFlag(s string) (*protocol.Capability, error) {
	c, err := ParseCaps(s)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// spawnLimitMax is the largest value a SpawnLimit carries (its value is u24).
const spawnLimitMax = 1<<24 - 1

// ParseSpawnLimit is the grammar of --spawn-depth and --max-descendants:
// "unbounded", or a decimal 0..16777215. 0 is a real limit ("none at all"),
// which is why unbounded is a word rather than a number.
func ParseSpawnLimit(s string) (protocol.SpawnLimit, error) {
	var l protocol.SpawnLimit
	if s == "unbounded" {
		return l, nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n > spawnLimitMax {
		return l, fmt.Errorf("%q: want a number 0..%d or unbounded", s, spawnLimitMax)
	}
	l.SetLimited(true)
	l.SetValue(uint32(n))
	return l, nil
}

// parseSpawnLimitFlag is --spawn-depth / --max-descendants.
func parseSpawnLimitFlag(s string) (*protocol.SpawnLimit, error) {
	l, err := ParseSpawnLimit(s)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// parseScopeFlag is --scope.
func parseScopeFlag(s string) (*protocol.TaskScope, error) {
	sc, err := ParseScope(s)
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// parseScopeForList is --scope-for, repeatable. Merged on every occurrence so
// an overlapping capability list is refused at the flag rather than one round
// trip later.
func parseScopeForList(specs []string) ([]protocol.ScopeOverride, error) {
	var out []protocol.ScopeOverride
	for _, one := range specs {
		_, ov, err := ParseScopeFor(one)
		if err != nil {
			return nil, err
		}
		merged, err := MergeScopeOverride(out, ov)
		if err != nil {
			return nil, err
		}
		out = merged
	}
	return out, nil
}
