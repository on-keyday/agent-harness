package main

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

func TestFormatAmbiguousCandidates(t *testing.T) {
	out := formatAmbiguousCandidates([]cli.RunnerCandidate{
		{Cid: "ws:10.0.0.1:1-1", Hostname: "gmkhost", MatchedRoot: "/repo", ActiveTasks: 1, MaxTasks: 8},
		{Cid: "ws:10.0.0.2:1-1", Hostname: "gmkhost-codex", MatchedRoot: "/repo", ActiveTasks: 0, MaxTasks: 8},
	})
	for _, want := range []string{"ws:10.0.0.1:1-1", "gmkhost-codex", "--runner", "/repo"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Single multi-profile runner: the combos share a cid and differ only by agent,
// so --runner cannot disambiguate — the hint must steer to --agent, and each
// row must show its profile.
func TestFormatAmbiguousCandidatesSameRunnerProfiles(t *testing.T) {
	out := formatAmbiguousCandidates([]cli.RunnerCandidate{
		{Cid: "ws:10.0.0.1:1-1", Hostname: "gmkhost", MatchedRoot: "/repo", Profile: "claude", MaxTasks: 8},
		{Cid: "ws:10.0.0.1:1-1", Hostname: "gmkhost", MatchedRoot: "/repo", Profile: "codex", MaxTasks: 8},
	})
	for _, want := range []string{"ambiguous agent", "--agent claude", "--agent codex", "agent=claude", "agent=codex"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--runner") {
		t.Errorf("same-cid ambiguity should not suggest --runner:\n%s", out)
	}
}
