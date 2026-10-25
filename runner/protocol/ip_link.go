package protocol

import (
	"errors"
	"fmt"
	"net/netip"
)

// Hand-written beside the generated code because `protoregen` owns message.go
// alone; the same arrangement client_endpoint.go and task_kind.go use.

// CarriesDatagrams reports whether a forward of this protocol moves datagrams
// rather than a byte stream: udp, and ip (one IP packet per datagram).
//
// Every gate on datagram behaviour asks this rather than `== udp` -- the relay,
// the counter group a row carries, the datagram cell -- so a protocol added
// to the datagram path is one edit here, not a hunt for udp-only tests.
func (p ForwardProtocol) CarriesDatagrams() bool {
	return p == ForwardProtocol_Udp || p == ForwardProtocol_Ip
}

// IpLinkMTU is the MTU both tun devices of a link are set to. Fixed in stage 1:
// the largest IP packet that fits one datagram at the transport's initial MTU
// is 1157, so 1100 crosses every leg at its initial MTU or above without an
// oversize drop.
const IpLinkMTU = 1100

// errIPv6Link is the spec's verbatim text; both the client and the server say
// it.
var errIPv6Link = errors.New("ipv6 link addresses are not implemented")

// NewIpLinkConfig builds the wire form of a link whose client end is client
// (its address with the shared prefix length) and whose runner end is runner,
// and validates it.
func NewIpLinkConfig(client netip.Prefix, runner netip.Addr) (IpLinkConfig, error) {
	var c IpLinkConfig
	if client.Addr().Is6() || runner.Is6() {
		return c, errIPv6Link
	}
	ca, ra := client.Addr().As4(), runner.As4()
	c.SetClientAddr(ca[:])
	c.SetRunnerAddr(ra[:])
	c.PrefixLen = uint8(client.Bits())
	c.Mtu = IpLinkMTU
	return c, c.Validate()
}

// Validate reports why a link cannot be brought up as configured. The client
// runs it before creating anything and the server runs it again, because a
// hand-built request can skip the client.
func (c *IpLinkConfig) Validate() error {
	switch {
	case c.AddrLen == 16:
		return errIPv6Link
	case c.AddrLen != 4:
		return fmt.Errorf("ip link: address length %d is neither 4 nor 16", c.AddrLen)
	case len(c.ClientAddr) != 4 || len(c.RunnerAddr) != 4:
		return fmt.Errorf("ip link: addresses are %d and %d bytes, want 4", len(c.ClientAddr), len(c.RunnerAddr))
	}
	if c.Mtu != IpLinkMTU {
		return fmt.Errorf("ip link: mtu %d is not %d (fixed in this version)", c.Mtu, IpLinkMTU)
	}
	// /32 is refused: the two ends would share no subnet, so each kernel would
	// need an explicit route to the other, and the harness adds no routes.
	if c.PrefixLen < 1 || c.PrefixLen > 31 {
		return fmt.Errorf("ip link: prefix /%d is outside 1-31 (a /32 shares no subnet with the peer)", c.PrefixLen)
	}
	client, runner := netip.AddrFrom4([4]byte(c.ClientAddr)), netip.AddrFrom4([4]byte(c.RunnerAddr))
	if client == runner {
		return fmt.Errorf("ip link: the two addresses are the same (%s)", client)
	}
	subnet := netip.PrefixFrom(client, int(c.PrefixLen)).Masked()
	if !subnet.Contains(runner) {
		return fmt.Errorf("ip link: %s is not inside %s", runner, subnet)
	}
	// A /31 has neither a network nor a broadcast address (RFC 3021).
	if c.PrefixLen < 31 {
		network, broadcast := subnet.Addr(), lastAddr(subnet)
		for _, a := range []netip.Addr{client, runner} {
			switch a {
			case network:
				return fmt.Errorf("ip link: %s is the network address of %s", a, subnet)
			case broadcast:
				return fmt.Errorf("ip link: %s is the broadcast address of %s", a, subnet)
			}
		}
	}
	return nil
}

// ClientPrefix is the client end's address with the shared prefix length; the
// zero Prefix when the config carries no usable address.
func (c *IpLinkConfig) ClientPrefix() netip.Prefix { return endPrefix(c.ClientAddr, c) }

// RunnerPrefix is the runner end's address with the shared prefix length.
func (c *IpLinkConfig) RunnerPrefix() netip.Prefix { return endPrefix(c.RunnerAddr, c) }

func endPrefix(b []byte, c *IpLinkConfig) netip.Prefix {
	if (c.AddrLen != 4 && c.AddrLen != 16) || len(b) != int(c.AddrLen) {
		return netip.Prefix{}
	}
	a, _ := netip.AddrFromSlice(b)
	return netip.PrefixFrom(a, int(c.PrefixLen))
}

// lastAddr is the highest address inside an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= uint32(1)<<host - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
