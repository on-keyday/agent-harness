package cli

import (
	"encoding/hex"
	"sort"

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
