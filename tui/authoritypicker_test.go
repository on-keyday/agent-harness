package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func pickerTask(b byte, prompt string) protocol.TaskInfo {
	var ti protocol.TaskInfo
	ti.Id.Id[0] = b
	ti.Prompt = []byte(prompt)
	return ti
}

// moveTo places the cursor on the first row whose label contains want.
func moveTo(t *testing.T, m *AuthorityPickerModel, want string) {
	t.Helper()
	start := m.cursor
	for {
		if strings.Contains(m.rows[m.cursor].label, want) {
			return
		}
		m.Move(1)
		if m.cursor == start {
			t.Fatalf("no reachable row containing %q", want)
		}
	}
}

// TestAuthorityPickerScopeSerialization drives real rows and asserts the
// serialized grammar for every base × ids combination in both modes. Every
// non-empty spec must round-trip through cli.ParseScope.
func TestAuthorityPickerScopeSerialization(t *testing.T) {
	target := pickerTask(0xaa, "target")
	sib := pickerTask(0xbb, "sibling")
	other := pickerTask(0xcc, "other")
	sibHex := FormatTaskID(sib.Id)
	otherHex := FormatTaskID(other.Id)

	cases := []struct {
		name    string
		session bool
		drive   func(m *AuthorityPickerModel)
		want    string
	}{
		{"regrant subtree no-ids", false, func(m *AuthorityPickerModel) {}, "subtree"},
		{"regrant none no-ids", false, func(m *AuthorityPickerModel) {
			moveTo(t, m, "base:")
			m.Toggle() // subtree -> none
		}, "none"},
		{"regrant global", false, func(m *AuthorityPickerModel) {
			moveTo(t, m, sibHex[:8])
			m.Toggle() // select an id, then cycle to global: ids must be ignored
			moveTo(t, m, "base:")
			m.Toggle()
			m.Toggle() // subtree -> none -> global
		}, "global"},
		{"regrant none+ids", false, func(m *AuthorityPickerModel) {
			moveTo(t, m, "base:")
			m.Toggle() // -> none
			moveTo(t, m, sibHex[:8])
			m.Toggle()
		}, "ids:" + sibHex},
		{"regrant subtree+ids two", false, func(m *AuthorityPickerModel) {
			moveTo(t, m, sibHex[:8])
			m.Toggle()
			moveTo(t, m, otherHex[:8])
			m.Toggle()
		}, "subtree+ids:" + sibHex + "," + otherHex},
		{"session subtree no-ids is empty", true, func(m *AuthorityPickerModel) {}, ""},
		{"session none", true, func(m *AuthorityPickerModel) {
			moveTo(t, m, "base:")
			m.Toggle()
		}, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m AuthorityPickerModel
			if tc.session {
				m.OpenSession(protocol.Capability_All, protocol.TaskScope{Base: protocol.ScopeBase_Subtree},
					[]protocol.TaskInfo{target, sib, other})
			} else {
				m.OpenRegrant(target, []protocol.TaskInfo{target, sib, other})
			}
			tc.drive(&m)
			_, spec, _, _ := m.Result()
			if spec != tc.want {
				t.Fatalf("spec = %q, want %q", spec, tc.want)
			}
			if spec != "" {
				if _, err := cli.ParseScope(spec); err != nil {
					t.Fatalf("spec %q does not round-trip: %v", spec, err)
				}
			}
		})
	}
}

// TestAuthorityPickerRegrantPrefill: caps bits lit, scope ids pre-checked,
// the target row absent from the list, cascade/keep-conns default off.
func TestAuthorityPickerRegrantPrefill(t *testing.T) {
	target := pickerTask(0xaa, "target")
	sib := pickerTask(0xbb, "sibling")
	target.Capabilities = protocol.Capability_Cancel | protocol.Capability_Notify
	target.Scope = protocol.TaskScope{Base: protocol.ScopeBase_None}
	target.Scope.Ids = append(target.Scope.Ids, sib.Id)
	target.Scope.IdsLen = 1
	targetHex := FormatTaskID(target.Id)
	sibHex := FormatTaskID(sib.Id)

	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target, sib})

	if m.TargetID() != targetHex {
		t.Fatalf("TargetID = %q, want %q", m.TargetID(), targetHex)
	}
	caps, spec, cascade, keep := m.Result()
	if caps != protocol.Capability_Cancel|protocol.Capability_Notify {
		t.Fatalf("prefilled caps = %v", caps)
	}
	if want := "ids:" + sibHex; spec != want {
		t.Fatalf("prefilled spec = %q, want %q", spec, want)
	}
	if cascade || keep {
		t.Fatal("cascade/keep-conns must default off")
	}
	for _, r := range m.rows {
		if strings.Contains(r.label, targetHex[:8]) {
			t.Fatal("target task must not appear in its own id list")
		}
	}
}

// TestAuthorityPickerGlobalSkipsTaskRows: while base==global the cursor
// skips task rows.
func TestAuthorityPickerGlobalSkipsTaskRows(t *testing.T) {
	target := pickerTask(0xaa, "t")
	sib := pickerTask(0xbb, "s")
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target, sib})
	moveTo(t, &m, "base:")
	m.Toggle()
	m.Toggle() // -> global
	sibHex := FormatTaskID(sib.Id)
	start := m.cursor
	for {
		if strings.Contains(m.rows[m.cursor].label, sibHex[:8]) {
			t.Fatal("cursor landed on a task row while base==global")
		}
		m.Move(1)
		if m.cursor == start {
			break
		}
	}
}

// TestAuthorityPickerBaseCycle: Space on the base row cycles
// subtree→none→global→subtree.
func TestAuthorityPickerBaseCycle(t *testing.T) {
	var m AuthorityPickerModel
	m.OpenRegrant(pickerTask(0xaa, "t"), nil)
	moveTo(t, &m, "base:")
	got := []protocol.ScopeBase{m.base}
	for i := 0; i < 3; i++ {
		m.Toggle()
		got = append(got, m.base)
	}
	want := []protocol.ScopeBase{
		protocol.ScopeBase_Subtree, protocol.ScopeBase_None,
		protocol.ScopeBase_Global, protocol.ScopeBase_Subtree,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cycle[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestAuthorityPickerStableWidth: the popup's width must not change when a
// selection lengthens the footer echo (the frame visibly jumped otherwise).
func TestAuthorityPickerStableWidth(t *testing.T) {
	target := pickerTask(0xaa, "t")
	sib := pickerTask(0xbb, "sibling")
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target, sib})
	m.SetSize(120, 40)
	before := lipgloss.Width(m.View())
	moveTo(t, &m, FormatTaskID(sib.Id)[:8])
	m.Toggle() // footer echo now carries a 32-hex id
	after := lipgloss.Width(m.View())
	if before != after {
		t.Fatalf("popup width changed %d -> %d after selecting an id", before, after)
	}
}

// TestAuthorityPickerSessionModeRows: session mode has no cascade/keep-conns
// rows and Result reports both false even after toggling everything.
func TestAuthorityPickerSessionModeRows(t *testing.T) {
	var m AuthorityPickerModel
	m.OpenSession(protocol.Capability_All, protocol.TaskScope{}, nil)
	for _, r := range m.rows {
		if strings.Contains(r.label, "cascade") || strings.Contains(r.label, "keep-conns") {
			t.Fatalf("session mode must not show %q", r.label)
		}
	}
	start := m.cursor
	for {
		m.Toggle()
		m.Move(1)
		if m.cursor == start {
			break
		}
	}
	if _, _, cascade, keep := m.Result(); cascade || keep {
		t.Fatal("session mode must report cascade=keep=false")
	}
	if !m.IsOpen() {
		t.Fatal("picker should still be open")
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("Close must close")
	}
}
