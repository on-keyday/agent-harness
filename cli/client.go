package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/objtrsf/objproto"
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
//
// When HARNESS_PROXY_VIA_RUNNER is set in the env, Dial routes through the
// Phase B objproto negotiated-proxy path (DialViaProxy) instead of dialing
// the server directly. HARNESS_TASK_ID is read for the proxy ceremony's
// task-binding check; if proxy_via is set but task_id is missing/invalid,
// Dial returns an error (no silent fall-back). Admin invocations from a
// laptop without HARNESS_PROXY_VIA_RUNNER keep dialing directly.
func Dial(ctx context.Context, peerCID objproto.ConnectionID) (*Client, error) {
	pc, err := DialPeerConn(ctx, peerCID)
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:    pc,
		pending: map[uint32]chan *protocol.TaskControlResponse{},
	}

	psk := GetPSK()
	pskRespCh := make(chan appwire.PskAuthStatus, 1)

	// Combined handler: PSK response during handshake, TaskControl after.
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		if kind == appwire.AppKind_PskAuth && len(payload) > 0 {
			select {
			case pskRespCh <- appwire.PskAuthStatus(payload[0]):
			default:
			}
			return
		}
		c.dispatchControl(kind, payload)
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
	pskErr := SendAndWaitPSK(pskCtx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, pc.Connection().GetTranscript(), pskRespCh)
	pskCancel()
	if pskErr != nil {
		pc.Close()
		return nil, &PSKAuthError{Err: pskErr}
	}

	// PSK exchange complete — switch to the pure app handler.
	pc.SetOnControl(c.dispatchControl)
	return c, nil
}

// dispatchControl is the peer ControlHandler. We only care about TaskControl
// kind — everything else (RunnerControl is server-side only here) is dropped.
func (c *Client) dispatchControl(kind appwire.AppKind, payload []byte) {
	if kind != appwire.AppKind_TaskControl {
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
	data := req.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
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
		// Single-point PermissionDenied recognition: if the server rejects an
		// operation due to missing capability, surface a typed error rather than
		// returning the raw response. All native and wasm callers funnel through
		// RoundTripTaskControl, so this one check covers every gated operation.
		if resp.Kind == protocol.TaskControlKind_PermissionDenied &&
			req.Kind != protocol.TaskControlKind_PermissionDenied {
			pd := resp.PermissionDenied()
			if pd != nil {
				return nil, &CapabilityDeniedError{
					RequestedKind: pd.RequestedKind,
					RequiredCap:   pd.RequiredCap,
				}
			}
		}
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

// DialPeerConn establishes a peer.Conn to the server, transparently routing
// through a runner proxy when HARNESS_PROXY_VIA_RUNNER is set in the env.
//
// All harness-cli subcommands — admin tools (ls, submit, cancel, ...) AND
// agent-side helpers (agent send, file push from inside claude, ...) — go
// through here. Detection is purely env-based:
//
//   - HARNESS_PROXY_VIA_RUNNER unset/empty → direct dial peer.Dial(peerCID)
//   - HARNESS_PROXY_VIA_RUNNER set         → DialViaProxy(parsed, taskID)
//
// On the proxy path HARNESS_TASK_ID is required (Phase B ceremony binds the
// proxy_request to a task running on the proxy_runner). The env always sets
// it inside runner-spawned processes; missing it surfaces as a loud error
// rather than a silent direct-dial fall-back.
func DialPeerConn(ctx context.Context, peerCID objproto.ConnectionID) (*peer.Conn, error) {
	proxyVia := strings.TrimSpace(os.Getenv("HARNESS_PROXY_VIA_RUNNER"))
	if proxyVia == "" {
		ep, err := BuildClientEndpoint(peerCID)
		if err != nil {
			return nil, err
		}
		go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)
		return peer.Dial(ctx, ep, peerCID, peer.DialConfig{
			Logger: slog.Default(),
		})
	}

	proxyCID, err := cliopts.ResolveServerCID(proxyVia)
	if err != nil {
		return nil, fmt.Errorf("HARNESS_PROXY_VIA_RUNNER parse: %w", err)
	}
	taskID, err := cliopts.ResolveTaskID("")
	if err != nil {
		return nil, fmt.Errorf("HARNESS_PROXY_VIA_RUNNER set but HARNESS_TASK_ID missing: %w", err)
	}
	slog.Info("cli: dialing server via runner proxy (Phase B)",
		"proxy_cid", proxyCID.String(),
		"server_cid", peerCID.String())
	return DialViaProxy(ctx, proxyCID, taskID)
}
