//go:build linux

package iplink

import (
	"errors"
	"net/netip"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// Without CAP_NET_ADMIN the kernel refuses TUNSETIFF, and the error must say so
// verbatim: it is what reaches an operator as a runner's setup detail.
func TestOpenTunWithoutCapabilityIsEPERM(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the capability is held")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("no /dev/net/tun on this host")
	}
	dev, err := OpenTun(netip.MustParsePrefix("10.200.0.1/30"), 1100)
	if err == nil {
		_ = dev.Close()
		t.Skip("the tun opened: this process holds CAP_NET_ADMIN (a user namespace?)")
	}
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("OpenTun = %v, want EPERM", err)
	}
}
