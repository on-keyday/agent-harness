package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Flags is the common flag set for all `harness-cli agent ...` subcommands.
// All fields except AuthTicket fall back to env (HARNESS_*); AuthTicket is env-only.
type Flags struct {
	ServerCID string
	TaskID    string
	RunnerID  string
	Hostname  string
	WSPath    string
}

// Conn wraps a peer.Conn that has completed an agentboard Hello handshake.
type Conn struct {
	pc       *peer.Conn
	taskID   protocol.TaskID
	runnerID protocol.RunnerID
	mu       sync.Mutex
	onCtl    func(kind wire.ApplicationPayloadKind, payload []byte)
}

func (c *Conn) Close() { c.pc.Close() }

func (c *Conn) PC() *peer.Conn { return c.pc }

func (c *Conn) TaskID() protocol.TaskID { return c.taskID }

func (c *Conn) RunnerID() protocol.RunnerID { return c.runnerID }

// SetOnControl installs a control-payload callback (overriding any prior).
// Subcommands use this to demux response messages by request_id.
// Safe to call multiple times; the latest callback wins.
func (c *Conn) SetOnControl(fn func(kind wire.ApplicationPayloadKind, payload []byte)) {
	c.mu.Lock()
	c.onCtl = fn
	c.mu.Unlock()
	c.pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		c.mu.Lock()
		f := c.onCtl
		c.mu.Unlock()
		if f != nil {
			f(kind, payload)
		}
	})
}

// SendRaw writes an agentboard message to the underlying connection.
func (c *Conn) SendRaw(msg *agentboard.AgentMessage) error {
	data := msg.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentMessage)})
	if _, _, err := c.pc.Connection().SendMessage(data); err != nil {
		return errors.Join(errors.New("agent: send failed"), err)
	}
	return nil
}

// protoToBoardRunnerID copies field-by-field (same shape, distinct Go types).
func protoToBoardRunnerID(p protocol.RunnerID) agentboard.RunnerID {
	var out agentboard.RunnerID
	out.SetTransport(p.Transport)
	out.SetIpAddr(p.IpAddr)
	out.Port = p.Port
	out.UniqueNumber = p.UniqueNumber
	return out
}

func protoToBoardTaskID(p protocol.TaskID) agentboard.TaskID {
	var out agentboard.TaskID
	out.Id = p.Id
	return out
}

// ConnectAgent dials the harness server, sends AgentBridgeHello, awaits OK,
// and returns the open Conn. Caller must Close it.
func ConnectAgent(ctx context.Context, f Flags) (*Conn, error) {
	cid, err := cliopts.ResolveServerCID(f.ServerCID)
	if err != nil {
		return nil, err
	}
	tid, err := cliopts.ResolveTaskID(f.TaskID)
	if err != nil {
		return nil, err
	}
	rid, err := cliopts.ResolveRunnerID(f.RunnerID)
	if err != nil {
		return nil, err
	}
	ticket, err := cliopts.ResolveAuthTicket()
	if err != nil {
		return nil, err
	}
	wsPath := cliopts.ResolveString(f.WSPath, "HARNESS_WS_PATH")
	if wsPath == "" {
		wsPath = cli.WebSocketPath
	} else {
		cli.WebSocketPath = wsPath
	}

	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   wsPath,
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		return nil, fmt.Errorf("ws endpoint: %w", err)
	}
	pc, err := peer.Dial(ctx, ep, cid, peer.DialConfig{
		Logger:       slog.Default(),
		PingInterval: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}

	helloRespCh := make(chan agentboard.HelloStatus, 1)
	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		if kind != wire.ApplicationPayloadKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(payload); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_HelloResponse {
			resp := msg.HelloResponse()
			if resp == nil {
				return
			}
			select {
			case helloRespCh <- resp.Status:
			default:
			}
		}
	})
	pc.Start(ctx)

	hostname := cliopts.ResolveString(f.Hostname, "HARNESS_HOSTNAME")
	hello := agentboard.AgentBridgeHello{
		RunnerId:   protoToBoardRunnerID(rid),
		TaskId:     protoToBoardTaskID(tid),
		AuthTicket: ticket,
	}
	if hostname != "" {
		hello.SetHostname([]byte(hostname))
	}
	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Hello}
	msg.SetHello(hello)
	data := msg.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentMessage)})
	if _, _, err := pc.Connection().SendMessage(data); err != nil {
		pc.Close()
		return nil, fmt.Errorf("send hello: %w", err)
	}

	select {
	case status := <-helloRespCh:
		if status != agentboard.HelloStatusOk {
			pc.Close()
			return nil, fmt.Errorf("hello rejected: %v", status)
		}
	case <-ctx.Done():
		pc.Close()
		return nil, ctx.Err()
	}
	return &Conn{pc: pc, taskID: tid, runnerID: rid}, nil
}
