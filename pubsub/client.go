package pubsub

import (
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/pubsub/protocol"
)

// ResponseHandler is called once with the broker's response to a JOIN/LEAVE
// the Client issued. The Client deletes its pending entry before invoking
// the handler, so a fresh Join/Leave with the same logical topic uses a new
// request ID and a separate handler.
type ResponseHandler func(*protocol.PubSubResponse)

// Client is the request_id correlator for pubsub responses on a single
// connection. It does NOT own the connection — callers wrap the bytes
// returned by JoinTopic / LeaveTopic with their own conn.SendMessage and
// feed every incoming Pubsub-kind payload through HandleResponse.
type Client struct {
	m              sync.Mutex
	responseMapper map[uint32]ResponseHandler
	reqID          uint32
}

// NewClient returns a ready-to-use Client with an initialized pending map.
func NewClient() *Client {
	return &Client{
		responseMapper: map[uint32]ResponseHandler{},
	}
}

// JoinTopic returns the wire-prefixed JOIN payload for the given topic and
// registers cb to receive the broker's response. Returns nil if the
// nickname is too long for the wire format.
func (c *Client) JoinTopic(nickName string, topic string, cb ResponseHandler) []byte {
	c.m.Lock()
	id := c.reqID
	c.reqID++
	c.responseMapper[id] = cb
	c.m.Unlock()
	return JoinTopic(id, nickName, topic)
}

// LeaveTopic returns the wire-prefixed LEAVE payload and registers cb.
func (c *Client) LeaveTopic(topic string, cb ResponseHandler) []byte {
	c.m.Lock()
	id := c.reqID
	c.reqID++
	c.responseMapper[id] = cb
	c.m.Unlock()
	return LeaveTopic(id, topic)
}

// HandleResponse decodes a PubSubResponse payload (the bytes after the wire
// kind byte) and dispatches it to the registered handler. Unknown
// request_ids are dropped silently — they may belong to a previous
// connection or be late arrivals after caller cancellation.
func (c *Client) HandleResponse(msg []byte) {
	resp := &protocol.PubSubResponse{}
	if err := resp.DecodeExact(msg); err != nil {
		slog.Error("failed to decode PubSubResponse", "error", err)
		return
	}
	c.m.Lock()
	cb, ok := c.responseMapper[resp.RequestId]
	if ok {
		delete(c.responseMapper, resp.RequestId)
	}
	c.m.Unlock()
	if !ok {
		return
	}
	cb(resp)
}
