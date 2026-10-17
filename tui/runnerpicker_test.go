package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

type pickRec struct {
	sel   cli.SelectorOpts
	agent string
	n     int
}

func (r *pickRec) retry(sel cli.SelectorOpts, agent string) tea.Cmd {
	r.sel, r.agent = sel, agent
	r.n++
	return func() tea.Msg { return nil }
}

var twoRunners = []cli.RunnerCandidate{
	{RunnerID: "11111111111111111111111111111111", Cid: "ws:a:1-1", Hostname: "a", Profiles: []string{"claude", "codex"}},
	{Cid: "ws:b:1-1", Hostname: "b", Profiles: []string{"claude"}},
}

func TestRunnerPickerTwoSteps(t *testing.T) {
	var p RunnerPickerModel
	var r pickRec
	if cmd := p.Open(twoRunners, r.retry); cmd != nil || !p.IsOpen() {
		t.Fatal("two runners must open step 1")
	}
	p.Key("1") // runner a → step 2 (two agents)
	if r.n != 0 || !p.IsOpen() {
		t.Fatal("runner with two agents must open step 2")
	}
	if _, label := p.Key("2"); label != "a  ws:a:1-1  (codex)" || r.n != 1 || r.agent != "codex" || r.sel.Runner != twoRunners[0].RunnerID {
		t.Fatalf("pick = %+v label=%q", r, label)
	}
	if p.IsOpen() {
		t.Fatal("picker stays open after a pick")
	}
}

func TestRunnerPickerSkipsSingleChoiceSteps(t *testing.T) {
	var p RunnerPickerModel
	var r pickRec
	p.Open(twoRunners, r.retry)
	p.Key("2") // runner b has one agent → fires immediately, pinned by cid
	if r.n != 1 || r.agent != "claude" || r.sel.Runner != "ws:b:1-1" {
		t.Fatalf("pick = %+v", r)
	}
	// One runner: step 1 is skipped.
	var q RunnerPickerModel
	var s pickRec
	q.Open(twoRunners[:1], s.retry)
	q.Key("1")
	if s.agent != "claude" {
		t.Fatalf("one runner: first key picks an agent, got %+v", s)
	}
}

// Review Focus 5: one runner, one agent — nothing to choose, the retry fires
// from Open itself.
func TestRunnerPickerNothingToChoose(t *testing.T) {
	var p RunnerPickerModel
	var r pickRec
	cmd := p.Open([]cli.RunnerCandidate{{Cid: "c", Profiles: []string{"x"}}}, r.retry)
	if cmd == nil || r.n != 1 || p.IsOpen() {
		t.Fatalf("cmd=%v rec=%+v open=%v", cmd != nil, r, p.IsOpen())
	}
}

// More than nine rows: j/k + Enter reach the tenth.
func TestRunnerPickerCursorReachesPastNine(t *testing.T) {
	agents := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10", "a11"}
	var p RunnerPickerModel
	var r pickRec
	p.Open([]cli.RunnerCandidate{{Cid: "c", Profiles: agents}}, r.retry)
	for i := 0; i < 9; i++ {
		p.Key("j")
	}
	p.Key("enter")
	if r.agent != "a10" {
		t.Fatalf("agent=%q want a10", r.agent)
	}
}

func TestRunnerPickerEscBacksOutOfStep2(t *testing.T) {
	var p RunnerPickerModel
	var r pickRec
	p.Open(twoRunners, r.retry)
	p.Key("1")
	p.Key("esc")
	if !p.IsOpen() || r.n != 0 {
		t.Fatal("esc in step 2 returns to step 1")
	}
	p.Key("esc")
	if p.IsOpen() {
		t.Fatal("esc in step 1 closes")
	}
}
