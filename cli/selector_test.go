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
	const id = "ws:127.0.0.1:8539-123"
	sel, err := buildSelector(SelectorOpts{Runner: id})
	if err != nil {
		t.Fatalf("buildSelector(%q): %v", id, err)
	}
	if sel.Kind != protocol.RunnerSelectorKind_ByRunnerId {
		t.Fatalf("kind = %v, want ByRunnerId", sel.Kind)
	}
	rid := sel.RunnerId()
	if rid == nil {
		t.Fatal("RunnerId() = nil")
	}
	if string(rid.Transport) != "ws" || rid.Port != 8539 || rid.IpAddrLen != 4 {
		t.Fatalf("unexpected RunnerID fields: transport=%q port=%d ipLen=%d", rid.Transport, rid.Port, rid.IpAddrLen)
	}
	if got := protocol.RunnerIDToConnID(*rid).String(); got != id {
		t.Fatalf("round-trip = %q, want %q", got, id)
	}
}

func TestBuildRunnerIDSelector_RejectsGarbage(t *testing.T) {
	if _, err := buildSelector(SelectorOpts{Runner: "not a cid or hex"}); err == nil {
		t.Fatal("expected error for a value that is neither ConnectionID nor hex")
	}
}
