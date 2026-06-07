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
	"github.com/on-keyday/objtrsf/trsf"
)

// ForwardSpec is one parsed -L forward: listen on BindAddr:LocalPort, and
// for each accepted connection have the runner dial RemoteHost:RemotePort.
type ForwardSpec struct {
	BindAddr   string
	LocalPort  int
	RemoteHost string
	RemotePort int
}

// ParseForwardSpec parses "[bind:]localport:remotehost:remoteport".
// bind defaults to 127.0.0.1 (do not expose the local port externally).
// IPv6 literal hosts are not supported (dogfood scope).
func ParseForwardSpec(s string) (ForwardSpec, error) {
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
				// AppendData stores the slice by reference and copies it
				// asynchronously (see trsf/send_stream.go: "data must be
				// copied before calling AppendData"). buf is reused next
				// iteration, so hand AppendData its own copy.
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if werr := st.AppendData(false, chunk); werr != nil {
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

// RemoteForwardSpec is one parsed -R forward: the runner listens on
// BindAddr:RunnerPort, and for each accepted connection the client dials
// DialHost:DialPort.
type RemoteForwardSpec struct {
	BindAddr   string
	RunnerPort int
	DialHost   string
	DialPort   int
}

// ParseRemoteForwardSpec parses "[bind:]runnerport:dialhost:dialport".
// bind defaults to 127.0.0.1 (on the runner). IPv6 literal hosts unsupported.
func ParseRemoteForwardSpec(s string) (RemoteForwardSpec, error) {
	parts := strings.Split(s, ":")
	var bind, dhost, rportS, dportS string
	switch len(parts) {
	case 3:
		bind, rportS, dhost, dportS = "127.0.0.1", parts[0], parts[1], parts[2]
	case 4:
		bind, rportS, dhost, dportS = parts[0], parts[1], parts[2], parts[3]
	default:
		return RemoteForwardSpec{}, fmt.Errorf("forward: bad -R spec %q (want [bind:]runnerport:dialhost:dialport)", s)
	}
	rport, err := strconv.Atoi(rportS)
	if err != nil || rport <= 0 || rport > 65535 {
		return RemoteForwardSpec{}, fmt.Errorf("forward: bad runner port in %q", s)
	}
	dport, err := strconv.Atoi(dportS)
	if err != nil || dport <= 0 || dport > 65535 {
		return RemoteForwardSpec{}, fmt.Errorf("forward: bad dial port in %q", s)
	}
	if dhost == "" {
		return RemoteForwardSpec{}, fmt.Errorf("forward: empty dial host in %q", s)
	}
	return RemoteForwardSpec{BindAddr: bind, RunnerPort: rport, DialHost: dhost, DialPort: dport}, nil
}

// remoteForwardConnNotifySize is the fixed wire size of a RemoteForwardConnNotify
// (one u64 stream id). Asserted in the protocol round-trip test.
const remoteForwardConnNotifySize = 8

// parseConnNotifies consumes as many whole RemoteForwardConnNotify records from
// buf as possible, returning the stream ids and the unconsumed remainder.
func parseConnNotifies(buf []byte) (ids []uint64, rest []byte) {
	for len(buf) >= remoteForwardConnNotifySize {
		var n protocol.RemoteForwardConnNotify
		if _, err := n.Decode(buf[:remoteForwardConnNotifySize]); err != nil {
			break
		}
		ids = append(ids, n.StreamId)
		buf = buf[remoteForwardConnNotifySize:]
	}
	return ids, buf
}

// OpenRemoteForward registers a remote forward and returns the server-created
// control stream (picked up by id) plus the assigned forwardId. The caller reads
// RemoteForwardConnNotify records off the control stream and dials per conn.
func (c *Client) OpenRemoteForward(ctx context.Context, taskIDHex string, sp RemoteForwardSpec) (trsf.BidirectionalStream, uint64, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, 0, fmt.Errorf("forward: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenPortForward}
	body := protocol.OpenPortForwardRequest{
		TaskId:     tid,
		Direction:  protocol.PortForwardDirection_Remote,
		RemotePort: uint16(sp.DialPort),
		BindPort:   uint16(sp.RunnerPort),
	}
	body.SetRemoteHost([]byte(sp.DialHost))
	body.SetBindAddr([]byte(sp.BindAddr))
	req.SetOpenPortForward(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	if resp.Kind != protocol.TaskControlKind_OpenPortForward {
		return nil, 0, fmt.Errorf("forward: unexpected response kind %v", resp.Kind)
	}
	r := resp.OpenPortForward()
	if r == nil {
		return nil, 0, errors.New("forward: response variant missing")
	}
	switch r.Status {
	case protocol.OpenPortForwardStatus_Ok:
	case protocol.OpenPortForwardStatus_NoSuchTask:
		return nil, 0, errors.New("forward: no such task (id unknown or task not running)")
	case protocol.OpenPortForwardStatus_RunnerOffline:
		return nil, 0, errors.New("forward: runner offline")
	case protocol.OpenPortForwardStatus_BindFailed:
		return nil, 0, errors.New("forward: runner failed to bind the listen port")
	default:
		return nil, 0, fmt.Errorf("forward: server error (status=%d)", r.Status)
	}
	// The control stream is server-created; pick it up by id (same pattern as
	// every other server-allocated stream).
	ctrl := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if ctrl == nil {
		return nil, 0, fmt.Errorf("forward: control stream %d not visible", r.StreamId)
	}
	return ctrl, r.ForwardId, nil
}

// RunRemoteForward registers each spec and reads its control stream, dialing the
// client-side target per arriving connection. Blocks until ctx is cancelled.
func RunRemoteForward(ctx context.Context, c *Client, taskIDHex string, specs []RemoteForwardSpec, logf func(string)) error {
	if logf == nil {
		logf = func(s string) { slog.Info(s) }
	}
	var wg sync.WaitGroup
	for _, sp := range specs {
		ctrl, fid, err := c.OpenRemoteForward(ctx, taskIDHex, sp)
		if err != nil {
			return err
		}
		logf(fmt.Sprintf("remote-forwarding runner:%s:%d -> %s:%d (task %s, fwd %d)",
			sp.BindAddr, sp.RunnerPort, sp.DialHost, sp.DialPort, taskIDHex[:min(12, len(taskIDHex))], fid))
		wg.Add(1)
		go func(sp RemoteForwardSpec, ctrl trsf.BidirectionalStream) {
			defer wg.Done()
			c.ServeRemoteForwardControl(ctx, sp, ctrl, logf)
		}(sp, ctrl)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// readRemoteForwardControl parses RemoteForwardConnNotify records off the control
// stream and, for each, dials the client-side target and splices. Buffers across
// ReadDirect boundaries so a coalesced/split notify is handled.
// ServeRemoteForwardControl runs the control-stream loop for an already-opened
// remote forward (see OpenRemoteForward): it dials the client-side target per
// arriving connection and returns when ctx is cancelled or the control stream
// closes. Callers that opened the forward themselves (e.g. the TUI, so it can
// confirm the bind before registering) use this to run the rest.
func (c *Client) ServeRemoteForwardControl(ctx context.Context, sp RemoteForwardSpec, ctrl trsf.BidirectionalStream, logf func(string)) {
	if logf == nil {
		logf = func(s string) { slog.Info(s) }
	}
	// CloseBoth on return is what tears the forward down: closing the control
	// stream makes the server's watcher send ClosePortForward to the runner so it
	// stops listening. Read with ctx so a forward stop (ctx cancel) actually
	// unblocks here — otherwise the goroutine leaks and the runner listener is
	// never released.
	defer ctrl.CloseBoth()
	var buf []byte
	for {
		data, eof, err := ctrl.ReadDirectContext(ctx, 64*1024)
		if len(data) > 0 {
			buf = append(buf, data...)
			var ids []uint64
			ids, buf = parseConnNotifies(buf)
			for _, id := range ids {
				go c.dialAndSplice(ctx, sp, trsf.StreamID(id), logf)
			}
		}
		if eof || err != nil {
			return
		}
	}
}

// dialAndSplice picks up the server-created data stream by id, dials the
// client-side target, and splices. On dial failure it closes the stream so the
// runner-side connection sees EOF (connection-refused semantics).
func (c *Client) dialAndSplice(ctx context.Context, sp RemoteForwardSpec, streamID trsf.StreamID, logf func(string)) {
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), streamID)
	if st == nil {
		logf(fmt.Sprintf("remote-forward: data stream %d not visible (lookup timeout)", uint64(streamID)))
		return
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(sp.DialHost, strconv.Itoa(sp.DialPort)))
	if err != nil {
		logf(fmt.Sprintf("remote-forward: dial %s:%d failed: %v", sp.DialHost, sp.DialPort, err))
		_ = st.CloseBoth()
		return
	}
	spliceConnStream(conn, st)
}
