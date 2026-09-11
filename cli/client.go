package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
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
	pending map[uint32]chan taskControlResult
}

// taskControlResult is what dispatchControl delivers to a waiting
// RoundTripTaskControl: the decoded response, or the reason there will never
// be one. The err arm exists for version skew — a response this build cannot
// decode (an older server answering a newer client) must FAIL the waiting
// call, not strand it: most CLI paths wait on context.Background(), so a
// dropped response was measured as a silent hang, with the decode error only
// on a log line nobody is reading mid-hang.
type taskControlResult struct {
	resp *protocol.TaskControlResponse
	err  error
}

// ErrResponseUndecodable marks a TaskControl response the local build could
// not decode — in practice a version-skewed server (its response is missing a
// field this build reads, or carries an arm it cannot parse). Retrying on the
// same connection cannot help; the fix is upgrading/restarting the SERVER.
var ErrResponseUndecodable = errors.New("task control response undecodable (version-skewed server? restart/upgrade the server)")

// Dial establishes the underlying peer.Conn and starts the receive loop
// with this Client's TaskControl-aware handler. The peerCID identifies
// which server peer to ECDH with (e.g. parsed from --server-cid). Pubsub-
// kind responses are handled by peer.Conn directly (it routes them to its
// pubsub.Client); TaskControl-kind responses land in c.dispatchControl below.
//
// kind is the ClientKind to announce in the merged PSK+identity handshake.
// Operator surfaces pass protocol.ClientKind_Cli / _Tui / _Webui; the merged
// builder auto-upgrades to Agent when the in-task env (HARNESS_RUNNER_ID /
// HARNESS_TASK_ID / HARNESS_AUTH_TICKET) is fully populated. Identity is
// established structurally in this first message — no separate SayHello call
// is needed or expected after Dial returns.
//
// When HARNESS_PROXY_VIA_RUNNER is set in the env, Dial routes through the
// Phase B objproto negotiated-proxy path (DialViaProxy) instead of dialing
// the server directly. HARNESS_TASK_ID is read for the proxy ceremony's
// task-binding check; if proxy_via is set but task_id is missing/invalid,
// Dial returns an error (no silent fall-back). Admin invocations from a
// laptop without HARNESS_PROXY_VIA_RUNNER keep dialing directly.
// A process that dials more than once — anything under PersistLoop — must use
// DialWith and keep ONE endpoint across reconnects. See
// peer.StartEndpointMaintenance for why that is the rule and not an
// optimisation.
func Dial(ctx context.Context, peerCID objproto.ConnectionID, kind protocol.ClientKind) (*Client, error) {
	pc, err := DialPeerConn(ctx, peerCID)
	if err != nil {
		return nil, err
	}
	return dialOn(ctx, pc, kind)
}

// DialWith is Dial on an endpoint the caller owns and keeps. Every reconnect
// opens a new Connection on that ONE endpoint, which is what an endpoint is
// for; the caller builds it once with NewProcessEndpoint.
func DialWith(ctx context.Context, ep objproto.Endpoint, peerCID objproto.ConnectionID, kind protocol.ClientKind) (*Client, error) {
	pc, err := DialPeerConnWith(ctx, ep, peerCID)
	if err != nil {
		return nil, err
	}
	return dialOn(ctx, pc, kind)
}

// dialOn is everything Dial does AFTER the connection exists: the merged
// PSK+identity handshake and the control-handler wiring. Both entry points
// share it, so the two cannot drift in what they authenticate.
func dialOn(ctx context.Context, pc *peer.Conn, kind protocol.ClientKind) (*Client, error) {
	c := &Client{
		conn:    pc,
		pending: map[uint32]chan taskControlResult{},
	}

	// Pick the binder secret by role: operator surfaces prove the operator psk
	// (HARNESS_OPERATOR_PSK), in-task agents prove the connect psk (HARNESS_PSK).
	// See resolveBinderPSK — keeping them in distinct env vars stops a runner
	// from inheriting the operator secret and leaking it to agents.
	psk := resolveBinderPSK()
	// Receives exactly one PskAuthResponse (brgen-decoded) from the server.
	pskRespCh := make(chan protocol.PskAuthResponse, 1)

	// Combined handler: PskAuth response during merged handshake, TaskControl after.
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		if kind == appwire.AppKind_PskAuth && len(payload) > 0 {
			var resp protocol.PskAuthResponse
			if _, err := resp.Decode(payload); err == nil {
				select {
				case pskRespCh <- resp:
				default:
				}
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
	pskErr := SendMergedHandshake(pskCtx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, pc.Connection().GetTranscript(), kind, pskRespCh)
	pskCancel()
	if pskErr != nil {
		pc.Close()
		// Only a NON-retryable explicit rejection (wrong PSK / bad ticket) is
		// fatal. A transport drop mid-handshake, or a RETRYABLE rejection
		// (NoIdentity = the server could not decode our hello, i.e. wire skew),
		// propagates as a plain error so PersistLoop reconnects instead of
		// exiting. See PskRejectedError.Retryable.
		var rej *PskRejectedError
		if errors.As(pskErr, &rej) && !rej.Retryable() {
			return nil, &PSKAuthError{Err: pskErr}
		}
		return nil, pskErr
	}

	// Merged handshake complete — switch to the pure app handler.
	pc.SetOnControl(c.dispatchControl)
	return c, nil
}

// NewProcessEndpoint builds the endpoint a long-lived client keeps for its
// whole life and starts its sweepers. Call it ONCE, outside any reconnect
// loop, and pass the result to DialWith on every attempt.
func NewProcessEndpoint(peerCID objproto.ConnectionID) (objproto.Endpoint, error) {
	ep, err := BuildClientEndpoint(peerCID)
	if err != nil {
		return nil, err
	}
	peer.StartEndpointMaintenance(ep)
	return ep, nil
}

// dispatchControl is the peer ControlHandler. We only care about TaskControl
// kind — everything else (RunnerControl is server-side only here) is dropped.
//
// A response that does not DECODE is still routed, as an error: RequestId is
// the second field, parsed before whichever arm failed, so the partially
// decoded struct almost always knows which caller is waiting. Dropping the
// response instead was measured (2026-08-21, new client × pre-kind-field
// server) as a silent hang — RoundTripTaskControl's only other exit is its
// context, and most CLI paths pass context.Background().
func (c *Client) dispatchControl(kind appwire.AppKind, payload []byte) {
	if kind != appwire.AppKind_TaskControl {
		return
	}
	resp := &protocol.TaskControlResponse{}
	if _, derr := resp.Decode(payload); derr != nil {
		slog.Error("cli.Client: decode TaskControlResponse", "err", derr)
		// Route the failure only when RequestId (kind u8 + request_id u32 =
		// the first 5 bytes) was actually parsed. On a shorter payload the
		// zero-valued field would name request 0 — a real id (nextReq starts
		// at 0), so delivering would fail an unrelated caller.
		if len(payload) >= 5 {
			c.deliver(resp.RequestId, taskControlResult{err: fmt.Errorf("%w: %w", ErrResponseUndecodable, derr)})
		}
		return
	}
	c.deliver(resp.RequestId, taskControlResult{resp: resp})
}

// deliver hands a result to the caller waiting on requestID, if any.
func (c *Client) deliver(requestID uint32, r taskControlResult) {
	c.mu.Lock()
	ch, ok := c.pending[requestID]
	if ok {
		delete(c.pending, requestID)
	}
	c.mu.Unlock()
	if ok {
		ch <- r
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
	ch := make(chan taskControlResult, 1)
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
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		resp := r.resp
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
// + objproto connection shutdown). It does NOT touch the objproto.Endpoint,
// and that is the ownership rule rather than a gap: the Endpoint is
// process-scoped and bundles every connection made on it, so what a Close
// ends is the Connection. peer.StartEndpointMaintenance carries the full
// statement.
//
// A *Client does not survive a reconnect — Dial binds the PSK handshake, the
// pending map and the control handler to one conn — so a reconnecting process
// builds a new Client each time and keeps the SAME endpoint: NewProcessEndpoint
// once, DialWith per attempt. (An earlier version of this comment said
// long-running embedders "reuse the same *Client for the lifetime of the
// program", which stopped being true the day PersistLoop landed.)
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
// It builds an endpoint of its own, which is right for a one-shot process and
// wrong for one that dials again: DialPeerConnWith is that caller's entry.
func DialPeerConn(ctx context.Context, peerCID objproto.ConnectionID) (*peer.Conn, error) {
	if strings.TrimSpace(os.Getenv("HARNESS_PROXY_VIA_RUNNER")) == "" {
		ep, err := NewProcessEndpoint(peerCID)
		if err != nil {
			return nil, err
		}
		return DialPeerConnWith(ctx, ep, peerCID)
	}
	return dialViaProxyFromEnv(ctx, peerCID)
}

// DialPeerConnWith dials on an endpoint the caller owns.
//
// The proxy branch is deliberately NOT given the endpoint: it dials a runner's
// listen address, which may be another transport entirely, and DialViaProxy
// builds what that leg needs. In practice no reconnecting process takes it —
// HARNESS_PROXY_VIA_RUNNER marks an in-task agent, and those are one-shot.
func DialPeerConnWith(ctx context.Context, ep objproto.Endpoint, peerCID objproto.ConnectionID) (*peer.Conn, error) {
	if strings.TrimSpace(os.Getenv("HARNESS_PROXY_VIA_RUNNER")) != "" {
		return dialViaProxyFromEnv(ctx, peerCID)
	}
	return peer.Dial(ctx, ep, peerCID, peer.DialConfig{
		Logger: slog.Default(),
	})
}

func dialViaProxyFromEnv(ctx context.Context, peerCID objproto.ConnectionID) (*peer.Conn, error) {
	proxyVia := strings.TrimSpace(os.Getenv("HARNESS_PROXY_VIA_RUNNER"))

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
