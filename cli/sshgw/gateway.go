//go:build !js

package sshgw

import (
	"context"
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

	mu    sync.Mutex
	seats map[string]bool // task id → a control session is live here
}

func newGateway(c *cli.Client) *Gateway {
	return &Gateway{client: c, seats: map[string]bool{}}
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
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("ssh-gateway: listen %s: %w", opts.Listen, err)
	}
	g := newGateway(c)
	g.ln, g.cfg = ln, cfg
	return g, nil
}

// Addr is the address actually bound, which is what to tell the operator when
// they asked for port 0.
func (g *Gateway) Addr() string { return g.ln.Addr().String() }

// Close stops the listener. Serve returns shortly after.
func (g *Gateway) Close() error { return g.ln.Close() }

// Serve accepts connections until ctx is cancelled or the listener fails.
// A cancelled context is an ordinary stop and returns nil.
func (g *Gateway) Serve(ctx context.Context) error {
	// Accept blocks on the listener, not on ctx, so cancellation has to reach
	// it by closing the socket out from under it.
	go func() {
		<-ctx.Done()
		_ = g.ln.Close()
	}()
	for {
		nConn, aerr := g.ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ssh-gateway: accept: %w", aerr)
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
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			// direct-tcpip lands here: `ssh -L` through the gateway would be a
			// second, drifting path to what `harness-cli forward` already does.
			_ = newCh.Reject(ssh.UnknownChannelType, fmt.Sprintf(
				"ssh-gateway serves only session channels (got %q); use `harness-cli forward` for port forwarding",
				newCh.ChannelType()))
			continue
		}
		go g.serveSession(ctx, sshConn.User(), newCh)
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
