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
