package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func limitOf(n uint32) protocol.SpawnLimit {
	var l protocol.SpawnLimit
	l.SetLimited(true)
	l.SetValue(n)
	return l
}

// Both wire builders carry the spawn limits and their presence bits.
func TestSessionOptsCarryLimits(t *testing.T) {
	opts := SessionOpts{
		SpawnDepth: limitOf(2), MaxDescendants: limitOf(7),
		SpawnDepthPresent: true, MaxDescendantsPresent: true,
	}
	oi := buildOpenInteractiveRequest("/r", opts)
	if oi.SpawnDepth.Value() != 2 || !oi.SpawnDepth.Limited() || oi.MaxDescendants.Value() != 7 ||
		!oi.SpawnDepthPresent() || !oi.MaxDescendantsPresent() {
		t.Errorf("buildOpenInteractiveRequest lost the limits: %+v", oi)
	}
}
