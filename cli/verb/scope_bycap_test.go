package verb

import (
	"reflect"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func byCapID(b byte) protocol.TaskID {
	var id protocol.TaskID
	for i := range id.Id {
		id.Id[i] = b
	}
	return id
}

func byCapScope(base protocol.ScopeBase, excludeSelf bool, ids ...protocol.TaskID) protocol.TaskScope {
	s := protocol.TaskScope{Base: base, Ids: ids, IdsLen: uint16(len(ids))}
	s.SetExcludeSelf(excludeSelf)
	return s
}

func byCapOverride(caps protocol.Capability, base protocol.ScopeBase, excludeSelf bool, ids ...protocol.TaskID) protocol.ScopeOverride {
	o := protocol.ScopeOverride{Caps: caps, Base: base, Ids: ids, IdsLen: uint16(len(ids))}
	o.SetExcludeSelf(excludeSelf)
	return o
}

type byCapCase struct {
	name      string
	caps      protocol.Capability
	base      protocol.TaskScope
	overrides []protocol.ScopeOverride
	want      map[string]string
}

func byCapCases() []byCapCase {
	a, b := byCapID(0xaa), byCapID(0x0b)
	gs := byCapScope(protocol.ScopeBase_Subtree, false)
	gs.VisBase = protocol.ScopeBase_Global
	gs.SetVisBasePresent(true)
	return []byCapCase{
		{name: "no overrides", caps: protocol.Capability_Cancel | protocol.Capability_FileRead, base: byCapScope(protocol.ScopeBase_Subtree, false), want: map[string]string{"cancel": "subtree", "file_read": "subtree"}},
		{name: "one override with ids", caps: protocol.Capability_Cancel | protocol.Capability_FileRead,
			base: byCapScope(protocol.ScopeBase_Subtree, false), overrides: []protocol.ScopeOverride{byCapOverride(protocol.Capability_Cancel, protocol.ScopeBase_None, false, a)}, want: map[string]string{"cancel": "ids:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "file_read": "subtree"}},
		{name: "two disjoint overrides", caps: protocol.Capability_Cancel | protocol.Capability_FileRead | protocol.Capability_ExecView,
			base: byCapScope(protocol.ScopeBase_Global, false), overrides: []protocol.ScopeOverride{
				byCapOverride(protocol.Capability_Cancel, protocol.ScopeBase_None, false),
				byCapOverride(protocol.Capability_ExecView, protocol.ScopeBase_Subtree, false, b)}, want: map[string]string{"cancel": "none", "exec_view": "subtree+ids:0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b", "file_read": "global"}},
		{name: "exclude self on base and override", caps: protocol.Capability_Cancel | protocol.Capability_FileRead,
			base: byCapScope(protocol.ScopeBase_Subtree, true), overrides: []protocol.ScopeOverride{byCapOverride(protocol.Capability_Cancel, protocol.ScopeBase_None, true, a)}, want: map[string]string{"cancel": "none-self+ids:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "file_read": "descendants"}},
		{name: "visibility pair", caps: protocol.Capability_Cancel, base: gs, want: map[string]string{"cancel": "subtree"}},
		{name: "ids out of order", caps: protocol.Capability_Cancel, base: byCapScope(protocol.ScopeBase_None, false, a, b), want: map[string]string{"cancel": "ids:0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b,aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
	}
}

// TestResolvedScopeByCapUnchanged records what ResolvedScopeByCap rendered
// before it moved onto package authority, so the move cannot change a label.
func TestResolvedScopeByCapUnchanged(t *testing.T) {
	for _, c := range byCapCases() {
		if got := ResolvedScopeByCap(c.caps, c.base, c.overrides); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
