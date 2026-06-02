package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

// ForwardSpec is one parsed -L forward: listen on BindAddr:LocalPort, and
// for each accepted connection have the runner dial RemoteHost:RemotePort.
type ForwardSpec struct {
	BindAddr   string
	LocalPort  int
	RemoteHost string
	RemotePort int
}

// parseForwardSpec parses "[bind:]localport:remotehost:remoteport".
// bind defaults to 127.0.0.1 (do not expose the local port externally).
// IPv6 literal hosts are not supported (dogfood scope).
func parseForwardSpec(s string) (ForwardSpec, error) {
	parts := strings.Split(s, ":")
	var bind, rhost, lportS, rportS string
	switch len(parts) {
	case 3:
		bind = "127.0.0.1"
		lportS, rhost, rportS = parts[0], parts[1], parts[2]
	case 4:
		bind, lportS, rhost, rportS = parts[0], parts[1], parts[2], parts[3]
	default:
		return ForwardSpec{}, fmt.Errorf("forward: bad spec %q (want [bind:]localport:remotehost:remoteport)", s)
	}
	lport, err := strconv.Atoi(lportS)
	if err != nil || lport <= 0 || lport > 65535 {
		return ForwardSpec{}, fmt.Errorf("forward: bad local port in %q", s)
	}
	rport, err := strconv.Atoi(rportS)
	if err != nil || rport <= 0 || rport > 65535 {
		return ForwardSpec{}, fmt.Errorf("forward: bad remote port in %q", s)
	}
	if rhost == "" {
		return ForwardSpec{}, fmt.Errorf("forward: empty remote host in %q", s)
	}
	return ForwardSpec{BindAddr: bind, LocalPort: lport, RemoteHost: rhost, RemotePort: rport}, nil
}

// OpenPortForward asks the server to wire a relayed stream to the runner
// for taskIDHex, which will dial remoteHost:remotePort. Returns the bidi
// stream the caller splices its accepted TCP connection against. Mirrors
// (*Client).OpenFileTransfer. This is a method on the long-lived *Client,
// so the TUI calls it directly on a.client (no fresh dial).
func (c *Client) OpenPortForward(ctx context.Context, taskIDHex, remoteHost string, remotePort int) (trsf.BidirectionalStream, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("forward: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenPortForward}
	body := protocol.OpenPortForwardRequest{
		TaskId:     tid,
		Direction:  protocol.PortForwardDirection_Local,
		RemotePort: uint16(remotePort),
	}
	body.SetRemoteHost([]byte(remoteHost))
	req.SetOpenPortForward(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_OpenPortForward {
		return nil, fmt.Errorf("forward: unexpected response kind %v", resp.Kind)
	}
	r := resp.OpenPortForward()
	if r == nil {
		return nil, errors.New("forward: response variant missing")
	}
	switch r.Status {
	case protocol.OpenPortForwardStatus_Ok:
	case protocol.OpenPortForwardStatus_NoSuchTask:
		return nil, errors.New("forward: no such task (id unknown or task not running)")
	case protocol.OpenPortForwardStatus_RunnerOffline:
		return nil, errors.New("forward: runner offline")
	default:
		return nil, fmt.Errorf("forward: server error (status=%d)", r.Status)
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return nil, fmt.Errorf("forward: stream %d not visible", r.StreamId)
	}
	return st, nil
}

// spliceConnStream pumps bytes between a net.Conn and a trsf bidi stream
// until either direction closes or errors, then tears down both. Same
// either-side-wins teardown as server.spliceBidi (correct for TCP, where a
// half-closed/RST peer must not leave the reverse relay blocked forever).
func spliceConnStream(conn net.Conn, st trsf.BidirectionalStream) {
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = st.CloseBoth()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // conn -> stream
		defer wg.Done()
		defer teardown()
		buf := make([]byte, 64*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := st.AppendData(false, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = st.AppendData(true)
				return
			}
		}
	}()
	go func() { // stream -> conn
		defer wg.Done()
		defer teardown()
		for {
			data, eof, err := st.ReadDirect(64 * 1024)
			if err != nil {
				return
			}
			if len(data) > 0 {
				if _, werr := conn.Write(data); werr != nil {
					return
				}
			}
			if eof {
				return
			}
		}
	}()
	wg.Wait()
}

// RunForward listens for each spec and bridges accepted connections to the
// runner via OpenPortForward. Blocks until ctx is cancelled, then closes all
// listeners. Per-connection errors are logged and isolated; the listener and
// sibling connections are unaffected.
func RunForward(ctx context.Context, c *Client, taskIDHex string, specs []ForwardSpec, logf func(string)) error {
	if logf == nil {
		logf = func(s string) { slog.Info(s) }
	}
	var lns []net.Listener
	for _, sp := range specs {
		ln, err := net.Listen("tcp", net.JoinHostPort(sp.BindAddr, strconv.Itoa(sp.LocalPort)))
		if err != nil {
			for _, l := range lns {
				_ = l.Close()
			}
			return fmt.Errorf("forward: listen %s:%d: %w", sp.BindAddr, sp.LocalPort, err)
		}
		lns = append(lns, ln)
		logf(fmt.Sprintf("forwarding %s:%d -> %s:%d (task %s)", sp.BindAddr, sp.LocalPort, sp.RemoteHost, sp.RemotePort, taskIDHex[:min(12, len(taskIDHex))]))
		go acceptLoop(ctx, c, taskIDHex, sp, ln, logf)
	}
	<-ctx.Done()
	for _, l := range lns {
		_ = l.Close()
	}
	return nil
}

func acceptLoop(ctx context.Context, c *Client, taskIDHex string, sp ForwardSpec, ln net.Listener, logf func(string)) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (ctx done) or fatal accept error
		}
		go func() {
			st, err := c.OpenPortForward(ctx, taskIDHex, sp.RemoteHost, sp.RemotePort)
			if err != nil {
				logf("forward: " + err.Error())
				_ = conn.Close()
				return
			}
			spliceConnStream(conn, st)
		}()
	}
}
