package runner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/iplink"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"golang.org/x/sys/unix"
)

// fakeTun stands in for a tun device: packets queued on in are read one per
// Read, and Writes land on out.
type fakeTun struct {
	in      chan []byte
	out     chan []byte
	readErr chan error
	once    sync.Once
	done    chan struct{}
}

func newFakeTun() *fakeTun {
	return &fakeTun{in: make(chan []byte, 8), out: make(chan []byte, 8), readErr: make(chan error, 1), done: make(chan struct{})}
}

func (d *fakeTun) Read(p []byte) (int, error) {
	select {
	case pkt := <-d.in:
		return copy(p, pkt), nil
	case err := <-d.readErr:
		return 0, err
	case <-d.done:
		return 0, os.ErrClosed
	}
}
func (d *fakeTun) Write(p []byte) (int, error) {
	select {
	case <-d.done:
		return 0, os.ErrClosed
	default:
	}
	d.out <- append([]byte(nil), p...)
	return len(p), nil
}
func (d *fakeTun) Close() error { d.once.Do(func() { close(d.done) }); return nil }
func (d *fakeTun) Name() string { return "hlink9" }
func (d *fakeTun) isClosed() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

type openCall struct {
	addr netip.Prefix
	mtu  int
}

// linkSession is a Session holding one known task, whose tun opener hands out
// dev (or fails with openErr) and records how it was called.
func linkSession(t *testing.T, dev *fakeTun, openErr error) (*Session, *mockSender, protocol.TaskID, *[]openCall) {
	t.Helper()
	ms := &mockSender{}
	calls := &[]openCall{}
	s := &Session{Sender: ms, Now: time.Now, NoWorktree: true}
	s.openTun = func(addr netip.Prefix, mtu int) (iplink.Device, error) {
		*calls = append(*calls, openCall{addr, mtu})
		if openErr != nil {
			return nil, openErr
		}
		return dev, nil
	}
	var tid protocol.TaskID
	tid.Id[0] = 0x1B
	s.mu.Lock()
	s.initMaps()
	s.reg.put(hex.EncodeToString(tid.Id[:]), &taskEntry{repoPath: t.TempDir()})
	s.mu.Unlock()
	return s, ms, tid, calls
}

func linkRequest(t *testing.T, tid protocol.TaskID, fid uint64) *protocol.RunnerOpenPortForwardRequest {
	t.Helper()
	cfg, err := protocol.NewIpLinkConfig(netip.MustParsePrefix("10.200.0.1/30"), netip.MustParseAddr("10.200.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	req := &protocol.RunnerOpenPortForwardRequest{TaskId: tid, Direction: protocol.PortForwardDirection_Link,
		Protocol: protocol.ForwardProtocol_Ip, ForwardId: fid}
	req.SetLink(cfg)
	return req
}

// runnerMessages decodes every control message the session sent.
func runnerMessages(t *testing.T, ms *mockSender) []*protocol.RunnerMessage {
	t.Helper()
	ms.mu.Lock()
	raw := append([][]byte(nil), ms.sent...)
	ms.mu.Unlock()
	var out []*protocol.RunnerMessage
	for _, b := range raw {
		out = append(out, decodeRunnerMsg(t, b))
	}
	return out
}

func setupResults(t *testing.T, ms *mockSender) []*protocol.ForwardSetupResult {
	var out []*protocol.ForwardSetupResult
	for _, m := range runnerMessages(t, ms) {
		if m.Kind == protocol.RunnerMessageType_ForwardSetupResult {
			out = append(out, m.ForwardSetupResult())
		}
	}
	return out
}

func forwardEndeds(t *testing.T, ms *mockSender) []*protocol.ForwardEnded {
	var out []*protocol.ForwardEnded
	for _, m := range runnerMessages(t, ms) {
		if m.Kind == protocol.RunnerMessageType_ForwardEnded {
			out = append(out, m.ForwardEnded())
		}
	}
	return out
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartIPLinkEPERMAnswersSetupFailed(t *testing.T) {
	s, ms, tid, _ := linkSession(t, nil, fmt.Errorf("TUNSETIFF: %w", unix.EPERM))
	s.handleOpenPortForward(context.Background(), linkRequest(t, tid, 5))
	res := setupResults(t, ms)
	if len(res) != 1 || res[0].Ok() || res[0].ForwardId != 5 || string(res[0].Detail) != "TUNSETIFF: operation not permitted" {
		t.Fatalf("setup results = %+v", res)
	}
	if _, ok := s.ipLinkRegistry().get(5); ok {
		t.Fatal("a failed link was registered")
	}
}

func TestStartIPLinkNotLinuxDetail(t *testing.T) {
	s, ms, tid, _ := linkSession(t, nil, iplink.ErrNotLinux)
	s.handleOpenPortForward(context.Background(), linkRequest(t, tid, 5))
	res := setupResults(t, ms)
	want := "ip links are Linux-only (this runner is " + runtime.GOOS + ")"
	if len(res) != 1 || res[0].Ok() || string(res[0].Detail) != want {
		t.Fatalf("setup results = %+v, want detail %q", res, want)
	}
}

func TestStartIPLinkUnknownTask(t *testing.T) {
	s, ms, _, calls := linkSession(t, newFakeTun(), nil)
	var other protocol.TaskID
	other.Id[0] = 0xEE
	s.handleOpenPortForward(context.Background(), linkRequest(t, other, 5))
	res := setupResults(t, ms)
	if len(res) != 1 || res[0].Ok() || string(res[0].Detail) != "unknown task" {
		t.Fatalf("setup results = %+v", res)
	}
	if len(*calls) != 0 {
		t.Fatal("a tun was opened for an unknown task")
	}
}

func TestIPLinkCarriesBothWays(t *testing.T) {
	dev := newFakeTun()
	s, ms, tid, calls := linkSession(t, dev, nil)
	s.handleOpenPortForward(context.Background(), linkRequest(t, tid, 5))
	t.Cleanup(func() { s.releaseForwardState() })
	if res := setupResults(t, ms); len(res) != 1 || !res[0].Ok() {
		t.Fatalf("setup results = %+v", res)
	}
	if len(*calls) != 1 || (*calls)[0].addr.String() != "10.200.0.2/30" || (*calls)[0].mtu != 1100 {
		t.Fatalf("opener calls = %+v, want the runner's address /30 and mtu 1100", *calls)
	}

	dg := protocol.ForwardDatagram{ForwardId: 5, Payload: []byte("to the runner")}
	s.handleForwardDatagram(&dg)
	select {
	case got := <-dev.out:
		if string(got) != "to the runner" {
			t.Fatalf("device got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the datagram never reached the device")
	}

	dev.in <- []byte("from the runner")
	eventually(t, func() bool {
		ms.mu.Lock()
		defer ms.mu.Unlock()
		return len(ms.datagrams) == 1
	})
	ms.mu.Lock()
	b := ms.datagrams[0]
	ms.mu.Unlock()
	var back protocol.ForwardDatagram
	if appwire.AppKind(b[0]) != appwire.AppKind_ForwardDatagram || back.DecodeExact(b[1:]) != nil ||
		back.ForwardId != 5 || back.FlowId != 0 || string(back.Payload) != "from the runner" {
		t.Fatalf("sent datagram = %+v", back)
	}
}

// Review Focus 1: the server closing a link is not the link failing.
func TestIPLinkCloseSendsNoForwardEnded(t *testing.T) {
	dev := newFakeTun()
	s, ms, tid, _ := linkSession(t, dev, nil)
	s.handleOpenPortForward(context.Background(), linkRequest(t, tid, 5))
	time.Sleep(10 * time.Millisecond) // let the pump park in Read

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ClosePortForward}
	req.SetClosePortForward(protocol.ClosePortForwardRequest{ForwardId: 5})
	dispatchRunnerRequest(context.Background(), s, s.logger(), appwire.AppKind_RunnerControl, req.MustAppend(nil))

	eventually(t, dev.isClosed)
	if _, ok := s.ipLinkRegistry().get(5); ok {
		t.Fatal("the link is still registered after ClosePortForward")
	}
	time.Sleep(50 * time.Millisecond)
	if fe := forwardEndeds(t, ms); len(fe) != 0 {
		t.Fatalf("a deliberate close sent ForwardEnded: %+v", fe)
	}
}

func TestIPLinkDeviceFailureSendsForwardEnded(t *testing.T) {
	dev := newFakeTun()
	s, ms, tid, _ := linkSession(t, dev, nil)
	s.handleOpenPortForward(context.Background(), linkRequest(t, tid, 5))
	dev.readErr <- errors.New("boom")
	eventually(t, func() bool { return len(forwardEndeds(t, ms)) == 1 })
	fe := forwardEndeds(t, ms)[0]
	if fe.ForwardId != 5 || !containsStr(string(fe.Detail), "boom") {
		t.Fatalf("ForwardEnded = %+v", fe)
	}
	if _, ok := s.ipLinkRegistry().get(5); ok {
		t.Fatal("a failed link is still registered")
	}
	// A ClosePortForward arriving later is a no-op.
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ClosePortForward}
	req.SetClosePortForward(protocol.ClosePortForwardRequest{ForwardId: 5})
	dispatchRunnerRequest(context.Background(), s, s.logger(), appwire.AppKind_RunnerControl, req.MustAppend(nil))
	if n := len(forwardEndeds(t, ms)); n != 1 {
		t.Fatalf("%d ForwardEnded sent, want 1", n)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestReleaseForwardStateClosesDatagramForwards(t *testing.T) {
	dev := newFakeTun()
	s, _, tid, _ := linkSession(t, dev, nil)
	s.handleOpenPortForward(context.Background(), linkRequest(t, tid, 5))

	// udp -R: a real listening socket.
	rreq := &protocol.RunnerOpenPortForwardRequest{TaskId: tid, Direction: protocol.PortForwardDirection_Remote,
		Protocol: protocol.ForwardProtocol_Udp, ForwardId: 6, BindPort: 0}
	rreq.SetBindAddr([]byte("127.0.0.1"))
	s.handleOpenPortForward(context.Background(), rreq)
	rem, ok := s.udpForwardRegistry().getRemote(6)
	if !ok {
		t.Fatal("udp -R did not bind")
	}
	remAddr := rem.conn.LocalAddr().String()

	// udp -L with one live flow toward a local echo target.
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	lreq := &protocol.RunnerOpenPortForwardRequest{TaskId: tid, Direction: protocol.PortForwardDirection_Local,
		Protocol: protocol.ForwardProtocol_Udp, ForwardId: 7, RemotePort: uint16(target.LocalAddr().(*net.UDPAddr).Port)}
	lreq.SetRemoteHost([]byte("127.0.0.1"))
	s.handleOpenPortForward(context.Background(), lreq)
	s.handleForwardDatagram(&protocol.ForwardDatagram{ForwardId: 7, FlowId: 1, Payload: []byte("x")})
	fwd, _ := s.udpForwardRegistry().get(7)
	fwd.mu.Lock()
	flow := fwd.flows[1]
	fwd.mu.Unlock()
	if flow == nil {
		t.Fatal("no flow socket was dialled")
	}

	sweepDone := make(chan struct{})
	go func() { s.udpForwardRegistry().sweep(context.Background()); close(sweepDone) }()

	s.releaseForwardState()

	if !dev.isClosed() {
		t.Error("the link's device is still open")
	}
	if ln, err := net.ListenPacket("udp", remAddr); err != nil {
		t.Errorf("the udp -R socket was not released: %v", err)
	} else {
		ln.Close()
	}
	if _, err := flow.conn.Write([]byte("y")); err == nil {
		t.Error("the udp -L flow socket is still open")
	}
	tel := sessionTelemetry{s}
	for _, id := range []uint64{5, 6, 7} {
		if _, ok := tel.ForwardDrops(id); ok {
			t.Errorf("forward %d still answers ForwardDrops", id)
		}
	}
	select {
	case <-sweepDone:
	case <-time.After(2 * time.Second):
		t.Error("the sweep kept running on a live ctx after its registry was released")
	}
}

func TestForwardDropsFindsIPLink(t *testing.T) {
	dev := newFakeTun()
	s, _, tid, _ := linkSession(t, dev, nil)
	s.handleOpenPortForward(context.Background(), linkRequest(t, tid, 5))
	t.Cleanup(func() { s.releaseForwardState() })
	if _, ok := (sessionTelemetry{s}).ForwardDrops(5); !ok {
		t.Fatal("ForwardDrops does not find the link")
	}
}
