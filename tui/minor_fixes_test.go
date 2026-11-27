package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Renaming a param through another param's name must not merge their
// references: references follow the param, not its spelling (minor 1).
func TestActionFormRenameCollisionKeepsReferences(t *testing.T) {
	v := cli.ActionView{Name: "a", Params: []cli.ActionParamView{{Name: "ref", Type: "token"}, {Name: "refs", Type: "token"}},
		Template: []cli.ActionElementView{{Literal: "run"}, {IsParam: true, Param: "ref"}, {IsParam: true, Param: "refs"}}}
	f := NewActionForm(&v)
	f.focusRow(rowParamName, 1, 0)
	f.Update(tea.KeyMsg{Type: tea.KeyBackspace}) // refs -> ref (collides)
	f.Update(typeKeys("x"))                      // ref -> refx
	got := f.Form().Template
	if got[1].Param != "ref" || got[2].Param != "refx" {
		t.Fatalf("references after the rename: %+v", got)
	}
}

// The action list arriving after the picker opened must not move the cursor
// to another row (minor 4).
func TestPickerCursorKeepsItsRowWhenActionsArrive(t *testing.T) {
	target := twoOverrides(t)
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	for i, r := range m.rows {
		if r.deleg && r.kind == rowCap && r.label == "cancel" {
			m.cursor = i
		}
	}
	before := m.rows[m.cursor]
	m.SetActionViews([]cli.ActionView{{ID: actA, Name: "deploy"}, {ID: actB, Name: "build"}})
	if after := m.rows[m.cursor]; after.kind != before.kind || after.label != before.label || after.deleg != before.deleg {
		t.Fatalf("cursor moved from %+v to %+v", before, after)
	}
}

// A failed action-list load is said as such, not as deleted actions or an
// empty list (minor 3).
func TestPickerActionLoadFailureIsSaid(t *testing.T) {
	target := twoOverrides(t)
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	m.SetActionViewsErr(errors.New("boom"))
	var labels []string
	for _, r := range m.rows {
		if r.ov == 0 && (r.kind == rowOvAction || r.kind == rowSection) {
			labels = append(labels, r.label)
		}
	}
	all := strings.Join(labels, " | ")
	if strings.Contains(all, "deleted") || strings.Contains(all, "no actions") || !strings.Contains(all, "boom") {
		t.Fatalf("labels after a failed load: %s", all)
	}
}

// Nothing is reported removed when nothing was sent (minor 6).
func TestRemovedOverrideNotReportedWhenNothingIsSent(t *testing.T) {
	a := &App{cmdresult: NewCmdResult()}
	target := twoOverrides(t)
	a.authorityPicker.OpenRegrant(target, []protocol.TaskInfo{target})
	rowOf(t, &a.authorityPicker, rowOvHead, 1, nil)
	a.authorityPicker.RemoveOverride()
	a.inAuthorityPicker(keyEnter)
	out := strings.Join(a.cmdresult.lines, "\n")
	if !strings.Contains(out, "not connected") || strings.Contains(out, "override") {
		t.Fatalf("result pane:\n%s", out)
	}
}

// Results the modals hide the results pane from are shown inside them
// (minor 7): the unenroll restore line, a refused action rm.
func TestModalsShowTheirOwnResults(t *testing.T) {
	a := &App{cmdresult: NewCmdResult(), runnerKeysModal: NewRunnerKeysModal(), actionsModal: NewActionsModal()}
	a.runnerKeysModal.SetSize(120, 30)
	a.runnerKeysModal.Open(nil, nil)
	a.Update(RunnerOfferMsg{Kind: "unenroll", Offer: cli.ForceOffer{OK: true, Text: "unenrolled 0a0a label=x\nundo: harness-cli runner enroll 0a0a"}})
	if !strings.Contains(a.runnerKeysModal.View(), "undo: harness-cli runner enroll") {
		t.Fatalf("the restore line must show in the modal:\n%s", a.runnerKeysModal.View())
	}
	a.runnerKeysModal.Close()
	a.actionsModal.SetSize(120, 30)
	a.actionsModal.Open()
	a.Update(ActionResultMsg{Verb: "action rm", Text: "action rm deploy: refused by the server", OK: false})
	if !strings.Contains(a.actionsModal.View(), "refused by the server") {
		t.Fatalf("a refused rm must show in the modal:\n%s", a.actionsModal.View())
	}
}
