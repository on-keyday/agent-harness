package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// Over budget on udp: refused and NOT handed to the transport. On ws the same
// bytes go out — ws rides TCP and is never checked.
func TestSendCheckedRefusesOverBudgetOnUDPOnly(t *testing.T) {
	sent := 0
	send := func([]byte) error { sent++; return nil }
	data := make([]byte, 1171)
	if err := sendChecked("udp", 1170, data, send); err == nil || sent != 0 {
		t.Fatalf("udp over budget: err=%v sent=%d, want an error and nothing sent", err, sent)
	}
	if err := sendChecked("ws", 1170, data, send); err != nil || sent != 1 {
		t.Fatalf("ws: err=%v sent=%d, want one send", err, sent)
	}
}

type closeRecorder struct {
	trsf.SendStream
	closed bool
}

func (c *closeRecorder) ID() trsf.StreamID { return 3 }
func (c *closeRecorder) Close() error      { c.closed = true; return nil }

// When the request naming a payload stream is never sent — refused for size,
// or a dead connection — the stream is closed instead of left behind for the
// life of the connection.
func TestTaskControlWithPayloadClosesTheStreamWhenTheRequestFails(t *testing.T) {
	st := &closeRecorder{}
	begin := func(*protocol.TaskControlRequest) (<-chan TaskControlResult, error) {
		return nil, errors.New("send: too large")
	}
	build := func(uint64) (*protocol.TaskControlRequest, error) {
		return &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenExecRun}, nil
	}
	if _, err := taskControlWithPayload(context.Background(), st, begin, build, []byte("x")); err == nil || !st.closed {
		t.Fatalf("err=%v closed=%v, want the send error and the stream closed", err, st.closed)
	}
}
