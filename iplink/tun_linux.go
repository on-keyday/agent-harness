//go:build linux

package iplink

import (
	"fmt"
	"net"
	"net/netip"
	"os"

	"golang.org/x/sys/unix"
)

// tunNameTemplate lets the kernel pick the number: hlink0, hlink1, ...
const tunNameTemplate = "hlink%d"

type tunDevice struct {
	*os.File
	name string
}

func (d *tunDevice) Name() string { return d.name }

// OpenTun creates a non-persistent tun device, gives it addr and mtu, and brings
// it up. Non-persistent: the device disappears when its descriptor closes, so a
// crashed process leaves nothing behind.
//
// Needs CAP_NET_ADMIN over the network namespace; without it TUNSETIFF answers
// EPERM, which is returned wrapped (errors.Is holds) and names the call.
func OpenTun(addr netip.Prefix, mtu int) (Device, error) {
	if !addr.Addr().Is4() {
		return nil, fmt.Errorf("tun: %s is not an IPv4 address", addr)
	}
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(fd)
		}
	}()

	ifr, err := unix.NewIfreq(tunNameTemplate)
	if err != nil {
		return nil, fmt.Errorf("tun: %w", err)
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return nil, fmt.Errorf("TUNSETIFF: %w", err)
	}
	name := ifr.Name()
	if err := configure(name, addr, mtu); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	// Non-blocking BEFORE os.NewFile, so the descriptor goes on Go's poller and
	// Close unblocks a Read parked on it. A blocking fd would leave that Read
	// stuck until the next packet, and Ctrl-C would hang on an idle link.
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, fmt.Errorf("%s: set nonblocking: %w", name, err)
	}
	ok = true
	return &tunDevice{File: os.NewFile(uintptr(fd), "/dev/net/tun"), name: name}, nil
}

// configure gives the device its address, netmask and mtu and brings it up,
// through an AF_INET socket -- the ioctls a tun fd itself does not take.
func configure(name string, addr netip.Prefix, mtu int) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer unix.Close(sock)

	set := func(req uint, what string, fill func(*unix.Ifreq) error) error {
		ifr, err := unix.NewIfreq(name)
		if err != nil {
			return err
		}
		if err := fill(ifr); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if err := unix.IoctlIfreq(sock, req, ifr); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
	a4 := addr.Addr().As4()
	mask := net.CIDRMask(addr.Bits(), 32)
	if err := set(unix.SIOCSIFADDR, "SIOCSIFADDR", func(r *unix.Ifreq) error { return r.SetInet4Addr(a4[:]) }); err != nil {
		return err
	}
	if err := set(unix.SIOCSIFNETMASK, "SIOCSIFNETMASK", func(r *unix.Ifreq) error { return r.SetInet4Addr(mask) }); err != nil {
		return err
	}
	if err := set(unix.SIOCSIFMTU, "SIOCSIFMTU", func(r *unix.Ifreq) error { r.SetUint32(uint32(mtu)); return nil }); err != nil {
		return err
	}
	flags, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCGIFFLAGS, flags); err != nil {
		return fmt.Errorf("SIOCGIFFLAGS: %w", err)
	}
	up := flags.Uint16() | unix.IFF_UP
	return set(unix.SIOCSIFFLAGS, "SIOCSIFFLAGS", func(r *unix.Ifreq) error { r.SetUint16(up); return nil })
}
