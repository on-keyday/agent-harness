package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

var (
	actA = strings.Repeat("a1", 16)
	actB = strings.Repeat("b2", 16)
	gone = strings.Repeat("dd", 16)
)

func actionID(t *testing.T, hexID string) protocol.ActionID {
	t.Helper()
	id, err := ParseTaskID(hexID) // 16 bytes of hex, as an action id is
	if err != nil {
		t.Fatal(err)
	}
	return protocol.ActionID{Id: id.Id}
}

func actionCond(t *testing.T, caps protocol.Capability, ids ...string) protocol.GrantCondition {
	t.Helper()
	g := protocol.GrantCondition{Caps: caps, Kind: protocol.ConditionKind_ActionSet}
	var set protocol.ActionSetCondition
	for _, id := range ids {
		set.Actions = append(set.Actions, actionID(t, id))
	}
	set.ActionsLen = uint8(len(set.Actions))
	g.SetActionSet(set)
	return g
}

// twoOverrides is a target holding a conditioned exec_run override and a
// plain cancel override.
func twoOverrides(t *testing.T) protocol.TaskInfo {
	run := protocol.ScopeOverride{Caps: protocol.Capability_ExecRun, Base: protocol.ScopeBase_Subtree}
	run.SetConditioned(true)
	cancel := protocol.ScopeOverride{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None}
	return protocol.TaskInfo{
		Id: protocol.TaskID{Id: [16]byte{1}}, Capabilities: protocol.Capability_All,
		Scope:           protocol.TaskScope{Base: protocol.ScopeBase_Subtree},
		Overrides:       []protocol.ScopeOverride{run, cancel},
		GrantConditions: []protocol.GrantCondition{actionCond(t, protocol.Capability_ExecRun, actA)},
	}
}

// rowOf puts the cursor on the first row of kind k in override ov (and, for
// a cap row, the bit; for an action row, the id).
func rowOf(t *testing.T, m *AuthorityPickerModel, k pickerRowKind, ov int, match func(pickerRow) bool) {
	t.Helper()
	for i, r := range m.rows {
		if r.kind == k && r.ov == ov && (match == nil || match(r)) {
			m.cursor = i
			return
		}
	}
	t.Fatalf("no row of kind %v in override %d", k, ov)
}

func TestRegrantKeepsUntouchedOverrideAndBody(t *testing.T) {
	target := twoOverrides(t)
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	rowOf(t, &m, rowOvBase, 1, nil) // the cancel override: none -> global
	m.Toggle()
	got := m.Overrides()
	if len(got) != 2 || !reflect.DeepEqual(got[0], target.Overrides[0]) {
		t.Fatalf("the untouched conditioned override must go back as it came:\n%+v\nwant\n%+v", got, target.Overrides[0])
	}
	if got[1].Caps != protocol.Capability_Cancel || got[1].Base != protocol.ScopeBase_Global {
		t.Fatalf("the edited override: %+v", got[1])
	}
	if conds := m.GrantConditions(); len(conds) != 1 || !reflect.DeepEqual(conds[0], target.GrantConditions[0]) {
		t.Fatalf("its condition body must go back as it came: %+v", conds)
	}
}

func TestRemovingAnOverrideDropsOnlyIt(t *testing.T) {
	target := twoOverrides(t)
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	rowOf(t, &m, rowOvHead, 1, nil)
	m.RemoveOverride()
	got := m.Overrides()
	if len(got) != 1 || got[0].Caps != protocol.Capability_ExecRun || len(m.GrantConditions()) != 1 {
		t.Fatalf("only the cancel override should be gone: %+v", got)
	}
	if r := m.Removed(); len(r) != 1 || !strings.Contains(r[0], "cancel") {
		t.Fatalf("the removal is named for the result line: %v", r)
	}
}

func TestClaimedBitNotSelectableInAnotherRow(t *testing.T) {
	target := twoOverrides(t)
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	rowOf(t, &m, rowOvCap, 1, func(r pickerRow) bool { return r.bit == protocol.Capability_ExecRun })
	if !m.rowDisabled(m.rows[m.cursor]) {
		t.Fatal("exec_run belongs to override 1; it must be disabled in override 2")
	}
	m.Toggle()
	if m.Overrides()[1].Caps&protocol.Capability_ExecRun != 0 {
		t.Fatal("a claimed bit was toggled on in a second override")
	}
}

func TestConditionChecklistForExecRunRow(t *testing.T) {
	target := twoOverrides(t)
	target.GrantConditions = []protocol.GrantCondition{actionCond(t, protocol.Capability_ExecRun, gone)}
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	m.SetActionViews([]cli.ActionView{{ID: actA, Name: "deploy"}, {ID: actB, Name: "build"}})
	var labels []string
	for _, r := range m.rows {
		if r.kind == rowOvAction && r.ov == 0 {
			labels = append(labels, r.label)
		}
	}
	if len(labels) != 3 || !strings.Contains(labels[0], "deleted "+gone[:8]) || !strings.Contains(labels[1], "deploy") {
		t.Fatalf("checklist rows: %q", labels)
	}
	// check deploy: the condition becomes {deploy, gone}, in the shared
	// parser's normalised order (the server's)
	rowOf(t, &m, rowOvAction, 0, func(r pickerRow) bool { return r.idHex == actA })
	m.Toggle()
	conds := m.GrantConditions()
	if len(conds) != 1 || conds[0].ActionSet() == nil || len(conds[0].ActionSet().Actions) != 2 ||
		conds[0].ActionSet().Actions[0] != actionID(t, actA) || conds[0].ActionSet().Actions[1] != actionID(t, gone) {
		t.Fatalf("condition after checking deploy: %+v", conds)
	}
	if ov := m.Overrides(); !ov[0].Conditioned() {
		t.Fatal("the edited exec_run override must stay marked conditioned")
	}
	// a forward_local override shows the ports text row, a mixed one none
	m.AddOverride()
	n := len(m.ovs) - 1
	rowOf(t, &m, rowOvCap, n, func(r pickerRow) bool { return r.bit == protocol.Capability_ForwardLocal })
	m.Toggle()
	hasPorts := func() bool {
		for _, r := range m.rows {
			if r.kind == rowOvPorts && r.ov == n {
				return true
			}
		}
		return false
	}
	if !hasPorts() {
		t.Fatal("a forward_local override offers the ports condition")
	}
	rowOf(t, &m, rowOvCap, n, func(r pickerRow) bool { return r.bit == protocol.Capability_Cancel })
	m.Toggle() // cancel is claimed by override 2: toggling must be refused
	rowOf(t, &m, rowOvCap, n, func(r pickerRow) bool { return r.bit == protocol.Capability_Notify })
	m.Toggle() // a mixed row takes no condition
	if hasPorts() {
		t.Fatal("a mixed override must not offer a condition")
	}
}

func TestPortsConditionIsTypedAndChecked(t *testing.T) {
	var m AuthorityPickerModel
	m.OpenSession(Authority{Caps: protocol.Capability_All, Scope: protocol.TaskScope{Base: protocol.ScopeBase_Subtree}}, nil)
	m.AddOverride()
	rowOf(t, &m, rowOvCap, 0, func(r pickerRow) bool { return r.bit == protocol.Capability_ForwardLocal })
	m.Toggle()
	rowOf(t, &m, rowOvPorts, 0, nil)
	for _, r := range "8000-8100,22" {
		m.TypeDigit(r)
	}
	if p := m.Problem(); p != "" {
		t.Fatalf("a valid range: %s", p)
	}
	conds := m.GrantConditions()
	if len(conds) != 1 || conds[0].PortRange() == nil || len(conds[0].PortRange().Spans) != 2 {
		t.Fatalf("ports condition: %+v", conds)
	}
	m.TypeDigit('-')
	m.TypeDigit('-')
	if m.Problem() == "" {
		t.Fatal("a malformed range must be reported before apply")
	}
}

// The session defaults edit overrides too, and what the picker built reaches
// the spawn request through the App's authority.
func TestSessionDefaultsEditOverrides(t *testing.T) {
	a := &App{cmdresult: NewCmdResult(), sessionCaps: protocol.Capability_All,
		sessionScope: protocol.TaskScope{Base: protocol.ScopeBase_Subtree}}
	a.authorityPicker.OpenSession(a.authority(), nil)
	a.authorityPicker.SetActionViews([]cli.ActionView{{ID: actA, Name: "deploy"}})
	a.authorityPicker.AddOverride()
	rowOf(t, &a.authorityPicker, rowOvCap, 0, func(r pickerRow) bool { return r.bit == protocol.Capability_ExecRun })
	a.authorityPicker.Toggle()
	rowOf(t, &a.authorityPicker, rowOvAction, 0, func(r pickerRow) bool { return r.idHex == actA })
	a.authorityPicker.Toggle()
	a.inAuthorityPicker(keyEnter)
	au := a.authority()
	if len(au.Overrides) != 1 || au.Overrides[0].Caps != protocol.Capability_ExecRun || !au.Overrides[0].Conditioned() ||
		len(au.GrantConditions) != 1 || au.GrantConditions[0].ActionSet() == nil {
		t.Fatalf("session authority: overrides=%+v conds=%+v", au.Overrides, au.GrantConditions)
	}
}
