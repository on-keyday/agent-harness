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
					nil, []protocol.TaskInfo{target, sib, other})
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
	m.OpenSession(protocol.Capability_All, protocol.TaskScope{}, nil, nil)
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

// TestAuthorityPickerParentMode drives the single-choice parent chooser: the
// detach row, the swap row (only when the target has a parent), a candidate
// row, and Toggle being inert.
func TestAuthorityPickerParentMode(t *testing.T) {
	parent := pickerTask(0xaa, "parent")
	target := pickerTask(0xbb, "target")
	target.CreatorTaskId = parent.Id
	other := pickerTask(0xcc, "other")
	otherHex := FormatTaskID(other.Id)

	var m AuthorityPickerModel
	m.OpenParent(target, []protocol.TaskInfo{parent, target, other})

	// Row 0 is the detach row.
	if p, detach, swap, ok := m.ParentChoice(); !ok || !detach || swap || p != "" {
		t.Fatalf("row 0 = (%q,%v,%v,%v), want detach", p, detach, swap, ok)
	}
	// The swap row names the current parent.
	moveTo(t, &m, "swap with "+FormatTaskID(parent.Id)[:8])
	if p, detach, swap, ok := m.ParentChoice(); !ok || detach || !swap || p != "" {
		t.Fatalf("swap row = (%q,%v,%v,%v)", p, detach, swap, ok)
	}
	// A candidate task row yields its id; Toggle changes nothing.
	moveTo(t, &m, otherHex[:8])
	m.Toggle()
	if p, detach, swap, ok := m.ParentChoice(); !ok || detach || swap || p != otherHex {
		t.Fatalf("task row = (%q,%v,%v,%v), want %q", p, detach, swap, ok, otherHex)
	}
	// The current parent's row is annotated.
	moveTo(t, &m, "current parent")
	if p, _, _, ok := m.ParentChoice(); !ok || p != FormatTaskID(parent.Id) {
		t.Fatalf("current-parent row = %q", p)
	}
	// The target itself is not offered.
	for _, r := range m.rows {
		if r.idHex == FormatTaskID(target.Id) {
			t.Fatal("the target lists itself as a parent candidate")
		}
	}

	// A rootless target gets no swap row.
	var m2 AuthorityPickerModel
	m2.OpenParent(other, []protocol.TaskInfo{parent, target, other})
	for _, r := range m2.rows {
		if r.kind == rowParentSwap {
			t.Fatal("swap row offered for an operator-rooted target")
		}
	}
}

// The picker edits the base scope and the id set; per-capability narrowings
// are typed on the cmdline. It must CARRY them regardless — they travel with
// the scope under one presence bit, so a re-grant that returned an empty list
// would erase the target's rules. That is the defect the cascade shipped with,
// and this is the same shape.
func TestPickerCarriesOverridesItDoesNotEdit(t *testing.T) {
	ov := []protocol.ScopeOverride{{
		Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None,
	}}
	target := protocol.TaskInfo{
		Id:           protocol.TaskID{Id: [16]byte{1}},
		Capabilities: protocol.Capability_All,
		Scope:        protocol.TaskScope{Base: protocol.ScopeBase_Subtree},
		Overrides:    ov,
	}

	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	if got := m.Overrides(); len(got) != 1 || got[0].Caps != protocol.Capability_Cancel {
		t.Fatalf("Overrides() = %+v, want the target's entry carried through", got)
	}

	// A session open with no overrides must not inherit the previous target's.
	m.OpenSession(protocol.Capability_All, protocol.TaskScope{}, nil, nil)
	if got := m.Overrides(); len(got) != 0 {
		t.Errorf("Overrides() = %+v after a fresh open, want empty", got)
	}
}

// mkScope builds a wire scope from the grammar, so these tests name a scope
// the way an operator does rather than assembling flag bits.
func mkScope(t *testing.T, spec string) protocol.TaskScope {
	t.Helper()
	sc, err := cli.ParseScope(spec)
	if err != nil {
		t.Fatalf("ParseScope(%q): %v", spec, err)
	}
	return sc
}

// pickerToggleRow moves the cursor onto the first row of kind k and toggles it.
func pickerToggleRow(t *testing.T, m *AuthorityPickerModel, k pickerRowKind) {
	t.Helper()
	for i, r := range m.rows {
		if r.kind == k {
			m.cursor = i
			m.Toggle()
			return
		}
	}
	t.Fatalf("no row of kind %v in the picker", k)
}

// The picker serialized with a local copy that knew three of the six bases, so
// re-granting a `descendants` task handed self straight back.
func TestPickerPreservesExcludeSelf(t *testing.T) {
	var m AuthorityPickerModel
	m.OpenRegrant(protocol.TaskInfo{
		Id:           protocol.TaskID{Id: [16]byte{1}},
		Capabilities: protocol.Capability_Spawn,
		Scope:        mkScope(t, "descendants"),
	}, nil)

	_, spec, _, _ := m.Result()
	if spec != "descendants" {
		t.Fatalf("re-grant spec = %q, want descendants — the picker gave self back", spec)
	}

	// And it is editable, not merely preserved.
	pickerToggleRow(t, &m, rowExcludeSelf)
	if _, spec, _, _ := m.Result(); spec != "subtree" {
		t.Errorf("after unticking exclude-self, spec = %q, want subtree", spec)
	}
	pickerToggleRow(t, &m, rowExcludeSelf)
	if _, spec, _, _ := m.Result(); spec != "descendants" {
		t.Errorf("after re-ticking, spec = %q, want descendants", spec)
	}
}

// The visibility pair is seeded from the target and must survive an apply that
// does not touch it. It used to be CARRIED because the picker had no control
// for it; now it has one, and "opened it and pressed enter" must still be a
// no-op — which is the property that actually mattered.
func TestPickerSeedsAndPreservesTheVisibilityHalf(t *testing.T) {
	sib := pickerTask(0xbb, "sibling")
	sibHex := FormatTaskID(sib.Id)
	var m AuthorityPickerModel
	m.OpenRegrant(protocol.TaskInfo{
		Id:           protocol.TaskID{Id: [16]byte{2}},
		Capabilities: protocol.Capability_ExecView,
		Scope:        mkScope(t, "global/subtree+vis-ids:"+sibHex),
	}, []protocol.TaskInfo{sib})

	_, spec, _, _ := m.Result()
	sc := mkScope(t, spec)
	if !sc.VisBasePresent() || sc.VisBase != protocol.ScopeBase_Global {
		t.Fatalf("spec %q lost the visibility rank on an untouched apply", spec)
	}
	if len(sc.VisIds) != 1 {
		t.Fatalf("spec %q lost the seeded vis-ids", spec)
	}
}

// The visibility row cycles through FOUR states, because "not stated" is one of
// them and is the default: an unstated rank follows the action base, and the
// zero ScopeBase is itself a legal rank. A three-state control would promote
// every default to an explicit rank the first time the picker was opened.
func TestPickerVisibilityRowCyclesThroughUnstated(t *testing.T) {
	var m AuthorityPickerModel
	m.OpenRegrant(protocol.TaskInfo{
		Id:    protocol.TaskID{Id: [16]byte{4}},
		Scope: mkScope(t, "subtree"),
	}, nil)

	want := []string{"subtree/subtree", "none/subtree", "global/subtree", "subtree"}
	for i, w := range want {
		pickerToggleRow(t, &m, rowVisBase)
		_, spec, _, _ := m.Result()
		if spec != w {
			t.Fatalf("cycle[%d] spec = %q, want %q", i, spec, w)
		}
		if _, err := cli.ParseScope(spec); err != nil {
			t.Fatalf("spec %q does not round-trip: %v", spec, err)
		}
	}
}

// vis-ids are a SECOND id set on the same task rows, reached by their own key.
// Space must keep meaning the action set: a row in one set and not the other is
// the whole point of "+vis-ids:" (see it, cannot act on it).
func TestPickerVisIDsAreASeparateSetFromTheActionIDs(t *testing.T) {
	target := pickerTask(0xaa, "target")
	sib := pickerTask(0xbb, "sibling")
	other := pickerTask(0xcc, "other")
	sibHex, otherHex := FormatTaskID(sib.Id), FormatTaskID(other.Id)

	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target, sib, other})

	moveTo(t, &m, sibHex[:8])
	m.Toggle() // action set
	moveTo(t, &m, otherHex[:8])
	m.ToggleVisID() // see-only set

	_, spec, _, _ := m.Result()
	want := "subtree+ids:" + sibHex + "+vis-ids:" + otherHex
	if spec != want {
		t.Fatalf("spec = %q, want %q", spec, want)
	}
	if _, err := cli.ParseScope(spec); err != nil {
		t.Fatalf("spec %q does not round-trip: %v", spec, err)
	}
}

// The action id set dies at the global rank because `global+ids:` does not
// parse. vis-ids have no such rule — `global/…+vis-ids:` parses, redundantly —
// so raising the visibility rank must NOT quietly drop the clause. Dropping it
// would mean opening such a task and pressing apply narrowed its scope, which
// is exactly the erase this dialog was fixed for once already.
func TestPickerVisIDsSurviveAGlobalVisibilityRank(t *testing.T) {
	target := pickerTask(0xaa, "target")
	sib := pickerTask(0xbb, "sibling")
	sibHex := FormatTaskID(sib.Id)

	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target, sib})
	moveTo(t, &m, sibHex[:8])
	m.ToggleVisID()
	if _, spec, _, _ := m.Result(); spec != "subtree+vis-ids:"+sibHex {
		t.Fatalf("spec = %q, want the vis-id set", spec)
	}

	// Raise visibility to global; the clause stays and the string still parses.
	for i := 0; i < 3; i++ {
		pickerToggleRow(t, &m, rowVisBase)
	}
	_, spec, _, _ := m.Result()
	if !strings.Contains(spec, "vis-ids:"+sibHex) {
		t.Fatalf("spec = %q dropped vis-ids when the rank went global", spec)
	}
	if _, err := cli.ParseScope(spec); err != nil {
		t.Fatalf("spec %q does not round-trip: %v", spec, err)
	}

	// The action set, by contrast, MUST die at global — the grammar has no
	// global+ids form, so keeping it would emit an unparseable string. Select
	// it BEFORE raising the base: once both ranks are global the row is dead
	// and the cursor will not land on it.
	moveTo(t, &m, sibHex[:8])
	m.Toggle()
	pickerToggleRow(t, &m, rowBase)
	pickerToggleRow(t, &m, rowBase) // subtree -> none -> global
	_, actSpec, _, _ := m.Result()
	if strings.Contains(actSpec, "+ids:") {
		t.Fatalf("spec = %q kept action ids under a global action base", actSpec)
	}
	if _, err := cli.ParseScope(actSpec); err != nil {
		t.Fatalf("spec %q does not round-trip: %v", actSpec, err)
	}
}

// Session mode's empty spec means "--scope not named". It must stay empty for
// the plain default and become explicit the moment anything is chosen —
// otherwise ticking exclude-self would silently do nothing on a spawn.
func TestPickerSessionModeEmptyOnlyForTheDefault(t *testing.T) {
	var m AuthorityPickerModel
	m.OpenSession(protocol.Capability_Spawn, protocol.TaskScope{Base: protocol.ScopeBase_Subtree}, nil, nil)
	if _, spec, _, _ := m.Result(); spec != "" {
		t.Errorf("plain default spec = %q, want the empty spawn default", spec)
	}
	pickerToggleRow(t, &m, rowExcludeSelf)
	if _, spec, _, _ := m.Result(); spec != "descendants" {
		t.Errorf("after ticking exclude-self, spec = %q, want descendants", spec)
	}
}

// All six bases must be reachable by driving the two controls, which is the
// property the WebUI copy failed and the picker copy failed with it.
func TestPickerReachesAllSixBases(t *testing.T) {
	seen := map[string]bool{}
	for _, excl := range []bool{false, true} {
		var m AuthorityPickerModel
		m.OpenRegrant(protocol.TaskInfo{Id: protocol.TaskID{Id: [16]byte{3}}}, nil)
		if excl {
			pickerToggleRow(t, &m, rowExcludeSelf)
		}
		// The base row cycles subtree -> none -> global -> subtree.
		for i := 0; i < 3; i++ {
			_, spec, _, _ := m.Result()
			if seen[spec] {
				t.Errorf("spec %q produced twice; the six scopes must be six strings", spec)
			}
			seen[spec] = true
			pickerToggleRow(t, &m, rowBase)
		}
	}
	for _, want := range []string{"subtree", "descendants", "none", "none-self", "global", "global-self"} {
		if !seen[want] {
			t.Errorf("%s is not reachable from the picker's controls", want)
		}
	}
}
