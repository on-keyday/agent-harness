package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

func TestRunnerPickerPick(t *testing.T) {
	var p RunnerPickerModel
	cands := []cli.RunnerCandidate{
		{Cid: "ws:10.0.0.1:1-1", Hostname: "h1", ActiveTasks: 1, MaxTasks: 8},
		{Cid: "ws:10.0.0.2:1-1", Hostname: "h2", ActiveTasks: 0, MaxTasks: 8},
	}
	p.Open(cands)
	if !p.IsOpen() {
		t.Fatal("want open")
	}
	if got := p.Pick("2"); got == nil || got.Cid != "ws:10.0.0.2:1-1" {
		t.Fatalf("Pick(2)=%v", got)
	}
	if got := p.Pick("3"); got != nil {
		t.Fatalf("Pick(3)=%v want nil (out of range)", got)
	}
	if got := p.Pick("x"); got != nil {
		t.Fatalf("Pick(x)=%v want nil (non-digit)", got)
	}
	p.Close()
	if p.IsOpen() {
		t.Fatal("want closed")
	}
}
