// Package iplink carries whole IP packets between a tun device and a forward's
// datagram path. The client and the runner each hold one Link per ip forward:
// a packet read from the device goes out as a ForwardDatagram, and a
// ForwardDatagram's payload is written to the device.
//
// The pump never reads a packet. That is what makes stage 1 a transparent link
// rather than a hop, and what leaves room for a router at either end later.
package iplink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"runtime"
	"sync/atomic"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Device is one packet per Read and one packet per Write -- what a tun opened
// with IFF_TUN|IFF_NO_PI is.
type Device interface {
	io.ReadWriteCloser
	Name() string
}

// Opener creates and configures a device with addr (this end's address and the
// shared prefix length) and mtu. OpenTun is the real one; tests pass their own.
type Opener func(addr netip.Prefix, mtu int) (Device, error)

// ErrNotLinux is what OpenTun answers on any other OS.
var ErrNotLinux = errors.New("ip links are Linux-only")

// Supported reports whether this build can open a tun at all, so a caller can
// refuse before it dials anything.
func Supported() bool { return runtime.GOOS == "linux" }

// Sender is what a link needs from the connection: the congestion-controlled
// datagram send and the size that fits one.
type Sender interface {
	SendDatagram(b []byte) error
	MaxDatagramSize() int
}

// Link is one end of an ip forward.
type Link struct {
	// Drops is what this end refused to send, by cause -- the per-hop
	// numbers a `forward ls --drops` asks each endpoint for.
	Drops protocol.ForwardDropCounters

	forwardID uint64
	dev       Device
	send      Sender
	closed    atomic.Bool
}

// NewLink pairs a device with the connection a forward's datagrams cross.
func NewLink(forwardID uint64, dev Device, send Sender) *Link {
	return &Link{forwardID: forwardID, dev: dev, send: send}
}

// Name is the device's kernel name (hlink0, ...).
func (l *Link) Name() string { return l.dev.Name() }

// Run carries packets from the device to the wire until the device fails or
// the link is closed. It returns nil when the link was closed or ctx ended --
// both are someone deciding to stop -- and the device's error otherwise.
//
// Each packet goes out on the CONGESTION-CONTROLLED send: an IP link is not a
// sender whose rate is bounded by construction, and a packet dropped because
// the window was closed is the loss the inner TCP is built to recover from.
func (l *Link) Run(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()
	buf := make([]byte, 65535)
	for {
		n, err := l.dev.Read(buf)
		if err != nil {
			if l.closed.Load() {
				return nil
			}
			return fmt.Errorf("tun read: %w", err)
		}
		if n == 0 {
			continue
		}
		// flow_id is always 0: one registration is one pair of devices.
		dg := protocol.ForwardDatagram{ForwardId: l.forwardID, Payload: buf[:n]}
		b, err := dg.Append([]byte{byte(appwire.AppKind_ForwardDatagram)})
		if err != nil {
			continue
		}
		if len(b) > l.send.MaxDatagramSize() {
			// No fragmentation anywhere below this. Counted here because the
			// server never sees a datagram refused on this leg.
			l.Drops.NoteOversize()
			continue
		}
		l.Drops.NoteSendError(l.send.SendDatagram(b))
	}
}

// Deliver writes one packet that crossed the wire to the device.
func (l *Link) Deliver(payload []byte) error {
	if _, err := l.dev.Write(payload); err != nil {
		if l.closed.Load() {
			return nil
		}
		return fmt.Errorf("tun write: %w", err)
	}
	return nil
}

// Close releases the device. Idempotent. It marks the link closed BEFORE
// closing the device, so the read that Close interrupts is reported as a stop
// rather than as a failure.
func (l *Link) Close() error {
	if l.closed.Swap(true) {
		return nil
	}
	return l.dev.Close()
}

// UpLine is the line each end logs when its device is up. The name is picked by
// the kernel and is not on the wire, so this is where an operator learns it.
func UpLine(name string, addr netip.Prefix, mtu int) string {
	return fmt.Sprintf("ip link: %s up, %s, mtu %d", name, addr, mtu)
}
