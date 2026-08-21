package cli

import (
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A response this build cannot decode must FAIL the waiting caller, not
// strand it. Measured before the fix (new client × pre-kind-field server):
// the decode error was logged and the response dropped, and `session
// snapshot` hung forever, because most CLI paths wait on
// context.Background(). RequestId is parsed before the failing arm, so the
// error is routable.
func TestUndecodableResponseFailsTheWaiterInsteadOfStranding(t *testing.T) {
	c := &Client{pending: map[uint32]chan taskControlResult{}}
	ch := make(chan taskControlResult, 1)
	c.pending[7] = ch

	// A valid response for request 7, truncated mid-arm — the shape an older
	// server's shorter AttachSessionResponse presents to a newer client.
	tc := &protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AttachSession, RequestId: 7}
	tc.SetAttach(protocol.AttachSessionResponse{Status: protocol.AttachSessionStatus_Ok, StreamId: 3})
	full := tc.MustAppend(nil)
	c.dispatchControl(appwire.AppKind_TaskControl, full[:len(full)-1])

	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatalf("expected an error result, got resp=%+v", r.resp)
		}
		if !errors.Is(r.err, ErrResponseUndecodable) {
			t.Fatalf("err=%v want ErrResponseUndecodable", r.err)
		}
	default:
		t.Fatal("nothing delivered: the caller would hang (the pre-fix behaviour)")
	}
}

// A payload too short to carry RequestId must NOT be routed: the zero-valued
// field would name request 0, which is a REAL id (nextReq starts at 0), so
// delivering the error would fail an unrelated caller.
func TestGarbagePayloadDoesNotFailRequestZero(t *testing.T) {
	c := &Client{pending: map[uint32]chan taskControlResult{}}
	ch := make(chan taskControlResult, 1)
	c.pending[0] = ch

	c.dispatchControl(appwire.AppKind_TaskControl, []byte{0xFF, 0xFF})

	select {
	case r := <-ch:
		t.Fatalf("request 0 wrongly failed by an unroutable payload: %+v", r)
	default:
	}
	if _, ok := c.pending[0]; !ok {
		t.Fatal("request 0's pending slot was consumed")
	}
}
