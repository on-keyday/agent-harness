package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A verb's body may send only the kinds its declaration lists (spec D14): the
// declaration feeds the MCP tool filter, and this is what keeps it true.
func TestExpectKindsRefusesAnUndeclaredKind(t *testing.T) {
	ctx := WithExpectedKinds(context.Background(), "record ls", []protocol.TaskControlKind{protocol.TaskControlKind_RecordList})
	c := &Client{pending: map[uint32]chan taskControlResult{}, expect: expectedKindsFrom(ctx)}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_Cancel}}

	_, err := c.RoundTripTaskControl(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "record ls") || !strings.Contains(err.Error(), "Sends") {
		t.Fatalf("RoundTripTaskControl of an undeclared kind: %v, want the declaration refusal", err)
	}
	if _, err := c.BeginTaskControl(req); err == nil || !strings.Contains(err.Error(), "Sends") {
		t.Fatalf("BeginTaskControl of an undeclared kind: %v, want the declaration refusal", err)
	}
	if len(c.pending) != 0 {
		t.Errorf("a refused request left %d pending entries", len(c.pending))
	}
	if err := c.expect.check(protocol.TaskControlKind_RecordList); err != nil {
		t.Fatalf("a declared kind was refused: %v", err)
	}
}

// Two MCP calls run at once (spec D18), each under its own context: one
// verb's declaration must not leak into the other's clients (V12).
func TestExpectedKindsArePerContext(t *testing.T) {
	a := expectedKindsFrom(WithExpectedKinds(context.Background(), "record ls", []protocol.TaskControlKind{protocol.TaskControlKind_RecordList}))
	b := expectedKindsFrom(WithExpectedKinds(context.Background(), "cancel", []protocol.TaskControlKind{protocol.TaskControlKind_Cancel}))
	if err := a.check(protocol.TaskControlKind_Cancel); err == nil {
		t.Error("record ls's client accepted cancel's kind")
	}
	if err := b.check(protocol.TaskControlKind_Cancel); err != nil {
		t.Errorf("cancel's client refused its own kind: %v", err)
	}
	// A context nothing was put on is not checked (the TUI, the WebUI), and a
	// verb with no declaration puts nothing on.
	if err := expectedKindsFrom(context.Background()).check(protocol.TaskControlKind_Cancel); err != nil {
		t.Errorf("an unchecked context refused a kind: %v", err)
	}
	if expectedKindsFrom(WithExpectedKinds(context.Background(), "x", nil)) != nil {
		t.Error("a nil declaration installed a check")
	}
	// A derived context keeps the set: bodies wrap the call's context in
	// timeouts before dialing.
	derived, cancel := context.WithCancel(WithExpectedKinds(context.Background(), "record ls", []protocol.TaskControlKind{protocol.TaskControlKind_RecordList}))
	defer cancel()
	if err := expectedKindsFrom(derived).check(protocol.TaskControlKind_Cancel); err == nil {
		t.Error("a derived context lost the declaration")
	}
}
