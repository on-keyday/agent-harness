package cli

import (
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestCandidatesFromResponse(t *testing.T) {
	var c protocol.RunnerCandidate
	c.SetCid([]byte("ws:10.0.0.2:1-1"))
	c.SetHostname([]byte("gmkhost-codex"))
	c.SetMatchedRoot([]byte("/home/x/repo"))
	c.ActiveTasks = 3
	c.MaxTasks = 8
	c.SetProfile([]byte("codex"))
	resp := &protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_AmbiguousRunner}
	resp.SetCandidates([]protocol.RunnerCandidate{c})

	got := candidatesFromResponse(resp)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].Cid != "ws:10.0.0.2:1-1" || got[0].Hostname != "gmkhost-codex" ||
		got[0].MatchedRoot != "/home/x/repo" || got[0].ActiveTasks != 3 || got[0].MaxTasks != 8 ||
		got[0].Profile != "codex" {
		t.Fatalf("mapping mismatch: %+v", got[0])
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
