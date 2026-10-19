package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestTierMark(t *testing.T) {
	for _, tc := range []struct {
		caps protocol.Capability
		mark string
		tier verb.CapTier
		ok   bool
	}{
		{protocol.Capability_Spawn | protocol.Capability_Purge | protocol.Capability_ExecView, "強2", verb.TierStrong, true},
		{protocol.Capability_ExecView, "弱1", verb.TierWeak, true},
		{protocol.Capability_ExecCowrite | protocol.Capability_FileRead, "中1", verb.TierMid, true},
	} {
		mark, tier, ok := TierMark(tc.caps)
		if mark != tc.mark || tier != tc.tier || ok != tc.ok {
			t.Errorf("TierMark(%v) = %q %v %v, want %q %v %v", tc.caps, mark, tier, ok, tc.mark, tc.tier, tc.ok)
		}
	}
	// Review Focus 1: no granular bit, no mark.
	if mark, _, ok := TierMark(protocol.Capability_None); ok || mark != "" {
		t.Errorf("TierMark(none) = %q %v, want no mark", mark, ok)
	}
}
