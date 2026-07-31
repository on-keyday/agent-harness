package cli

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// RawConn is one port forward whose client-side endpoint is this process rather
// than a socket. The runner still dials host:port exactly as it does for -L; the
// difference is that nothing local is bound, so a browser (which cannot listen)
// and a stdio filter (which has nothing to listen for) can both hold this end.
//
// It owns two streams: data carries the bytes, ctrl is the registration handle
// whose EOF deregisters the forward server-side and whose Closed record is how a
// `forward kill` from another surface reaches us.
type RawConn struct {
	data      trsf.BidirectionalStream
	ctrl      trsf.BidirectionalStream
	forwardID uint64
	// closedLocally records that WE closed ctrl. Without it the control
	// watcher reads the EOF of the stream it just closed and reports "server
	// connection lost" — blaming the server for our own teardown. Observed
	// against a dummy harness: a forward whose target refused the connection
	// printed exactly that, sending the operator to check a healthy server.
	closedLocally atomic.Bool
	// stopWatch ends the control watcher. It is derived from a context the
	// CALLER cannot cancel — see OpenRawForward — so Close is the only thing
	// that stops it from this side.
	stopWatch context.CancelFunc
}

// OpenRawForward opens the data stream, registers the forward so it is listable
// and killable, and starts watching the control stream. Registration failure
// closes the data stream: a forward that is running but absent from `forward ls`
// is exactly the state the registry exists to prevent.
//
// ctx bounds the OPEN — the two RPCs below — and nothing else. The connection
// itself lives until Close, or until the far end ends the registration. The
// control watcher used to run on ctx, which quietly made every caller that
// bounds its dial with a timeout self-destruct: the TUI cancels its connect
// timeout the instant the open returns, the watcher woke on that cancellation,
// and its deferred onEnd closed the data stream — so a raw connect from the
// TUI reported "connection closed" the moment it was established. Callers tear
// down with Close (spliceStdio, CloseRawPane and the TUI pump all do); ctx is
// not, and must not be, the teardown signal.
func OpenRawForward(ctx context.Context, c *Client, taskIDHex, host string, port int, logf func(string)) (*RawConn, error) {
	if logf == nil {
		logf = func(string) {}
	}
	data, err := c.OpenPortForward(ctx, taskIDHex, host, port)
	if err != nil {
		return nil, err
	}
	ctrl, fid, err := c.RegisterPortForward(ctx, taskIDHex, protocol.PortForwardDirection_Local,
		"", 0, host, port, protocol.ClientEndpointKind_InProcess)
	if err != nil {
		_ = data.CloseBoth()
		return nil, fmt.Errorf("raw forward: register: %w", err)
	}
	watchCtx, stopWatch := context.WithCancel(context.WithoutCancel(ctx))
	rc := &RawConn{data: data, ctrl: ctrl, forwardID: fid, stopWatch: stopWatch}
	go rc.watchControl(watchCtx, logf)
	return rc, nil
}

// ForwardID is the server-assigned registration id, as shown by `forward ls`.
func (r *RawConn) ForwardID() uint64 { return r.forwardID }

// Send writes bytes to the far end. The buffer is copied: AppendData keeps the
// slice by reference and copies asynchronously, so a caller reusing its read
// buffer (every pump does) would otherwise corrupt bytes already handed over.
func (r *RawConn) Send(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	chunk := make([]byte, len(b))
	copy(chunk, b)
	return r.data.AppendData(false, chunk)
}

// Recv returns the next chunk from the far end. The bool reports EOF, matching
// trsf's ReadDirectContext shape so consumers need no second idiom.
func (r *RawConn) Recv(ctx context.Context) ([]byte, bool, error) {
	return r.data.ReadDirectContext(ctx, 64*1024)
}

// Close tears down both streams. Closing ctrl is what deregisters the forward,
// so a closed pane leaves no entry behind in `forward ls`.
func (r *RawConn) Close() error {
	r.closedLocally.Store(true)
	if r.stopWatch != nil {
		r.stopWatch()
	}
	_ = r.data.CloseBoth()
	return r.ctrl.CloseBoth()
}

// watchControl ends the connection when the registration ends. Lines produced
// after a local Close are dropped: the only one that can arrive then is the EOF
// of the control stream we closed ourselves, and reporting it as a lost server
// would be a false diagnosis. A real remote kill or a real server death still
// reports, because both arrive before anything local closes.
func (r *RawConn) watchControl(ctx context.Context, logf func(string)) {
	quiet := func(line string) {
		if r.closedLocally.Load() {
			return
		}
		logf(line)
	}
	serveForwardControl(ctx, r.ctrl, quiet, func() { _ = r.data.CloseBoth() })
}
