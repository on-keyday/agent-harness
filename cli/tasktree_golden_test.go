package cli

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
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

// TestBuildTaskGrantsAgreesWithEnforcement holds the tree's lines to the sets
// the server gates with. The golden record above pins "same as before"; this
// pins "same as authority.ActionSet", so a change to how a scope is read
// cannot leave the picture disagreeing with the gate.
func TestBuildTaskGrantsAgreesWithEnforcement(t *testing.T) {
	tree := goldenTree()
	idx := map[string][]string{}
	byID := map[string]protocol.TaskInfo{}
	for _, task := range tree {
		h := hex.EncodeToString(task.Id.Id[:])
		byID[h] = task
		if task.CreatorTaskId != (protocol.TaskID{}) {
			p := hex.EncodeToString(task.CreatorTaskId.Id[:])
			idx[p] = append(idx[p], h)
		}
	}
	// The gate is the bit AND the scope (server authorize: hasCap, then inScope).
	acts := func(from string, bit protocol.Capability, to string) bool {
		task := byID[from]
		if task.Capabilities&bit == 0 {
			return false
		}
		all, set := authority.ActionSet(from, authority.ScopeFromWire(task.Scope, task.Overrides), bit, idx)
		return all || set[to]
	}
	eachBit := func(m protocol.Capability, fn func(protocol.Capability)) {
		for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
			if m&bit != 0 {
				fn(bit)
			}
		}
	}

	g := BuildTaskGrants(tree)
	for _, e := range g.Edges {
		eachBit(e.Caps, func(bit protocol.Capability) {
			if !acts(e.From, bit, e.To) {
				t.Errorf("edge %s>%s carries %v, which ActionSet does not grant", e.From[:2], e.To[:2], bit)
			}
		})
	}
	for from, gl := range g.Global {
		task := byID[from]
		eachBit(gl.Caps, func(bit protocol.Capability) {
			if all, _ := authority.ActionSet(from, authority.ScopeFromWire(task.Scope, task.Overrides), bit, idx); !all || task.Capabilities&bit == 0 {
				t.Errorf("global mark on %s carries %v, which ActionSet does not make global", from[:2], bit)
			}
		})
	}
	for child, r := range g.ParentReach {
		c := byID[child]
		parent := hex.EncodeToString(c.CreatorTaskId.Id[:])
		eachBit(byID[parent].Capabilities, func(bit protocol.Capability) {
			if got, want := r.Caps&bit != 0, acts(parent, bit, child); got != want {
				t.Errorf("creator edge %s>%s: %v shown=%v, ActionSet=%v", parent[:2], child[:2], bit, got, want)
			}
		})
	}
}
