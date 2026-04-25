// Package peer is the shared endpoint-side wrapper that both the cli and the
// runner build on top of. It owns the WS + ECDH + trsf + objproto plumbing
// and the receive-loop dispatch, leaving each side to layer its own control
// payload handler (TaskControl RPC pending for cli, RunnerRequest dispatch
// for the runner) on top via SetOnControl.
//
// Lifecycle:
//
//	pc, err := peer.Dial(ctx, peer.DialConfig{...})  // WS+ECDH+trsf+AutoSend+AutoPing only
//	pc.SetOnControl(handler)                          // optional; before Start
//	pc.Start(ctx)                                     // spawns AutoReceive goroutine
//	defer pc.Close()
//	... use pc.Session() / pc.Transport() / pc.Pubsub() / pc.Publish(...) ...
//	return pc.Wait(ctx)                               // long-running endpoints only
package peer

import (
	"context"
	"crypto/ecdh"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// ControlHandler receives every application-kind payload that isn't Pubsub
// (those are routed to the embedded pubsub.Client first). The kind argument
// is the original wire.ApplicationPayloadKind from msg.Data[0]; payload is
// msg.Data[1:].
type ControlHandler func(kind wire.ApplicationPayloadKind, payload []byte)

// Conn wraps an objproto.Connection together with its trsf.Transport and a
// pubsub.Client correlator. Both cli.Client and the runner embed one of
// these and layer their RPC / dispatch logic on top.
type Conn struct {
	sess  objproto.Connection
	trans trsf.Transport
	pub   *pubsub.Client
	log   *slog.Logger

	onControl atomic.Pointer[ControlHandler]
	started   atomic.Bool
	done      chan struct{}

	pubmu     sync.Mutex
	pubTopics map[string]*pubTopic
}

// DialConfig configures a peer.Conn.
type DialConfig struct {
	// Addr is the server's "host:port".
	Addr string
	// UniqueNumber is the trailing token in the synthetic ConnectionID
	// the endpoint uses ("ws:127.0.0.1:<port>-<UniqueNumber>"). Runner
	// uses 1111, cli uses 2222 — match what the server's registry expects.
	UniqueNumber uint16
	// Logger; defaults to slog.Default() when nil.
	Logger *slog.Logger
	// PingInterval; defaults to 30s when zero.
	PingInterval time.Duration
}

// Dial opens a WebSocket session, runs the ECDH handshake, sets up the trsf
// transport, and starts AutoSend + AutoPing. AutoReceive is NOT started yet
// — the caller must call SetOnControl (optional) and then Start before any
// inbound message can be processed. This split exists so callers whose
// handler depends on values produced by Dial (e.g. the runner's session,
// which holds the peer.Conn-backed Sender) can finish wiring before the
// receive loop begins.
func Dial(ctx context.Context, cfg DialConfig) (*Conn, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}

	sess, err := transport.WebSocketSession(cfg.Logger, cfg.Addr, nil, objproto.SessionModeClient)
	if err != nil {
		return nil, fmt.Errorf("ws session: %w", err)
	}
	cidStr := fmt.Sprintf("ws:127.0.0.1:%s-%d", portFrom(cfg.Addr), cfg.UniqueNumber)
	conn, err := objproto.DoECDHHandshake(ctx, sess,
		objproto.MustParseConnectionID(cidStr),
		ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, cfg.Logger)

	c := &Conn{
		sess:      conn,
		trans:     p,
		pub:       pubsub.NewClient(),
		log:       cfg.Logger,
		done:      make(chan struct{}),
		pubTopics: map[string]*pubTopic{},
	}
	go trsf.AutoSend(ctx, p, conn, nil)
	go trsf.AutoPing(ctx, conn, cfg.PingInterval)
	return c, nil
}

// SetOnControl registers (or replaces) the handler invoked from the receive
// goroutine for every non-Pubsub payload kind. Safe to call before or after
// Start; safe to call concurrently with the receive loop.
func (c *Conn) SetOnControl(h ControlHandler) {
	if h == nil {
		c.onControl.Store(nil)
		return
	}
	c.onControl.Store(&h)
}

// Start spawns the AutoReceive goroutine. Idempotent — second and later
// calls are no-ops, so callers can defensively call Start without tracking
// state. The goroutine runs until ctx is cancelled or the underlying
// connection closes (peer-sent Close, network error, etc).
func (c *Conn) Start(ctx context.Context) {
	if !c.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer close(c.done)
		trsf.AutoReceive(ctx, c.trans, c.sess, c.dispatch)
	}()
}

// Wait blocks until the AutoReceive goroutine returns OR ctx is cancelled.
// Returns ctx.Err() in the latter case, nil in the former. If Start was
// never called, returns immediately with nil — there is nothing to wait on.
func (c *Conn) Wait(ctx context.Context) error {
	if !c.started.Load() {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return nil
	}
}

// Close sends a wire-level Close to the peer (best-effort; lets the server
// deregister the runner / drop the subscriber immediately instead of
// waiting for the idle GC) and then releases the underlying objproto
// session.
func (c *Conn) Close() {
	_ = trsf.SendClose(c.sess)
	_ = c.sess.Close()
}

// Session returns the underlying objproto.Connection. Callers use it for
// raw SendMessage when they want bypass the pubsub.Client / Publish helpers
// (e.g. issuing a TaskControl request, sending the runner Hello).
func (c *Conn) Session() objproto.Connection { return c.sess }

// Transport returns the trsf.Transport. Callers use it to look up streams
// by id (GetBidirectionalStream / GetReceiveStream) when an RPC response
// hands back a stream_id.
func (c *Conn) Transport() trsf.Transport { return c.trans }

// Pubsub returns the request_id-correlator for JOIN/LEAVE responses on
// this connection. Outbound JOIN/LEAVE bytes still go through Session's
// SendMessage; this just bookkeeps the response handler map.
func (c *Conn) Pubsub() *pubsub.Client { return c.pub }

// Logger returns the logger this Conn was constructed with.
func (c *Conn) Logger() *slog.Logger { return c.log }

// dispatch is the AutoReceive callback. It strips the wire kind byte,
// routes Pubsub-kind messages to the embedded pubsub.Client (always —
// this is the one piece of routing peer owns), and forwards the rest to
// the registered ControlHandler. With no handler set, non-Pubsub kinds
// are dropped silently.
func (c *Conn) dispatch(msg *objproto.Message, err error) {
	if err != nil {
		// io.EOF when the peer sent Close; any other err signals network failure.
		return
	}
	if msg == nil || len(msg.Data) == 0 {
		return
	}
	kind := wire.ApplicationPayloadKind(msg.Data[0])
	if kind == wire.ApplicationPayloadKind_Pubsub {
		c.pub.HandleResponse(msg.Data[1:])
		return
	}
	if h := c.onControl.Load(); h != nil {
		(*h)(kind, msg.Data[1:])
	}
}

// portFrom extracts the port portion from a "host:port" string. Falls back
// to the full string if no colon is found.
func portFrom(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}
