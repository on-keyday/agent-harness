package main

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

func TestFormatAmbiguousCandidates(t *testing.T) {
	out := formatAmbiguousCandidates([]cli.RunnerCandidate{
		{RunnerID: strings.Repeat("a", 32), Cid: "ws:h:1-1", Hostname: "gmkhost", MatchedRoot: "/r", MaxTasks: 8, Profiles: []string{"claude", "codex"}},
		{Cid: "ws:h:2-2", Hostname: "gmkhost", MatchedRoot: "/r", MaxTasks: 8, Profiles: []string{"claude"}},
	})
	for _, want := range []string{"2 runners", "--runner " + strings.Repeat("a", 32), "--agent <claude|codex>", "--runner ws:h:2-2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// One runner: --runner is noise, and the agents are the whole choice.
func TestFormatAmbiguousCandidatesOneRunner(t *testing.T) {
	out := formatAmbiguousCandidates([]cli.RunnerCandidate{{Cid: "ws:h:1-1", Hostname: "h", Profiles: []string{"claude", "codex", "agy"}}})
	if strings.Contains(out, "--runner") || !strings.Contains(out, "3 agents") || !strings.Contains(out, "--agent <claude|codex|agy>") {
		t.Fatalf("got:\n%s", out)
	}
}
