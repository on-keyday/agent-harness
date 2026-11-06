package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A request whose body does not decode still names its kind and request id,
// including a kind this server does not know: that is the version-skewed
// client the diagnostic exists for.
func TestUndecodableRequestStillNamesItsHeader(t *testing.T) {
	h := protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind(250), RequestId: 42}
	payload := append(h.MustAppend(nil), 0xff, 0xff) // a body no arm decodes
	kind, reqID := requestHeaderOf(payload)
	if reqID != 42 || kind != protocol.TaskControlKind(250).String() {
		t.Fatalf("requestHeaderOf = (%q, %d), want (%q, 42)", kind, reqID, protocol.TaskControlKind(250).String())
	}
	if kind, reqID := requestHeaderOf([]byte{1, 2}); kind != "?" || reqID != 0 {
		t.Fatalf("short payload: requestHeaderOf = (%q, %d), want (\"?\", 0)", kind, reqID)
	}
}
