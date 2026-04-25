package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Client is the CLI/TUI-facing endpoint. It owns a peer.Conn (which handles
// the WS+ECDH+trsf+AutoSend/Receive/Ping plumbing and the pubsub.Client
// correlator) and adds a TaskControl request/response correlator on top:
// every RoundTripTaskControl call assigns a fresh request_id, the response
// is routed back to the waiting goroutine via the pending map, and ctx
// cancellation reclaims the slot.
type Client struct {
	conn *peer.Conn

	mu      sync.Mutex
	nextReq uint32
	pending map[uint32]chan *protocol.TaskControlResponse
}

// Dial establishes the underlying peer.Conn and starts the receive loop
// with this Client's TaskControl-aware handler. Pubsub-kind responses are
// handled by peer.Conn directly (it routes them to its pubsub.Client);
// TaskControl-kind responses land in c.dispatchControl below.
func Dial(ctx context.Context, addr string) (*Client, error) {
	pc, err := peer.Dial(ctx, peer.DialConfig{
		Addr:         addr,
		UniqueNumber: 2222,
		Logger:       slog.Default(),
	})
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:    pc,
		pending: map[uint32]chan *protocol.TaskControlResponse{},
	}
	pc.SetOnControl(c.dispatchControl)
	pc.Start(ctx)
	return c, nil
}

// dispatchControl is the peer ControlHandler. We only care about TaskControl
// kind — everything else (RunnerControl is server-side only here) is dropped.
func (c *Client) dispatchControl(kind wire.ApplicationPayloadKind, payload []byte) {
	if kind != wire.ApplicationPayloadKind_TaskControl {
		return
	}
	resp := &protocol.TaskControlResponse{}
	if _, derr := resp.Decode(payload); derr != nil {
		slog.Error("cli.Client: decode TaskControlResponse", "err", derr)
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[resp.RequestId]
	if ok {
		delete(c.pending, resp.RequestId)
	}
	c.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// Conn exposes the underlying objproto.Connection for callers that need to
// SendMessage directly (e.g. raw JOIN bytes from pubsub.Client.JoinTopic).
func (c *Client) Conn() objproto.Connection { return c.conn.Session() }

// Transport returns the trsf transport — used by callers that need to wait
// on server-initiated streams.
func (c *Client) Transport() trsf.Transport { return c.conn.Transport() }

// Pubsub returns the embedded pubsub.Client used for JOIN/LEAVE round-trips.
func (c *Client) Pubsub() *pubsub.Client { return c.conn.Pubsub() }

// Peer returns the underlying peer.Conn for callers that want to use its
// JoinAndGetStream / Publish helpers directly without re-implementing the
// JOIN+lookup+header dance.
func (c *Client) Peer() *peer.Conn { return c.conn }

// RoundTripTaskControl assigns a fresh request_id, sends the request, and
// blocks until the matching response arrives or ctx is cancelled. Concurrent
// callers are correlated independently — no implicit serialization beyond the
// objproto.Connection's send mutex.
func (c *Client) RoundTripTaskControl(ctx context.Context, req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	c.mu.Lock()
	id := c.nextReq
	c.nextReq++
	ch := make(chan *protocol.TaskControlResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req.RequestId = id
	data := req.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
	if _, _, err := c.conn.Session().SendMessage(data); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("send: %w", err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp := <-ch:
		return resp, nil
	}
}

// Close tears down the underlying peer.Conn (best-effort wire-level Close
// + objproto session shutdown).
func (c *Client) Close() {
	c.conn.Close()
}
