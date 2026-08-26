//go:build !js

package sshgw

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"golang.org/x/crypto/ssh"
)

// DefaultListen is where the gateway binds when the operator names no address.
// Loopback, so the default configuration is the one that needs no keys.
const DefaultListen = "127.0.0.1:2222"

// Options configures one gateway listener.
type Options struct {
	// Listen is the bind address. Off loopback, AuthorizedKeysPath becomes
	// mandatory — see BuildServerConfig.
	Listen string
	// HostKeyPath is the ed25519 host key; generated on first run, then reused.
	HostKeyPath string
	// AuthorizedKeysPath is optional on a loopback bind. When set, every
	// connection is gated against it wherever the listener is bound.
	AuthorizedKeysPath string
}

// Gateway is a bound listener, ready to serve. Listen returns one; Serve runs
// it. The two are separate so a caller can report a bind failure as a failure
// rather than as a gateway that started and then stopped — the same split
// tui.DoStartRemoteForward makes for the same reason.
type Gateway struct {
	client *cli.Client
	ln     net.Listener
	cfg    *ssh.ServerConfig
	// harnessDone is the client's peer.Conn.Done — see Serve for why a gateway
	// needs it spelled out where every sibling gets it for free. Nil only in
	// unit tests, which never Serve; a nil channel is never ready in a select,
	// so it needs no guard at the point of use.
	harnessDone <-chan struct{}

	mu     sync.Mutex
	seats  map[string]bool   // task id → a control session is live here
	conns  map[net.Conn]bool // ssh connections accepted and not yet finished
	closed bool              // shutdown ran; late Accepts are refused
}

func newGateway(c *cli.Client) *Gateway {
	g := &Gateway{client: c, seats: map[string]bool{}, conns: map[net.Conn]bool{}}
	if c != nil {
		g.harnessDone = c.Peer().Done()
	}
	return g
}

// claim reserves what the mode needs, reporting whether the session may start.
//
// Only control needs anything. It is the one attach that evicts another client
// server-side (SessionMux.Attach cancels the previous controller and closes its
// stream), so a second control session through this gateway would take the seat
// from whatever holds it — including the operator's own TUI. Refusing is
// visible to whoever typed the command; taking is not.
func (g *Gateway) claim(taskID string, mode protocol.AttachMode) bool {
	if mode != protocol.AttachMode_Control {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seats[taskID] {
		return false
	}
	g.seats[taskID] = true
	return true
}

// release gives back what claim took. Modes that claim nothing release nothing:
// a cowriter ending must not free a controller's seat.
func (g *Gateway) release(taskID string, mode protocol.AttachMode) {
	if mode != protocol.AttachMode_Control {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seats, taskID)
}

// Listen loads the keys, builds the ssh configuration and binds the socket.
// Everything that can fail because of how the operator configured this fails
// here, before anything reports itself as running.
//
// c is an already-connected client: the TUI passes its long-lived one and
// harness-cli passes the one it dialled. The gateway never dials, so there is
// no short-lived form for a *With split to distinguish.
func Listen(c *cli.Client, opts Options) (*Gateway, error) {
	if opts.Listen == "" {
		opts.Listen = DefaultListen
	}
	if opts.HostKeyPath == "" {
		opts.HostKeyPath = DefaultHostKeyPath("")
	}
	hostKey, err := LoadOrCreateHostKey(opts.HostKeyPath)
	if err != nil {
		return nil, err
	}
	var authorized []ssh.PublicKey
	if opts.AuthorizedKeysPath != "" {
		if authorized, err = LoadAuthorizedKeys(opts.AuthorizedKeysPath); err != nil {
			return nil, err
		}
	}
	cfg, err := BuildServerConfig(hostKey, authorized, opts.Listen)
	if err != nil {
		return nil, err
	}
	g := newGateway(c)
	// A gateway whose harness connection is ALREADY gone would bind, serve, and
	// stop again in the same breath — the started-then-contradicted shape this
	// split exists to avoid. The TUI can hand one over: between a disconnect and
	// the reconnect that rebinds a.client, the client it holds is the dead one.
	select {
	case <-g.harnessDone:
		return nil, fmt.Errorf("ssh-gateway: not connected to the harness server; nothing to attach through")
	default:
	}
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("ssh-gateway: listen %s: %w", opts.Listen, err)
	}
	g.ln, g.cfg = ln, cfg
	return g, nil
}

// Addr is the address actually bound, which is what to tell the operator when
// they asked for port 0.
func (g *Gateway) Addr() string { return g.ln.Addr().String() }

// ErrHarnessDisconnected ends Serve when the harness connection the gateway
// serves from goes away. It is an error rather than an ordinary stop because
// nobody asked for it: `harness-cli ssh-gateway` exits non-zero on it, and the
// TUI prints it and clears the gateway it was showing as listening.
var ErrHarnessDisconnected = errors.New(
	"ssh-gateway: the harness connection closed; the listener stopped, and so did every session it served")

// Close stops the listener and drops the ssh connections accepted through it.
// Serve returns shortly after. Idempotent.
func (g *Gateway) Close() error { g.shutdown(); return nil }

// shutdown closes the listener, closes every ssh connection still being served,
// and latches so an Accept that was already in flight cannot register a new one.
//
// Closing the accepted sockets is not tidiness. The goroutines serving them
// block reading the SSH transport, which no context reaches, so without this a
// stopped gateway kept them — and every session pumping through them — alive.
// `harness-cli ssh-gateway` only ever looked correct here because the process
// exits on Ctrl-C and takes them with it; the TUI, which stays, did not.
func (g *Gateway) shutdown() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	conns := make([]net.Conn, 0, len(g.conns))
	for c := range g.conns {
		conns = append(conns, c)
	}
	g.conns = nil
	g.mu.Unlock()
	_ = g.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

// track registers an accepted connection, reporting false once shutdown has
// run — the caller closes it rather than serving a gateway that is stopping.
func (g *Gateway) track(c net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.conns[c] = true
	return true
}

// untrack forgets a connection whose serveConn has returned. Deleting from the
// nil map shutdown leaves behind is a no-op, which is the point: a connection
// closing during shutdown must not have to coordinate with it.
func (g *Gateway) untrack(c net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.conns, c)
}

// Serve accepts connections until ctx is cancelled, the harness connection dies,
// or the listener fails. A cancelled context is an ordinary stop and returns nil.
//
// The harness connection has to be watched EXPLICITLY here, and it is the one
// place in this layer where that is true. Every sibling long-lived client verb
// blocks on a stream the connection carries, so it dies with the connection for
// free — see the reconnect comment in tui/app.go: the forwards "die with the
// connection that held their control streams". A gateway holds no such stream.
// It blocks in Accept on a local socket that knows nothing about objproto, so
// without this the connection could close and leave the port bound, `ssh-gateway
// status` still claiming to listen, and every attach through it failing against
// a client that can no longer reach the server — until the operator stopped it
// by hand.
func (g *Gateway) Serve(ctx context.Context) error {
	// Accept blocks on the listener, not on ctx, so cancellation has to reach
	// it by closing the socket out from under it. lost distinguishes the two
	// endings that both arrive here as a closed listener.
	served := make(chan struct{})
	defer close(served)
	lost := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-g.harnessDone:
			close(lost)
		case <-served:
			return // Serve already returned; nothing to tear down
		}
		g.shutdown()
	}()
	for {
		nConn, aerr := g.ln.Accept()
		if aerr != nil {
			select {
			case <-lost:
				return ErrHarnessDisconnected
			default:
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ssh-gateway: accept: %w", aerr)
		}
		if !g.track(nConn) {
			_ = nConn.Close()
			continue
		}
		go g.serveConn(ctx, nConn, g.cfg)
	}
}

// Run is Listen followed by Serve, for a caller with nothing to do in between.
func Run(ctx context.Context, c *cli.Client, opts Options) error {
	g, err := Listen(c, opts)
	if err != nil {
		return err
	}
	defer g.Close()
	return g.Serve(ctx)
}

// serveConn completes the ssh handshake and dispatches the connection's
// channels. One connection may open several; each gets its own session.
func (g *Gateway) serveConn(ctx context.Context, nConn net.Conn, cfg *ssh.ServerConfig) {
	defer g.untrack(nConn)
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		// A failed handshake is ordinary — a port scan, a wrong key, a client
		// that hung up. There is nobody to tell.
		_ = nConn.Close()
		return
	}
	defer sshConn.Close()
	// Global requests (keepalive@openssh.com and friends) must be drained or
	// the connection stalls; none of them mean anything here.
	//
	// `ssh -R` arrives here as the tcpip-forward global request and is refused
	// by the drain. No sentence goes with that refusal, and not for lack of
	// trying: SSH_MSG_REQUEST_FAILURE carries no reason field, so a global
	// request can only be answered yes or no. `harness-cli forward -R` is the
	// verb that does this; direct-tcpip below is the half of forwarding an ssh
	// client can be told about.
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			go g.serveSession(ctx, sshConn.User(), newCh)
		case "direct-tcpip":
			// `ssh -L` and `ssh -W` through the gateway. See serveDirectTCPIP
			// for why this is served now after being refused for one release.
			go g.serveDirectTCPIP(ctx, sshConn.User(), newCh)
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, fmt.Sprintf(
				"ssh-gateway serves session and direct-tcpip channels (got %q)",
				newCh.ChannelType()))
		}
	}
}

// HostOf and PortOf split a bind address for an `ssh -p PORT user@HOST` hint.
// An address that does not split is returned whole rather than guessed at: the
// hint is a convenience, and a wrong one is worse than none.
func HostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return addr
	}
	return host
}

func PortOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return port
}
