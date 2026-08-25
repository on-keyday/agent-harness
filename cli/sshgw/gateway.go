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

type gateway struct {
	client *cli.Client

	mu    sync.Mutex
	seats map[string]bool // task id → a control session is live here
}

func newGateway(c *cli.Client) *gateway {
	return &gateway{client: c, seats: map[string]bool{}}
}

// claim reserves what the mode needs, reporting whether the session may start.
//
// Only control needs anything. It is the one attach that evicts another client
// server-side (SessionMux.Attach cancels the previous controller and closes its
// stream), so a second control session through this gateway would take the seat
// from whatever holds it — including the operator's own TUI. Refusing is
// visible to whoever typed the command; taking is not.
func (g *gateway) claim(taskID string, mode protocol.AttachMode) bool {
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
func (g *gateway) release(taskID string, mode protocol.AttachMode) {
	if mode != protocol.AttachMode_Control {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seats, taskID)
}

// Run serves ssh connections until ctx is cancelled or the listener fails.
//
// c is an already-connected client: the TUI passes its long-lived one and
// harness-cli passes the one it dialled. The gateway never dials, so there is
// no short-lived form for a *With split to distinguish.
func Run(ctx context.Context, c *cli.Client, opts Options) error {
	if opts.Listen == "" {
		opts.Listen = DefaultListen
	}
	hostKey, err := LoadOrCreateHostKey(opts.HostKeyPath)
	if err != nil {
		return err
	}
	var authorized []ssh.PublicKey
	if opts.AuthorizedKeysPath != "" {
		if authorized, err = LoadAuthorizedKeys(opts.AuthorizedKeysPath); err != nil {
			return err
		}
	}
	cfg, err := BuildServerConfig(hostKey, authorized, opts.Listen)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("ssh-gateway: listen %s: %w", opts.Listen, err)
	}
	defer ln.Close()
	// Accept blocks on the listener, not on ctx, so cancellation has to reach
	// it by closing the socket out from under it.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	g := newGateway(c)
	for {
		nConn, aerr := ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil // cancelled: an ordinary stop, not a failure
			}
			return fmt.Errorf("ssh-gateway: accept: %w", aerr)
		}
		go g.serveConn(ctx, nConn, cfg)
	}
}

// serveConn completes the ssh handshake and dispatches the connection's
// channels. One connection may open several; each gets its own session.
func (g *gateway) serveConn(ctx context.Context, nConn net.Conn, cfg *ssh.ServerConfig) {
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
