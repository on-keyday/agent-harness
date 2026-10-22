package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const delegResumeID = "d7df8fe8dc239a515c4f2de02e05ee78"

// A fresh spawn carries the session's default bound; a typed flag wins.
func TestSpawnCarriesSessionDefaultBound(t *testing.T) {
	a := New(Config{})
	a.sessionDelegateCaps = protocol.Capability_FileRead
	a.sessionDelegateScope = protocol.TaskScope{Base: protocol.ScopeBase_None}

	act, err := ParseCommand(`session new`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	opts := a.spawnAuthority(act.(verb.SpawnAction), 0).opts(sessionRequest{})
	if opts.DelegateCaps != protocol.Capability_FileRead || opts.DelegateScope.Base != protocol.ScopeBase_None {
		t.Errorf("fresh spawn bound = %v / %v, want the session default", opts.DelegateCaps, opts.DelegateScope.Base)
	}

	act, err = ParseCommand(`session new --delegate-caps cancel`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if opts := a.spawnAuthority(act.(verb.SpawnAction), 0).opts(sessionRequest{}); opts.DelegateCaps != protocol.Capability_Cancel {
		t.Errorf("typed --delegate-caps = %v, want cancel over the default", opts.DelegateCaps)
	}
}

// Review Focus 4: the session default never re-grants a resumed task's bound
// — not on a typed resume that names no bound flag, and not on r / R / u / U,
// which go through the session authority.
func TestResumeNeverSendsSessionDefaultBound(t *testing.T) {
	a := New(Config{})
	a.sessionDelegateCaps = protocol.Capability_FileRead
	a.sessionDelegateScope = protocol.TaskScope{Base: protocol.ScopeBase_None}

	act, err := ParseCommand(`session new --resume `+delegResumeID, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	v := act.(verb.SpawnAction)
	opts := a.spawnAuthority(v, 0).opts(sessionRequest{ResumeTaskID: v.ResumeTaskID})
	if opts.DelegateCapsPresent || opts.DelegateScopePresent {
		t.Errorf("typed resume re-grants the session bound: caps %v scope %v", opts.DelegateCapsPresent, opts.DelegateScopePresent)
	}
	if opts := a.authority().opts(sessionRequest{ResumeTaskID: delegResumeID}); opts.DelegateCapsPresent || opts.DelegateScopePresent {
		t.Errorf("r/R/u/U authority re-grants the session bound: caps %v scope %v", opts.DelegateCapsPresent, opts.DelegateScopePresent)
	}

	act, err = ParseCommand(`session new --resume `+delegResumeID+` --delegate-scope descendants`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	v = act.(verb.SpawnAction)
	if opts := a.spawnAuthority(v, 0).opts(sessionRequest{ResumeTaskID: v.ResumeTaskID}); !opts.DelegateScopePresent || opts.DelegateCapsPresent {
		t.Errorf("typed --delegate-scope on resume: present caps %v scope %v, want scope only", opts.DelegateCapsPresent, opts.DelegateScopePresent)
	}
}

// pickerDelegRow moves the cursor to the first delegation-section row of kind
// k whose label contains want.
func pickerDelegRow(t *testing.T, m *AuthorityPickerModel, k pickerRowKind, want string) {
	t.Helper()
	for i, r := range m.rows {
		if r.deleg && r.kind == k && strings.Contains(r.label, want) {
			m.cursor = i
			return
		}
	}
	t.Fatalf("no delegation row of kind %v containing %q", k, want)
}

func TestPickerDelegationSection(t *testing.T) {
	target := pickerTask(0xaa, "target")
	target.Capabilities = protocol.Capability_FileRead
	target.DelegateCaps = protocol.Capability_FileRead
	target.DelegateScope = protocol.TaskScope{Base: protocol.ScopeBase_Subtree}
	target.DelegateScope.SetExcludeSelf(true)

	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	if dcaps, spec, changed := m.Delegation(); dcaps != protocol.Capability_FileRead || spec != "descendants" || changed {
		t.Fatalf("prefill = %v %q changed=%v, want file_read descendants unchanged", dcaps, spec, changed)
	}

	pickerDelegRow(t, &m, rowCap, "cancel")
	m.Toggle()
	caps, _, _, _ := m.Result()
	dcaps, _, changed := m.Delegation()
	if caps != protocol.Capability_FileRead || dcaps != protocol.Capability_FileRead|protocol.Capability_Cancel || !changed {
		t.Errorf("toggling cancel in the section: own %v, bound %v changed=%v; want own unchanged", caps, dcaps, changed)
	}

	pickerDelegRow(t, &m, rowExcludeSelf, "")
	m.Toggle()
	if _, spec, _ := m.Delegation(); spec != "subtree" {
		t.Errorf("bound scope after un-excluding self = %q, want subtree", spec)
	}
}

func TestDetailShowsBound(t *testing.T) {
	out := formatTaskDetail(protocol.TaskInfo{}, nil)
	if !strings.Contains(out, "delegate:      none\n") || !strings.Contains(out, "delegate-scope: subtree\n") {
		t.Errorf("detail lacks the default bound lines:\n%s", out)
	}
}

// The audit modal marks a holder that may hand its bit down, as the CLI does.
func TestCapsAuditModalMarksDelegators(t *testing.T) {
	a := New(Config{})
	a.width, a.height = 160, 40
	var id protocol.TaskID
	id.Id[0] = 0xab
	setAuditTasks(a, protocol.TaskInfo{Id: id, Status: protocol.TaskStatus_Running,
		Capabilities: protocol.Capability_Spawn, DelegateCaps: protocol.Capability_Spawn})
	a = press(t, a, runes("i"), enter) // the first row is spawn
	if v := a.capsAuditModal.View(); !strings.Contains(v, " deleg") {
		t.Fatalf("holder that may delegate spawn lacks the deleg mark:\n%s", v)
	}
}

// A caps set result line names the change (surface-parity item 29), the bound
// included: a bound-only re-grant used to print an empty summary.
func TestSetCapsSummaryNamesTheBound(t *testing.T) {
	dc := protocol.Capability_FileRead
	ds := protocol.TaskScope{Base: protocol.ScopeBase_None}
	got := setCapsSummary(cli.SetCapsOpts{DelegateCaps: &dc, DelegateScope: &ds})
	if got != "delegate=file_read  delegate-scope=none" {
		t.Errorf("summary = %q", got)
	}
}

// caps reach in the TUI reads the aged listing, as every view does: the
// granter's age advances between snapshots.
func TestCapsReachAgesTheGranter(t *testing.T) {
	a := New(Config{})
	var ti protocol.TaskInfo
	ti.Id.Id[0] = 0x44
	ti.Status = protocol.TaskStatus_Running
	ti.Capabilities = protocol.Capability_Cancel
	ti.AuthoritySource = protocol.AuthoritySource_OperatorCapsSet
	ti.AuthorityAt = 5
	ti.AuthorityAgeMs = 1000
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{ti}})
	id := FormatTaskID(ti.Id)
	a.authorityRecvAt[id] = a.authorityRecvAt[id].Add(-10 * time.Minute)
	tuiVerbs{a: a}.CapsReach(verb.CapsReachAction{TaskID: id})
	if got := a.cmdresult.View(); !strings.Contains(got, "operator_caps_set@operator (10m ago)") {
		t.Errorf("caps reach granter age not advanced:\n%s", got)
	}
}
