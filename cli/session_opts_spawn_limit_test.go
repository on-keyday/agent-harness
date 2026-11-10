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
	sub := buildSubmitRequest("/r", "p", opts)
	if sub.SpawnDepth.Value() != 2 || !sub.SpawnDepth.Limited() || sub.MaxDescendants.Value() != 7 ||
		!sub.SpawnDepthPresent() || !sub.MaxDescendantsPresent() {
		t.Errorf("buildSubmitRequest lost the limits: %+v", sub)
	}
	oi := buildOpenInteractiveRequest("/r", opts)
	if oi.SpawnDepth.Value() != 2 || !oi.SpawnDepth.Limited() || oi.MaxDescendants.Value() != 7 ||
		!oi.SpawnDepthPresent() || !oi.MaxDescendantsPresent() {
		t.Errorf("buildOpenInteractiveRequest lost the limits: %+v", oi)
	}
}

// A spawn refused by a limit reports the server's text: which limit, and the
// numbers.
func TestSubmitSpawnLimitCarriesServerText(t *testing.T) {
	msg := "your own descendant limit is reached (1/1); resume an existing task, or ask the operator to prune or raise the limit"
	s := &protocol.SubmitResponse{Status: protocol.SubmitStatus_SpawnLimit}
	s.SetErrorMsg([]byte(msg))
	err := submitStatusError(t.Context(), nil, s)
	if err == nil || err.Error() != "submit spawn_limit: "+msg {
		t.Errorf("err = %v, want \"submit spawn_limit: \" + the server text, once", err)
	}
}
