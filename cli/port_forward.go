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
	"time"

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

// RunForward listens for each spec, registers it with the server (so a
// different client can list and kill it), and bridges accepted connections to
// the runner via OpenPortForward. Blocks until every spec's forward has
// stopped (killed remotely, or ctx cancelled), then closes all listeners.
// Per-connection errors are logged and isolated; the listener and sibling
// connections are unaffected.
//
// onRegistered, when non-nil, is called synchronously right after each spec's
// RegisterPortForward succeeds, with the server-assigned forward id. Callers
// that need to name their own forward later — e.g. the TUI's tasks-pane P/B
// stop keys, which must route through KillPortForwardWith like every other
// stop path — have no other way to learn the id: RunForward blocks for the
// whole lifetime of the forward(s) and only reports it via this callback, not
// a return value.
func RunForward(ctx context.Context, c *Client, taskIDHex string, specs []ForwardSpec, logf func(string), onRegistered func(sp ForwardSpec, id uint64)) error {
	if logf == nil {
		logf = func(s string) { slog.Info(s) }
	}
	var lns []net.Listener
	closeAll := func() {
		for _, l := range lns {
			_ = l.Close()
		}
	}
	// One context for the whole call, cancelled on every exit path including the
	// error returns below. Without this, a mid-loop failure would abandon the
	// specs already started: their listeners close but their control-stream
	// readers stay parked and their server-side registrations stay live until the
	// peer connection eventually errors out — a forward the list shows that is
	// not actually running. Control-stream EOF is what deregisters a forward
	// server-side, so every exit path must actually close the streams.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	var wg sync.WaitGroup
	// abort cancels every started spec, waits for its goroutines, and closes the
	// listeners. Use it on every error return, never a bare closeAll().
	abort := func() {
		runCancel()
		wg.Wait()
		closeAll()
	}
	for _, sp := range specs {
		ln, err := net.Listen("tcp", net.JoinHostPort(sp.BindAddr, strconv.Itoa(sp.LocalPort)))
		if err != nil {
			abort()
			return fmt.Errorf("forward: listen %s:%d: %w", sp.BindAddr, sp.LocalPort, err)
		}
		lns = append(lns, ln)
		// Register the port the kernel actually gave us (sp.LocalPort may be 0).
		bound := ln.Addr().(*net.TCPAddr).Port
		ctrl, fid, rerr := c.RegisterPortForward(ctx, taskIDHex, protocol.PortForwardDirection_Local,
			sp.BindAddr, bound, sp.RemoteHost, sp.RemotePort)
		if rerr != nil {
			// A forward the server does not know about cannot be listed or
			// killed, which is the whole point of registering — fail loudly.
			abort()
			return fmt.Errorf("forward: register %s:%d: %w", sp.BindAddr, bound, rerr)
		}
		if onRegistered != nil {
			onRegistered(sp, fid)
		}
		fwdCtx, cancel := context.WithCancel(runCtx)
		logf(fmt.Sprintf("forwarding %s:%d -> %s:%d (task %s, fwd %d)",
			sp.BindAddr, bound, sp.RemoteHost, sp.RemotePort, taskIDHex[:min(12, len(taskIDHex))], fid))
		go acceptLoop(fwdCtx, c, taskIDHex, sp, ln, logf)
		wg.Add(1)
		go func(ctrl trsf.BidirectionalStream) {
			defer wg.Done()
			defer cancel()
			c.serveLocalForwardControl(fwdCtx, ctrl, logf)
		}(ctrl)
	}
	wg.Wait()
	closeAll()
	return nil
}

// serveLocalForwardControl reads the forward's control stream. The client never
// writes on it; it returns on a closed event (deliberate stop) or on EOF/error
// (the server went away). Returning cancels the forward's context, which closes
// the listener and every connection spliced through it. Closes ctrl itself
// (matching ServeRemoteForwardControl) — a Local registration is reaped
// server-side only on control-stream EOF, so a caller that forgot to close it
// would leave a permanent ghost registration.
func (c *Client) serveLocalForwardControl(ctx context.Context, ctrl trsf.BidirectionalStream, logf func(string)) {
	defer ctrl.CloseBoth()
	var buf []byte
	for {
		data, eof, err := ctrl.ReadDirectContext(ctx, 64*1024)
		if len(data) > 0 {
			buf = append(buf, data...)
			var evs []protocol.PortForwardEvent
			evs, buf = parsePortForwardEvents(buf)
			for _, ev := range evs {
				if ev.Kind == protocol.PortForwardEventKind_Closed {
					logf(closedReasonLine(ev.Closed().Reason))
					return
				}
			}
		}
		if eof || err != nil {
			if ctx.Err() == nil {
				logf("forward stopped: server connection lost")
			}
			return
		}
	}
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
			// A killed forward drops its established connections too. The CLI
			// exits straight after RunForward returns and would drop them
			// anyway; doing it here makes TUI/WebUI-started forwards behave
			// the same instead of leaking a live splice.
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				select {
				case <-ctx.Done():
					_ = conn.Close()
					_ = st.CloseBoth()
				case <-stop:
				}
			}()
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
	// DialNetwork selects how the client dials the local target: "tcp"
	// (default; DialHost:DialPort) or "unix" (DialHost is the socket path,
	// DialPort ignored). Used by X11 forwarding to reach a UNIX X server.
	DialNetwork string
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
	return RemoteForwardSpec{BindAddr: bind, RunnerPort: rport, DialHost: dhost, DialPort: dport, DialNetwork: "tcp"}, nil
}

// parsePortForwardEvents consumes as many whole PortForwardEvent records from buf
// as possible, returning them and the unconsumed remainder. Records are no longer
// a fixed size (the tag selects the payload), so the decoder itself reports where
// each record ends.
func parsePortForwardEvents(buf []byte) (evs []protocol.PortForwardEvent, rest []byte) {
	for len(buf) > 0 {
		var ev protocol.PortForwardEvent
		r, err := ev.Decode(buf)
		if err != nil {
			break // partial record: keep it for the next read
		}
		evs = append(evs, ev)
		buf = r
	}
	return evs, buf
}

// RegisterPortForward registers one forward with the server and returns the
// server-created control stream plus the assigned forward id. The caller has
// already bound its listener (local) or is asking the runner to bind (remote).
func (c *Client) RegisterPortForward(ctx context.Context, taskIDHex string, dir protocol.PortForwardDirection,
	bindAddr string, bindPort int, targetHost string, targetPort int) (trsf.BidirectionalStream, uint64, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, 0, fmt.Errorf("forward: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RegisterPortForward}
	body := protocol.RegisterPortForwardRequest{TaskId: tid, Direction: dir,
		BindPort: uint16(bindPort), TargetPort: uint16(targetPort)}
	body.SetBindAddr([]byte(bindAddr))
	body.SetTargetHost([]byte(targetHost))
	req.SetRegisterPortForward(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	r := resp.RegisterPortForward()
	if resp.Kind != protocol.TaskControlKind_RegisterPortForward || r == nil {
		return nil, 0, fmt.Errorf("forward: unexpected response kind %v", resp.Kind)
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
	ctrl := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if ctrl == nil {
		// The server already committed the registration — and, for -R, the
		// runner's listener is already bound — before this pickup failed.
		// harness-cli's process exit would reap it via connection death, but
		// the TUI/WebUI hold a long-lived Client and would otherwise carry a
		// permanent ghost row (and, for -R, an orphan runner listener)
		// forever: the spec's failure-mode table assumes control-stream EOF
		// always deregisters, which only holds if a registration whose
		// control stream was never even picked up doesn't just sit there.
		// Best-effort clean it up. ctx may already be why the wait failed
		// (cancelled), so the kill gets its own short-lived context rather
		// than reusing it.
		killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if kerr := c.KillPortForwardWith(killCtx, r.ForwardId); kerr != nil {
			slog.Warn("forward: register pickup failed and best-effort cleanup kill also failed",
				"forward_id", r.ForwardId, "err", kerr)
		}
		cancel()
		return nil, 0, fmt.Errorf("forward: control stream %d not visible", r.StreamId)
	}
	return ctrl, r.ForwardId, nil
}

// OpenRemoteForward registers a remote forward. Kept as a thin wrapper so the
// TUI's existing call site (tui/portforward.go:262) is unchanged.
func (c *Client) OpenRemoteForward(ctx context.Context, taskIDHex string, sp RemoteForwardSpec) (trsf.BidirectionalStream, uint64, error) {
	return c.RegisterPortForward(ctx, taskIDHex, protocol.PortForwardDirection_Remote,
		sp.BindAddr, sp.RunnerPort, sp.DialHost, sp.DialPort)
}

// RunRemoteForward registers each spec and reads its control stream, dialing the
// client-side target per arriving connection. Returns when every spec's forward
// has stopped — each one ends on a Closed event (someone ran `forward kill`) or
// on ctx cancellation. It is therefore NOT safe to assume this only returns at
// caller-cancel time: callers that print to a terminal the foreground has since
// flipped into raw mode, or that push onto a channel nothing is draining, must
// hold for a mid-flight return. See cli/x11.go and tui/interactive.go.
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
		dialTarget := sp.DialHost
		if sp.DialNetwork != "unix" {
			dialTarget = fmt.Sprintf("%s:%d", sp.DialHost, sp.DialPort)
		}
		logf(fmt.Sprintf("remote-forwarding runner:%s:%d -> %s (task %s, fwd %d)",
			sp.BindAddr, sp.RunnerPort, dialTarget, taskIDHex[:min(12, len(taskIDHex))], fid))
		wg.Add(1)
		go func(sp RemoteForwardSpec, ctrl trsf.BidirectionalStream) {
			defer wg.Done()
			c.ServeRemoteForwardControl(ctx, sp, ctrl, logf)
		}(sp, ctrl)
	}
	// Return once every spec's forward has stopped — killed remotely, or ctx
	// cancelled — not solely on ctx.Done(). Blocking on ctx.Done() alone
	// means a -R forward killed while ctx is still live (no Ctrl-C) would
	// never let this function return: the exact orphan-terminal symptom this
	// feature exists to remove.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		wg.Wait()
	}
	return nil
}

// ServeRemoteForwardControl runs the control-stream loop for an already-opened
// remote forward (see OpenRemoteForward): it dispatches PortForwardEvent records
// off the control stream — dialing the client-side target per arriving
// connection, or stopping on an explicit close — and returns when ctx is
// cancelled, a `closed` event arrives, or the control stream itself EOFs.
// Buffers across ReadDirect boundaries so a coalesced/split record is handled.
// Callers that opened the forward themselves (e.g. the TUI, so it can confirm
// the bind before registering) use this to run the rest.
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
			var evs []protocol.PortForwardEvent
			evs, buf = parsePortForwardEvents(buf)
			for _, ev := range evs {
				switch ev.Kind {
				case protocol.PortForwardEventKind_ConnNotify:
					go c.dialAndSplice(ctx, sp, trsf.StreamID(ev.ConnNotify().StreamId), logf)
				case protocol.PortForwardEventKind_Closed:
					logf(closedReasonLine(ev.Closed().Reason))
					return
				}
			}
		}
		if eof || err != nil {
			// ReadDirectContext returns ctx.Err() on cancellation, and
			// cancellation IS the ordinary stop path (X11 session end,
			// Ctrl-C, TUI stop). Reporting a lost server there would make
			// the server-died message the one thing printed on every clean
			// exit — destroying the distinction this record type exists for.
			if ctx.Err() == nil {
				logf("remote-forward: server connection lost")
			}
			return
		}
	}
}

// closedReasonLine renders why a forward stopped. Kept distinct from the EOF
// path so an operator can tell a deliberate kill from a dead server.
func closedReasonLine(r protocol.PortForwardCloseReason) string {
	switch r {
	case protocol.PortForwardCloseReason_TaskGone:
		return "forward stopped: task is no longer running"
	default:
		return "forward stopped: killed remotely"
	}
}

// dialForwardTarget dials the client-side target described by sp, honoring
// sp.DialNetwork ("unix" → DialHost is a socket path; otherwise TCP).
func dialForwardTarget(sp RemoteForwardSpec) (net.Conn, error) {
	if sp.DialNetwork == "unix" {
		return net.Dial("unix", sp.DialHost)
	}
	return net.Dial("tcp", net.JoinHostPort(sp.DialHost, strconv.Itoa(sp.DialPort)))
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
	conn, err := dialForwardTarget(sp)
	if err != nil {
		target := sp.DialHost
		if sp.DialNetwork != "unix" {
			target = net.JoinHostPort(sp.DialHost, strconv.Itoa(sp.DialPort))
		}
		logf(fmt.Sprintf("remote-forward: dial %s/%s failed: %v", sp.DialNetwork, target, err))
		_ = st.CloseBoth()
		return
	}
	spliceConnStream(conn, st)
}
