package verb

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func granularCaps() []protocol.Capability {
	var out []protocol.Capability
	for _, c := range GrantableCaps() {
		if c != protocol.Capability_None && c != protocol.Capability_All {
			out = append(out, c)
		}
	}
	return out
}

// A bit added to GrantableCaps must be placed explicitly: read the table, not
// the accessor, so a zero-value default cannot satisfy this.
func TestEveryGrantableCapHasATier(t *testing.T) {
	for _, c := range granularCaps() {
		if _, ok := capAuditTable[c]; !ok {
			t.Errorf("%s has no entry in capAuditTable: give it a tier, a scope kind and its implications", c)
		}
	}
}

func TestAuditOrderIsStrongFirstAndComplete(t *testing.T) {
	order := AuditOrder()
	if len(order) != len(granularCaps()) || len(order) != 25 {
		t.Fatalf("AuditOrder has %d bits, want 25 (every granular bit)", len(order))
	}
	if order[0] != protocol.Capability_Spawn {
		t.Errorf("AuditOrder()[0] = %s, want spawn", order[0])
	}
	for i := 1; i < len(order); i++ {
		if CapTierOf(order[i]) < CapTierOf(order[i-1]) {
			t.Errorf("%s (%s) follows %s (%s): tiers must not go back up", order[i], CapTierOf(order[i]).Label(), order[i-1], CapTierOf(order[i-1]).Label())
		}
	}
}

func TestTierTableMatchesTheSpec(t *testing.T) {
	want := map[CapTier][]protocol.Capability{
		TierStrong: {protocol.Capability_Spawn, protocol.Capability_Resume, protocol.Capability_ExecControl, protocol.Capability_BoardPurge, protocol.Capability_Prune, protocol.Capability_BoardSend, protocol.Capability_RunnerAdmin, protocol.Capability_RunnerEnroll, protocol.Capability_ExecTap, protocol.Capability_ForwardTap, protocol.Capability_ForwardLocal, protocol.Capability_ForwardRemote, protocol.Capability_ForwardLink},
		TierMid:    {protocol.Capability_ExecCowrite, protocol.Capability_ExecRun, protocol.Capability_FileWrite, protocol.Capability_Cancel, protocol.Capability_BoardPeer, protocol.Capability_BoardObserve, protocol.Capability_ResumeExact, protocol.Capability_BoardPersist},
		TierWeak:   {protocol.Capability_ExecView, protocol.Capability_ExecResize, protocol.Capability_FileRead, protocol.Capability_Notify},
	}
	for tier, caps := range want {
		for _, c := range caps {
			if got := CapTierOf(c); got != tier {
				t.Errorf("%s is %s, want %s", c, got.Label(), tier.Label())
			}
		}
	}
	none := []protocol.Capability{protocol.Capability_Spawn, protocol.Capability_Notify, protocol.Capability_RunnerAdmin, protocol.Capability_RunnerEnroll, protocol.Capability_BoardObserve, protocol.Capability_BoardSend, protocol.Capability_BoardPersist}
	for _, c := range granularCaps() {
		isNone := false
		for _, n := range none {
			isNone = isNone || n == c
		}
		if (CapScopeKindOf(c) == ScopeNone) != isNone {
			t.Errorf("%s scope kind = %v, want ScopeNone=%v", c, CapScopeKindOf(c), isNone)
		}
	}
	if got := CapImpliedBy(protocol.Capability_ExecView); len(got) != 2 {
		t.Errorf("exec_view implied by %v, want exec_cowrite and exec_control", got)
	}
	if got := CapImpliedBy(protocol.Capability_ResumeExact); len(got) != 1 || got[0] != protocol.Capability_Resume {
		t.Errorf("resume_exact implied by %v, want resume", got)
	}
}

// surface-parity item 33: the TUI answers with its modal, so a --json it
// cannot honour is refused there rather than dropped.
func TestCapsAuditJSONIsNotATUIFlag(t *testing.T) {
	if _, err := ParseCmdCapsAudit(TUI, []string{"--json"}, nil); err == nil {
		t.Fatal("caps audit --json parsed on the TUI, which has no JSON form to give")
	}
	if _, err := ParseCmdCapsAudit(WebUI, []string{"--json"}, nil); err != nil {
		t.Fatalf("caps audit --json on the WebUI: %v", err)
	}
}
