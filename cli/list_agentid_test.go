package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestAgentStr(t *testing.T) {
	cases := []struct {
		bin      string
		injected bool
		want     string
	}{
		{"claude", true, "agent=claude+skills"},
		{"gemini", false, "agent=gemini"},
		{"bash", false, "agent=bash"},
		{"", false, "agent=?"},
		{"", true, "agent=?+skills"},
	}
	for _, c := range cases {
		if got := agentStr(c.bin, c.injected); got != c.want {
			t.Errorf("agentStr(%q,%v)=%q want %q", c.bin, c.injected, got, c.want)
		}
	}
}

func profileNames(names ...string) []protocol.AgentProfileName {
	ps := make([]protocol.AgentProfileName, len(names))
	for i, n := range names {
		ps[i].SetName([]uint8(n))
	}
	return ps
}

func TestAgentProfilesStr(t *testing.T) {
	cases := []struct {
		profiles []protocol.AgentProfileName
		bin      string
		injected bool
		want     string
	}{
		// Multi-profile runner: full set, in advertised order.
		{profileNames("claude", "codex"), "claude", true, "agent=claude,codex+skills"},
		{profileNames("claude", "codex"), "claude", false, "agent=claude,codex"},
		// Single advertised profile still renders from the profile list.
		{profileNames("codex"), "codex", false, "agent=codex"},
		// Legacy runner (no advertised profiles) falls back to AgentBin.
		{nil, "claude", true, "agent=claude+skills"},
		{nil, "bash", false, "agent=bash"},
	}
	for _, c := range cases {
		if got := agentProfilesStr(c.profiles, c.bin, c.injected); got != c.want {
			t.Errorf("agentProfilesStr(%v,%q,%v)=%q want %q", c.profiles, c.bin, c.injected, got, c.want)
		}
	}
}
