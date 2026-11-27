package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func rkViews() ([]cli.RunnerKeyView, []cli.RunnerDialView) {
	return []cli.RunnerKeyView{{Key: strings.Repeat("0a", 32), Prefix: "0a0a0a0a0a0a0a0a", Label: "lab", EnrolledBy: "operator",
			Bound: []string{}, Runners: []string{"aa000000 h1"}}},
		[]cli.RunnerDialView{{ID: "00000007", KeyPrefix: "0a0a0a0a0a0a0a0a", State: "waiting", Candidates: []string{"h:7001"}}}
}

func TestRunnerKeysModalListsKeysAndDials(t *testing.T) {
	m := NewRunnerKeysModal()
	m.SetSize(120, 30)
	m.Open(nil, nil)
	m.SetViews(rkViews())
	v := m.View()
	for _, want := range []string{"0a0a0a0a0a0a0a0a", "label=lab", "unbound", "aa000000 h1", "00000007", "candidates=h:7001", "state=waiting"} {
		if !strings.Contains(v, want) {
			t.Errorf("modal lacks %q:\n%s", want, v)
		}
	}
}

// Unenroll asks plainly first; force is offered only after in_use, listing
// the runners on the key; an ok answer goes back to the list.
func TestRunnerKeysModalUnenrollForcesOnlyAfterInUse(t *testing.T) {
	m := NewRunnerKeysModal()
	m.SetSize(120, 30)
	m.Open(nil, nil)
	m.SetViews(rkViews())
	m.BeginRemove()
	if m.mode != rkConfirm || !strings.Contains(m.prompt, "unenroll") || m.force {
		t.Fatalf("first ask: mode=%v prompt=%q force=%v", m.mode, m.prompt, m.force)
	}
	req := m.Confirm()
	if req.kind != "unenroll" || req.force || req.ref != strings.Repeat("0a", 32) {
		t.Fatalf("first request must not force: %+v", req)
	}
	m.ApplyOffer(cli.ForceOffer{Text: "in use", NeedsForce: true, InTheWay: []string{"aa000000 h1"}})
	if m.mode != rkConfirm || !m.force || !strings.Contains(m.prompt, "aa000000 h1") {
		t.Fatalf("force offer: mode=%v force=%v prompt=%q", m.mode, m.force, m.prompt)
	}
	if req := m.Confirm(); !req.force {
		t.Fatalf("the second request forces: %+v", req)
	}
	m.ApplyOffer(cli.ForceOffer{Text: "unenrolled", OK: true})
	if m.mode != rkList {
		t.Fatalf("ok returns to the list, mode=%v", m.mode)
	}
}

func TestRunnerKeysModalDialFormNeedsAKey(t *testing.T) {
	m := NewRunnerKeysModal()
	m.SetSize(120, 30)
	m.Open(nil, nil)
	m.SetViews(nil, nil)
	m.OpenDialForm()
	m.cands.SetValue("h:7001")
	if p := m.formProblem(); !strings.Contains(p, "key") {
		t.Fatalf("a dial record without a key must be refused: %q", p)
	}
	m.SetViews(rkViews())
	m.OpenDialForm()
	m.cands.SetValue("h:7001")
	if p := m.formProblem(); p != "" {
		t.Fatalf("with a key: %q", p)
	}
	if req := m.formRequest(); req.kind != "dial add" || req.ref != strings.Repeat("0a", 32) {
		t.Fatalf("dial add request: %+v", req)
	}
}

// Kick, from the runners table: a plain ask, then force only after in_use,
// listing the runner's tasks.
func TestRunnerKickAsksForceOnlyAfterInUse(t *testing.T) {
	a := &App{cmdresult: NewCmdResult()}
	r := protocol.RunnerInfo{Id: protocol.RunnerID{Id: [16]byte{0xaa}}}
	r.SetHostname([]byte("h1"))
	a.askKick(r)
	if a.kickAsk == nil || a.kickAsk.force || !strings.Contains(a.kickAsk.prompt, "h1") {
		t.Fatalf("first ask: %+v", a.kickAsk)
	}
	a.applyKickOffer(RunnerOfferMsg{Kind: "kick", Ref: a.kickAsk.ref, Offer: cli.ForceOffer{Text: "in use", NeedsForce: true, InTheWay: []string{"bb00"}}})
	if a.kickAsk == nil || !a.kickAsk.force || !strings.Contains(a.kickAsk.prompt, "bb00") {
		t.Fatalf("force ask: %+v", a.kickAsk)
	}
	a.applyKickOffer(RunnerOfferMsg{Kind: "kick", Offer: cli.ForceOffer{Text: "left", OK: true}})
	if a.kickAsk != nil {
		t.Fatalf("an answer that needs no force ends the question: %+v", a.kickAsk)
	}
}
