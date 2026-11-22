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
	restore := ExpectKinds("record ls", []protocol.TaskControlKind{protocol.TaskControlKind_RecordList})
	c := &Client{pending: map[uint32]chan taskControlResult{}}
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

	if err := checkExpectedKind(protocol.TaskControlKind_RecordList); err != nil {
		t.Fatalf("a declared kind was refused: %v", err)
	}

	restore()
	if err := checkExpectedKind(protocol.TaskControlKind_Cancel); err != nil {
		t.Fatalf("after restore nothing is checked, got %v", err)
	}
}
