package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRunnerSummaryCounts(t *testing.T) {
	mk := func(s protocol.RunnerStatus) protocol.RunnerInfo { return protocol.RunnerInfo{Status: s} }
	got := RunnerSummary([]protocol.RunnerInfo{
		mk(protocol.RunnerStatus_Idle), mk(protocol.RunnerStatus_Busy),
		mk(protocol.RunnerStatus_Busy), mk(protocol.RunnerStatus_Busy),
		mk(protocol.RunnerStatus_Offline),
	})
	if want := "runners 5: 1 idle 3 busy 1 offline"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	// idle and busy keep their zero; an absent status is omitted.
	if got, want := RunnerSummary(nil), "runners 0: 0 idle 0 busy"; got != want {
		t.Errorf("empty summary = %q, want %q", got, want)
	}
}

func TestRunnerStatusCounts(t *testing.T) {
	rs := []protocol.RunnerInfo{{Status: protocol.RunnerStatus_Idle}, {Status: protocol.RunnerStatus_Busy}, {Status: protocol.RunnerStatus_Offline}, {Status: protocol.RunnerStatus_Busy}}
	idle, busy, offline := RunnerStatusCounts(rs)
	if idle != 1 || busy != 2 || offline != 1 {
		t.Fatalf("got %d %d %d", idle, busy, offline)
	}
	if RunnerStatusWord(protocol.RunnerStatus_Idle) != "Idle" || RunnerStatusWord(protocol.RunnerStatus_Busy) != "Busy" || RunnerStatusWord(protocol.RunnerStatus_Offline) != "Offline" {
		t.Fatal("status words changed")
	}
}
