package cli

import (
	"encoding/hex"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/authority"
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

// TreePrefix renders the ├─ / └─ / │ gutter for a row. An empty isLast
// (depth 0) yields "".
//
// Single implementation shared by every surface: the CLI writes it into a text
// line, the TUI into its Task column, and the WebUI into a monospace span, so
// the three cannot drift into three different-looking trees.
//
// It takes the []bool it actually reads rather than a TaskTreeRow, so
// ThreadRow — whose rows carry the same per-level last-child flags for a
// reply chain — draws an identical gutter without either type knowing about
// the other.
func TreePrefix(isLast []bool) string {
	if len(isLast) == 0 {
		return ""
	}
	out := make([]rune, 0, len(isLast)*3)
	for _, last := range isLast[:len(isLast)-1] {
		if last {
			out = append(out, ' ', ' ', ' ')
		} else {
			out = append(out, '│', ' ', ' ')
		}
	}
	if isLast[len(isLast)-1] {
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

// GrantKind ranks what an id-named grant lets its holder do to the target.
// The order is the rank: when one task names another through several clauses,
// the edge carries the strongest.
type GrantKind int

const (
	// GrantView: the target is visible and nothing more — a vis-ids entry, or
	// an action id under a mask that holds no bit.
	GrantView GrantKind = iota + 1
	// GrantOverride: only the capabilities of the --scope-for overrides that
	// name the target reach it.
	GrantOverride
	// GrantAction: the base scope's ids, so every bit the task holds outside
	// its overrides reaches the target.
	GrantAction
)

func (k GrantKind) String() string {
	switch k {
	case GrantView:
		return "view"
	case GrantOverride:
		return "override"
	case GrantAction:
		return "action"
	}
	return ""
}

// TaskGrantEdge is one task naming another by id in its scope.
type TaskGrantEdge struct {
	From, To string
	Kind     GrantKind
	// Caps is every bit that reaches To through an id clause; 0 for GrantView.
	Caps protocol.Capability
}

// TaskGlobalGrant is a task whose scope reaches every task through a `global`
// base. It is a property of the node, not an edge: it has no target to draw to.
type TaskGlobalGrant struct {
	Kind GrantKind
	Caps protocol.Capability // 0 for GrantView
}

// TaskGrants is the authority the creator tree does NOT show: what a task
// reaches beyond its own subtree, by naming ids or by a global base.
type TaskGrants struct {
	// Edges are sorted by From, then To, so the drawing is stable across polls.
	Edges []TaskGrantEdge
	// Offscreen counts, per From, the edges whose target is not in the input
	// set (pruned). They are counted rather than dropped so a grant the
	// diagram cannot place is still said to exist.
	Offscreen map[string]int
	Global    map[string]TaskGlobalGrant
	// ParentReach is, per child id, what its direct creator reaches of it —
	// what the creator edge carries. Only children whose creator is in the
	// input set have an entry. One level only: a grandparent's subtree reaches
	// the child too, and that is the grandparent's edge, not this one.
	ParentReach map[string]TaskReach
}

// TaskReach is what one task reaches of another.
type TaskReach struct {
	Caps protocol.Capability
	// Visible: the target is in the holder's visibility set. Caps can be
	// non-zero while this is false (`scope=none +exec_view:global` acts on
	// what it is handed but lists nothing).
	Visible bool
}

// creatorReach resolves what t reaches of target, a task in t's subtree,
// through package authority: per literally held bit, whether ActionSet covers
// the target, and whether VisibleSet does. The index needs only the one edge
// from t to target to put target in t's subtree.
func creatorReach(t protocol.TaskInfo, target protocol.TaskID) TaskReach {
	s := authority.ScopeFromWire(t.Scope, t.Overrides)
	from, to := hex.EncodeToString(t.Id.Id[:]), hex.EncodeToString(target.Id[:])
	children := map[string][]string{from: {to}}
	var r TaskReach
	for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
		if t.Capabilities&bit == 0 {
			continue
		}
		if all, set := authority.ActionSet(from, s, bit, children); all || set[to] {
			r.Caps |= bit
		}
	}
	all, vis := authority.VisibleSet(from, s, children)
	r.Visible = all || vis[to]
	return r
}

// BuildTaskGrants reads the id clauses and global bases out of each task's
// scope and overrides.
//
// The capability a clause carries follows scopeForCap: an override's ids get
// that override's mask, the base scope's ids get every held bit no override
// claims. An id reached by a clause whose bits are all zero is still DISCLOSED
// to the task (message.bgn, ScopeOverride), so it becomes a view edge rather
// than vanishing.
//
// An edge that a global grant of the same task already covers is dropped: a
// line to one target says nothing a global marker does not, and drawing it
// would suggest the id were the reason for the reach. The view edge is the
// exception that proves it: under a global ACTION reach an id clause still adds
// VISIBILITY unless the visibility rank is global too, so it stays, as a view.
func BuildTaskGrants(tasks []protocol.TaskInfo) TaskGrants {
	hexID := func(t protocol.TaskID) string { return hex.EncodeToString(t.Id[:]) }
	present := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		present[hexID(t.Id)] = true
	}

	out := TaskGrants{Offscreen: map[string]int{}, Global: map[string]TaskGlobalGrant{},
		ParentReach: map[string]TaskReach{}}
	byID := make(map[string]protocol.TaskInfo, len(tasks))
	for _, t := range tasks {
		byID[hexID(t.Id)] = t
	}
	for _, t := range tasks {
		from := hexID(t.Id)
		if p, ok := byID[hexID(t.CreatorTaskId)]; ok && t.CreatorTaskId != (protocol.TaskID{}) && p.Id != t.Id {
			out.ParentReach[from] = creatorReach(p, t.Id)
		}
		held := t.Capabilities
		s := authority.ScopeFromWire(t.Scope, t.Overrides)
		var overMask protocol.Capability
		for _, o := range s.Overrides {
			overMask |= o.Caps
		}

		// Per held bit, the clause authority resolves it through: the base
		// scope's (an action clause) or an override's. What that clause names
		// is what the bit carries; a global clause reaches every task.
		type reach struct {
			action, override protocol.Capability
		}
		targets := map[string]*reach{}
		at := func(k string) *reach {
			if targets[k] == nil {
				targets[k] = &reach{}
			}
			return targets[k]
		}
		var globalAction, globalOverride protocol.Capability
		for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
			if held&bit == 0 {
				continue
			}
			base, _, ids := s.ForCap(bit)
			override := overMask&bit != 0
			if base == protocol.ScopeBase_Global {
				if override {
					globalOverride |= bit
				} else {
					globalAction |= bit
				}
			}
			for _, id := range ids {
				if override {
					at(id).override |= bit
				} else {
					at(id).action |= bit
				}
			}
		}
		visGlobal := s.VisRank() == protocol.ScopeBase_Global
		globalCaps := globalAction | globalOverride
		switch {
		case globalAction != 0:
			out.Global[from] = TaskGlobalGrant{Kind: GrantAction, Caps: globalCaps}
		case globalOverride != 0:
			out.Global[from] = TaskGlobalGrant{Kind: GrantOverride, Caps: globalCaps}
		case visGlobal:
			out.Global[from] = TaskGlobalGrant{Kind: GrantView}
		}

		// Every id a clause names is disclosed to the task even when no held
		// bit reaches it through that clause (message.bgn, ScopeOverride), so
		// each becomes at least a view target.
		for _, id := range s.IDs {
			at(id)
		}
		for _, o := range s.Overrides {
			for _, id := range o.IDs {
				at(id)
			}
		}
		for _, id := range s.VisIDs {
			at(id)
		}
		delete(targets, from) // self is always in reach; not a grant

		for to, r := range targets {
			action := r.action &^ globalCaps
			caps := (r.action | r.override) &^ globalCaps
			var kind GrantKind
			switch {
			case action != 0:
				kind = GrantAction
			case caps != 0:
				kind = GrantOverride
			case !visGlobal:
				kind = GrantView
			default:
				continue
			}
			if !present[to] {
				out.Offscreen[from]++
				continue
			}
			out.Edges = append(out.Edges, TaskGrantEdge{From: from, To: to, Kind: kind, Caps: caps})
		}
	}
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].From != out.Edges[j].From {
			return out.Edges[i].From < out.Edges[j].From
		}
		return out.Edges[i].To < out.Edges[j].To
	})
	return out
}
