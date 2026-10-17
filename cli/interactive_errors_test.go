package cli

import (
	"errors"
	"reflect"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestCandidatesFromBody(t *testing.T) {
	var rc protocol.RunnerCandidate
	rc.RunnerId.Id[0] = 0xab
	rc.SetCid([]byte("ws:10.0.0.1:1-2"))
	rc.SetHostname([]byte("h1"))
	rc.SetMatchedRoot([]byte("/r"))
	rc.ActiveTasks, rc.MaxTasks = 1, 4
	var a, b protocol.AgentProfileName
	a.SetName([]byte("claude"))
	b.SetName([]byte("codex"))
	rc.SetProfiles([]protocol.AgentProfileName{a, b})
	var legacy protocol.RunnerCandidate // zero identity
	legacy.SetCid([]byte("ws:10.0.0.2:1-2"))
	var body protocol.RunnerCandidatesBody
	body.SetCandidates([]protocol.RunnerCandidate{rc, legacy})

	got := candidatesFromBody(&body)
	want := RunnerCandidate{RunnerID: rc.RunnerId.Hex(), Cid: "ws:10.0.0.1:1-2", Hostname: "h1", MatchedRoot: "/r", ActiveTasks: 1, MaxTasks: 4, Profiles: []string{"claude", "codex"}}
	if len(got) != 2 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if got[1].RunnerID != "" {
		t.Fatalf("zero identity must map to an empty RunnerID, got %q", got[1].RunnerID)
	}
	if c := candidatesFromBody(&protocol.RunnerCandidatesBody{}); len(c) != 0 {
		t.Fatalf("empty body -> %v", c)
	}
}

func TestFlattenCandidates(t *testing.T) {
	cs := []RunnerCandidate{{Cid: "a", Profiles: []string{"claude", "codex"}}, {Cid: "b", Profiles: []string{"claude"}}}
	got := FlattenCandidates(cs)
	if len(got) != 3 || got[0].Profile != "claude" || got[1].Profile != "codex" || got[2].Candidate.Cid != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestAmbiguousRunnerErrorIsAs(t *testing.T) {
	err := error(&AmbiguousRunnerError{Candidates: []RunnerCandidate{{Cid: "ws:1", Hostname: "h"}}})
	var are *AmbiguousRunnerError
	if !errors.As(err, &are) {
		t.Fatal("errors.As failed")
	}
	if len(are.Candidates) != 1 || are.Candidates[0].Cid != "ws:1" {
		t.Fatalf("candidates lost: %+v", are.Candidates)
	}
	if err.Error() == "" {
		t.Fatal("empty Error() string")
	}
}
