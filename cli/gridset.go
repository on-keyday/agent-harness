package cli

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A session-viewer grid answers one question — WHICH sessions am I looking at —
// and it is reachable from a key, a button and two command lines. This file is
// that question's single answer: the set, and the label naming it.
//
// The alternative shipped first and was wrong: each entry point filtered for
// itself, so the TUI and the WebUI could disagree about who is whose child, and
// the label was formatted twice in two languages. Everything here is decided in
// Go, and the wasm bridge hands JS the finished pair (cmd/harness-webui-wasm's
// gridSet), the same division TaskTreeLayout draws for the tree diagram.

// shortID is the 8-hex form every grid surface labels a task with — the pane
// headers, the tree gutter's `by=`, and the scope labels below.
func shortID(hexID string) string {
	if len(hexID) > 8 {
		return hexID[:8]
	}
	return hexID
}

// TaskScopeIDs returns the tasks that taskHex's scope names INDIVIDUALLY —
// the `ids:` half of its TaskScope, resolved against the visible task set.
//
// It is the half the creator tree cannot reach. A scope names a task by id
// precisely when that task is not in the holder's subtree, so a supervisor's
// granted peers are invisible to any walk of who-spawned-whom. Reading the
// stored scope is also why there is no scope grammar for an operator to type:
// `subtree` / `ids:` mean what they mean ON A TASK, and a task is what is
// being asked about here.
//
// A named id the operator cannot see (pruned since the grant, or outside their
// own visibility) is skipped: there is no session to tile for it, and a
// placeholder row would claim something is there. Returns nil when taskHex
// itself is not in tasks.
func TaskScopeIDs(tasks []protocol.TaskInfo, taskHex string) []protocol.TaskInfo {
	want := strings.ToLower(strings.TrimSpace(taskHex))
	var holder *protocol.TaskInfo
	byID := make(map[string]protocol.TaskInfo, len(tasks))
	for i, t := range tasks {
		id := hex.EncodeToString(t.Id.Id[:])
		byID[id] = t
		if id == want {
			holder = &tasks[i]
		}
	}
	if holder == nil {
		return nil
	}
	out := make([]protocol.TaskInfo, 0, len(holder.Scope.Ids))
	seen := make(map[string]bool, len(holder.Scope.Ids))
	for _, id := range holder.Scope.Ids {
		h := hex.EncodeToString(id.Id[:])
		if seen[h] {
			continue
		}
		seen[h] = true
		if t, ok := byID[h]; ok {
			out = append(out, t)
		}
	}
	return out
}

// anchorSet is the shared body of the subtree and descendants modes: the
// anchor's subtree followed by the tasks its scope names, deduped, plus the
// label suffix that says whether the second half contributed anything.
//
// The suffix is conditional on purpose. "+desc" on a task with no granted
// peers is the whole truth; printing "+ids×0" would be noise on the common
// case, and printing nothing when there ARE granted peers would hide why a
// pane appeared that is nobody's child.
func anchorSet(tasks []protocol.TaskInfo, anchor string) ([]protocol.TaskInfo, string) {
	out := TaskSubtree(tasks, anchor)
	seen := make(map[string]bool, len(out))
	for _, t := range out {
		seen[hex.EncodeToString(t.Id.Id[:])] = true
	}
	named := 0
	for _, t := range TaskScopeIDs(tasks, anchor) {
		if seen[hex.EncodeToString(t.Id.Id[:])] {
			continue // already in the subtree; a scope may name a descendant
		}
		out = append(out, t)
		named++
	}
	suffix := "+desc"
	if named > 0 {
		suffix = fmt.Sprintf("+desc+ids×%d", named)
	}
	return out, suffix
}

// GridSet picks the tasks a grid should consider and returns the label naming
// that choice, so every surface shows the same words for the same set.
//
// It answers WHICH TASKS, never which of them can be tiled: a pane needs a live
// interactive session, and that predicate belongs to the viewer (the TUI's
// gridLiveTasks, the WebUI's liveInteractiveTasks, which also subtracts the
// per-session toggles). Keeping the two apart is what lets a caller say "the
// subtree has four tasks and none of them is watchable" instead of an
// undifferentiated empty.
//
// An anchor or a named id the operator cannot see is an ERROR, not an empty
// set: those ids were typed, and a typo that silently shows nothing is the
// failure mode this repo keeps fixing. A legitimately empty result — the
// descendants of a task that has none — is not an error; the caller reports it.
func GridSet(tasks []protocol.TaskInfo, mode GridScopeMode, anchorHex string, idHexes []string) ([]protocol.TaskInfo, string, error) {
	anchor := strings.ToLower(strings.TrimSpace(anchorHex))
	present := make(map[string]protocol.TaskInfo, len(tasks))
	for _, t := range tasks {
		present[hex.EncodeToString(t.Id.Id[:])] = t
	}
	needAnchor := func() error {
		if anchor == "" {
			return fmt.Errorf("grid %s: needs a task id to anchor on", mode)
		}
		if _, ok := present[anchor]; !ok {
			return fmt.Errorf("grid %s: task %s is not in the visible task set", mode, shortID(anchor))
		}
		return nil
	}

	switch mode {
	case GridAll:
		out := make([]protocol.TaskInfo, len(tasks))
		copy(out, tasks)
		// Stable so two opens of an unnarrowed grid agree on pane order; the
		// viewer re-sorts by activity, this only removes map-order noise.
		sort.SliceStable(out, func(i, j int) bool {
			return hex.EncodeToString(out[i].Id.Id[:]) < hex.EncodeToString(out[j].Id.Id[:])
		})
		return out, "all", nil

	case GridSubtree:
		if err := needAnchor(); err != nil {
			return nil, "", err
		}
		set, suffix := anchorSet(tasks, anchor)
		return set, shortID(anchor) + suffix, nil

	case GridDescendants:
		if err := needAnchor(); err != nil {
			return nil, "", err
		}
		// anchorSet leads with the anchor (TaskSubtree does), so the rest is
		// the tail. Dropping it here rather than inside keeps "the subtree"
		// meaning the subtree for TaskSubtree's other callers.
		set, suffix := anchorSet(tasks, anchor)
		if len(set) > 0 {
			set = set[1:]
		}
		return set, shortID(anchor) + strings.Replace(suffix, "+desc", "/desc-only", 1), nil

	case GridIds:
		if len(idHexes) == 0 {
			return nil, "", fmt.Errorf("grid ids: no task ids given")
		}
		out := make([]protocol.TaskInfo, 0, len(idHexes))
		var missing []string
		seen := make(map[string]bool, len(idHexes))
		for _, raw := range idHexes {
			id := strings.ToLower(strings.TrimSpace(raw))
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			t, ok := present[id]
			if !ok {
				missing = append(missing, shortID(id))
				continue
			}
			out = append(out, t)
		}
		if len(missing) > 0 {
			return nil, "", fmt.Errorf("grid ids: not in the visible task set: %s", strings.Join(missing, ", "))
		}
		return out, fmt.Sprintf("ids×%d", len(out)), nil

	default:
		return nil, "", fmt.Errorf("unknown grid mode %q (valid: %s)", string(mode), GridScopeModes)
	}
}
