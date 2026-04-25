package cli

import (
	"context"
	"crypto/ecdh"
	"fmt"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Client is a thin wrapper over an objproto.Connection used for one-shot
// TaskControl request/response round-trips from the CLI.
type Client struct {
	conn objproto.Connection
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

// Dial establishes a WebSocket session, ECDH handshake, and returns a ready Client.
func Dial(ctx context.Context, addr string) (*Client, error) {
	sess, err := transport.WebSocketSession(slog.Default(), addr, nil, objproto.SessionModeClient)
	if err != nil {
		return nil, fmt.Errorf("ws session: %w", err)
	}
	// Build a valid ConnectionID using the same format as the runner: "ws:127.0.0.1:<port>-<id>"
	cidStr := fmt.Sprintf("ws:127.0.0.1:%s-2222", portFrom(addr))
	conn, err := objproto.DoECDHHandshake(ctx, sess,
		objproto.MustParseConnectionID(cidStr),
		ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Conn exposes the underlying connection — used by the logs/watch subcommands that need
// trsf streams. submit/ls/cancel use only the round-trip helper.
func (c *Client) Conn() objproto.Connection { return c.conn }

// roundTripTaskControl sends a TaskControlRequest and reads a single TaskControlResponse.
// The wire kind byte is prepended on send and stripped on receive.
func (c *Client) roundTripTaskControl(req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	data := req.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
	if _, _, err := c.conn.SendMessage(data); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	msg, err := c.conn.ReceiveMessage()
	if err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	if len(msg.Data) == 0 || wire.ApplicationPayloadKind(msg.Data[0]) != wire.ApplicationPayloadKind_TaskControl {
		return nil, fmt.Errorf("unexpected response kind")
	}
	resp := &protocol.TaskControlResponse{}
	if _, err := resp.Decode(msg.Data[1:]); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return resp, nil
}

// Close releases the underlying connection.
func (c *Client) Close() { _ = c.conn.Close() }
