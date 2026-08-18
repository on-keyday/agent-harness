package cli

import (
	"encoding/hex"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TaskTreeRow is one task placed in the creator tree, flattened into the order
// a renderer draws it in.
//
// Flattened rather than nested on purpose: the CLI, the TUI table and the WebUI
// list all draw "rows in order", so handing each of them a nested structure
// would make all three walk the tree themselves — three walks that can and will
// disagree. One walk here, three dumb renderers.
type TaskTreeRow struct {
	Task  protocol.TaskInfo
	Depth int
	// IsLast[d] reports whether the row is the last child at depth d. It is
	// what decides └─ versus ├─ at the row's own level, and whether each
	// ancestor column still needs a │ drawn through it.
	IsLast []bool
	// Orphan marks a task whose creator is not in the input set — pruned, or
	// outside the caller's scope. Such a task is shown at root rather than
	// hidden: a tree view re-orders a listing, it never filters one.
	Orphan bool
}

// BuildTaskTree arranges tasks under their creators.
//
// Roots are the operator-created tasks (zero creator id) plus any orphan.
// Siblings are ordered by CreatedAt, then by task id so the order is total and
// stable across polls — an unstable order under a 5s refresh moves rows under
// the reader's cursor.
//
// Every input task comes back exactly once. The visited set that guarantees it
// also makes a creator cycle terminate; the server refuses to build one
// (SetParentStatus.would_cycle), so that is defence against a broken record
// replayed from the WAL rather than against normal operation.
func BuildTaskTree(tasks []protocol.TaskInfo) []TaskTreeRow {
	if len(tasks) == 0 {
		return nil
	}

	hexID := func(t protocol.TaskID) string { return hex.EncodeToString(t.Id[:]) }
	zero := protocol.TaskID{}

	present := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		present[hexID(t.Id)] = true
	}

	children := make(map[string][]protocol.TaskInfo, len(tasks))
	var roots []protocol.TaskInfo
	orphan := make(map[string]bool)
	for _, t := range tasks {
		parent := hexID(t.CreatorTaskId)
		switch {
		case t.CreatorTaskId == zero:
			roots = append(roots, t)
		case !present[parent]:
			// Creator gone or invisible: surface at root, flagged.
			orphan[hexID(t.Id)] = true
			roots = append(roots, t)
		default:
			children[parent] = append(children[parent], t)
		}
	}

	byCreation := func(s []protocol.TaskInfo) {
		sort.SliceStable(s, func(i, j int) bool {
			if s[i].CreatedAt != s[j].CreatedAt {
				return s[i].CreatedAt < s[j].CreatedAt
			}
			return hexID(s[i].Id) < hexID(s[j].Id)
		})
	}
	byCreation(roots)
	for k := range children {
		byCreation(children[k])
	}

	out := make([]TaskTreeRow, 0, len(tasks))
	visited := make(map[string]bool, len(tasks))

	var walk func(t protocol.TaskInfo, isLast []bool)
	walk = func(t protocol.TaskInfo, isLast []bool) {
		id := hexID(t.Id)
		if visited[id] {
			return
		}
		visited[id] = true
		out = append(out, TaskTreeRow{
			Task: t,
			// Depth is len(isLast) rather than a separate counter so the two
			// can never disagree about where the row sits.
			Depth:  len(isLast),
			IsLast: append([]bool(nil), isLast...),
			Orphan: orphan[id],
		})
		kids := children[id]
		for i, k := range kids {
			walk(k, append(isLast, i == len(kids)-1))
		}
	}
	for _, r := range roots {
		walk(r, nil)
	}

	// A cycle leaves its members unreachable from any root. They are still
	// tasks the caller can see, so they go at the end rather than vanishing.
	for _, t := range tasks {
		if !visited[hexID(t.Id)] {
			visited[hexID(t.Id)] = true
			out = append(out, TaskTreeRow{Task: t, Depth: 0, Orphan: true})
		}
	}
	return out
}

// TaskSubtree returns anchorHex's task plus every task transitively created by
// it, in the order BuildTaskTree places them (the anchor, then its descendants
// pre-order). An anchor that is not in tasks yields nil — a caller that
// mistyped an id must get nothing back, never the unfiltered set.
//
// It reads BuildTaskTree's output rather than walking the creator links a
// second time: those rows are a pre-order walk, so a node's descendants are
// exactly the rows that follow it until the depth drops back to its own. That
// is the same property TaskTreeLayout recovers parents from, and it keeps the
// hierarchy defined in ONE place — a viewer filtering by its own walk could
// disagree with the tree drawn next to it about who is whose child.
//
// Two consequences of reusing that walk, both wanted: an orphan (creator
// pruned or out of scope) is a root here too and keeps its own children, and a
// task caught in a creator cycle — which the server refuses to build but a
// replayed WAL record could carry — is parked at depth 0 by BuildTaskTree and
// so comes back alone instead of dragging in whatever follows it.
func TaskSubtree(tasks []protocol.TaskInfo, anchorHex string) []protocol.TaskInfo {
	anchor := strings.ToLower(strings.TrimSpace(anchorHex))
	if anchor == "" {
		return nil
	}
	rows := BuildTaskTree(tasks)
	for i, r := range rows {
		if hex.EncodeToString(r.Task.Id.Id[:]) != anchor {
			continue
		}
		out := []protocol.TaskInfo{r.Task}
		for _, d := range rows[i+1:] {
			if d.Depth <= r.Depth {
				break
			}
			out = append(out, d.Task)
		}
		return out
	}
	return nil
}

// TreePrefix renders the ├─ / └─ / │ gutter for a row. Depth 0 yields "".
//
// Single implementation shared by every surface: the CLI writes it into a text
// line, the TUI into its Task column, and the WebUI into a monospace span, so
// the three cannot drift into three different-looking trees.
func TreePrefix(row TaskTreeRow) string {
	if len(row.IsLast) == 0 {
		return ""
	}
	out := make([]rune, 0, len(row.IsLast)*3)
	for _, last := range row.IsLast[:len(row.IsLast)-1] {
		if last {
			out = append(out, ' ', ' ', ' ')
		} else {
			out = append(out, '│', ' ', ' ')
		}
	}
	if row.IsLast[len(row.IsLast)-1] {
		out = append(out, '└', '─', ' ')
	} else {
		out = append(out, '├', '─', ' ')
	}
	return string(out)
}

// TaskTreeNodePos is one node placed on a grid: Depth is the row, Col is the
// horizontal slot. Both are UNITLESS — the renderer multiplies them by whatever
// pixel spacing it wants. Keeping the geometry here rather than in the WebUI's
// JavaScript is the same call BuildTaskTree makes: it is the half that can be
// wrong in interesting ways (overlapping siblings, a parent off-centre), and Go
// is where this repo can test it.
type TaskTreeNodePos struct {
	ID     string
	Parent string // "" for a root, and for an orphan whose creator is off-screen
	Depth  int
	Col    float64
	Orphan bool
}

// TaskTreeLayout assigns grid positions to the rows BuildTaskTree produced.
//
// Leaves take consecutive columns in tree order; every other node centres over
// its own children. That is the classic tidy-tree assignment, and it is chosen
// over "indent by depth" because a diagram whose parents sit above the middle
// of their subtree reads as a hierarchy, while a left-aligned one reads as a
// ladder.
//
// Orphans are roots for layout purposes and carry no Parent, so the renderer
// never draws an edge toward a node that is not on screen.
func TaskTreeLayout(rows []TaskTreeRow) []TaskTreeNodePos {
	if len(rows) == 0 {
		return nil
	}
	// rows is a pre-order walk, so a node's children are exactly the following
	// rows at depth+1 until the depth drops back. Recovering the parent from
	// that is cheaper than re-deriving it from creator ids, and it cannot
	// disagree with the tree that was already built.
	parentOf := make(map[int]int, len(rows)) // row index -> parent row index, -1 for roots
	stack := []int{}
	for i, r := range rows {
		for len(stack) > r.Depth {
			stack = stack[:len(stack)-1]
		}
		if r.Depth == 0 || len(stack) == 0 {
			parentOf[i] = -1
		} else {
			parentOf[i] = stack[len(stack)-1]
		}
		stack = append(stack, i)
	}
	children := make(map[int][]int, len(rows))
	for i := range rows {
		if p := parentOf[i]; p >= 0 {
			children[p] = append(children[p], i)
		}
	}

	cols := make([]float64, len(rows))
	next := 0.0
	// Post-order: a node's column needs its children's, so they must be placed
	// first. Iterative to avoid a deep recursion on a pathological chain.
	var place func(i int)
	place = func(i int) {
		kids := children[i]
		if len(kids) == 0 {
			cols[i] = next
			next++
			return
		}
		for _, k := range kids {
			place(k)
		}
		cols[i] = (cols[kids[0]] + cols[kids[len(kids)-1]]) / 2
	}
	for i := range rows {
		if parentOf[i] == -1 {
			place(i)
		}
	}

	out := make([]TaskTreeNodePos, 0, len(rows))
	for i, r := range rows {
		parent := ""
		if p := parentOf[i]; p >= 0 {
			parent = hex.EncodeToString(rows[p].Task.Id.Id[:])
		}
		out = append(out, TaskTreeNodePos{
			ID:     hex.EncodeToString(r.Task.Id.Id[:]),
			Parent: parent,
			Depth:  r.Depth,
			Col:    cols[i],
			Orphan: r.Orphan,
		})
	}
	return out
}
