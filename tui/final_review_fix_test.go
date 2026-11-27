package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A command-line `scope` replaces the session overrides; the conditions the
// picker attached to the old ones must go with them (final review I1).
func TestCommandLineScopeClearsSessionConditions(t *testing.T) {
	a := &App{cmdresult: NewCmdResult(),
		sessionOverrides:       []protocol.ScopeOverride{{Caps: protocol.Capability_ExecRun}},
		sessionGrantConditions: []protocol.GrantCondition{{Caps: protocol.Capability_ExecRun, Kind: protocol.ConditionKind_ActionSet}}}
	sc := protocol.TaskScope{Base: protocol.ScopeBase_Subtree}
	tuiVerbs{a}.CapsSetDefaults(verb.SetDefaultsAction{Scope: &sc})
	if au := a.authority(); len(au.GrantConditions) != 0 || len(au.Overrides) != 0 {
		t.Fatalf("stale session conditions: overrides=%+v conds=%+v", au.Overrides, au.GrantConditions)
	}
}

// The Runner keys forms answer inside the modal: dial once leaves its form on
// ok and shows the refusal otherwise; a client-side error shows in the form
// (final review I2).
func TestRunnerKeysFormsAnswerInTheModal(t *testing.T) {
	a := &App{cmdresult: NewCmdResult(), runnerKeysModal: NewRunnerKeysModal()}
	a.runnerKeysModal.Open(nil, nil)
	a.runnerKeysModal.OpenDialOnceForm()
	a.Update(ServerDialResultMsg{RunnerCID: "ws:h:1-*", Status: protocol.DialRunnerStatus_Ok})
	if a.runnerKeysModal.mode != rkList {
		t.Fatalf("dial once ok must leave the form, mode=%v", a.runnerKeysModal.mode)
	}
	a.runnerKeysModal.OpenDialOnceForm()
	a.Update(ServerDialResultMsg{RunnerCID: "ws:h:1-*", Err: errors.New("parse runner CID: bad")})
	if a.runnerKeysModal.mode != rkDialOnce || !strings.Contains(a.runnerKeysModal.formErr, "bad") {
		t.Fatalf("dial once refusal must show in the form: mode=%v err=%q", a.runnerKeysModal.mode, a.runnerKeysModal.formErr)
	}
	a.runnerKeysModal.OpenDialForm()
	a.Update(RunnerKeyResultMsg{Verb: "runner dial add", Err: errors.New("candidate \"x\": want transport:host:port-id")})
	if a.runnerKeysModal.mode != rkDialAdd || !strings.Contains(a.runnerKeysModal.formErr, "transport") {
		t.Fatalf("a client-side error must show in the form: mode=%v err=%q", a.runnerKeysModal.mode, a.runnerKeysModal.formErr)
	}
}

// With many tasks the enroll form still shows the row being typed into, and
// fits the terminal (final review I3).
func TestRunnerKeysEnrollFormScrolls(t *testing.T) {
	var tasks []protocol.TaskInfo
	for i := 0; i < 60; i++ {
		tasks = append(tasks, pickerTask(byte(i+1), fmt.Sprintf("t%d", i)))
	}
	m := NewRunnerKeysModal()
	m.SetSize(100, 24)
	m.Open(tasks, nil)
	m.OpenEnrollForm()
	v := m.View()
	if !strings.Contains(v, "public key") || strings.Count(v, "\n")+1 > 24 {
		t.Fatalf("the first row must show and the view fit 24 lines (%d):\n%s", strings.Count(v, "\n")+1, v)
	}
	m.row = 2 + 59 // the last task
	if v := m.View(); !strings.Contains(v, FormatTaskID(tasks[59].Id)[:8]) || strings.Count(v, "\n")+1 > 24 {
		t.Fatalf("the cursor row must scroll into view:\n%s", v)
	}
}

func TestActionsListScrolls(t *testing.T) {
	var views []cli.ActionView
	for i := 0; i < 30; i++ {
		v := deployView()
		v.Name = fmt.Sprintf("act%02d", i)
		views = append(views, v)
	}
	m := NewActionsModal()
	m.SetSize(100, 24)
	m.Open()
	m.SetViews(views, nil)
	m.cur = 29
	v := m.View()
	if !strings.Contains(v, "act29") || strings.Count(v, "\n")+1 > 24 {
		t.Fatalf("the selected action must show and the view fit (%d lines):\n%s", strings.Count(v, "\n")+1, v)
	}
}

// An override marked conditioned whose body is unknown reaches nothing (C7);
// editing it must not turn it into an unconditioned grant (final review
// minor 5b, graded Important).
func TestUnknownConditionOverrideIsNotWidenedByAnEdit(t *testing.T) {
	ov := protocol.ScopeOverride{Caps: protocol.Capability_ExecRun, Base: protocol.ScopeBase_Subtree}
	ov.SetConditioned(true)
	target := protocol.TaskInfo{Id: protocol.TaskID{Id: [16]byte{1}}, Capabilities: protocol.Capability_All,
		Scope: protocol.TaskScope{Base: protocol.ScopeBase_Subtree}, Overrides: []protocol.ScopeOverride{ov}}
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	rowOf(t, &m, rowOvBase, 0, nil)
	m.Toggle()
	if got := m.Overrides(); len(got) != 1 || !got[0].Conditioned() || got[0].Base != protocol.ScopeBase_Subtree {
		t.Fatalf("an unknown-condition override was edited or lost its marker: %+v", got)
	}
	rowOf(t, &m, rowOvHead, 0, nil)
	if !strings.Contains(m.overrideLine(m.rows[m.cursor]), "unknown") {
		t.Fatalf("the head must say the condition is unknown: %q", m.overrideLine(m.rows[m.cursor]))
	}
}
