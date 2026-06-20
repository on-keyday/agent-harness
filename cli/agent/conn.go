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

// Conn wraps a peer.Conn that has completed a ClientHello handshake.
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

// ConnectAgent dials the harness server, performs the merged PSK+identity
// handshake (PskAuthRequest{role=client, client_hello=Agent}), awaits the
// PskAuthResponse, and returns the open Conn. Caller must Close it.
//
// Identity is established structurally in the merged handshake — no separate
// TaskControl ClientHello is sent. cli.SendMergedHandshake is reused directly;
// the binder logic lives exclusively there.
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
	// Receives exactly one PskAuthResponse (brgen-decoded) from the server.
	// Mirrors cli.Client.Dial wiring exactly.
	pskRespCh := make(chan protocol.PskAuthResponse, 1)

	// Combined handler: decodes PskAuthResponse during the merged handshake,
	// then becomes a no-op (subcommands install their own handler via SetOnControl).
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		if kind == appwire.AppKind_PskAuth && len(payload) > 0 {
			var resp protocol.PskAuthResponse
			if _, err := resp.Decode(payload); err == nil {
				select {
				case pskRespCh <- resp:
				default:
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
	// SendMergedHandshake builds PskAuthRequest{role=client, binder (or empty),
	// client_hello=<buildMergedClientHello(Cli)>}. When the in-task agent env
	// (HARNESS_RUNNER_ID / HARNESS_TASK_ID / HARNESS_AUTH_TICKET) is fully
	// populated, buildMergedClientHello auto-upgrades kind to Agent with AgentInfo —
	// agentboard connections are always in-task agents, so the ClientHello carries
	// kind=Agent + ticket. Binder logic is not duplicated here.
	pskErr := cli.SendMergedHandshake(pskCtx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, pc.Connection().GetTranscript(), protocol.ClientKind_Cli, pskRespCh)
	pskCancel()
	if pskErr != nil {
		pc.Close()
		return nil, pskErr
	}

	return &Conn{pc: pc, taskID: tid, runnerID: rid}, nil
}
