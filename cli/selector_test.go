//go:build !js

package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The ConnectionID string form is what `harness-cli ls` prints in the id=
// column; --runner must accept it and produce a RunnerID that round-trips back
// to the same CID (which is exactly what the server matches against).
func TestBuildRunnerIDSelector_AcceptsLsConnIDString(t *testing.T) {
	// The two --runner forms select DIFFERENT things, and that is the point of
	// keeping both: an address pins a connection, an identity pins the process.
	const addr = "ws:127.0.0.1:8539-123"
	sel, err := buildSelector(SelectorOpts{Runner: addr})
	if err != nil {
		t.Fatalf("buildSelector(%q): %v", addr, err)
	}
	if sel.Kind != protocol.RunnerSelectorKind_ByConnId {
		t.Fatalf("kind = %v, want ByConnId for an address", sel.Kind)
	}
	cid := sel.ConnId()
	if cid == nil {
		t.Fatal("ConnId() = nil")
	}
	if got := cid.ToObjproto().String(); got != addr {
		t.Fatalf("round-trip = %q, want %q", got, addr)
	}
}

func TestBuildRunnerIDSelector_HexFormPinsTheProcess(t *testing.T) {
	const hexID = "a1b2c3d4000000000000000000000009"
	sel, err := buildSelector(SelectorOpts{Runner: hexID})
	if err != nil {
		t.Fatalf("buildSelector(%q): %v", hexID, err)
	}
	if sel.Kind != protocol.RunnerSelectorKind_ByRunnerId {
		t.Fatalf("kind = %v, want ByRunnerId for a 32-hex identity", sel.Kind)
	}
	rid := sel.RunnerId()
	if rid == nil {
		t.Fatal("RunnerId() = nil")
	}
	if got := rid.Hex(); got != hexID {
		t.Fatalf("identity = %q, want %q", got, hexID)
	}
}

// --ip is not platform-special: the wasm build used to refuse it outright while
// its command input happily parsed one, so pin that it builds a selector here.
func TestBuildIPSelector_Works(t *testing.T) {
	sel, err := buildSelector(SelectorOpts{IP: "10.0.0.4"})
	if err != nil {
		t.Fatalf("buildSelector(--ip): %v", err)
	}
	if sel.Kind != protocol.RunnerSelectorKind_ByIp {
		t.Fatalf("kind = %v, want ByIp", sel.Kind)
	}
}

func TestBuildRunnerIDSelector_RejectsGarbage(t *testing.T) {
	if _, err := buildSelector(SelectorOpts{Runner: "not a cid or hex"}); err == nil {
		t.Fatal("expected error for a value that is neither ConnectionID nor hex")
	}
}
