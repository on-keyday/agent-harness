package agent

import (
	"errors"
	"io"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A reply to an operator message with no reply route must say so, and say
// what to do instead -- the generic "send rejected: no_reply_route" names
// neither.
func TestSendResult_NoReplyRouteIsItsOwnError(t *testing.T) {
	resp := &protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentSend}
	resp.SetAgentSend(protocol.AgentSendResponse{Status: protocol.SendStatus_NoReplyRoute})
	err := sendResult(cli.TaskControlResult{Resp: resp}, 42, 3, "positional", io.Discard)
	var nr *cli.NoReplyRouteError
	if !errors.As(err, &nr) || nr.InReplyTo != 42 {
		t.Fatalf("err = %v, want NoReplyRouteError for 42", err)
	}
}
