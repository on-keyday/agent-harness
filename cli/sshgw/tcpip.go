//go:build !js

package sshgw

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/sshgw/sshwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"golang.org/x/crypto/ssh"
)

// parseDirectTCPIP decodes a direct-tcpip channel-open payload (RFC 4254 §7.2)
// into the host and port the runner is being asked to dial.
//
// The originator pair the payload also carries is decoded and dropped: it names
// a socket on the SSH CLIENT's machine, which has no bearing on what the runner
// dials. It is described in sshwire.bgn anyway, because it is on the wire.
func parseDirectTCPIP(payload []byte) (string, int, error) {
	var dt sshwire.DirectTcpip
	if err := dt.DecodeExact(payload); err != nil {
		return "", 0, fmt.Errorf("direct-tcpip payload: %w", err)
	}
	if len(dt.Host) == 0 {
		return "", 0, fmt.Errorf("direct-tcpip: empty host")
	}
	if dt.Port == 0 || dt.Port > 65535 {
		return "", 0, fmt.Errorf("direct-tcpip: port %d out of range", dt.Port)
	}
	return string(dt.Host), int(dt.Port), nil
}

// serveDirectTCPIP answers one direct-tcpip channel — what `ssh -L` opens per
// accepted connection, and what `ssh -W` opens once — by having the task's
// RUNNER dial host:port and splicing the channel to it.
//
// This reverses the gateway's original refusal of the channel type. That
// refusal said the harness already has `forward` and a second path would drift;
// the second half does not follow, because this is not a second path. It is the
// same data plane reached through a different front door: cli.OpenRawForward is
// exactly what `forward -W` calls, and the registration it makes is the same
// one, so a connection forwarded through the gateway is listed and killable by
// `forward ls` / `forward kill` like any other. What the refusal cost was every
// ssh client that does its own forwarding — VS Code Remote-SSH, `ssh -J` chains,
// any tool whose config already says LocalForward.
//
// The listener stays on the SSH CLIENT's side, which is why nothing here binds a
// port and why the gateway never sees an `ssh -L` spec: it only ever sees the
// connections that listener accepted, one channel each.
//
// The user-name suffix does not gate this, matching `exec`. `.control` /
// `.view` / bare choose how a SHELL session ATTACHES, and a forward never
// attaches; treating `.view` as read-only here would advertise an authority
// boundary the gateway does not have, since reaching it at all already means
// holding the operator's credentials.
func (g *Gateway) serveDirectTCPIP(ctx context.Context, user string, newCh ssh.NewChannel) {
	taskID, _, err := ParseUserName(user)
	if err != nil {
		_ = newCh.Reject(ssh.Prohibited, err.Error())
		return
	}
	host, port, err := parseDirectTCPIP(newCh.ExtraData())
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	// The forward is opened BEFORE the channel is accepted, so a task that does
	// not exist or a runner that is offline travels back as a channel-open
	// rejection, where the client prints the reason. Accepting first and then
	// closing would reach the operator as a bare connection reset and read as
	// the TARGET having refused them — a diagnosis pointing at the wrong host.
	rc, err := cli.OpenRawForward(ctx, g.client, taskID, host, port, protocol.ClientEndpointKind_InProcessSshGateway, func(line string) {
		slog.Info("ssh-gateway: " + line)
	})
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, fmt.Sprintf("ssh-gateway: %v", err))
		return
	}
	ch, requests, err := newCh.Accept()
	if err != nil {
		_ = rc.Close()
		return
	}
	// A direct-tcpip channel carries no requests, but an undrained request
	// channel stalls the whole connection, so they are drained and dropped.
	go ssh.DiscardRequests(requests)
	spliceChannelForward(ctx, ch, rc)
}

// spliceChannelForward pumps bytes between an ssh channel and a raw forward
// until either end stops, then tears both down.
//
// Either-side-wins, matching cli.spliceConnStream rather than cli.spliceStdio.
// Both ends here are TCP-shaped, and a half-closed or reset peer must not leave
// the reverse direction parked forever. spliceStdio deliberately survives a
// near-side EOF because its near side is a pipe a shell closes the instant it
// has written a request (`printf 'GET …' | forward -W host:80`); an `ssh -L`
// connection has no such idiom, and its EOF means the TCP peer went away.
func spliceChannelForward(ctx context.Context, ch ssh.Channel, rc *cli.RawConn) {
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = ch.Close()
			_ = rc.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // ssh client -> runner
		defer wg.Done()
		defer teardown()
		buf := make([]byte, 64*1024)
		for {
			n, rerr := ch.Read(buf)
			if n > 0 {
				// Send copies, so reusing buf across iterations is safe.
				if serr := rc.Send(buf[:n]); serr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() { // runner -> ssh client
		defer wg.Done()
		defer teardown()
		for {
			data, eof, rerr := rc.Recv(ctx)
			if len(data) > 0 {
				if _, werr := ch.Write(data); werr != nil {
					return
				}
			}
			if eof || rerr != nil {
				return
			}
		}
	}()
	wg.Wait()
}
