package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"

	"github.com/on-keyday/agent-harness/iplink"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// ParseIPLink turns --ip and --ip-peer into a link: local is this client's tun
// address with the shared prefix ("10.200.0.1/30"), peer the runner's
// ("10.200.0.2"). The TUI's line parser calls this too, so the two surfaces
// cannot accept different addresses; the server runs the same Validate.
func ParseIPLink(local, peer string) (protocol.IpLinkConfig, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(local))
	if err != nil {
		return protocol.IpLinkConfig{}, fmt.Errorf("--ip wants <address>/<prefix>, got %q", local)
	}
	a, err := netip.ParseAddr(strings.TrimSpace(peer))
	if err != nil {
		return protocol.IpLinkConfig{}, fmt.Errorf("--ip-peer wants an address, got %q", peer)
	}
	return protocol.NewIpLinkConfig(p, a)
}

// ParseIPLinkLine reads the TUI's one-line form, "<client-addr>/<prefix>
// <runner-addr>" -- the same two values --ip and --ip-peer take.
func ParseIPLinkLine(s string) (protocol.IpLinkConfig, error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return protocol.IpLinkConfig{}, fmt.Errorf("ip link: want <client-addr>/<prefix> <runner-addr>, got %q", s)
	}
	return ParseIPLink(f[0], f[1])
}

// IPLinkSpecString renders a link for the listing: "ip 10.200.0.1/30 <->
// 10.200.0.2". Not a spec any parser accepts -- a link has no workspace form.
func IPLinkSpecString(c *protocol.IpLinkConfig) string {
	return fmt.Sprintf("ip %s <-> %s", c.ClientPrefix(), c.RunnerPrefix().Addr())
}

// ForwardClosedError is how a link learns its forward was ended from the
// server's side: killed, its task gone, its runner gone or failed.
type ForwardClosedError struct {
	Reason protocol.PortForwardCloseReason
	Detail string
}

func (e *ForwardClosedError) Error() string {
	c := protocol.PortForwardClosed{Reason: e.Reason}
	c.SetDetail([]byte(e.Detail))
	return closedReasonLine(&c)
}

// IPLinkOptions are what a caller other than harness-cli may change.
type IPLinkOptions struct {
	// Open creates the device; nil means iplink.OpenTun.
	Open iplink.Opener
	// OnRegistered, when non-nil, is called with the server-assigned id once
	// the link is registered, so a caller can name it in a later kill.
	OnRegistered func(id uint64)
}

// RunIPLink brings up this client's end of an ip link, registers it, and
// carries packets until the link ends. In order: the device first, so nothing
// is registered for a device that cannot exist (as -L binds its listener
// before registering); then the registration, which waits for the runner's
// device; then the pump.
//
// It returns nil when ctx ends -- the caller stopped it -- a
// *ForwardClosedError when the server ended the forward, and an error when the
// device failed or the server went away. Blocks; the caller owns the goroutine.
func RunIPLink(ctx context.Context, c *Client, taskIDHex string, cfg protocol.IpLinkConfig,
	logf func(string), opts IPLinkOptions) error {
	if logf == nil {
		logf = func(s string) { slog.Info(s) }
	}
	open := opts.Open
	if open == nil {
		open = iplink.OpenTun
	}
	addr := cfg.ClientPrefix()
	dev, err := open(addr, int(cfg.Mtu))
	if err != nil {
		return fmt.Errorf("ip link: %w", err)
	}
	logf(iplink.UpLine(dev.Name(), addr, int(cfg.Mtu)))

	ctrl, fid, err := c.RegisterIPLink(ctx, taskIDHex, cfg)
	if err != nil {
		_ = dev.Close()
		return err
	}
	if opts.OnRegistered != nil {
		opts.OnRegistered(fid)
	}
	logf(fmt.Sprintf("%s (task %s, fwd %d)", IPLinkSpecString(&cfg), taskIDHex[:min(12, len(taskIDHex))], fid))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var devErr error
	var devErrOnce sync.Once
	fail := func(err error) {
		devErrOnce.Do(func() {
			devErr = err
			logf("ip link: " + err.Error())
			cancel()
		})
	}
	l := iplink.NewLink(fid, dev, c.conn)
	registerIPLinkClient(fid, &ipLinkClient{link: l, fail: fail})
	defer func() {
		unregisterIPLinkClient(fid)
		_ = l.Close()
	}()
	go func() {
		if err := l.Run(runCtx); err != nil {
			fail(err)
		}
	}()

	// serveForwardControl closes ctrl on the way out, which is what tells the
	// server the client end is gone.
	closed := serveForwardControl(runCtx, ctrl, logf, nil)
	switch {
	case closed != nil:
		return &ForwardClosedError{Reason: closed.Reason, Detail: string(closed.Detail)}
	case devErr != nil:
		return fmt.Errorf("ip link: %w", devErr)
	case ctx.Err() != nil:
		return nil
	}
	return errors.New("forward stopped: server connection lost")
}

// RegisterIPLink registers an ip × link forward. The server holds the answer
// until the runner's device is up, so an error here can carry the runner's own
// reason (link_failed, "runner: ...").
func (c *Client) RegisterIPLink(ctx context.Context, taskIDHex string, cfg protocol.IpLinkConfig) (trsf.BidirectionalStream, uint64, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, 0, fmt.Errorf("forward: parse task id: %w", err)
	}
	// os_socket: the tun is an OS device. Savability is refused on the
	// direction instead (PortForwardConfigSpec).
	body := protocol.RegisterPortForwardRequest{TaskId: tid, Direction: protocol.PortForwardDirection_Link,
		ClientEndpoint: protocol.ClientEndpointKind_OsSocket,
		Protocol:       protocol.ForwardProtocol_Ip, Route: protocol.DataPlaneRoute_Splice}
	body.SetLink(cfg) // Protocol is ip, set above
	return c.registerForward(ctx, body)
}

// ipLinkClient is one link this process holds: the pump, and what to do when
// writing a delivered packet to its device fails.
type ipLinkClient struct {
	link *iplink.Link
	fail func(error)
}

// ipLinkClients is the third registry dispatchDatagram routes through, beside
// the udp -L and -R ones, and process-wide for the same reason they are.
var (
	ipLinkClientsMu sync.Mutex
	ipLinkClients   = map[uint64]*ipLinkClient{}
)

func registerIPLinkClient(id uint64, l *ipLinkClient) {
	ipLinkClientsMu.Lock()
	ipLinkClients[id] = l
	ipLinkClientsMu.Unlock()
}

func unregisterIPLinkClient(id uint64) {
	ipLinkClientsMu.Lock()
	delete(ipLinkClients, id)
	ipLinkClientsMu.Unlock()
}

func lookupIPLinkClient(id uint64) (*ipLinkClient, bool) {
	ipLinkClientsMu.Lock()
	defer ipLinkClientsMu.Unlock()
	l, ok := ipLinkClients[id]
	return l, ok
}
