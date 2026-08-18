package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func layoutOf(tasks []protocol.TaskInfo) map[byte]TaskTreeNodePos {
	out := map[byte]TaskTreeNodePos{}
	for _, n := range TaskTreeLayout(BuildTaskTree(tasks)) {
		// Tests key by the single distinguishing byte mkTask writes.
		b, _ := hexByte(n.ID)
		out[b] = n
	}
	return out
}

// hexByte recovers the leading byte the test fixtures encode in the id.
func hexByte(idHex string) (byte, bool) {
	if len(idHex) < 2 {
		return 0, false
	}
	var v byte
	for i := 0; i < 2; i++ {
		c := idHex[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | (c - '0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | (c - 'a' + 10)
		default:
			return 0, false
		}
	}
	return v, true
}

// TestTaskTreeLayout_DepthIsTheRow: depth maps straight to the vertical band,
// which is what makes "who spawned whom" readable at a glance.
func TestTaskTreeLayout_DepthIsTheRow(t *testing.T) {
	pos := layoutOf([]protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
		mkTask('d', 'b', 30),
	})
	for id, want := range map[byte]int{'a': 0, 'b': 1, 'd': 2} {
		if got := pos[id].Depth; got != want {
			t.Errorf("%c: Depth = %d, want %d", id, got, want)
		}
	}
}

// TestTaskTreeLayout_LeavesGetDistinctColumns: siblings must not overlap, or
// the diagram draws two nodes on top of each other.
func TestTaskTreeLayout_LeavesGetDistinctColumns(t *testing.T) {
	pos := layoutOf([]protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
		mkTask('c', 'a', 30),
		mkTask('d', 'a', 40),
	})
	seen := map[float64]byte{}
	for _, id := range []byte{'b', 'c', 'd'} {
		col := pos[id].Col
		if prev, dup := seen[col]; dup {
			t.Errorf("%c and %c share column %v", prev, id, col)
		}
		seen[col] = id
	}
}

// TestTaskTreeLayout_ParentCentersOverItsChildren is what makes the picture
// read as a hierarchy rather than a left-aligned ladder.
func TestTaskTreeLayout_ParentCentersOverItsChildren(t *testing.T) {
	pos := layoutOf([]protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
		mkTask('c', 'a', 30),
	})
	mid := (pos['b'].Col + pos['c'].Col) / 2
	if pos['a'].Col != mid {
		t.Errorf("parent Col = %v, want %v (midpoint of its children)", pos['a'].Col, mid)
	}
}

// TestTaskTreeLayout_SeparateRootsDoNotOverlap: two operator-created roots and
// their subtrees must occupy disjoint horizontal space.
func TestTaskTreeLayout_SeparateRootsDoNotOverlap(t *testing.T) {
	pos := layoutOf([]protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
		mkTask('x', 0, 30),
		mkTask('y', 'x', 40),
	})
	if !(pos['b'].Col < pos['y'].Col) {
		t.Errorf("subtrees overlap: b at %v, y at %v", pos['b'].Col, pos['y'].Col)
	}
	if !(pos['a'].Col < pos['x'].Col) {
		t.Errorf("roots not ordered: a at %v, x at %v", pos['a'].Col, pos['x'].Col)
	}
}

// TestTaskTreeLayout_CarriesParentAndOrphan: the renderer draws edges from
// Parent and marks orphans, so both have to survive the layout pass.
func TestTaskTreeLayout_CarriesParentAndOrphan(t *testing.T) {
	rows := BuildTaskTree([]protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
		mkTask('z', 'q', 30), // creator not in the set
	})
	nodes := TaskTreeLayout(rows)
	byB := map[byte]TaskTreeNodePos{}
	for _, n := range nodes {
		b, _ := hexByte(n.ID)
		byB[b] = n
	}
	if byB['a'].Parent != "" {
		t.Errorf("root carries Parent %q, want empty", byB['a'].Parent)
	}
	if byB['b'].Parent == "" {
		t.Error("child lost its Parent — no edge would be drawn")
	}
	if !byB['z'].Orphan {
		t.Error("orphan flag lost in layout")
	}
	if byB['z'].Parent != "" {
		t.Errorf("orphan carries Parent %q: it would draw an edge to a node that is not on screen", byB['z'].Parent)
	}
	if len(nodes) != 3 {
		t.Errorf("layout returned %d nodes for 3 tasks", len(nodes))
	}
}
