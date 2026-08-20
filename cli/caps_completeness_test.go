package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// GrantableCaps is the list every operator surface enumerates from: the CLI
// `caps` catalog, the TUI authority picker (cli.CapsCatalog), and the WebUI
// chips (wasm harnessCapList). Nothing makes the compiler notice when a bit is
// added to the Capability enum and to `all` but not to this list — it simply
// stops being offered, and, worse, the WebUI's [all] button computes its mask
// as the OR of the chips it was given (capsAllBits() over capList()), so it
// would silently grant a SUBSET of what the server calls `all`.
//
// That is the same shape as the toTaskInfo / toRunnerInfo mapper omissions
// (server/mapper_completeness_test.go): a hand-maintained list beside a
// generated type, with no compiler link between them.
func TestGrantableCapsCoversEveryBitOfAll(t *testing.T) {
	var or protocol.Capability
	for _, c := range GrantableCaps() {
		// none and all are the quick-set aliases, not granular bits; the wasm
		// bridge filters them out of the chip list the same way.
		if c == protocol.Capability_None || c == protocol.Capability_All {
			continue
		}
		or |= c
	}
	if or != protocol.Capability_All {
		missing := protocol.Capability_All &^ or
		extra := or &^ protocol.Capability_All
		t.Errorf("OR(GrantableCaps) = %#x, want Capability_All = %#x (missing %#x, unexpected %#x) — "+
			"a bit reached the enum but not the list every UI enumerates from",
			uint32(or), uint32(protocol.Capability_All), uint32(missing), uint32(extra))
	}
}

// Every granular entry must round-trip through ParseCaps under the name the
// catalog prints, or a name shown by `harness-cli caps` is one the same binary
// refuses to accept.
func TestEveryGrantableCapParsesByItsPrintedName(t *testing.T) {
	for _, c := range GrantableCaps() {
		name := c.String()
		got, err := ParseCaps(name)
		if err != nil {
			t.Errorf("ParseCaps(%q) — the catalog prints this name but the parser rejects it: %v", name, err)
			continue
		}
		if got != c {
			t.Errorf("ParseCaps(%q) = %#x, want %#x", name, uint32(got), uint32(c))
		}
	}
}

// The three attach caps are ranked, and the catalog's ordering is what an
// operator scans when deciding what to grant. Weakest first is deliberate: the
// first plausible-looking match in a list is the one people pick, and here that
// should be the one that can only read.
func TestAttachCapsAreListedWeakestFirst(t *testing.T) {
	idx := map[protocol.Capability]int{}
	for i, c := range GrantableCaps() {
		idx[c] = i
	}
	view, okV := idx[protocol.Capability_ExecView]
	cowrite, okC := idx[protocol.Capability_ExecCowrite]
	control, okX := idx[protocol.Capability_ExecControl]
	if !okV || !okC || !okX {
		t.Fatal("one of the three attach caps is missing from GrantableCaps")
	}
	if !(view < cowrite && cowrite < control) {
		t.Errorf("attach caps listed view=%d cowrite=%d control=%d; want strictly increasing "+
			"so the read-only one is met first", view, cowrite, control)
	}
}
