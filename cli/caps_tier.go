package cli

import (
	"fmt"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// UnboundedScopeLabel is the scope cell of a bit no task scope bounds (spawn,
// notify, runner_admin, board_observe, board_send): its reach is the server's
// shared resources whatever the task's scope says, so it reads as global, and
// （固定） says no --scope / --scope-for narrows it.
const UnboundedScopeLabel = "global（固定）"

// TierMark is the tree's per-task capability mark: the strongest tier the mask
// holds and how many of its bits are at that tier, e.g. "強3". ok is false for
// a mask with no granular bit, which gets no mark at all.
func TierMark(caps protocol.Capability) (mark string, tier verb.CapTier, ok bool) {
	n := 0
	for _, c := range verb.AuditOrder() {
		if caps&c != c {
			continue
		}
		if !ok {
			tier, ok = verb.CapTierOf(c), true
		}
		if verb.CapTierOf(c) == tier {
			n++
		}
	}
	if !ok {
		return "", 0, false
	}
	return fmt.Sprintf("%s%d", tier.Label(), n), tier, true
}
