package iplink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// fakeDevice is a tun stand-in: packets queued on in are read one per Read,
// and every Write lands on out.
type fakeDevice struct {
	in      chan []byte
	out     chan []byte
	readErr error
	once    sync.Once
	done    chan struct{}
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{in: make(chan []byte, 16), out: make(chan []byte, 16), done: make(chan struct{})}
}

func (d *fakeDevice) Read(p []byte) (int, error) {
	if d.readErr != nil {
		return 0, d.readErr
	}
	select {
	case pkt := <-d.in:
		return copy(p, pkt), nil
	case <-d.done:
		return 0, os.ErrClosed
	}
}

func (d *fakeDevice) Write(p []byte) (int, error) {
	select {
	case <-d.done:
		return 0, os.ErrClosed
	default:
	}
	d.out <- append([]byte(nil), p...)
	return len(p), nil
}

func (d *fakeDevice) Close() error { d.once.Do(func() { close(d.done) }); return nil }
func (d *fakeDevice) Name() string { return "fake0" }

var _ Device = (*fakeDevice)(nil)
var _ io.ReadWriteCloser = (*fakeDevice)(nil)

type fakeSender struct {
	mu   sync.Mutex
	max  int
	err  error
	sent [][]byte
}

func (s *fakeSender) SendDatagram(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, append([]byte(nil), b...))
	return nil
}
func (s *fakeSender) MaxDatagramSize() int { return s.max }
func (s *fakeSender) Sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.sent...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startLink(t *testing.T, dev *fakeDevice, send *fakeSender) (*Link, chan error) {
	t.Helper()
	l := NewLink(7, dev, send)
	done := make(chan error, 1)
	go func() { done <- l.Run(context.Background()) }()
	t.Cleanup(func() { _ = l.Close() })
	return l, done
}

func TestLinkCarriesPacketsBothWays(t *testing.T) {
	dev, send := newFakeDevice(), &fakeSender{max: 1169}
	l, _ := startLink(t, dev, send)

	pkt := []byte("an ip packet")
	dev.in <- pkt
	waitFor(t, func() bool { return len(send.Sent()) == 1 })
	b := send.Sent()[0]
	if appwire.AppKind(b[0]) != appwire.AppKind_ForwardDatagram {
		t.Fatalf("kind byte = %d", b[0])
	}
	var dg protocol.ForwardDatagram
	if err := dg.DecodeExact(b[1:]); err != nil {
		t.Fatal(err)
	}
	if dg.ForwardId != 7 || dg.FlowId != 0 || !bytes.Equal(dg.Payload, pkt) {
		t.Fatalf("datagram = %+v", dg)
	}

	if err := l.Deliver([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-dev.out:
		if string(got) != "reply" {
			t.Fatalf("device got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver wrote nothing")
	}
}

func TestLinkCountsOversize(t *testing.T) {
	dev, send := newFakeDevice(), &fakeSender{max: 100}
	l, _ := startLink(t, dev, send)
	dev.in <- make([]byte, 200)
	waitFor(t, func() bool { return l.Drops.Snapshot(7).DroppedOversize == 1 })
	if n := len(send.Sent()); n != 0 {
		t.Fatalf("%d datagrams sent, want none", n)
	}
}

func TestLinkCountsCongestion(t *testing.T) {
	dev, send := newFakeDevice(), &fakeSender{max: 1169, err: trsf.ErrCongestionBlocked}
	l, _ := startLink(t, dev, send)
	dev.in <- []byte("p")
	waitFor(t, func() bool { return l.Drops.Snapshot(7).DroppedCongestion == 1 })
}

// Review Focus 1: a deliberate close is not a failure.
func TestLinkRunReturnsNilAfterClose(t *testing.T) {
	dev, send := newFakeDevice(), &fakeSender{max: 1169}
	l, done := startLink(t, dev, send)
	time.Sleep(10 * time.Millisecond) // let Run block in Read
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Close = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Close")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestLinkRunReturnsDeviceError(t *testing.T) {
	dev, send := newFakeDevice(), &fakeSender{max: 1169}
	dev.readErr = errors.New("boom")
	_, done := startLink(t, dev, send)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("Run = %v, want an error wrapping boom", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on a device error")
	}
}

func TestUpLine(t *testing.T) {
	got := UpLine("hlink0", netip.MustParsePrefix("10.200.0.1/30"), 1100)
	if got != "ip link: hlink0 up, 10.200.0.1/30, mtu 1100" {
		t.Fatalf("UpLine = %q", got)
	}
}
