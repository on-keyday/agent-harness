package cli

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/iplink"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestParseIPLink(t *testing.T) {
	cfg, err := ParseIPLink("10.200.0.1/30", "10.200.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientPrefix().String() != "10.200.0.1/30" || cfg.RunnerPrefix().String() != "10.200.0.2/30" || cfg.Mtu != 1100 {
		t.Fatalf("cfg = %+v", cfg)
	}
	for _, c := range []struct{ local, peer, want string }{
		{"10.200.0.1", "10.200.0.2", `--ip wants <address>/<prefix>, got "10.200.0.1"`},
		{"10.200.0.1/30", "x", `--ip-peer wants an address, got "x"`},
		{"fd00::1/64", "fd00::2", "ipv6 link addresses are not implemented"},
		{"10.200.0.1/32", "10.200.0.2", "prefix /32 is outside 1-31"},
	} {
		_, err := ParseIPLink(c.local, c.peer)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseIPLink(%q, %q) = %v, want it to contain %q", c.local, c.peer, err, c.want)
		}
	}
}

func TestParseIPLinkLine(t *testing.T) {
	want, _ := ParseIPLink("10.200.0.1/30", "10.200.0.2")
	for _, line := range []string{"10.200.0.1/30 10.200.0.2", "  10.200.0.1/30\t 10.200.0.2  "} {
		got, err := ParseIPLinkLine(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if got.ClientPrefix() != want.ClientPrefix() || got.RunnerPrefix() != want.RunnerPrefix() {
			t.Fatalf("%q parsed to %+v", line, got)
		}
	}
	for _, line := range []string{"10.200.0.1/30", "a b c", ""} {
		if _, err := ParseIPLinkLine(line); err == nil || !strings.Contains(err.Error(), "want <client-addr>/<prefix> <runner-addr>") {
			t.Errorf("%q: error = %v", line, err)
		}
	}
}

func TestIPLinkSpecString(t *testing.T) {
	cfg, _ := ParseIPLink("10.200.0.1/30", "10.200.0.2")
	if got := IPLinkSpecString(&cfg); got != "ip 10.200.0.1/30 <-> 10.200.0.2" {
		t.Fatalf("IPLinkSpecString = %q", got)
	}
}

func TestRegisterResponseError(t *testing.T) {
	resp := func(s protocol.OpenPortForwardStatus, detail string) *protocol.RegisterPortForwardResponse {
		r := &protocol.RegisterPortForwardResponse{Status: s}
		r.SetDetail([]byte(detail))
		return r
	}
	for _, c := range []struct {
		r    *protocol.RegisterPortForwardResponse
		want string
	}{
		{resp(protocol.OpenPortForwardStatus_LinkFailed, "runner: x"), "forward: ip link failed: runner: x"},
		{resp(protocol.OpenPortForwardStatus_BindFailed, "runner: x"), "forward: runner failed to bind the listen port: runner: x"},
		{resp(protocol.OpenPortForwardStatus_BindFailed, ""), "forward: runner failed to bind the listen port"},
		{resp(protocol.OpenPortForwardStatus_InternalError, "invalid ip link: y"), "forward: server refused: invalid ip link: y"},
		{resp(protocol.OpenPortForwardStatus_NoSuchTask, ""), "forward: no such task (id unknown or task not running)"},
		{resp(protocol.OpenPortForwardStatus_RunnerOffline, ""), "forward: runner offline"},
	} {
		err := registerResponseError(c.r)
		if err == nil || err.Error() != c.want {
			t.Errorf("status %v detail %q: %v, want %q", c.r.Status, c.r.Detail, err, c.want)
		}
	}
	if err := registerResponseError(resp(protocol.OpenPortForwardStatus_Ok, "")); err != nil {
		t.Errorf("Ok: %v", err)
	}
}

func TestClosedReasonLine(t *testing.T) {
	closed := func(r protocol.PortForwardCloseReason, detail string) *protocol.PortForwardClosed {
		c := &protocol.PortForwardClosed{Reason: r}
		c.SetDetail([]byte(detail))
		return c
	}
	for _, c := range []struct {
		in   *protocol.PortForwardClosed
		want string
	}{
		{closed(protocol.PortForwardCloseReason_Killed, ""), "forward stopped: killed remotely"},
		{closed(protocol.PortForwardCloseReason_TaskGone, ""), "forward stopped: task is no longer running"},
		{closed(protocol.PortForwardCloseReason_RunnerGone, ""), "forward stopped: the runner's connection ended"},
		{closed(protocol.PortForwardCloseReason_EndpointFailed, "runner: boom"), "forward stopped: the runner's end failed: runner: boom"},
	} {
		if got := closedReasonLine(c.in); got != c.want {
			t.Errorf("%v: %q, want %q", c.in.Reason, got, c.want)
		}
	}
}

// memTun is a tun stand-in for the client's registry tests.
type memTun struct {
	mu       sync.Mutex
	writes   [][]byte
	writeErr error
	once     sync.Once
	done     chan struct{}
}

func newMemTun() *memTun { return &memTun{done: make(chan struct{})} }
func (d *memTun) Read(p []byte) (int, error) {
	<-d.done
	return 0, os.ErrClosed
}
func (d *memTun) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	d.writes = append(d.writes, append([]byte(nil), p...))
	return len(p), nil
}
func (d *memTun) Close() error { d.once.Do(func() { close(d.done) }); return nil }
func (d *memTun) Name() string { return "mem0" }
func (d *memTun) Writes() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.writes...)
}

type nullSender struct{}

func (nullSender) SendDatagram([]byte) error { return nil }
func (nullSender) MaxDatagramSize() int      { return 1169 }

func datagramFor(id uint64, payload string) []byte {
	dg := protocol.ForwardDatagram{ForwardId: id, Payload: []byte(payload)}
	return dg.MustAppend(nil)
}

// Review Focus 3: two links in one process, each datagram to its own device.
func TestDispatchDatagramRoutesToItsLink(t *testing.T) {
	d7, d8 := newMemTun(), newMemTun()
	var failed []error
	registerIPLinkClient(7, &ipLinkClient{link: iplink.NewLink(7, d7, nullSender{}), fail: func(error) {}})
	registerIPLinkClient(8, &ipLinkClient{link: iplink.NewLink(8, d8, nullSender{}), fail: func(err error) { failed = append(failed, err) }})
	defer unregisterIPLinkClient(7)

	c := &Client{}
	c.dispatchDatagram(appwire.AppKind_ForwardDatagram, datagramFor(8, "for eight"))
	if w := d8.Writes(); len(w) != 1 || string(w[0]) != "for eight" {
		t.Fatalf("device 8 got %q", w)
	}
	if w := d7.Writes(); len(w) != 0 {
		t.Fatalf("device 7 got %q", w)
	}

	d8.mu.Lock()
	d8.writeErr = errors.New("tun gone")
	d8.mu.Unlock()
	c.dispatchDatagram(appwire.AppKind_ForwardDatagram, datagramFor(8, "fails"))
	if len(failed) != 1 || !strings.Contains(failed[0].Error(), "tun gone") {
		t.Fatalf("fail calls = %v", failed)
	}

	unregisterIPLinkClient(8)
	c.dispatchDatagram(appwire.AppKind_ForwardDatagram, datagramFor(8, "after"))
	if n := len(d8.Writes()); n != 1 {
		t.Fatalf("a datagram reached an unregistered link (%d writes)", n)
	}
}

func TestForwardDropsFindsIPLink(t *testing.T) {
	registerIPLinkClient(9, &ipLinkClient{link: iplink.NewLink(9, newMemTun(), nullSender{}), fail: func(error) {}})
	defer unregisterIPLinkClient(9)
	if _, ok := forwardDrops(9); !ok {
		t.Fatal("forwardDrops does not find the link")
	}
}
