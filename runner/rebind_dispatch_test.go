package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// noStreams never finds a stream: the state a rebind is in until the frame that
// makes the server's stream visible has been read off the connection.
type noStreams struct{ lookups atomic.Int32 }

func (n *noStreams) GetBidirectionalStream(trsf.StreamID) trsf.BidirectionalStream {
	n.lookups.Add(1)
	return nil
}
func (*noStreams) GetReceiveStream(trsf.StreamID) trsf.ReceiveStream { return nil }

// A RebindSession must not hold the receive loop while it waits for its stream.
// dispatchRunnerRequest runs ON that loop (trsf.AutoReceive calls it
// synchronously), and the loop is also what reads the stream frame the wait is
// for — so a wait there can only end by timing out. Measured on the live fleet
// as 5 failed rebinds in 102, each exactly 2s after the last, and on a dummy
// instance as a HoldTasks ack that missed its window behind the same wait.
func TestRebindSessionDoesNotBlockTheReceiveLoop(t *testing.T) {
	const id = "91560a69a243764d181ed3f6bd7f76cf"
	var killed atomic.Bool
	e := &taskEntry{cancel: func() { killed.Store(true) }, relay: &sessionRelay{}}
	e.started.Store(true)
	reg := NewTaskRegistry()
	reg.put(id, e)
	streams := &noStreams{}
	s := &Session{reg: reg, Sender: nopSender{}, Streams: streams}

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_RebindSession}
	req.SetRebindSession(protocol.RebindSessionRequest{TaskId: taskIDOf(t, id), StreamId: 4})
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	dispatchRunnerRequest(context.Background(), s, s.logger(), appwire.AppKind_RunnerControl, payload)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("dispatching a RebindSession held the receive loop for %v", d)
	}
	// The rebind still runs — and, with no stream ever appearing, still ends in
	// the failure path that kills the child.
	deadline := time.Now().Add(10 * time.Second)
	for !killed.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("the rebind never completed (lookups=%d)", streams.lookups.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
