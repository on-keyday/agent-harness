package cli

import (
	"flag"
	"io"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The grammar primitives moved down into cli/verb: package cli parses the
// board family itself (cmd_board.go), so cli must be free to import verb and
// never the reverse -- otherwise declaring `board` closes an import cycle.
//
// These re-exports exist because roughly thirty call sites across
// cmd/harness-cli, cli, cli/agent and tui name these symbols, and relocating a
// function is not a reason to touch all of them in the same commit. They are
// aliases and one-line forwards, never reimplementations: there is still one
// definition of each.

// ParsePermuted parses fs tolerating flags written after positionals.
func ParsePermuted(fs *flag.FlagSet, args []string) ([]string, error) {
	return verb.ParsePermuted(fs, args)
}

// --- caps ---

// CapInfo describes one grantable capability.
type CapInfo = verb.CapInfo

// CapsFlagUsage is the --caps flag's help text.
const CapsFlagUsage = verb.CapsFlagUsage

// ParseCaps parses a comma-separated capability mask.
func ParseCaps(s string) (protocol.Capability, error) { return verb.ParseCaps(s) }

// GrantableCaps lists the capability values a --caps flag may name.
func GrantableCaps() []protocol.Capability { return verb.GrantableCaps() }

// CapDescription summarises what one capability authorizes.
func CapDescription(c protocol.Capability) string { return verb.CapDescription(c) }

// CapsCatalog is the grantable capability catalog.
func CapsCatalog() []CapInfo { return verb.CapsCatalog() }

// CapNames renders capability values as their names.
func CapNames(caps []protocol.Capability) []string { return verb.CapNames(caps) }

// CapsLabel renders a capability mask as a label.
func CapsLabel(c protocol.Capability) string { return verb.CapsLabel(c) }

// WriteCaps prints the capability catalog.
func WriteCaps(w io.Writer, asJSON bool) error { return verb.WriteCaps(w, asJSON) }

// --- scope ---

// ScopeInfo describes one scope form.
type ScopeInfo = verb.ScopeInfo

// ScopeGrammar is the one-line --scope syntax summary.
const ScopeGrammar = verb.ScopeGrammar

// ScopeForFlagUsage is the --scope-for flag's help text.
const ScopeForFlagUsage = verb.ScopeForFlagUsage

// ParseScope parses a --scope spec.
func ParseScope(str string) (protocol.TaskScope, error) { return verb.ParseScope(str) }

// ScopeLabel renders a scope as a label.
func ScopeLabel(s protocol.TaskScope) string { return verb.ScopeLabel(s) }

// ScopeSpec renders scope parts back into a spec string.
func ScopeSpec(base string, excludeSelf bool, ids []string, visBase string, visIds []string) (string, error) {
	return verb.ScopeSpec(base, excludeSelf, ids, visBase, visIds)
}

// ScopesCatalog is the scope-form catalog.
func ScopesCatalog() []ScopeInfo { return verb.ScopesCatalog() }

// ParseScopeFor parses one --scope-for override.
func ParseScopeFor(str string) (protocol.Capability, protocol.ScopeOverride, error) {
	return verb.ParseScopeFor(str)
}

// MergeScopeOverride folds one override into a list.
func MergeScopeOverride(in []protocol.ScopeOverride, ov protocol.ScopeOverride) ([]protocol.ScopeOverride, error) {
	return verb.MergeScopeOverride(in, ov)
}

// OverridesLabel renders per-capability overrides as a label.
func OverridesLabel(in []protocol.ScopeOverride) string { return verb.OverridesLabel(in) }

// ResolvedScopeByCap maps each granted capability to its effective scope.
func ResolvedScopeByCap(caps protocol.Capability, base protocol.TaskScope, overrides []protocol.ScopeOverride) map[string]string {
	return verb.ResolvedScopeByCap(caps, base, overrides)
}

// --- grid ---

// GridScopeMode names how a grid chose its tasks.
type GridScopeMode = verb.GridScopeMode

const (
	// GridAll is every task the operator can see.
	GridAll = verb.GridAll
	// GridSubtree is an anchor's working set.
	GridSubtree = verb.GridSubtree
	// GridDescendants is that set without the anchor.
	GridDescendants = verb.GridDescendants
	// GridIds is exactly the tasks named.
	GridIds = verb.GridIds
)

// GridScopeModes is the one-line grid-mode syntax summary.
const GridScopeModes = verb.GridScopeModes

// GridArgsUsage is the one-line `grid` usage shared by every surface.
const GridArgsUsage = verb.GridArgsUsage

// ParseGridScopeMode converts a mode string from a command line or the bridge.
func ParseGridScopeMode(s string) (GridScopeMode, error) { return verb.ParseGridScopeMode(s) }

// ParseGridArgs parses the `grid` command's arguments.
func ParseGridArgs(args []string) (GridScopeMode, string, []string, error) {
	return verb.ParseGridArgs(args)
}

// GridArgsString renders a grid selection back into an argument string.
func GridArgsString(mode GridScopeMode, anchor string, ids []string) string {
	return verb.GridArgsString(mode, anchor, ids)
}
