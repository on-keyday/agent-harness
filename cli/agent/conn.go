package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// trsfStreamID converts a uint64 wire id to trsf.StreamID.
func trsfStreamID(id uint64) trsf.StreamID { return trsf.StreamID(id) }

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
	onCtl    func(kind appwire.AppKind, payload []byte)
}

func (c *Conn) Close() { c.pc.Close() }

func (c *Conn) PC() *peer.Conn { return c.pc }

func (c *Conn) TaskID() protocol.TaskID { return c.taskID }

func (c *Conn) RunnerID() protocol.RunnerID { return c.runnerID }

// SetOnControl installs a control-payload callback (overriding any prior).
// Subcommands use this to demux response messages by request_id.
// Safe to call multiple times; the latest callback wins.
func (c *Conn) SetOnControl(fn func(kind appwire.AppKind, payload []byte)) {
	c.mu.Lock()
	c.onCtl = fn
	c.mu.Unlock()
	c.pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
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
	data := msg.MustAppend([]byte{byte(appwire.AppKind_AgentMessage)})
	if _, _, err := c.pc.Connection().SendMessage(data); err != nil {
		return errors.Join(errors.New("agent: send failed"), err)
	}
	return nil
}

// FetchDeliveredPayload resolves the server-initiated send-stream
// referenced by DeliveredMessage.PayloadStreamId, reads the full body to
// EOF, and returns the bytes. Mirrors waitForReceiveStream / runner's
// waitForAssignTaskBody — the trsf stream-creation frame may not be
// visible by the time the agentboard response envelope arrives, so we
// poll briefly before reading.
func (c *Conn) FetchDeliveredPayload(ctx context.Context, streamID uint64) ([]byte, error) {
	if streamID == 0 {
		return nil, fmt.Errorf("delivered message stream_id is 0")
	}
	id := trsfStreamID(streamID)
	tr := c.pc.Transport()
	st := tr.GetReceiveStream(id)
	if st == nil {
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
	wait:
		for st == nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				return nil, fmt.Errorf("payload stream %d not visible after 2s", id)
			case <-tick.C:
				st = tr.GetReceiveStream(id)
				if st != nil {
					break wait
				}
			}
		}
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("payload stream read: %w", err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			return raw, nil
		}
	}
}

// ConnectAgent dials the harness server, sends a ClientHello (kind=agent)
// over the TaskControl app-kind (0x41), awaits OK, and returns the open Conn.
// Caller must Close it.
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

	// Proxy detection lives in cli.DialPeerConn — env-based, shared by every
	// harness-cli subcommand. The agent path historically had its own copy
	// of the env check; that was a design miss (admin tools and agent tools
	// are the same binary and should share the dial strategy). DialPeerConn
	// reads HARNESS_TASK_ID itself for the proxy ceremony.
	pc, err := cli.DialPeerConn(ctx, cid)
	if err != nil {
		return nil, fmt.Errorf("agent dial: %w", err)
	}

	psk := cli.GetPSK()
	pskRespCh := make(chan appwire.PskAuthStatus, 1)
	helloRespCh := make(chan protocol.ClientHelloStatus, 1)

	// Combined handler: routes PskAuth responses during PSK phase,
	// then TaskControl ClientHelloResponse during Hello phase.
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		switch kind {
		case appwire.AppKind_PskAuth:
			if len(payload) > 0 {
				select {
				case pskRespCh <- appwire.PskAuthStatus(payload[0]):
				default:
				}
			}
		case appwire.AppKind_TaskControl:
			var resp protocol.TaskControlResponse
			if _, err := resp.Decode(payload); err != nil {
				return
			}
			if resp.Kind == protocol.TaskControlKind_ClientHello {
				if r := resp.ClientHello(); r != nil {
					select {
					case helloRespCh <- r.Status:
					default:
					}
				}
			}
		}
	})
	pc.Start(ctx)

	pskCtx, pskCancel := context.WithCancel(ctx)
	go func() {
		defer pskCancel()
		select {
		case <-pc.Done():
		case <-pskCtx.Done():
		}
	}()
	pskErr := cli.SendAndWaitPSK(pskCtx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, pc.Connection().GetTranscript(), pskRespCh)
	pskCancel()
	if pskErr != nil {
		pc.Close()
		return nil, pskErr
	}

	hostname := cliopts.ResolveString(f.Hostname, "HARNESS_HOSTNAME")
	info := protocol.AgentInfo{RunnerId: rid, TaskId: tid, AuthTicket: ticket}
	info.SetHostname([]byte(hostname)) // 0-len when empty is fine
	hello := protocol.ClientHello{Kind: protocol.ClientKind_Agent}
	hello.SetAgentInfo(info) // Kind is set first (discriminator), then the field
	req := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
	req.SetClientHello(hello)
	data := req.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
	if _, _, err := pc.Connection().SendMessage(data); err != nil {
		pc.Close()
		return nil, fmt.Errorf("send hello: %w", err)
	}

	select {
	case status := <-helloRespCh:
		if status != protocol.ClientHelloStatus_Ok {
			pc.Close()
			return nil, fmt.Errorf("hello rejected: %v", status)
		}
	case <-ctx.Done():
		pc.Close()
		return nil, ctx.Err()
	}
	return &Conn{pc: pc, taskID: tid, runnerID: rid}, nil
}
