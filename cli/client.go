package cli

import (
	"context"
	"crypto/ecdh"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Client wraps a fully-set-up connection (objproto + trsf + AutoSend/Receive
// + AutoPing) and provides synchronous TaskControl round-trips correlated by
// request_id. AutoReceive is the only consumer of the underlying recv queue
// — TaskControl responses are dispatched into this Client's pending map and
// Pubsub responses go to the embedded pubsub.Client; nothing races with
// trsf's stream-frame handling.
type Client struct {
	conn      objproto.Connection
	p         trsf.Transport
	pubClient *pubsub.Client

	mu      sync.Mutex
	nextReq uint32
	pending map[uint32]chan *protocol.TaskControlResponse
}

// portFrom extracts the port portion from a "host:port" string.
// Falls back to the full string if no colon is found.
func portFrom(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}

// Dial establishes a WebSocket session, ECDH handshake, trsf streams, and
// the supporting goroutines (AutoSend / AutoReceive / AutoPing). The returned
// Client is ready for both TaskControl round-trips and pubsub subscriptions.
func Dial(ctx context.Context, addr string) (*Client, error) {
	sess, err := transport.WebSocketSession(slog.Default(), addr, nil, objproto.SessionModeClient)
	if err != nil {
		return nil, fmt.Errorf("ws session: %w", err)
	}
	cidStr := fmt.Sprintf("ws:127.0.0.1:%s-2222", portFrom(addr))
	conn, err := objproto.DoECDHHandshake(ctx, sess,
		objproto.MustParseConnectionID(cidStr),
		ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	c := &Client{
		conn:      conn,
		p:         trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, slog.Default()),
		pubClient: pubsub.NewClient(),
		pending:   map[uint32]chan *protocol.TaskControlResponse{},
	}
	go trsf.AutoSend(ctx, c.p, conn, nil)
	go trsf.AutoReceive(ctx, c.p, conn, c.dispatch)
	go trsf.AutoPing(ctx, conn, 30*time.Second)
	return c, nil
}

// Conn exposes the underlying connection (for callers that need to send raw
// wire-prefixed bytes, e.g. pubsub.Client.JoinTopic results).
func (c *Client) Conn() objproto.Connection { return c.conn }

// Transport returns the trsf transport — used by callers that need to wait
// on server-initiated streams.
func (c *Client) Transport() trsf.Transport { return c.p }

// Pubsub returns the embedded pubsub.Client used for JOIN/LEAVE round-trips.
func (c *Client) Pubsub() *pubsub.Client { return c.pubClient }

// dispatch is the AutoReceive callback. It routes Pubsub-kind messages to
// pubsub.Client and TaskControl-kind messages to the pending request map.
// Other kinds are ignored (no client uses RunnerControl as a client today).
func (c *Client) dispatch(msg *objproto.Message, err error) {
	if err != nil {
		// io.EOF when the server sent Close; any other err signals network failure.
		return
	}
	if msg == nil || len(msg.Data) == 0 {
		return
	}
	switch wire.ApplicationPayloadKind(msg.Data[0]) {
	case wire.ApplicationPayloadKind_Pubsub:
		c.pubClient.HandleResponse(msg.Data[1:])
	case wire.ApplicationPayloadKind_TaskControl:
		resp := &protocol.TaskControlResponse{}
		if _, derr := resp.Decode(msg.Data[1:]); derr != nil {
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
}

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
	if _, _, err := c.conn.SendMessage(data); err != nil {
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

// Close sends a wire-level Close to the peer (best-effort; the server
// uses it to deregister the runner / drop the subscriber immediately
// instead of waiting for the idle GC) and then releases the underlying
// objproto connection.
func (c *Client) Close() {
	_ = trsf.SendClose(c.conn)
	_ = c.conn.Close()
}
