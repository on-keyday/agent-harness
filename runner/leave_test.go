package runner

import (
	"context"
	"testing"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestLeaveCallsOnLeave(t *testing.T) {
	got := make(chan string, 1)
	s := &Session{OnLeave: func(by string) { got <- by }}
	l := protocol.LeaveRequest{}
	l.SetBy([]byte("task:00112233445566778899aabbccddeeff"))
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_Leave}
	req.SetLeave(l)
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatchRunnerRequest(context.Background(), s, nil, appwire.AppKind_RunnerControl, payload)
	select {
	case by := <-got:
		if by != "task:00112233445566778899aabbccddeeff" {
			t.Fatalf("by = %q", by)
		}
	default:
		t.Fatal("leave did not reach OnLeave")
	}
}
