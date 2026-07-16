package server

import "testing"

// toRunnerInfo must echo the advertised agent-profile set to operator surfaces,
// not just the default AgentBin. Regression: the server stored AgentProfiles on
// RunnerEntry for dispatch but omitted SetAgentProfiles in toRunnerInfo, so the
// TUI/WebUI agent pickers only ever showed the default profile (codex etc. were
// invisible). Caught by driving the live TUI.
func TestToRunnerInfoEchoesAgentProfiles(t *testing.T) {
	e := RunnerEntry{
		Hostname:      "gmkhost",
		AgentBin:      "claude",
		AgentProfiles: []string{"claude", "codex"},
		Conn:          &fakeConn{id: buildTestCID("ws:127.0.0.1:8539-1")},
	}
	info := toRunnerInfo(e)
	if len(info.AgentProfiles) != 2 {
		t.Fatalf("AgentProfiles len = %d, want 2", len(info.AgentProfiles))
	}
	if string(info.AgentProfiles[0].Name) != "claude" || string(info.AgentProfiles[1].Name) != "codex" {
		t.Fatalf("AgentProfiles = [%q %q], want [claude codex]",
			info.AgentProfiles[0].Name, info.AgentProfiles[1].Name)
	}
}
