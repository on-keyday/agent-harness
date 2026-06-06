package runner

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

// fakeStreamCreator hands out a noopBidiStream with a fixed id, standing in for
// pc.Transport() in remote port-forward tests.
type fakeStreamCreator struct{ id trsf.StreamID }

func (f *fakeStreamCreator) CreateBidirectionalStream() trsf.BidirectionalStream {
	return &noopBidiStream{streamID: f.id}
}

// TestRemoteForwardListener_AcceptsAndNotifies verifies the runner's
// remote-forward listener: each accepted connection creates a stream and is
// announced to the server via RunnerMessage{RemoteForwardConn}.
func TestRemoteForwardListener_AcceptsAndNotifies(t *testing.T) {
	ms := &mockSender{}
	s := &Session{Sender: ms, creator: &fakeStreamCreator{id: 4242}, Now: time.Now}

	ln, err := s.startRemoteForwardListener(context.Background(), 7, "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	var found *protocol.RemoteForwardConn
	for time.Now().Before(deadline) {
		ms.mu.Lock()
		msgs := append([][]byte{}, ms.sent...)
		ms.mu.Unlock()
		for _, raw := range msgs {
			m := decodeRunnerMsg(t, raw)
			if m.Kind == protocol.RunnerMessageType_RemoteForwardConn {
				found = m.RemoteForwardConn()
				break
			}
		}
		if found != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("no RemoteForwardConn sent after a connection arrived")
	}
	if found.ForwardId != 7 || found.StreamId != 4242 {
		t.Fatalf("RemoteForwardConn = %+v, want forwardId 7 streamId 4242", found)
	}
}

// TestCloseRemoteForwardListener verifies the listener registry closes the
// tracked listener (so a subsequent accept fails).
func TestCloseRemoteForwardListener(t *testing.T) {
	s := &Session{Now: time.Now}
	ln, err := s.startRemoteForwardListener(context.Background(), 9, "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.rforwardListeners().add(9, ln)
	addr := ln.Addr().String()
	s.rforwardListeners().close(9)

	// The listener is closed: a dial should fail (no acceptor) — give the OS a
	// moment to release the port.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return // closed as expected
		}
		_ = c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listener still accepting after close")
}
