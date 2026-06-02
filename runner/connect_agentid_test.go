package runner

import "testing"

func TestAgentBinBase(t *testing.T) {
	cases := map[string]string{
		"claude":          "claude",
		"/usr/bin/gemini": "gemini",
		"./codex":         "codex",
		"bash":            "bash",
		"":                "", // empty stays empty (not ".")
	}
	for in, want := range cases {
		if got := agentBinBase(in); got != want {
			t.Errorf("agentBinBase(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSkillsInjected(t *testing.T) {
	// injected unless no-worktree without force-inject
	if !skillsInjected(false, false) {
		t.Error("worktree mode should inject")
	}
	if skillsInjected(true, false) {
		t.Error("no-worktree without force should NOT inject")
	}
	if !skillsInjected(true, true) {
		t.Error("no-worktree with force should inject")
	}
}
