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
	"github.com/on-keyday/agent-harness/transport"
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
// with this Client's TaskControl-aware handler. The peerCID identifies
// which server peer to ECDH with (e.g. parsed from --server-cid). Pubsub-
// kind responses are handled by peer.Conn directly (it routes them to its
// pubsub.Client); TaskControl-kind responses land in c.dispatchControl below.
func Dial(ctx context.Context, peerCID objproto.ConnectionID) (*Client, error) {
	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   WebSocketPath,
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		return nil, fmt.Errorf("ws endpoint: %w", err)
	}
	pc, err := peer.Dial(ctx, ep, peerCID, peer.DialConfig{
		Logger: slog.Default(),
	})
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:    pc,
		pending: map[uint32]chan *protocol.TaskControlResponse{},
	}

	psk := GetPSK()
	pskRespCh := make(chan wire.PskAuthStatus, 1)

	// Combined handler: PSK response during handshake, TaskControl after.
	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		if kind == wire.ApplicationPayloadKind_PskAuth && len(payload) > 0 {
			select {
			case pskRespCh <- wire.PskAuthStatus(payload[0]):
			default:
			}
			return
		}
		c.dispatchControl(kind, payload)
	})
	pc.Start(ctx)

	if err := SendAndWaitPSK(ctx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, pskRespCh); err != nil {
		pc.Close()
		return nil, err
	}

	// PSK exchange complete — switch to the pure app handler.
	pc.SetOnControl(c.dispatchControl)
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
func (c *Client) Conn() objproto.Connection { return c.conn.Connection() }

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
	if _, _, err := c.conn.Connection().SendMessage(data); err != nil {
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
// + objproto connection shutdown). The objproto.Endpoint constructed by
// Dial is intentionally not torn down here — it has no Close API and is
// leaked until process exit. cli subcommands are short-lived processes,
// so this is acceptable; long-running embedders (e.g. the tui) reuse the
// same *Client for the lifetime of the program.
func (c *Client) Close() {
	c.conn.Close()
}
