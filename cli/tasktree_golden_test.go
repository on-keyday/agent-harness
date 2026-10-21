package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files under testdata/")

// goldenTree exercises every rule BuildTaskGrants resolves: the three bases,
// visibility pairs, overrides with and without ids, exclude_self, ids naming a
// sibling, an ancestor, the task itself and an unlisted task, an orphan, and an
// exec_control-only holder.
func goldenTree() []protocol.TaskInfo {
	scope := func(base protocol.ScopeBase, excludeSelf bool, ids, visIDs []protocol.TaskID) protocol.TaskScope {
		s := protocol.TaskScope{Base: base, Ids: ids, IdsLen: uint16(len(ids)), VisIds: visIDs, VisIdsLen: uint16(len(visIDs))}
		s.SetExcludeSelf(excludeSelf)
		return s
	}
	withVis := func(s protocol.TaskScope, vis protocol.ScopeBase) protocol.TaskScope {
		s.VisBase = vis
		s.SetVisBasePresent(true)
		return s
	}
	ov := func(caps protocol.Capability, base protocol.ScopeBase, ids ...protocol.TaskID) protocol.ScopeOverride {
		return protocol.ScopeOverride{Caps: caps, Base: base, Ids: ids, IdsLen: uint16(len(ids))}
	}

	p := mkTask('p', 0, 10)
	p.Capabilities = protocol.Capability_Cancel | protocol.Capability_FileRead | protocol.Capability_ExecView
	p.Scope = scope(protocol.ScopeBase_Subtree, false, []protocol.TaskID{tid('u')}, []protocol.TaskID{tid('w')})
	p.Overrides = []protocol.ScopeOverride{ov(protocol.Capability_ExecView, protocol.ScopeBase_Global)}

	c := mkTask('c', 'p', 20)
	c.Capabilities = protocol.Capability_Cancel | protocol.Capability_FileRead
	c.Scope = scope(protocol.ScopeBase_None, true, []protocol.TaskID{tid('s'), tid('p')}, nil)

	s := mkTask('s', 'p', 30)
	s.Capabilities = protocol.Capability_ExecControl
	s.Scope = withVis(scope(protocol.ScopeBase_Subtree, false, nil, nil), protocol.ScopeBase_Global)

	g := mkTask('g', 'c', 40)
	g.Capabilities = protocol.Capability_Cancel
	g.Scope = scope(protocol.ScopeBase_None, false, []protocol.TaskID{tid('g'), tid('x')}, nil)

	o := mkTask('o', 'z', 50) // creator 'z' is not listed: an orphan
	o.Capabilities = protocol.Capability_Cancel
	o.Scope = scope(protocol.ScopeBase_Global, false, nil, nil)

	u := mkTask('u', 0, 60)
	u.Capabilities = protocol.Capability_FileRead
	u.Scope = withVis(scope(protocol.ScopeBase_Global, false, nil, nil), protocol.ScopeBase_None)

	w := mkTask('w', 0, 70)
	w.Capabilities = protocol.Capability_Cancel | protocol.Capability_ExecView
	w.Scope = scope(protocol.ScopeBase_Subtree, false, nil, nil)
	w.Overrides = []protocol.ScopeOverride{ov(protocol.Capability_Cancel, protocol.ScopeBase_None, tid('p'))}

	n := mkTask('n', 'w', 80) // holds nothing; its ids only disclose
	n.Scope = scope(protocol.ScopeBase_None, false, []protocol.TaskID{tid('p')}, nil)

	return []protocol.TaskInfo{p, c, s, g, o, u, w, n}
}

// TestBuildTaskGrantsGolden records BuildTaskGrants' output over goldenTree,
// so moving its resolution onto package authority cannot change a single edge,
// label or count. Regenerate with -update only for an intended change.
func TestBuildTaskGrantsGolden(t *testing.T) {
	got, err := json.MarshalIndent(BuildTaskGrants(goldenTree()), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "task_grants_golden.json")
	if *updateGolden {
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run with -update to create it): %v", path, err)
	}
	if string(append(got, '\n')) != string(want) {
		t.Errorf("BuildTaskGrants changed:\n got: %s\nwant: %s", got, want)
	}
}
