package verb

import (
	"fmt"
	"strings"
)

// The grid mode enum is grammar: it is what `--under` and `--descendants`
// parse INTO, and ParseGridArgs beside it cannot name its result without it.
// It was split out of cli/gridset.go, which keeps the other half -- GridSet
// and TaskScopeIDs, the selection over protocol.TaskInfo that CONSUMES a mode.
// That file's own comment already drew this line ("the grammar itself lives in
// cli.ParseGridArgs"); this move makes the package boundary agree with it,
// rather than dragging task-selection logic into a parse-only package.

// GridScopeMode names how a grid chose its tasks. The string values are the
// wire/CLI spelling, shared by the TUI cmdline verb and the WebUI's grid
// command so one grammar covers both.
type GridScopeMode string

const (
	// GridAll is every task the operator can see — the unnarrowed grid.
	GridAll GridScopeMode = "all"
	// GridSubtree is an anchor's working set: itself, every task it spawned
	// transitively, AND the tasks its own scope names individually.
	//
	// Both halves, because either alone is a half-answer. The creator tree
	// says what the task STARTED; a scope's `ids:` names peers it was handed
	// that are nobody's descendant — which is the whole reason they had to be
	// named. A supervisor working with a task it did not spawn is exactly the
	// case a subtree-only grid cannot show.
	GridSubtree GridScopeMode = "subtree"
	// GridDescendants is the same set with the anchor itself left out: for
	// when that one session is already on screen somewhere else and its
	// workers are what is missing.
	GridDescendants GridScopeMode = "descendants"
	// GridIds is exactly the tasks named, in the order they were named. It
	// never expands a subtree — the caller enumerated, so enumeration is the
	// answer (`--scope ids:` draws the same line).
	GridIds GridScopeMode = "ids"
)

// ParseGridScopeMode converts a mode string from a command line or the wasm
// bridge. An unknown mode is an error rather than a silent fallback to "all":
// a typo must not quietly widen the view to the whole fleet.
func ParseGridScopeMode(s string) (GridScopeMode, error) {
	switch m := GridScopeMode(strings.TrimSpace(s)); m {
	case GridAll, GridSubtree, GridDescendants, GridIds:
		return m, nil
	default:
		return "", fmt.Errorf("unknown grid mode %q (valid: %s)", s, GridScopeModes)
	}
}

// GridScopeModes is the one-line syntax summary, shared by flag help and error
// messages.
const GridScopeModes = "all | subtree | descendants | ids"
