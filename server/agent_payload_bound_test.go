package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/on-keyday/objtrsf/trsf"
)

// countingRecvStream serves `remaining` bytes, then EOF. It records how many
// bytes the reader actually consumed and whether Cancel was called, which is
// what lets a test tell "rejected after buffering the whole body" apart from
// "stopped reading at the limit". A plain byte-slice stub cannot: both
// outcomes return the same error.
type countingRecvStream struct {
	mu        sync.Mutex
	remaining int
	served    int
	cancelled bool
}

func (c *countingRecvStream) ReadDirect(maxN uint64) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelled {
		return nil, false, context.Canceled
	}
	if c.remaining == 0 {
		return nil, true, nil
	}
	n := int(maxN)
	if n > c.remaining {
		n = c.remaining
	}
	c.remaining -= n
	c.served += n
	return make([]byte, n), c.remaining == 0, nil
}

func (c *countingRecvStream) ReadDirectContext(_ context.Context, maxN uint64) ([]byte, bool, error) {
	return c.ReadDirect(maxN)
}

func (c *countingRecvStream) Read(p []byte) (int, error) {
	data, _, err := c.ReadDirect(uint64(len(p)))
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, io.EOF
	}
	return copy(p, data), nil
}

func (c *countingRecvStream) ReadContext(_ context.Context, p []byte) (int, error) {
	return c.Read(p)
}

func (c *countingRecvStream) ID() trsf.StreamID { return 1 }

func (c *countingRecvStream) HasRecvData() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remaining > 0
}

func (c *countingRecvStream) EOF() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remaining == 0
}

func (c *countingRecvStream) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = true
}

func (c *countingRecvStream) stats() (served int, cancelled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.served, c.cancelled
}

// recvStreamConn is a fakeConn whose GetReceiveStream resolves to a supplied
// stub, so the agentboard payload-stream path can be driven without a wire.
type recvStreamConn struct {
	*fakeConn
	st trsf.ReceiveStream
}

func (r *recvStreamConn) GetReceiveStream(trsf.StreamID) trsf.ReceiveStream { return r.st }

// TestReadAgentPayloadStream_StopsAtMaxPayload pins the memory bound on the
// one server-side path an agent reaches with NO capability at all: agent send
// is absent from requiredCap, so `caps: none` ("data-plane only") reaches it.
// The read must abandon an over-long body instead of buffering it to EOF and
// judging the length afterwards, and it must cancel the receive stream so the
// sender stops rather than being drained.
func TestReadAgentPayloadStream_StopsAtMaxPayload(t *testing.T) {
	const max = 1024
	st := &countingRecvStream{remaining: 64 * payloadReadChunk}
	conn := &recvStreamConn{fakeConn: &fakeConn{}, st: st}

	_, err := readAgentPayloadStream(conn, 1, max)

	if !errors.Is(err, errPayloadTooLarge) {
		t.Fatalf("err = %v, want errPayloadTooLarge", err)
	}
	served, cancelled := st.stats()
	if want := max + payloadReadChunk; served > want {
		t.Errorf("consumed %d bytes, want <= %d: the whole body is buffered before the limit is applied",
			served, want)
	}
	if !cancelled {
		t.Error("receive stream not cancelled: the sender is left free to keep streaming")
	}
}

// TestReadAgentPayloadStream_AcceptsExactlyMaxPayload keeps the bound on the
// same side of the boundary as agentboard.Board.Send, which rejects only
// len(payload) > MaxPayload. An off-by-one here would reject a body the board
// itself accepts.
func TestReadAgentPayloadStream_AcceptsExactlyMaxPayload(t *testing.T) {
	const max = 1024
	st := &countingRecvStream{remaining: max}
	conn := &recvStreamConn{fakeConn: &fakeConn{}, st: st}

	payload, err := readAgentPayloadStream(conn, 1, max)

	if err != nil {
		t.Fatalf("readAgentPayloadStream: %v", err)
	}
	if len(payload) != max {
		t.Errorf("payload = %d bytes, want %d", len(payload), max)
	}
	if _, cancelled := st.stats(); cancelled {
		t.Error("an exactly-at-limit body must not be cancelled")
	}
}
